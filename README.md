# gatewayapiprocessor

OpenTelemetry Collector processor that enriches spans, logs, and metrics with **normalized Kubernetes Gateway API attributes** — `k8s.gateway.*`, `k8s.httproute.*`, `k8s.gatewayclass.*` — parsed from the opaque `route_name` strings emitted by Envoy-family controllers (Envoy Gateway, Kgateway, Istio) and from Linkerd's route labels.

Demo artifact for **"The Legend of Config: Breath of the Cluster"** — ObsSummit North America 2026.

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

## Self-telemetry

The processor emits its own spans, metrics, and logs off the Collector's
`service.telemetry` stanza — no separate knob, no separate exporter. When
`service.telemetry.metrics.level=none` (or the Tracer/Meter provider is
absent) the instruments compile to no-ops, so running with telemetry
disabled costs nothing. See [ISI-688](https://paperclip.isitobservable.com/ISI/issues/ISI-688) for the rollout motivation.

### Enabling

Nothing to flip on the processor. In the Collector config:

```yaml
service:
  telemetry:
    logs:
      level: info
    metrics:
      level: detailed   # or "basic" for just counters
      address: 0.0.0.0:8888
    traces:
      level: detailed
```

All gatewayapiprocessor self-metrics surface under the OTel scope
`github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor`.

### Metrics

| Name | Type | Labels | Purpose |
|---|---|---|---|
| `gatewayapiprocessor_routes_indexed` | UpDownCounter | `gateway_class`, `route_kind` | Size of each in-memory route index — tracks CRD population over time. |
| `gatewayapiprocessor_enrichments_total` | Counter | `signal` (`metrics|traces|logs`), `outcome` (`stamped|dropped|ambiguous_owner`) | Per-record enrichment outcomes. |
| `gatewayapiprocessor_informer_events_total` | Counter | `resource` (`HTTPRoute|GRPCRoute|Gateway|GatewayClass`), `event` (`add|update|delete|sync`) | Kubernetes informer volume. |
| `gatewayapiprocessor_enrichment_duration` | Histogram (seconds) | `signal` | Per-batch enrichment latency. |
| `gatewayapiprocessor_backend_ref_fallback_total` | Counter | `outcome` (`resolved|ambiguous|unresolved`) | Fallback-path attribution outcomes when no parser matches. |
| `gatewayapiprocessor_status_conditions_stamped_total` | Counter | — | Records whose `Accepted`/`ResolvedRefs` conditions were stamped. Gated by `emit_status_conditions=true`. |

No UID, route name, or pod identity appears in any label set — cardinality is
bounded by `gateway_class × route_kind × signal × outcome` even at very large
cluster scale.

### Spans

- `gatewayapiprocessor.EnrichBatch` (`SpanKind=Internal`) — one per
  `ConsumeTraces/Logs/Metrics` call. Attributes: `signal`, `items`,
  `gatewayapiprocessor.self=true`.

The `gatewayapiprocessor.self=true` attribute is a guard against self-
enrichment: if the Collector's own telemetry pipeline re-enters the processor,
filter those spans out with a simple attribute predicate rather than parsing
scope names.

### Logs

Emitted via the Collector-supplied logger:

- **INFO** on startup — feature matrix (`enrich.traces/logs/metrics`,
  `emit_status_conditions`, `backend_ref_fallback.enabled`, parser count).
- **INFO** per informer-sync completion (`HTTPRoute`, `GRPCRoute`, `Gateway`,
  `GatewayClass`).
- **WARN** on ambiguous backendRef ownership — sampled at one log per 30 s
  across the whole processor; the accompanying
  `gatewayapiprocessor_backend_ref_fallback_total{outcome="ambiguous"}` counter
  still records every event.
- **ERROR** on informer start or sync failures.
- **DEBUG** on informer tombstone-type mismatches (`HTTPRoute`/`GRPCRoute`
  delete paths).


## Custom collector image

Built via OCB ([builder-config.yaml](./builder-config.yaml)) and published to GHCR:

```
ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21
```

Multi-arch: `linux/amd64`, `linux/arm64`. Tag matches `VERSIONS.md` date.

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
