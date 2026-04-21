# gatewayapiprocessor

[![CI](https://github.com/henrikrexed/gatewayapiprocessor/actions/workflows/ci.yaml/badge.svg)](https://github.com/henrikrexed/gatewayapiprocessor/actions/workflows/ci.yaml)
[![Build](https://github.com/henrikrexed/gatewayapiprocessor/actions/workflows/build.yaml/badge.svg)](https://github.com/henrikrexed/gatewayapiprocessor/actions/workflows/build.yaml)
[![Docs](https://github.com/henrikrexed/gatewayapiprocessor/actions/workflows/docs.yaml/badge.svg)](https://henrikrexed.github.io/gatewayapiprocessor/)
[![GHCR Image](https://img.shields.io/badge/ghcr.io-otelcol--gatewayapi-blue)](https://github.com/henrikrexed/gatewayapiprocessor/pkgs/container/otelcol-gatewayapi)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

> **A Kubernetes-aware OpenTelemetry Collector processor that turns opaque `route_name` strings into normalized `k8s.gateway.*`, `k8s.httproute.*`, and `k8s.gatewayclass.*` attributes — the same shape across Envoy Gateway, Kgateway, Istio ambient, and Linkerd.**

`gatewayapiprocessor` runs Kubernetes informers for `Gateway`, `HTTPRoute`, `GRPCRoute`, and `GatewayClass`, parses upstream route identity emitted by the data plane, and stamps normalized attributes on every span, log, and metric data point. It also reads CR status (`HTTPRoute.status.parents[].conditions`) — something `transformprocessor`/OTTL cannot — so a misconfigured `backendRef` shows up directly in your telemetry.

- 📘 **Docs site:** https://henrikrexed.github.io/gatewayapiprocessor/
- 🎤 **Talk context:** *The Legend of Config: Breath of the Cluster* — ObsSummit North America 2026 ([ISI-661](https://paperclip.isitobservable.com/ISI/issues/ISI-661)).
- 📐 **Processor spec:** [ISI-670 processor-spec](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec).

---

## Why this processor

| Data plane                         | What it hands you on a span/metric                             | What you actually want                                          |
| ---------------------------------- | -------------------------------------------------------------- | --------------------------------------------------------------- |
| Envoy Gateway / Kgateway / Istio   | `route_name="httproute/default/api/rule/0/match/0"` (opaque)   | `k8s.httproute.name="api"`, `k8s.httproute.namespace="default"` |
| Linkerd                            | `route_name`, `route_kind`, `route_namespace` (three labels)   | Same normalized shape as above                                  |
| **Any of the above (misconfig)**   | Nothing — CR status lives on the `HTTPRoute`, not on telemetry | `k8s.httproute.accepted=true`, `k8s.httproute.resolved_refs=false` |

One processor, one schema, every backend: join traces/logs/metrics on HTTPRoute identity without a vendor-specific dashboard.

## Quick start (3-minute path)

> Requires the custom collector image built from this repo — `ghcr.io/henrikrexed/otelcol-gatewayapi:2026-04-21`. See [custom collector image](#custom-collector-image) to build your own.

**1. Apply the RBAC and the Collector CR:**

```bash
kubectl apply -f deploy/40-collector/rbac.yaml
kubectl apply -f deploy/40-collector/collector.yaml
```

**2. Minimum viable config** (defaults cover Envoy/Kgateway/Istio + Linkerd + passthrough):

```yaml
# collector.yaml (excerpt)
processors:
  gatewayapi: {}   # defaults from factory.createDefaultConfig are enough for most clusters

service:
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters:  [otlphttp]
```

> Pipeline placement rule: `gatewayapi` sits **after** `memory_limiter` and `k8sattributes`, **before** `batch`. See [how-it-works](https://henrikrexed.github.io/gatewayapiprocessor/how-it-works/).

**3. Verify attribute stamping:**

```bash
kubectl apply -f deploy/30-demo/httproute-api.yaml
kubectl wait --for=condition=Accepted=True httproute/api --namespace demo --timeout=120s

# Tail collector logs — you should see the processor log "informers synced"
kubectl logs -n otel-system deploy/otelcol-gatewayapi | grep -i gatewayapi
```

Every span that carries a recognizable `route_name` now also carries `k8s.httproute.name`.

**4. See the hero demo (one live action):**

```bash
make demo        # stand up the full pinned stack (kind + ambient + kgateway + OBI + OTel Demo)
make break       # 503 spike on k8s.httproute.name=api, k8s.httproute.resolved_refs=false
make fix         # restore the backendRef — green line returns in ~30s
make clean
```

The Grafana dashboard in `backends/grafana/dashboards/before-after.json` renders this as a before/after panel; the same query runs as DQL in `backends/dynatrace/notebook.json`.

## Compatibility

Versions below are **pinned** for the demo and CI revalidation. Bumps require a PR against [`VERSIONS.md`](./VERSIONS.md) and re-record of the DevOps fallback clip ([ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)).

| Component                    | Pinned version  |
| ---------------------------- | --------------- |
| Gateway API CRDs             | `v1.3.0`        |
| Kgateway                     | `v2.1.0`        |
| Istio (ambient profile)      | `1.26.0`        |
| OpenTelemetry Collector      | `v0.124.0`      |
| OpenTelemetry Operator       | `v0.124.0`      |
| OBI (opentelemetry-ebpf-…)   | `v0.8.0`        |
| OpenTelemetry semconv        | `1.40.0`        |
| Go                           | `1.23`          |
| Kubernetes (kind node image) | `v1.32.0`       |

The processor compiles against **Gateway API v1** (`sigs.k8s.io/gateway-api/apis/v1`). Earlier alpha/beta versions of the CRDs are out of scope.

## Configuration

> The canonical source for config is [`gatewayapiprocessor/config.go`](./gatewayapiprocessor/config.go). Defaults below are produced by `createDefaultConfig` in [`gatewayapiprocessor/factory.go`](./gatewayapiprocessor/factory.go).

```yaml
processors:
  gatewayapi:
    # Kubernetes client auth. Reuses the collector's ServiceAccount by default.
    auth_type: serviceAccount            # serviceAccount | kubeConfig | none
    kube_config_path: ""                 # required when auth_type=kubeConfig

    # Informer scope.
    watch:
      namespaces: []                     # empty = watch all namespaces (cluster-scoped RBAC)
      resync_period: 5m

    # Bounded wait for informer caches on Start() — fails fast if RBAC is missing.
    informer_sync_timeout: 30s

    # Parser plug-ins run in order. First parser yielding (namespace, name) wins.
    # "passthrough" MUST be last (enforced by Validate()).
    parsers:
      - name: envoy
        controllers:
          - "^gateway\\.envoyproxy\\.io/gatewayclass-controller$"
          - "^kgateway\\.dev/gatewayclass-controller$"
          - "^istio\\.io/gateway-controller$"
        source_attribute: "route_name"
        # Named groups required: ns, name. Optional: rule, match.
        format_regex: "^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)(?:/rule/(?P<rule>\\d+))?(?:/match/(?P<match>\\d+))?"
      - name: linkerd
        controllers:
          - "^linkerd\\.io/gateway-controller$"
        linkerd_labels:
          route_name: "route_name"
          route_kind: "route_kind"
          route_namespace: "route_namespace"
          parent_name: "parent_name"
      - name: passthrough
        source_attribute: "route_name"
        passthrough_attribute: "k8s.gatewayapi.raw_route_name"

    # Enrichment scope. Traces and logs default on; metrics opt-in.
    enrich:
      traces: true
      logs: true
      metrics: true
      # Cardinality guard — stripped before emit on the metrics pipeline.
      exclude_from_metric_attributes:
        - "k8s.httproute.uid"
        - "k8s.gateway.uid"
        - "k8s.gatewayapi.raw_route_name"

    # Read HTTPRoute.status.parents[].conditions and stamp
    # k8s.httproute.accepted / k8s.httproute.resolved_refs.
    emit_status_conditions: true

    # Probabilistic fallback when no upstream route_name is present.
    backendref_fallback:
      enabled: true
      source_attribute: "server.address"
```

Full field reference and per-parser examples: https://henrikrexed.github.io/gatewayapiprocessor/configuration/

## Attribute reference

Every attribute this processor stamps is defined as a constant in [`gatewayapiprocessor/attrs.go`](./gatewayapiprocessor/attrs.go). The table below is the operator-facing summary; the docs site has cardinality guidance and per-parser examples.

| Attribute                           | Source                                                              |
| ----------------------------------- | ------------------------------------------------------------------- |
| `k8s.gateway.name`                  | `Gateway.metadata.name`                                             |
| `k8s.gateway.namespace`             | `Gateway.metadata.namespace`                                        |
| `k8s.gateway.uid`                   | `Gateway.metadata.uid` *(stripped from metrics by default)*         |
| `k8s.gateway.listener.name`         | `Gateway.spec.listeners[].name`                                     |
| `k8s.gatewayclass.name`             | `Gateway.spec.gatewayClassName`                                     |
| `k8s.gatewayclass.controller`       | `GatewayClass.spec.controllerName`                                  |
| `k8s.httproute.name`                | `HTTPRoute.metadata.name`                                           |
| `k8s.httproute.namespace`           | `HTTPRoute.metadata.namespace`                                      |
| `k8s.httproute.uid`                 | `HTTPRoute.metadata.uid` *(stripped from metrics by default)*       |
| `k8s.httproute.rule_index`          | Parsed from upstream `route_name` (Envoy-family only)               |
| `k8s.httproute.match_index`         | Parsed from upstream `route_name` (Envoy-family only)               |
| `k8s.httproute.parent_ref`          | `<group>/<kind>/<ns>/<name>` from `spec.parentRefs[0]`              |
| `k8s.httproute.accepted`            | `status.parents[].conditions[type=Accepted].status`                 |
| `k8s.httproute.resolved_refs`       | `status.parents[].conditions[type=ResolvedRefs].status`             |
| `k8s.grpcroute.name`                | `GRPCRoute.metadata.name`                                           |
| `k8s.grpcroute.namespace`           | `GRPCRoute.metadata.namespace`                                      |
| `k8s.gatewayapi.raw_route_name`     | Raw upstream string when passthrough matches                        |
| `k8s.gatewayapi.parser`             | Parser id that handled the record (`envoy`/`linkerd`/`passthrough`) |

## Custom collector image

The processor is not in `opentelemetry-collector-contrib` yet, so the collector image is built via OCB from [`builder-config.yaml`](./builder-config.yaml):

```bash
make ocb-install      # installs builder v0.124.0
make build-collector  # produces ./_build/otelcol-gatewayapi
```

CI publishes the multi-arch image to GHCR on every tag under `VERSIONS.md`:

```
ghcr.io/henrikrexed/otelcol-gatewayapi:2026-04-21     # also tagged :latest until the next bump
```

Platforms: `linux/amd64`, `linux/arm64`.

## Repo layout

```
gatewayapiprocessor/         Go module — the processor itself
  ├── config.go              Config struct + Validate()
  ├── factory.go             createDefaultConfig + traces/logs/metrics factories
  ├── processor.go           ConsumeTraces/Logs/Metrics + parser chain
  ├── informer.go            Gateway API informers + cache sync
  ├── index.go               route / backendRef secondary indexes
  ├── attrs.go               Attribute key constants (the contract surface)
  └── parser/                envoy / linkerd / passthrough plug-ins
deploy/                      kind-cluster manifests for `make demo`
backends/                    Grafana dashboards + Dynatrace notebook
docs/                        mkdocs Material site source
mkdocs.yml                   mkdocs site config
builder-config.yaml          OCB collector build manifest
Makefile                     make demo / make test / make lint / make break / make fix
VERSIONS.md                  pinned manifest (authoritative)
```

## Documentation

The full docs site is built with **MkDocs Material** and published to GitHub Pages.

| Section                                                                                       | What's in it                                                                      |
| --------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| [Home](https://henrikrexed.github.io/gatewayapiprocessor/)                                    | Value prop, when to use / when not to use.                                        |
| [Getting started](https://henrikrexed.github.io/gatewayapiprocessor/getting-started/)         | Install via OCB, minimum viable pipeline, verification.                           |
| [Configuration](https://henrikrexed.github.io/gatewayapiprocessor/configuration/)             | Full field reference + worked examples per parser (envoy, linkerd, passthrough).  |
| [Attribute reference](https://henrikrexed.github.io/gatewayapiprocessor/attribute-reference/) | Every stamped attribute documented with cardinality guidance.                     |
| [How it works](https://henrikrexed.github.io/gatewayapiprocessor/how-it-works/)               | Informer architecture, single-owner index, cardinality guard, backendRef fallback.|
| [Troubleshooting](https://henrikrexed.github.io/gatewayapiprocessor/troubleshooting/)         | Ambiguous attribution, informer sync timeout, Istio ambient gotchas.              |
| [Roadmap](https://henrikrexed.github.io/gatewayapiprocessor/roadmap/)                         | Phase 3.5 scope expansion (kgateway CRDs, Istio CRDs, NetworkPolicy).             |

Local preview:

```bash
pip install mkdocs-material
mkdocs serve       # http://127.0.0.1:8000
```

## Stability

**Development.** The processor is shipped at `component.StabilityLevelDevelopment` in [`factory.go`](./gatewayapiprocessor/factory.go). The attribute contract in [`attrs.go`](./gatewayapiprocessor/attrs.go) is the surface treated as stable — any rename is a breaking change and is called out in the release notes.

## License

Apache-2.0 — matches `opentelemetry-collector-contrib` so the processor can be upstreamed later (see [roadmap](https://henrikrexed.github.io/gatewayapiprocessor/roadmap/)).

## Contact

- Talk owner: Henrik Rexed ([@henrikrexed](https://github.com/henrikrexed))
- Paperclip project: [Talks](https://paperclip.isitobservable.com/ISI/projects/talks)
- Parent task: [ISI-670](https://paperclip.isitobservable.com/ISI/issues/ISI-670)
