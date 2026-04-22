# gatewayapiprocessor

OpenTelemetry Collector processor that enriches spans, logs, and metrics with
**normalized Kubernetes Gateway API attributes** &mdash;
`k8s.gateway.*`, `k8s.httproute.*`, `k8s.gatewayclass.*` &mdash; regardless of
which Gateway API data plane produced the signal.

## What it does

Envoy-family collectors (Envoy Gateway, Kgateway, Istio) emit opaque strings
like:

```
route_name = "httproute/default/api/rule/0/match/0"
```

Linkerd emits three separate labels. Neither surfaces HTTPRoute CR status
(`Accepted`, `ResolvedRefs`). `gatewayapiprocessor` stamps the same normalized
attributes on every signal so you can join traces, logs, and metrics on
HTTPRoute identity in a single dashboard.

## Attribute contract

The processor writes a stable set of attribute keys. These are the contract
surface &mdash; any bump is a breaking change.

| Attribute                         | Where it comes from                          |
| --------------------------------- | -------------------------------------------- |
| `k8s.gateway.name`                | Parent Gateway resource                       |
| `k8s.gateway.namespace`           | Parent Gateway resource                       |
| `k8s.gateway.uid`                 | Parent Gateway resource (excluded on metrics) |
| `k8s.gateway.listener.name`       | Matching listener on the Gateway              |
| `k8s.gatewayclass.name`           | Parent GatewayClass                           |
| `k8s.gatewayclass.controller`     | `GatewayClass.spec.controllerName`            |
| `k8s.httproute.name`              | Matched HTTPRoute                             |
| `k8s.httproute.namespace`         | Matched HTTPRoute                             |
| `k8s.httproute.uid`               | Matched HTTPRoute (excluded on metrics)       |
| `k8s.httproute.rule_index`        | Envoy parser capture group `rule`             |
| `k8s.httproute.match_index`       | Envoy parser capture group `match`            |
| `k8s.httproute.parent_ref`        | Resolved `parentRef` reference                |
| `k8s.httproute.accepted`          | `status.conditions[type=Accepted]`            |
| `k8s.httproute.resolved_refs`     | `status.conditions[type=ResolvedRefs]`        |
| `k8s.grpcroute.name`              | Matched GRPCRoute                             |
| `k8s.grpcroute.namespace`         | Matched GRPCRoute                             |
| `k8s.gatewayapi.raw_route_name`   | Unparsable string (passthrough, excl. metrics)|
| `k8s.gatewayapi.parser`           | Parser that won the lookup                    |

## Value proposition

- **Cross-data-plane normalization** &mdash; the same dashboard works whether
  traffic runs through Envoy Gateway, Kgateway, Istio ambient, or Linkerd.
- **Gateway API status surfaced as telemetry** &mdash; `Accepted` and
  `ResolvedRefs` ride on every signal, so misconfigurations (`resolved_refs=false`)
  are visible in the same query path as latency and error rate.
- **Cardinality guard by default** &mdash; UID-like attributes are stripped
  before records are emitted on the metrics pipeline (configurable via
  `enrich.exclude_from_metric_attributes`).

## Where to start

- [Getting started](getting-started.md) &mdash; install and first run in under 5 minutes.
- [Requirements](requirements.md) &mdash; collector, Gateway API, Kubernetes, RBAC.
- [Configuration reference](configuration.md) &mdash; every key, type, and default.
- [Examples](examples.md) &mdash; minimal, full, and common scenarios.
- [Architecture](architecture.md) &mdash; how informers and parsers fit together.

## Stability

All three signal processors (`traces`, `logs`, `metrics`) ship at
`StabilityLevelDevelopment`. The component type key is `gatewayapi`.

## License

Apache-2.0. Matches `opentelemetry-collector-contrib` so upstream contribution
stays open.
