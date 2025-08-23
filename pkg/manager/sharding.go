package manager

import (
	"time"

	"sigs.k8s.io/multicluster-runtime/pkg/manager/sharder"
)

// Option mutates mcManager after construction.
type Option func(*mcManager)

// WithSharder replaces the default HRW sharder.
func WithSharder(s sharder.Sharder) Option { return func(m *mcManager) { m.sharder = s } }

// WithPeerWeight allows heterogenous peers.
func WithPeerWeight(w uint32) Option { return func(m *mcManager) { m.peerWeight = w } }

// WithShardLease configures the shard lease name/namespace (for fencing)
func WithShardLease(ns, name string) Option {
	return func(m *mcManager) { m.shardLeaseNS, m.shardLeaseName = ns, name }
}

// WithPerClusterLease enables/disables per-cluster fencing.
func WithPerClusterLease(on bool) Option { return func(m *mcManager) { m.perClusterLease = on } }

// WithOwnershipIntervals tunes loop cadences.
func WithOwnershipIntervals(probe, rehash time.Duration) Option {
	return func(m *mcManager) { m.ownershipProbe = probe; m.ownershipRehash = rehash }
}

// WithLeaseTimings configures fencing lease timings.
func WithLeaseTimings(duration, renew, throttle time.Duration) Option {
	return func(m *mcManager) {
		m.leaseDuration = duration
		m.leaseRenew = renew
		m.fenceThrottle = throttle
	}
}

// Configure applies options to a Manager if it is an *mcManager.
func Configure(m Manager, opts ...Option) {
	if x, ok := m.(*mcManager); ok {
		for _, o := range opts {
			o(x)
		}
	}
}
