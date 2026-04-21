---
title: Configuration
---

# Configuration

The config shape in this page is the canonical source shown in [`config.go`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/gatewayapiprocessor/config.go). The factory defaults (which the docs on this page assume) live in [`factory.go#createDefaultConfig`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/gatewayapiprocessor/factory.go).

## Full reference

```yaml
processors:
  gatewayapi:
    auth_type: serviceAccount            # serviceAccount | kubeConfig | none
    kube_config_path: ""                 # required when auth_type=kubeConfig

    watch:
      namespaces: []                     # empty = all namespaces; requires cluster-scoped RBAC
      resync_period: 5m

    informer_sync_timeout: 30s           # fails Start() if caches do not sync in this window

    parsers:                             # order matters — first match wins; passthrough MUST be last
      - name: envoy
        controllers:
          - "^gateway\\.envoyproxy\\.io/gatewayclass-controller$"
          - "^kgateway\\.dev/gatewayclass-controller$"
          - "^istio\\.io/gateway-controller$"
        source_attribute: "route_name"
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

    enrich:
      traces: true
      logs: true
      metrics: true
      exclude_from_metric_attributes:    # stripped before emit on the metrics pipeline
        - "k8s.httproute.uid"
        - "k8s.gateway.uid"
        - "k8s.gatewayapi.raw_route_name"

    emit_status_conditions: true         # stamp k8s.httproute.accepted / .resolved_refs

    backendref_fallback:
      enabled: true
      source_attribute: "server.address" # probabilistic; see How it works
```

## Field-by-field

### `auth_type`

| Value             | Behavior                                                                  |
| ----------------- | ------------------------------------------------------------------------- |
| `serviceAccount`  | (default) Use the in-cluster service account token at `/var/run/secrets`. |
| `kubeConfig`      | Read credentials from `kube_config_path`. Useful for local development.   |
| `none`            | Do not start informers. Processor becomes a no-op. Used in unit tests.    |

### `kube_config_path`

Required when `auth_type=kubeConfig`. Path to a kubeconfig file readable by the collector process.

### `watch.namespaces`

Empty list (default) = watch **all** namespaces. Any non-empty list makes the processor create one informer factory per namespace, which requires corresponding `Role` + `RoleBinding` objects (not `ClusterRole` + `ClusterRoleBinding`).

### `watch.resync_period`

Forces a full re-list on each informer at this interval. Default `5m`. Informers always react to live events; this is the safety net for dropped events. Setting `0` disables periodic re-list — the tradeoff is that a dropped watch event could leave stale entries in the cache until the next event.

### `informer_sync_timeout`

Bounds how long `Start()` will wait for informer caches to warm up. Default `30s`. If the collector never transitions to ready within this window, check collector logs for `Forbidden` errors — usually RBAC.

### `parsers`

Ordered plug-in chain. Each record walks the chain until a parser returns `Matched=true`. `passthrough` **MUST** be the last entry — [`Config.Validate()`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/gatewayapiprocessor/config.go) rejects configs where it isn't.

Individual parser options are documented on their own pages:

- [Envoy-family parser](parser-envoy.md) — Envoy Gateway, Kgateway, Istio.
- [Linkerd parser](parser-linkerd.md)
- [Passthrough parser](parser-passthrough.md)

### `enrich.traces` / `enrich.logs` / `enrich.metrics`

Booleans. Default on for all three. Setting a signal to `false` makes the processor a no-op for that pipeline (no lookup, no stamping) — useful if you only care about one signal type.

### `enrich.exclude_from_metric_attributes`

Cardinality guard. These attributes are stripped from the record **before** it is emitted on the metrics pipeline. The defaults remove UIDs (high cardinality per pod restart) and the raw route name (opaque; not useful as a metric dimension). Traces and logs still carry these attributes — the strip is metrics-only.

!!! warning "Istio Telemetry footgun"
    The spec flags this as the Istio Telemetry API footgun: applying a UID-carrying dimension to a metric in Istio/Envoy explodes cardinality. Keeping this guard on by default is deliberate.

### `emit_status_conditions`

When `true` (default), the processor reads `HTTPRoute.status.parents[].conditions` from the informer cache (no extra API call) and stamps:

- `k8s.httproute.accepted` — boolean, from `conditions[type=Accepted].status == "True"`.
- `k8s.httproute.resolved_refs` — boolean, from `conditions[type=ResolvedRefs].status == "True"`.

Set to `false` if you don't want these attributes — the informer cache is already populated, so this is purely a contract knob, not a cost saver.

### `backendref_fallback`

When no parser matches and the span carries a `server.address` equal to a known Service IP/host referenced by any HTTPRoute's `backendRefs`, the processor attributes the span to that HTTPRoute. This is **probabilistic** — if multiple HTTPRoutes share the same backend Service, the fallback picks the first indexed match and tags the record with `k8s.gatewayapi.parser=backendref_fallback` so you know.

| Field              | Default             | Behavior                                              |
| ------------------ | ------------------- | ----------------------------------------------------- |
| `enabled`          | `true`              | Turn the fallback on/off.                             |
| `source_attribute` | `"server.address"`  | Which attribute to key the lookup on.                 |

Disable if you run a strict mesh where every span *must* have an upstream route identity — the fallback lookup will mask the bug you're trying to catch.

## Validation

`Config.Validate()` enforces:

1. `auth_type` is one of the allowed values.
2. `auth_type=kubeConfig` ⇒ `kube_config_path` is non-empty.
3. At least one parser is configured.
4. `passthrough` is the last parser (when present).
5. Every `controllers` regex compiles.
6. The `envoy` parser's `format_regex` compiles **and** exposes named groups `ns` and `name`.
7. `watch.resync_period` ≥ 0.
8. `informer_sync_timeout` ≥ 0.

A validation failure blocks collector start — fix and restart.
