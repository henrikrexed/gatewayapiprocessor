---
title: Attribute reference
---

# Attribute reference

Every attribute `gatewayapiprocessor` stamps is declared as a constant in [`gatewayapiprocessor/attrs.go`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/gatewayapiprocessor/attrs.go). That file is the contract surface — any rename is a breaking change and appears in the release notes.

Attributes are grouped by the CR they come from.

## Gateway-level

Read from the `Gateway` matched via `HTTPRoute.spec.parentRefs[0]`.

| Attribute                       | Type     | Cardinality *(per cluster)* | Source                                  |
| ------------------------------- | -------- | --------------------------- | --------------------------------------- |
| `k8s.gateway.name`              | string   | Low (tens)                  | `Gateway.metadata.name`                 |
| `k8s.gateway.namespace`         | string   | Low                         | `Gateway.metadata.namespace`            |
| `k8s.gateway.uid`               | string   | Low                         | `Gateway.metadata.uid` ⚠ metrics-stripped |
| `k8s.gateway.listener.name`     | string   | Low                         | `Gateway.spec.listeners[].name`         |

## GatewayClass-level

Read from the `GatewayClass` matched via `Gateway.spec.gatewayClassName`.

| Attribute                         | Type   | Cardinality | Source                                                             |
| --------------------------------- | ------ | ----------- | ------------------------------------------------------------------ |
| `k8s.gatewayclass.name`           | string | Low         | `Gateway.spec.gatewayClassName`                                    |
| `k8s.gatewayclass.controller`     | string | Low         | `GatewayClass.spec.controllerName`                                 |

## HTTPRoute-level

Read from the `HTTPRoute` matched by `(namespace, name)` returned by a parser.

| Attribute                         | Type   | Cardinality           | Source                                                                    |
| --------------------------------- | ------ | --------------------- | ------------------------------------------------------------------------- |
| `k8s.httproute.name`              | string | Medium (hundreds)     | `HTTPRoute.metadata.name`                                                 |
| `k8s.httproute.namespace`         | string | Low                   | `HTTPRoute.metadata.namespace`                                            |
| `k8s.httproute.uid`               | string | Medium                | `HTTPRoute.metadata.uid` ⚠ metrics-stripped                               |
| `k8s.httproute.rule_index`        | int    | Low (0..N-rules)      | Parsed from upstream route id (Envoy-family only)                         |
| `k8s.httproute.match_index`       | int    | Low (0..N-matches)    | Parsed from upstream route id (Envoy-family only)                         |
| `k8s.httproute.parent_ref`        | string | Low                   | `"<group>/<kind>/<ns>/<name>"` from `spec.parentRefs[0]`                  |
| `k8s.httproute.accepted`          | bool   | Binary                | `status.parents[].conditions[type=Accepted].status`                       |
| `k8s.httproute.resolved_refs`     | bool   | Binary                | `status.parents[].conditions[type=ResolvedRefs].status`                   |

## GRPCRoute-level

Set only for records carrying gRPC identity (Envoy's `route_name` with a GRPCRoute prefix or Linkerd's `route_kind=GRPCRoute`). Mutually exclusive with the HTTPRoute attributes on the same record.

| Attribute                   | Type   | Cardinality           | Source                                |
| --------------------------- | ------ | --------------------- | ------------------------------------- |
| `k8s.grpcroute.name`        | string | Medium                | `GRPCRoute.metadata.name`             |
| `k8s.grpcroute.namespace`   | string | Low                   | `GRPCRoute.metadata.namespace`        |

## Passthrough / fallback

Stamped when no structured parser matched.

| Attribute                         | Type   | Source                                                                  |
| --------------------------------- | ------ | ----------------------------------------------------------------------- |
| `k8s.gatewayapi.raw_route_name`   | string | Raw upstream string. ⚠ metrics-stripped by default.                     |
| `k8s.gatewayapi.parser`           | string | Parser id: `envoy` / `linkerd` / `passthrough` / `backendref_fallback`. |

---

## Cardinality guidance

`gatewayapiprocessor` defaults align with the cardinality rules from [processor-spec §1.4](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec):

- **Safe on spans and logs.** Every attribute above. Traces and logs are already high-cardinality surfaces; adding route identity adds no meaningful cost.
- **Metrics: opt-in only.** `k8s.httproute.rule_index` and `k8s.httproute.match_index` are metric-safe. **UIDs and `raw_route_name` are not**, and they're stripped from metrics by default via [`enrich.exclude_from_metric_attributes`](../configuration/index.md#enrichexclude_from_metric_attributes).

!!! warning "Istio Telemetry footgun"
    If you re-enable UID stamping on metrics and then wire the collector output into Istio Telemetry (which applies them as span→metric dimensions), you can easily hit the classic time-series cardinality explosion. Keep the defaults unless you've measured.

## Mapping to upstream semconv

The `k8s.*` root is chosen to match `k8sattributesprocessor`. None of these attributes are in published OpenTelemetry semconv yet — a future semconv PR is tracked as a stretch goal on the [roadmap](../roadmap/index.md). Until that lands, treat these keys as project-local with a stable shape.

## See also

- [Configuration → Enrichment scope](../configuration/index.md#enrichtraces--enrichlogs--enrichmetrics)
- [How it works → Per-signal processing](../how-it-works/index.md#per-signal-processing)
