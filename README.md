# gatewayapiprocessor

OpenTelemetry Collector processor that enriches spans, logs, and metrics with **normalized Kubernetes Gateway API attributes** — `k8s.gateway.*`, `k8s.httproute.*`, `k8s.gatewayclass.*` — parsed from the opaque `route_name` strings emitted by Envoy-family controllers (Envoy Gateway, Kgateway, Istio) and from Linkerd's route labels.

Demo artifact for **"The Legend of Config: Breath of the Cluster"** — ObsSummit North America 2026.

- **Talk parent:** [ISI-661](https://paperclip.isitobservable.com/ISI/issues/ISI-661)
- **Processor spec:** [ISI-670#document-processor-spec](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec)
- **Demo runbook:** [ISI-671#document-demo-steps](https://paperclip.isitobservable.com/ISI/issues/ISI-671#document-demo-steps)

## Why

Envoy-family collectors hand you `route_name="httproute/default/api/rule/0/match/0"` as an opaque string. Linkerd hands you three separate labels. Neither surfaces HTTPRoute CR status (`Accepted`, `ResolvedRefs`). This processor stamps the **same normalized attributes on every signal** regardless of the underlying data plane, so you can join traces/logs/metrics on HTTPRoute identity and show Gateway API misconfigurations without a vendor-specific dashboard.

## Demo layout

The demo is operated by hand against a homelab Kubernetes cluster — there is no `make demo` automation. The authoritative runbook lives on ISI-671 as `demo-steps` (§A preconditions, §B preflight, §C install order, §D prewarm, §E the live beat, §F fallback, §H teardown).

`make steps` prints a short version of §C for quick recall. Every command runs against the homelab kubeconfig, with the single observability backend being **Dynatrace** (via OTLP-HTTP).

### Quickstart (once the homelab cluster is up)

```bash
# Print the install order:
make steps

# After walking through §C, the live beats are:
make break    # flip HTTPRoute backendRef to a non-existent Service
# ...wait ~15–30s for ingest, show the Notebook...
make fix      # revert
```

## Architecture (brief)

- **Informers** watch `Gateway`, `HTTPRoute`, `GRPCRoute`, `GatewayClass`.
- **Parsers** plug in per GatewayClass controllerName (`envoy`, `linkerd`, `passthrough`).
- **Enrichment** stamps `k8s.gateway.*` / `k8s.httproute.*` on traces, logs, and metrics. UID fields excluded on metrics to prevent cardinality explosion.
- **Pipeline placement** (required): **after** `memory_limiter` and `k8sattributes`, **before** `batch`. See `deploy/40-collector/collector.yaml`.

See [gatewayapiprocessor/README.md](./gatewayapiprocessor/README.md) for the full attribute schema and config reference.

## Gateway API + GAMMA (service mesh) surface

The demo exercises the same `HTTPRoute` CRD in **two modes** against the same cluster, which is the load-bearing slide:

| Mode | parentRef | File | What it shows |
|---|---|---|---|
| Ingress | `Gateway/ingress` (Kgateway) | `deploy/30-demo/otel-demo.yaml` | North-south traffic, the hero `make break` / `make fix` beat. |
| Mesh (GAMMA) | `Service/cartservice` + `Service/checkoutservice` | `deploy/10-mesh/gamma-routes.yaml` | East-west traffic through the Istio ambient waypoint. |

Mesh policies under `deploy/10-mesh/policies/`:

- `traffic-split.yaml` — 90/10 weighted canary on `/api/cart` (ingress-bound HTTPRoute).
- `peer-authentication.yaml` — namespace-level `STRICT` mTLS `PeerAuthentication`.
- `authorization-policy.yaml` — principals-scoped `AuthorizationPolicy` on `checkoutservice`.

The processor stamps the same normalized `k8s.httproute.*` attributes on both ingress and mesh traffic, so the Dynatrace Notebook can join the two views on one HTTPRoute identity.

## Custom collector image

Built via OCB ([builder-config.yaml](./builder-config.yaml)) and published to GHCR:

```
ghcr.io/henrikrexed/otelcol-gatewayapi:2026-04-21
```

Multi-arch: `linux/amd64`, `linux/arm64`. Tag matches `VERSIONS.md` date.

## Versions

All demo component versions are pinned in [`VERSIONS.md`](./VERSIONS.md). The `VERSIONS.md` date is authoritative for the collector image tag. Bumps require a PR and re-record of the DevOps fallback clip ([ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)).

## Repo layout

```
gatewayapiprocessor/         Go module (the processor itself)
deploy/
  00-operators/              CRDs + OTel Operator + ambient ztunnel
  10-mesh/                   waypoint + kgateway + GAMMA routes + policies
  20-obi/                    OBI DaemonSet (eBPF HTTP telemetry)
  30-demo/                   OTel Demo v2.2.0 + ingress HTTPRoute/GRPCRoute
  40-collector/              custom collector CR + RBAC
  break-backendref.yaml      hero demo beat (apply)
  fix-backendref.yaml        hero demo beat (revert)
backends/dynatrace/          Dynatrace OTLP target config + notebook.json
.github/workflows/           CI (go test + lint), OCB image build, weekly revalidation
builder-config.yaml          OCB manifest
Makefile                     make steps / make break / make fix / make test / make lint
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
