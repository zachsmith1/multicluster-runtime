# Sharded Kind Example

This demo runs two local processes of a multicluster manager that split ownership of downstream "clusters" discovered by the Kind provider (each Kind cluster == one multicluster "cluster"). Synchronization is decided by HRW hashing across peers, then serialized with per-cluster fencing Leases so exactly one peer reconciles a given cluster at a time.

**Key Components:**
- **Peer membership (presence)**: `coordination.k8s.io/Lease` with prefix `mcr-peer-*` in a chosen coordination cluster
- **Per-cluster fencing (synchronization)**: `coordination.k8s.io/Lease` with prefix `mcr-shard-<cluster>` in the same coordination cluster

Controllers attach per-cluster watches when synchronization starts, and cleanly detach & re-attach when it transfers.

## Prerequisites

- Docker running
- [Kind](https://kind.sigs.k8s.io/) installed
- `kubectl`
- Go 1.21+

## Create Kind clusters

Create a few clusters that the example will discover (use a common prefix):

```bash
kind create cluster --name fleet-alpha
kind create cluster --name fleet-beta
kind create cluster --name fleet-gamma
```

Pick one cluster to be the coordination cluster (where peer/fence Leases live) and set your current context to it:

```bash
kubectl config use-context kind-fleet-alpha
```

## Run

Open two terminals and run one instance per terminal with distinct peer IDs. Limit discovery to clusters with the `fleet-` prefix so we don't attach to unrelated Kind clusters on your machine.

Important: run from the example directory (so the example’s `go.mod` replace directives are used), or use Go’s `-C` flag from repo root.

Terminal A:
```bash
# Option A: from the example directory
cd examples/sharded-kind
PEER_ID=peer-a KIND_PREFIX=fleet- go run .

# Option B: from repo root using -C
PEER_ID=peer-a KIND_PREFIX=fleet- go run -C examples/sharded-kind .
```

Terminal B:
```bash
# Option A: from the example directory
cd examples/sharded-kind
PEER_ID=peer-b KIND_PREFIX=fleet- go run .

# Option B: from repo root using -C
PEER_ID=peer-b KIND_PREFIX=fleet- go run -C examples/sharded-kind .
```

Each process:
- Discovers all local Kind clusters whose names start with `fleet-`
- Uses the current kubeconfig context (e.g., `kind-fleet-alpha`) to coordinate via Leases in `kube-system`
- Competes for per-cluster fences so that only one peer runs each cluster at a time

## Observe

You should see logs like:

```bash
synchronization start  {"cluster":"fleet-beta","peer":"peer-a"}
synchronization start  {"cluster":"fleet-gamma","peer":"peer-b"}
```

Peer/Shard Leases (in the coordination cluster):
```bash
# Peer membership (one per running process)
kubectl -n kube-system get lease | grep '^mcr-peer-'

# Per-cluster fencing (one per Kind cluster)
kubectl -n kube-system get lease | grep '^mcr-shard-'
```

Who owns a given cluster?
```bash
C=fleet-beta
kubectl -n kube-system get lease mcr-shard-$C \
  -o custom-columns=HOLDER:.spec.holderIdentity,RENEW:.spec.renewTime
```

## Test Synchronization

Create a ConfigMap on a specific Kind cluster and verify only the owning peer reconciles it:
```bash
# Create in fleet-gamma
kubectl --context kind-fleet-gamma -n default create cm test-$(date +%s) \
  --from-literal=ts=$(date +%s) --dry-run=client -oyaml | \
  kubectl --context kind-fleet-gamma apply -f -
```

Check logs in each terminal; only the peer listed as the holder for `mcr-shard-fleet-gamma` should log:
```bash
# In terminal for PEER_ID=peer-a or peer-b, look for lines like:
Reconciling ConfigMap  {"cluster":"fleet-gamma","ns":"default","name":"test-..."}
```

Rebalancing demo:
1. Stop one terminal (Ctrl+C). Within ~30s, its peer Lease expires.
2. Watch per-cluster fences move to the remaining peer:
   ```bash
   kubectl -n kube-system get lease -o custom-columns=NAME:.metadata.name,HOLDER:.spec.holderIdentity | grep '^mcr-shard-'
   ```
3. Restart the stopped terminal; watch shards redistribute.

## Notes

- The coordination cluster is simply the current kubeconfig context when you start the processes. You can choose any one of the Kind clusters for this role.
- Set a different `KIND_PREFIX` if you prefer other cluster name patterns.
- Omit `PEER_ID` to default to your hostname. When running multiple processes on the same machine, set distinct `PEER_ID` values.

## Cleanup

```bash
kind delete cluster --name fleet-alpha
kind delete cluster --name fleet-beta
kind delete cluster --name fleet-gamma
```

