# gno-node-health

A Kubernetes operator that detects a [gno.land](https://gno.land) node which is **up but no longer producing blocks**.

```console
$ kubectl get gnh -n gno
NAME     HEIGHT   REACHABLE   SYNCED   ADVANCING   AGE
rpc-01   48055    True        True     True        2m
rpc-02   48020    True        True     False       2m
```

`rpc-02` is the interesting one. Its pod at that moment:

```console
$ kubectl get pod -l app=gno-rpc-02 -n gno
NAME                          STATUS    RESTARTS   READY
gno-rpc-02-74fc68dc87-w7t84   Running   0          true
```

Running, zero restarts, ready, answering RPC, and reporting itself in sync. Its chain has been dead for two minutes. Every conventional health check in the cluster says green; `Advancing=False` is the only thing that disagrees.

## Why this has to be an operator

A halted chain still serves `/status` and still returns `catching_up: false`. Telling a healthy node from a dead one means **comparing the block height across two points in time** — so it needs memory, and somewhere durable to keep it.

| Condition | Derived from | Could a probe do it? |
| --- | --- | --- |
| `Reachable` | the RPC answered | yes, that's a liveness probe |
| `Synced` | `sync_info.catching_up` | yes, a readiness probe could parse it |
| `Advancing` | height delta over time | **no** — needs state across observations |

That state is `status.lastHeightChangeTime`. It exists nowhere else: not on the node, not in the API server, not derivable from any single request. Producing it requires a process that runs continuously, remembers, and writes its memory somewhere durable — which is the definition of an operator, and why a Helm chart, a probe or a Grafana panel cannot substitute.

## Try it

Needs Go 1.26+, Docker, `kubectl` and [kind](https://kind.sigs.k8s.io/). Takes about two minutes.

```bash
kind create cluster --name gno-node-health
make docker-build IMG=gno-node-health:v0.1.0
docker build -f hack/fakenode/Dockerfile -t fake-gno-node:v0.1.0 .
kind load docker-image gno-node-health:v0.1.0 fake-gno-node:v0.1.0 --name gno-node-health
make deploy IMG=gno-node-health:v0.1.0
kubectl apply -f hack/demo/fake-nodes.yaml
kubectl apply -f hack/demo/gnonodehealth.yaml
```

Both nodes go healthy within ~30s. Now kill one chain without killing its process:

```bash
kubectl -n gno port-forward svc/gno-rpc-02 26657:26657 &
curl -X POST localhost:26657/admin/freeze
kubectl -n gno get gnh -w
```

`rpc-02` flips to `Advancing=False` once the height has been stuck past `stallThreshold`. `POST /admin/resume` heals it within one interval.

```console
$ kubectl -n gno describe gnh rpc-02
Message:  height 48020 unchanged for 1m26s
Reason:   HeightStalled
Status:   False
Type:     Advancing
```

## Build, publish, deploy

The quickstart above loads images straight into kind. For a real cluster, the image has to come from a registry the cluster can pull from.

### 1. Build

`IMG` is the one variable that matters; every target below takes it.

```bash
export IMG=ghcr.io/sw360cab/gno-node-health:v0.1.0
make docker-build IMG=$IMG
```

On an Apple Silicon Mac this produces an **arm64** image. If your cluster runs amd64 nodes (most EKS node groups do), that image will not start — the pod fails with `exec format error`. Build multi-arch instead, which builds *and* pushes in one step:

```bash
make docker-buildx IMG=$IMG PLATFORMS=linux/amd64,linux/arm64
```

### 2. Publish

```bash
docker login ghcr.io -u sw360cab
make docker-push IMG=$IMG
```

Two things that bite on a first push:

- A new **ghcr.io package is private by default**. Either make it public on the package settings page, or give the cluster an `imagePullSecret`.
- For **ECR**, create the repository first and log in with
  `aws ecr get-login-password --region eu-west-1 | docker login --username AWS --password-stdin <account>.dkr.ecr.eu-west-1.amazonaws.com`

### 3. Deploy

Two supported routes.

**From the repo**, which renders the kustomize overlay and applies it:

```bash
make deploy IMG=$IMG
```

**From a single self-contained YAML**, which is how cert-manager and External Secrets ship. Generate it once:

```bash
make build-installer IMG=$IMG
```

`dist/install.yaml` then holds all 12 objects — namespace, CRD, ServiceAccount, RBAC, Service and Deployment — with your image baked in. Attach it to a GitHub release and anyone installs with:

```bash
kubectl apply -f https://github.com/sw360cab/gno-node-health/releases/download/v0.1.0/install.yaml
```

Note that both `make deploy` and `make build-installer` run `kustomize edit set image`, which **rewrites `config/manager/kustomization.yaml` in place**. Either commit that deliberately as part of a release, or `git checkout --` it afterwards.

### 4. Verify

```bash
kubectl -n gno-node-health-system rollout status deploy/gno-node-health-controller-manager
kubectl -n gno-node-health-system logs deploy/gno-node-health-controller-manager | grep -i forbidden
```

Leader election means the pod waits for the previous lease to expire (~15s) before reconciling, so a restarted operator looks idle for a moment. An empty `forbidden` grep is the real check — RBAC failures are the usual first-deploy problem, and this operator needs `services` (to resolve `serviceRef`) plus `events.k8s.io` (a *different* API group from core `events`).

### 5. Uninstall

Order matters.

```bash
make undeploy
make uninstall
```

`make uninstall` removes the CRD, and **deleting a CRD deletes every resource of that kind cluster-wide, immediately**. Remove the controller first so nothing is mid-reconcile.

## The API

```yaml
apiVersion: monitoring.k8s.gno.land/v1alpha1
kind: GnoNodeHealth
spec:
  serviceRef:
    name: gno-rpc-02
    port: rpc-public
  interval: 5s
  timeout: 3s
  stallThreshold: 20s
```

The controller resolves `serviceRef` to a cluster-local URL, probes `GET /status`, folds the result into `status`, and returns `ctrl.Result{RequeueAfter: interval}`. No watch can tell you a chain stopped, so progress is driven by a timer — structurally the same as how External Secrets handles `refreshInterval`.

## Design notes

**It observes, it does not remediate.** Deliberately. Auto-restarting a stalled *validator* risks double-signing and therefore slashing, and a halted chain is usually a consensus-wide condition that restarting one node will not fix. Establishing truth is the hard part; acting on it is a policy decision, left open. Same split as [node-problem-detector](https://github.com/kubernetes/node-problem-detector). The natural next step is safe: flip a label so a stalled RPC node drops out of its Service endpoints.

**`Advancing` starts `Unknown`, not `True`.** One height is not a trend. Defaulting to `True` would mark a node that died *before* the resource was created as healthy forever.

**Unreachable means `Unknown`, not `False`.** If the probe fails we do not know whether the node is synced or advancing. `metav1.Condition` is tri-state for exactly this.

**A failed probe keeps the last known height.** During an incident, "last seen at 48020, unreachable since 14:02" beats a zeroed field. `lastProbeTime` says how stale it is.

**Status is only written when it changed.** An unconditional write wakes the watch, which reconciles, which writes again. Paired with `GenerationChangedPredicate`, a steady node produces exactly one reconcile per interval and zero writes.

**tm2 encodes `int64` as a quoted JSON string.** `amino` writes `"latest_block_height": "48213"`, not `48213`, so a plain `int64` field fails to unmarshal. [`internal/gnorpc`](internal/gnorpc/client.go) handles both forms. It does not import `github.com/gnolang/gno` — pulling the whole tm2 tree in to read four fields is a bad trade.

## Tests

```bash
make test
make lint
```

`internal/gnorpc` is unit-tested against `httptest`, including the amino quoting and a non-JSON body (the realistic ingress-502 case). The controller runs under [envtest](https://book.kubebuilder.io/reference/envtest.html) — a real apiserver and etcd, no cluster — with an injected prober, so a ten-minute stall is tested deterministically in milliseconds by backdating `lastHeightChangeTime` rather than sleeping.

Two bugs were caught this way and are worth knowing about: `patchStatus` once snapshotted the already-mutated status and so silently stopped writing after the first reconcile, and the first-observation branch was unreachable because a fresh resource has `previousHeight == 0`.

## Layout

```
api/v1alpha1/gnonodehealth_types.go   the CRD, as Go structs
internal/controller/                  the reconcile loop
internal/gnorpc/                      minimal tm2 RPC client
hack/fakenode/                        a Gno node impersonator with a freeze switch
hack/demo/                            two fake nodes and two GnoNodeHealth resources
config/                               generated by controller-gen; do not hand-edit
```

`hack/fakenode` earns its place: a real `gnoland` needs genesis, keys and peers, takes minutes to start, and **cannot be told to halt on command** — so the failure this operator exists to detect would otherwise be unreproducible.

## Licence

Apache 2.0.
