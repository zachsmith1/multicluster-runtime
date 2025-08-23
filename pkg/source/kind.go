package source

import (
	"context"
	"sync"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	crsource "sigs.k8s.io/controller-runtime/pkg/source"

	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// Kind creates a KindSource with the given cache provider.
func Kind[O client.Object](
	obj O,
	hf mchandler.TypedEventHandlerFunc[O, mcreconcile.Request],
	preds ...predicate.TypedPredicate[O],
) SyncingSource[O] {
	return TypedKind[O, mcreconcile.Request](obj, hf, preds...)
}

// TypedKind creates a KindSource with the given cache provider.
func TypedKind[O client.Object, R mcreconcile.ClusterAware[R]](
	obj O,
	hf mchandler.TypedEventHandlerFunc[O, R],
	preds ...predicate.TypedPredicate[O],
) TypedSyncingSource[O, R] {
	return &kind[O, R]{
		obj:        obj,
		handler:    hf,
		predicates: preds,
		project:    func(_ cluster.Cluster, o O) (O, error) { return o, nil },
		resync:     0, // no periodic resync by default
	}
}

type kind[O client.Object, R mcreconcile.ClusterAware[R]] struct {
	obj        O
	handler    mchandler.TypedEventHandlerFunc[O, R]
	predicates []predicate.TypedPredicate[O]
	project    func(cluster.Cluster, O) (O, error)
	resync     time.Duration
}

type clusterKind[O client.Object, R mcreconcile.ClusterAware[R]] struct {
	clusterName string
	cl          cluster.Cluster
	obj         O
	h           handler.TypedEventHandler[O, R]
	preds       []predicate.TypedPredicate[O]
	resync      time.Duration

	mu           sync.Mutex
	registration toolscache.ResourceEventHandlerRegistration
	activeCtx    context.Context
}

// WithProjection sets the projection function for the KindSource.
func (k *kind[O, R]) WithProjection(project func(cluster.Cluster, O) (O, error)) TypedSyncingSource[O, R] {
	k.project = project
	return k
}

func (k *kind[O, R]) ForCluster(name string, cl cluster.Cluster) (crsource.TypedSource[R], error) {
	obj, err := k.project(cl, k.obj)
	if err != nil {
		return nil, err
	}
	return &clusterKind[O, R]{
		clusterName: name,
		cl:          cl,
		obj:         obj,
		h:           k.handler(name, cl),
		preds:       k.predicates,
		resync:      k.resync,
	}, nil
}

func (k *kind[O, R]) SyncingForCluster(name string, cl cluster.Cluster) (crsource.TypedSyncingSource[R], error) {
	src, err := k.ForCluster(name, cl)
	if err != nil {
		return nil, err
	}
	return src.(crsource.TypedSyncingSource[R]), nil
}

// WaitForSync satisfies TypedSyncingSource.
func (ck *clusterKind[O, R]) WaitForSync(ctx context.Context) error {
	if !ck.cl.GetCache().WaitForCacheSync(ctx) {
		return ctx.Err()
	}
	return nil
}

// Start registers a removable handler on the (scoped) informer and removes it on ctx.Done().
func (ck *clusterKind[O, R]) Start(ctx context.Context, q workqueue.TypedRateLimitingInterface[R]) error {
	log := log.FromContext(ctx).WithValues("cluster", ck.clusterName, "source", "kind")

	// Check if we're already started with this context
	ck.mu.Lock()
	if ck.registration != nil && ck.activeCtx != nil {
		// Check if the active context is still valid
		select {
		case <-ck.activeCtx.Done():
			// Previous context cancelled, need to clean up and re-register
			log.V(1).Info("previous context cancelled, cleaning up for re-registration")
			// Clean up old registration is handled below
		default:
			// Still active with same context - check if it's the same context
			if ck.activeCtx == ctx {
				ck.mu.Unlock()
				log.V(1).Info("handler already registered with same context")
				return nil
			}
			// Different context but old one still active - this shouldn't happen
			log.V(1).Info("different context while old one active, will re-register")
		}
	}
	ck.mu.Unlock()

	inf, err := ck.getInformer(ctx, ck.obj)
	if err != nil {
		log.Error(err, "get informer failed")
		return err
	}

	// If there's an old registration, remove it first
	ck.mu.Lock()
	if ck.registration != nil {
		log.V(1).Info("removing old event handler registration")
		if err := inf.RemoveEventHandler(ck.registration); err != nil {
			log.Error(err, "failed to remove old event handler")
		}
		ck.registration = nil
		ck.activeCtx = nil
	}
	ck.mu.Unlock()

	// predicate helpers
	passCreate := func(e event.TypedCreateEvent[O]) bool {
		for _, p := range ck.preds {
			if !p.Create(e) {
				return false
			}
		}
		return true
	}
	passUpdate := func(e event.TypedUpdateEvent[O]) bool {
		for _, p := range ck.preds {
			if !p.Update(e) {
				return false
			}
		}
		return true
	}
	passDelete := func(e event.TypedDeleteEvent[O]) bool {
		for _, p := range ck.preds {
			if !p.Delete(e) {
				return false
			}
		}
		return true
	}

	// typed event builders
	makeCreate := func(o client.Object) event.TypedCreateEvent[O] {
		return event.TypedCreateEvent[O]{Object: any(o).(O)}
	}
	makeUpdate := func(oo, no client.Object) event.TypedUpdateEvent[O] {
		return event.TypedUpdateEvent[O]{ObjectOld: any(oo).(O), ObjectNew: any(no).(O)}
	}
	makeDelete := func(o client.Object) event.TypedDeleteEvent[O] {
		return event.TypedDeleteEvent[O]{Object: any(o).(O)}
	}

	// Adapter that forwards to controller handler, honoring ctx.
	h := toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(i interface{}) {
			if ctx.Err() != nil {
				return
			}
			if o, ok := i.(client.Object); ok {
				e := makeCreate(o)
				if passCreate(e) {
					ck.h.Create(ctx, e, q)
				}
			}
		},
		UpdateFunc: func(oo, no interface{}) {
			if ctx.Err() != nil {
				return
			}
			ooObj, ok1 := oo.(client.Object)
			noObj, ok2 := no.(client.Object)
			if ok1 && ok2 {
				log.V(1).Info("kind source update", "cluster", ck.clusterName,
					"name", noObj.GetName(), "ns", noObj.GetNamespace())
				e := makeUpdate(ooObj, noObj)
				if passUpdate(e) {
					ck.h.Update(ctx, e, q)
				}
			}
		},
		DeleteFunc: func(i interface{}) {
			if ctx.Err() != nil {
				return
			}
			// be robust to tombstones (provider should already unwrap)
			if ts, ok := i.(toolscache.DeletedFinalStateUnknown); ok {
				i = ts.Obj
			}
			if o, ok := i.(client.Object); ok {
				e := makeDelete(o)
				if passDelete(e) {
					ck.h.Delete(ctx, e, q)
				}
			}
		},
	}

	// Register via removable API.
	reg, addErr := inf.AddEventHandlerWithResyncPeriod(h, ck.resync)
	if addErr != nil {
		log.Error(addErr, "AddEventHandlerWithResyncPeriod failed")
		return addErr
	}

	// Store registration and context
	ck.mu.Lock()
	ck.registration = reg
	ck.activeCtx = ctx
	ck.mu.Unlock()

	log.V(1).Info("kind source handler registered", "hasRegistration", reg != nil)

	// Defensive: ensure cache is synced.
	if !ck.cl.GetCache().WaitForCacheSync(ctx) {
		ck.mu.Lock()
		_ = inf.RemoveEventHandler(ck.registration)
		ck.registration = nil
		ck.activeCtx = nil
		ck.mu.Unlock()
		log.V(1).Info("cache not synced; handler removed")
		return ctx.Err()
	}
	log.V(1).Info("kind source cache synced")

	// Wait for context cancellation in a goroutine
	go func() {
		<-ctx.Done()
		ck.mu.Lock()
		defer ck.mu.Unlock()

		// Only remove if this is still our active registration
		if ck.activeCtx == ctx && ck.registration != nil {
			if err := inf.RemoveEventHandler(ck.registration); err != nil {
				log.Error(err, "failed to remove event handler on context cancel")
			}
			ck.registration = nil
			ck.activeCtx = nil
			log.V(1).Info("kind source handler removed due to context cancellation")
		}
	}()

	return nil
}

// getInformer resolves the informer from the cluster cache (provider returns a scoped informer).
func (ck *clusterKind[O, R]) getInformer(ctx context.Context, obj client.Object) (crcache.Informer, error) {
	return ck.cl.GetCache().GetInformer(ctx, obj)
}
