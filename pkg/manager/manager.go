/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manager

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	"sigs.k8s.io/multicluster-runtime/pkg/manager/peers"
	"sigs.k8s.io/multicluster-runtime/pkg/manager/sharder"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// LocalCluster is the name of the local cluster.
const LocalCluster = ""

// Manager is a multi-cluster-aware manager, like the controller-runtime Cluster,
// but without the direct embedding of cluster.Cluster.
type Manager interface {
	// Add will set requested dependencies on the component, and cause the component to be
	// started when Start is called.
	// Depending on if a Runnable implements LeaderElectionRunnable interface, a Runnable can be run in either
	// non-leaderelection mode (always running) or leader election mode (managed by leader election if enabled).
	Add(Runnable) error

	// Elected is closed when this manager is elected leader of a group of
	// managers, either because it won a leader election or because no leader
	// election was configured.
	Elected() <-chan struct{}

	// AddMetricsServerExtraHandler adds an extra handler served on path to the http server that serves metrics.
	// Might be useful to register some diagnostic endpoints e.g. pprof.
	//
	// Note that these endpoints are meant to be sensitive and shouldn't be exposed publicly.
	//
	// If the simple path -> handler mapping offered here is not enough,
	// a new http server/listener should be added as Runnable to the manager via Add method.
	AddMetricsServerExtraHandler(path string, handler http.Handler) error

	// AddHealthzCheck allows you to add Healthz checker
	AddHealthzCheck(name string, check healthz.Checker) error

	// AddReadyzCheck allows you to add Readyz checker
	AddReadyzCheck(name string, check healthz.Checker) error

	// Start starts all registered Controllers and blocks until the context is cancelled.
	// Returns an error if there is an error starting any controller.
	//
	// If LeaderElection is used, the binary must be exited immediately after this returns,
	// otherwise components that need leader election might continue to run after the leader
	// lock was lost.
	Start(ctx context.Context) error

	// GetWebhookServer returns a webhook.Server
	GetWebhookServer() webhook.Server

	// GetLogger returns this manager's logger.
	GetLogger() logr.Logger

	// GetControllerOptions returns controller global configuration options.
	GetControllerOptions() config.Controller

	// GetCluster returns a cluster for the given identifying cluster name. Get
	// returns an existing cluster if it has been created before.
	// If no cluster is known to the provider under the given cluster name,
	// an error should be returned.
	GetCluster(ctx context.Context, clusterName string) (cluster.Cluster, error)

	// ClusterFromContext returns the default cluster set in the context.
	ClusterFromContext(ctx context.Context) (cluster.Cluster, error)

	// GetManager returns a manager for the given cluster name.
	GetManager(ctx context.Context, clusterName string) (manager.Manager, error)

	// GetLocalManager returns the underlying controller-runtime manager of the
	// host. This is equivalent to GetManager(LocalCluster).
	GetLocalManager() manager.Manager

	// GetProvider returns the multicluster provider, or nil if it is not set.
	GetProvider() multicluster.Provider

	// GetFieldIndexer returns a client.FieldIndexer that adds indexes to the
	// multicluster provider (if set) and the local manager.
	GetFieldIndexer() client.FieldIndexer

	multicluster.Aware
}

// Options are the arguments for creating a new Manager.
type Options = manager.Options

// Runnable allows a component to be started.
// It's very important that Start blocks until
// it's done running.
type Runnable interface {
	manager.Runnable
	multicluster.Aware
}

var _ Manager = &mcManager{}

type mcManager struct {
	manager.Manager
	provider multicluster.Provider

	mcRunnables []multicluster.Aware

	peers   peerRegistry
	sharder sharder.Sharder
	self    sharder.PeerInfo

	engMu   sync.Mutex
	engaged map[string]*engagement

	shardLeaseNS, shardLeaseName string
	perClusterLease              bool

	leaseDuration time.Duration
	leaseRenew    time.Duration
	fenceThrottle time.Duration

	peerLeasePrefix                 string
	peerWeight                      uint32
	ownershipProbe, ownershipRehash time.Duration
}

type engagement struct {
	name    string
	cl      cluster.Cluster
	ctx     context.Context
	cancel  context.CancelFunc
	started bool

	fence   *leaseGuard
	nextTry time.Time
}

func (m *mcManager) fenceName(cluster string) string {
	if m.perClusterLease {
		return fmt.Sprintf("%s-%s", m.shardLeaseName, cluster)
	}
	return m.shardLeaseName
}

type peerRegistry interface {
	Self() sharder.PeerInfo
	Snapshot() []sharder.PeerInfo
	Run(ctx context.Context) error
}

