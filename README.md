# gatewayapiprocessor

OpenTelemetry Collector processor that enriches spans, logs, and metrics with **normalized Kubernetes Gateway API attributes** — `k8s.gateway.*`, `k8s.httproute.*`, `k8s.gatewayclass.*` — parsed from the opaque `route_name` strings emitted by Envoy-family controllers (Envoy Gateway, Kgateway, Istio) and from Linkerd's route labels.

Demo artifact for **"The Legend of Config: Breath of the Cluster"** — ObsSummit North America 2026.

- **Docs site:** [https://henrikrexed.github.io/gatewayapiprocessor/](https://henrikrexed.github.io/gatewayapiprocessor/)
- **Talk parent:** [ISI-661](https://paperclip.isitobservable.com/ISI/issues/ISI-661)
- **Processor spec:** [ISI-670#document-processor-spec](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec)
- **Provisioning task:** [ISI-676](https://paperclip.isitobservable.com/ISI/issues/ISI-676)

## Why

Envoy-family collectors hand you `route_name="httproute/default/api/rule/0/match/0"` as an opaque string. Linkerd hands you three separate labels. Neither surfaces HTTPRoute CR status (`Accepted`, `ResolvedRefs`). This processor stamps the **same normalized attributes on every signal** regardless of the underlying data plane, so you can join traces/logs/metrics on HTTPRoute identity and show Gateway API misconfigurations without a vendor-specific dashboard.

## Quickstart

Single `kubectl apply` target — matches the talk's hero demo.

```bash
# Clone and bring up the full pinned stack (kind + ambient + kgateway + OBI + OTel Demo + custom collector):
git clone https://github.com/isi-observable/gatewayapiprocessor
cd gatewayapiprocessor
make demo

# Verify the demo HTTPRoute is Accepted:
kubectl wait --for=condition=Accepted=True httproute/api --timeout=120s

# Break it (the live stage action):
kubectl apply -f deploy/break-backendref.yaml

# Open the Grafana dashboard — 503 spike labelled k8s.httproute.name=api,
# with k8s.httproute.resolved_refs=false.

# Fix it (on-screen reversion):
kubectl apply -f deploy/fix-backendref.yaml

# Tear down:
make clean
```

## Architecture (brief)

- **Informers** watch `Gateway`, `HTTPRoute`, `GRPCRoute`, `GatewayClass`.
- **Parsers** plug in per GatewayClass controllerName (`envoy`, `linkerd`, `passthrough`).
- **Enrichment** stamps `k8s.gateway.*` / `k8s.httproute.*` on traces, logs, and metrics. UID fields excluded on metrics to prevent cardinality explosion.
- **Pipeline placement** (required): **after** `memory_limiter` and `k8sattributes`, **before** `batch`. See `deploy/40-collector/collector.yaml`.

See [gatewayapiprocessor/README.md](./gatewayapiprocessor/README.md) for the full attribute schema and config reference.

## Custom collector image

Built via OCB ([builder-config.yaml](./builder-config.yaml)) and published to GHCR:

```
ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21
```

Multi-arch: `linux/amd64`, `linux/arm64`. Tag matches `VERSIONS.md` date.

### Adding the processor to your own OCB build

The Go module is nested one level inside the repo, so the OCB import path is **not** the bare repo URL. Use the full nested path in your own `builder-config.yaml`:

```yaml
processors:
  - gomod: github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor v0.1.0
```

When developing against a local checkout, add a matching `replaces` entry pointing one level back from the OCB output dir to the processor sources at the repo root:

```yaml
replaces:
  - github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor => ../gatewayapiprocessor
```

The `../gatewayapiprocessor` form (one level up from the generated `_build/go.mod`) is required — using `./gatewayapiprocessor` resolves to `_build/gatewayapiprocessor` and breaks `go mod tidy` (see [ISI-693](https://paperclip.isitobservable.com/ISI/issues/ISI-693)). CI strips the `replaces` block before the image build.

## RBAC

The processor's informers need `get/list/watch` on `gateways`, `httproutes`, `grpcroutes`, and `gatewayclasses` in `gateway.networking.k8s.io`. RBAC posture follows the `watch.namespaces` config: cluster-scoped by default, namespace-scoped when `watch.namespaces` is set.

### Cluster-scoped (default — `watch.namespaces` empty)

Use this when the processor watches the whole cluster:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: otelcol-gatewayapi
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: [gateways, httproutes, grpcroutes, gatewayclasses]
    verbs: [get, list, watch]
  # Optional: only if emit_status_conditions is enabled.
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: [gateways/status, httproutes/status, grpcroutes/status]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: otelcol-gatewayapi
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: otelcol-gatewayapi
subjects:
  - kind: ServiceAccount
    name: otelcol
    namespace: otel-system
```

### Namespace-scoped (when `watch.namespaces: [...]` is set)

If the processor config restricts informers to specific namespaces:

```yaml
processors:
  gatewayapi:
    watch:
      namespaces: [team-a, team-b]
```

then a `ClusterRole` is **not** required for this processor — bind a `Role` per watched namespace instead. This is the least-privilege posture and is preferred for multi-tenant clusters where the collector should not see CRs outside its slice.

```yaml
# Repeat this block per namespace listed in watch.namespaces.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: otelcol-gatewayapi
  namespace: team-a
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: [gateways, httproutes, grpcroutes]
    verbs: [get, list, watch]
  # Optional: only if emit_status_conditions is enabled.
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: [gateways/status, httproutes/status, grpcroutes/status]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: otelcol-gatewayapi
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: otelcol-gatewayapi
subjects:
  - kind: ServiceAccount
    name: otelcol
    namespace: otel-system
```

`GatewayClass` is a cluster-scoped resource, so when `watch.namespaces` is set the processor skips the GatewayClass informer and the `gatewayclasses` verb is intentionally absent above. `k8sattributesprocessor` (pods/namespaces/nodes/replicasets/...) still needs its own `ClusterRole` — that scope is set by `k8sattributes`, not by this processor.

## Processor self-telemetry

The processor emits its own OTel metrics via `processor.Settings.TelemetrySettings` (FR-7, [ISI-688](https://paperclip.isitobservable.com/ISI/issues/ISI-688)). The instrument types are load-bearing for the processor-internal Grafana dashboard — Grafana queries must match the instrument shape or the panels read empty.

| Instrument | Type | Notes |
|---|---|---|
| `routes_indexed` | **UpDownCounter** (`Int64UpDownCounter`) | Net add/remove deltas as informers process `Add`/`Update`/`Delete` events. Read via `sum by (...) (delta(...))` in PromQL — **do not** treat it as a gauge. A separate `gauge` read of `len(routes)` was considered and rejected (architecture review §7.3 #2 on [ISI-691#document-architecture](https://paperclip.isitobservable.com/ISI/issues/ISI-691#document-architecture)). |

Other FR-7 instruments (`enrich_total`, `backend_ref_fallback_total`, etc.) follow standard `Counter` semantics. The full instrument list and cardinality analysis lives in the architecture doc §5.1.

## Versions

All demo component versions are pinned in [`VERSIONS.md`](./VERSIONS.md). The `VERSIONS.md` date is authoritative for the collector image tag. Bumps require a PR and re-record of the DevOps fallback clip ([ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)).

## Repo layout

```
gatewayapiprocessor/         Go module (the processor itself)
deploy/                      kind-cluster manifests for make demo
backends/                    Grafana dashboards, Dynatrace notebook
.github/workflows/           CI (go test + lint), OCB image build, weekly revalidation
builder-config.yaml          OCB manifest
Makefile                     make demo / make clean / make test / make lint
VERSIONS.md                  pinned manifest (authoritative)
```

## Branch protection (manual setup)

Branch protection on `main` is configured manually by the repo owner. The
expected rules:

- Require PRs before merging; at least 1 approving review.
- Require `CODEOWNERS` review for changes touching `.github/`, `Dockerfile`,
  `builder-config.yaml`, `VERSIONS.md`, or `docs/` / `mkdocs.yml`.
- Require these status checks to pass before merging:
  - `CI / test (go 1.23)`
  - `CI / test (go 1.24)`
  - `CI / golangci-lint`
  - `CI / OCB build (smoke)`
- Require branches to be up to date before merging.
- Dismiss stale reviews on new commits.

Rules are enforced by Henrik in repo Settings → Branches. CI workflows in
`.github/workflows/` provide the status checks referenced above.

## License

Apache-2.0. Matches `opentelemetry-collector-contrib` so we can upstream later.

## Contact

- ObsSummit talk owner: Henrik Rexed (@henrikrexed)
- Paperclip project: [Talks](https://paperclip.isitobservable.com/ISI/projects/talks)
