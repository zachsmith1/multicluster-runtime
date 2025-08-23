# Sharded Namespace Example

This demo runs two replicas of a multicluster manager that **split ownership** of downstream "clusters" discovered by the *namespace provider* (each Kubernetes Namespace == one cluster). Ownership is decided by HRW hashing across peers, then serialized with per-cluster fencing Leases so exactly one peer reconciles a given cluster at a time.

**Key Components:**
- **Peer membership (presence)**: `coordination.k8s.io/Lease` with prefix `mcr-peer-*`
- **Per-cluster fencing (ownership)**: `coordination.k8s.io/Lease` with prefix `mcr-shard-<namespace>`

Controllers attach per-cluster watches when ownership starts, and cleanly detach & re-attach when ownership transfers.

## Build the image

From repo root:

```bash
docker build -t mcr-namespace:dev -f examples/sharded-namespace/Dockerfile .
```

If using KinD:
```bash
kind create cluster --name mcr-demo
kind load docker-image mcr-namespace:dev --name mcr-demo
```

## Deploy

```bash
kubectl apply -k examples/sharded-namespace/manifests
```

## Observe

Tail logs from pods:

```bash
kubectl -n mcr-demo get pods
kubectl -n mcr-demo logs statefulset/sharded-namespace -f
```

You should see lines like:
```bash
"ownership start    {"cluster": "zoo", "peer": "sharded-namespace-0"}"
"ownership start    {"cluster": "jungle", "peer": "sharded-namespace-1"}"

Reconciling ConfigMap    {"controller": "multicluster-configmaps", "controllerGroup": "", "controllerKind": "ConfigMap", "reconcileID": "4f1116b3-b5 │
│ 4e-4e6a-b84f-670ca5cfc9ce", "cluster": "zoo", "ns": "default", "name": "elephant"}  

Reconciling ConfigMap    {"controller": "multicluster-configmaps", "controllerGroup": "", "controllerKind": "ConfigMap", "reconcileID": "688b8467-f5 │
│ d3-491b-989e-87bc8aad780e", "cluster": "jungle", "ns": "default", "name": "monkey"} 
```

Check Leases:
```bash
# Peer membership (one per pod)
kubectl -n kube-system get lease | grep '^mcr-peer-'

# Per-cluster fencing (one per namespace/"cluster")
kubectl -n kube-system get lease | grep '^mcr-shard-'
```

Who owns a given cluster?
```bash
C=zoo
kubectl -n kube-system get lease mcr-shard-$C \
  -o custom-columns=HOLDER:.spec.holderIdentity,RENEW:.spec.renewTime
```


## Test Ownership

Scale down to 1 replica and watch ownership consolidate:
```bash
# Scale down
kubectl -n mcr-demo scale statefulset/sharded-namespace --replicas=1


# Watch leases lose their holders as pods terminate
watch 'kubectl -n kube-system get lease -o custom-columns=NAME:.metadata.name,HOLDER:.spec.holderIdentity | grep "^mcr-shard-"'

# Wait for all clusters to be owned by the single remaining pod (~30s)
kubectl -n kube-system wait --for=jsonpath='{.spec.holderIdentity}'=sharded-namespace-0 \
 lease/mcr-shard-zoo lease/mcr-shard-jungle lease/mcr-shard-island --timeout=60s

```
Create/patch a ConfigMap and confirm the single owner reconciles it:
```bash
# Pick a cluster and create a test ConfigMap
C=island
kubectl -n "$C" create cm test-$(date +%s) --from-literal=ts=$(date +%s) --dry-run=client -oyaml | kubectl apply -f -

# Verify only pod-q reconciles it (since it owns everything now)
kubectl -n mcr-demo logs pod/sharded-namespace-0 --since=100s | grep "Reconciling ConfigMap.*$C"
```

Scale up to 3 replicas and watch ownership rebalance:
```bash
# Scale up
kubectl -n mcr-demo scale statefulset/sharded-namespace --replicas=3
# Watch leases regain holders as pods start
watch 'kubectl -n kube-system get lease -o custom-columns=NAME:.metadata.name,HOLDER:.spec.holderIdentity | grep "^mcr-shard-"'

# Create a cm in the default ns which belongs to sharded-namespace-2
C=default
kubectl -n "$C" create cm test-$(date +%s) --from-literal=ts=$(date +%s) --dry-run=client -oyaml | kubectl apply -f -
# Verify only pod-2 reconciles it (since it owns the default ns now)
kubectl -n mcr-demo logs pod/sharded-namespace-2 --since=100s | grep "Reconciling ConfigMap.*$C"
```

## Tuning
In your example app (e.g., examples/sharded-namespace/main.go), configure fencing and timings:

```go
mcmanager.Configure(mgr,
  // Per-cluster fencing Leases live here as mcr-shard-<namespace>
  mcmanager.WithShardLease("kube-system", "mcr-shard"),
  mcmanager.WithPerClusterLease(true), // enabled by default
  
  // Optional: tune fencing timings (duration, renew, throttle):
  // mcmanager.WithLeaseTimings(30*time.Second, 10*time.Second, 750*time.Millisecond),
  
  // Optional: peer weight for HRW:
  // mcmanager.WithPeerWeight(1),
)
```

The peer registry uses mcr-peer-* automatically and derives the peer ID from the pod hostname (StatefulSet ordinal).

## Cleanup

```bash
kubectl delete -k examples/sharded-namespace/manifests
kind delete cluster --name mcr-demo

```

## Notes

- This example assumes the `peers` and `sharder` code we wrote is integrated and `mcmanager.Configure` hooks it up (defaults: HRW + Lease registry; hostname as peer ID).
- If your repo uses a different module path, the Dockerfile build context may need minor tweaking—otherwise it compiles against your local packages.