// New returns a new Manager for creating Controllers. The provider is used to
// discover and manage clusters. With a provider set to nil, the manager will
// behave like a regular controller-runtime manager.
func New(config *rest.Config, provider multicluster.Provider, opts manager.Options) (Manager, error) {
	mgr, err := manager.New(config, opts)
	if err != nil {
		return nil, err
	}
	return WithMultiCluster(mgr, provider)
}

// WithMultiCluster wraps a host manager to run multi-cluster controllers.
func WithMultiCluster(mgr manager.Manager, provider multicluster.Provider) (Manager, error) {
	m := &mcManager{
		Manager:  mgr,
		provider: provider,
		sharder:  sharder.NewHRW(),
		engaged:  map[string]*engagement{},

		shardLeaseNS:   "kube-system",
		shardLeaseName: "mcr-shard",

		peerLeasePrefix: "mcr-peer",
		perClusterLease: true,

		leaseDuration: 20 * time.Second,
		leaseRenew:    10 * time.Second,
		fenceThrottle: 750 * time.Millisecond,

		peerWeight:      1,
		ownershipProbe:  5 * time.Second,
		ownershipRehash: 15 * time.Second,
	}

	m.peers = peers.NewLeaseRegistry(mgr.GetClient(), m.shardLeaseNS, m.peerLeasePrefix, "", m.peerWeight)
	m.self = m.peers.Self()

	// Start ownership loop as a manager Runnable.
	if err := mgr.Add(m.newOwnershipRunnable()); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *mcManager) newOwnershipRunnable() manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		m.GetLogger().Info("ownership runnable starting", "peer", m.self.ID)
		errCh := make(chan error, 1)
		go func() { errCh <- m.peers.Run(ctx) }()

		m.recomputeOwnership(ctx)
		t := time.NewTicker(m.ownershipProbe)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-errCh:
				if err != nil {
					return fmt.Errorf("peer registry stopped: %w", err)
				}
				return nil
			case <-t.C:
				m.GetLogger().V(1).Info("ownership tick", "peers", len(m.peers.Snapshot()))
				m.recomputeOwnership(ctx)
			}
		}
	})
}

func (m *mcManager) recomputeOwnership(parent context.Context) {
	peers := m.peers.Snapshot()
	self := m.self

	type toStart struct {
		name string
		cl   cluster.Cluster
		ctx  context.Context
	}
	var starts []toStart
	var stops []context.CancelFunc

	now := time.Now()

	m.engMu.Lock()
	for name, e := range m.engaged {
		should := m.sharder.ShouldOwn(name, peers, self)

		switch {
		case should && !e.started:
			// ensure fence exists
			if e.fence == nil {
				onLost := func(cluster string) func() {
					return func() {
						// best-effort stop if we lose the lease mid-flight
						m.GetLogger().Info("lease lost; stopping", "cluster", cluster, "peer", self.ID)
						m.engMu.Lock()
						if ee := m.engaged[cluster]; ee != nil && ee.started && ee.cancel != nil {
							ee.cancel()
							ee.started = false
						}
						m.engMu.Unlock()
					}
				}(name)
				e.fence = newLeaseGuard(
					m.Manager.GetClient(),
					m.shardLeaseNS, m.fenceName(name), m.self.ID,
					m.leaseDuration, m.leaseRenew, onLost,
				)
			}

			// throttle attempts
			if now.Before(e.nextTry) {
				continue
			}

			// try to take the fence; if we fail, set nextTry and retry on next tick
			if !e.fence.TryAcquire(parent) {
				e.nextTry = now.Add(m.fenceThrottle)
				continue
			}

			// acquired fence; start the cluster
			ctx, cancel := context.WithCancel(parent)
			e.ctx, e.cancel, e.started = ctx, cancel, true
			starts = append(starts, toStart{name: e.name, cl: e.cl, ctx: ctx})
			m.GetLogger().Info("ownership start", "cluster", name, "peer", self.ID)

		case !should && e.started:
			// stop + release fence
			if e.cancel != nil {
				stops = append(stops, e.cancel)
			}
			if e.fence != nil {
				go e.fence.Release(parent)
			}
			e.cancel = nil
			e.started = false
			e.nextTry = time.Time{} // reset throttle so new owner can grab quickly
			m.GetLogger().Info("ownership stop", "cluster", name, "peer", self.ID)
		}
	}
	m.engMu.Unlock()

	for _, c := range stops {
		c()
	}
	for _, s := range starts {
		go m.startForCluster(s.ctx, s.name, s.cl)
	}
}

// GetCluster returns a cluster for the given identifying cluster name. Get
// returns an existing cluster if it has been created before.
// If no cluster is known to the provider under the given cluster name,
// an error should be returned.
func (m *mcManager) GetCluster(ctx context.Context, clusterName string) (cluster.Cluster, error) {
	if clusterName == LocalCluster {
		return m.Manager, nil
	}
	if m.provider == nil {
		return nil, fmt.Errorf("no multicluster provider set, but cluster %q passed", clusterName)
	}
	return m.provider.Get(ctx, clusterName)
}

// ClusterFromContext returns the default cluster set in the context.
func (m *mcManager) ClusterFromContext(ctx context.Context) (cluster.Cluster, error) {
	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("no cluster set in context, use ReconcilerWithCluster helper when building the controller")
	}
	return m.GetCluster(ctx, clusterName)
}

// GetLocalManager returns the underlying controller-runtime manager of the host.
func (m *mcManager) GetLocalManager() manager.Manager {
	return m.Manager
}

// GetProvider returns the multicluster provider, or nil if it is not set.
func (m *mcManager) GetProvider() multicluster.Provider {
	return m.provider
}

// Add will set requested dependencies on the component, and cause the component to be
// started when Start is called.
func (m *mcManager) Add(r Runnable) (err error) {
	m.mcRunnables = append(m.mcRunnables, r)
	defer func() {
		if err != nil {
			m.mcRunnables = m.mcRunnables[:len(m.mcRunnables)-1]
		}
	}()

	return m.Manager.Add(r)
}

// Engage gets called when the component should start operations for the given
// Cluster. ctx is cancelled when the cluster is disengaged.
func (m *mcManager) Engage(parent context.Context, name string, cl cluster.Cluster) error {
	m.engMu.Lock()
	e, ok := m.engaged[name]
	if !ok {
		e = &engagement{name: name, cl: cl}
		m.engaged[name] = e

		// cleanup when provider disengages the cluster
		go func(pctx context.Context, key string) {
			<-pctx.Done()
			m.engMu.Lock()
			if ee, ok := m.engaged[key]; ok {
				if ee.started && ee.cancel != nil {
					ee.cancel()
				}
				if ee.fence != nil {
					// best-effort release fence so a new owner can acquire immediately
					go ee.fence.Release(context.Background())
				}
				delete(m.engaged, key)
			}
			m.engMu.Unlock()
		}(parent, name)
	} else {
		// provider may hand a new cluster impl for the same name
		e.cl = cl
	}
	m.engMu.Unlock()
	// No starting here; recomputeOwnership() will start/stop safely.
	return nil
}

func (m *mcManager) startForCluster(ctx context.Context, name string, cl cluster.Cluster) {
	for _, r := range m.mcRunnables {
		if err := r.Engage(ctx, name, cl); err != nil {
			m.GetLogger().Error(err, "failed to engage", "cluster", name)
			// best-effort: cancel + mark stopped so next tick can retry
			m.engMu.Lock()
			if e := m.engaged[name]; e != nil && e.cancel != nil {
				e.cancel()
				e.started = false
			}
			m.engMu.Unlock()
			return
		}
	}
}

func (m *mcManager) GetManager(ctx context.Context, clusterName string) (manager.Manager, error) {
	cl, err := m.GetCluster(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	return &scopedManager{
		Manager: m,
		Cluster: cl,
	}, nil
}

type fieldIndexerFunc func(context.Context, client.Object, string, client.IndexerFunc) error

func (f fieldIndexerFunc) IndexField(ctx context.Context, obj client.Object, fieldName string, indexerFunc client.IndexerFunc) error {
	return f(ctx, obj, fieldName, indexerFunc)
}

// GetFieldIndexer returns a client.FieldIndexer that adds indexes to the
// multicluster provider (if set) and to the local cluster if not.
func (m *mcManager) GetFieldIndexer() client.FieldIndexer {
	return fieldIndexerFunc(func(ctx context.Context, obj client.Object, fieldName string, indexerFunc client.IndexerFunc) error {
		if m.provider != nil {
			if err := m.provider.IndexField(ctx, obj, fieldName, indexerFunc); err != nil {
				return fmt.Errorf("failed to index field %q on multi-cluster provider: %w", fieldName, err)
			}
			return nil
		}
		return m.Manager.GetFieldIndexer().IndexField(ctx, obj, fieldName, indexerFunc)
	})
}

var _ manager.Manager = &scopedManager{}

type scopedManager struct {
	Manager
	cluster.Cluster
}

// Add adds a Runnable to the manager.
func (p *scopedManager) Add(r manager.Runnable) error {
	return p.Manager.GetLocalManager().Add(r)
}

// Start starts the manager.
func (p *scopedManager) Start(ctx context.Context) error {
	return p.Manager.GetLocalManager().Start(ctx)
}

// GetFieldIndexer returns the field indexer.
func (p *scopedManager) GetFieldIndexer() client.FieldIndexer {
	return p.Cluster.GetFieldIndexer()
}
