---
title: Minimum viable pipeline
---

# Minimum viable pipeline

The factory defaults ([`createDefaultConfig`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/gatewayapiprocessor/factory.go)) cover the common case: watch all namespaces, parse Envoy/Kgateway/Istio route strings, fall through to Linkerd labels, fall through again to passthrough. Most clusters need no config at all.

## Traces

```yaml
receivers:
  otlp:
    protocols:
      grpc:
      http:

processors:
  memory_limiter:
    check_interval: 1s
    limit_percentage: 80
    spike_limit_percentage: 25
  k8sattributes: {}
  gatewayapi: {}              # <-- defaults cover most clusters
  batch: {}

exporters:
  otlphttp:
    endpoint: http://tempo:4318

service:
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters:  [otlphttp]
```

## Logs

```yaml
service:
  pipelines:
    logs:
      receivers:  [otlp, filelog/envoy-accesslog]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters:  [otlphttp/loki]
```

## Metrics

```yaml
service:
  pipelines:
    metrics:
      receivers:  [otlp, prometheus]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters:  [prometheus, otlphttp]
```

!!! tip "Pipeline placement rule"
    Place `gatewayapi` **after** `memory_limiter` and **after** `k8sattributes` (so you don't pay the HTTPRoute lookup cost on memory-dropped records, and so `k8sattributes` has already populated `k8s.pod.*`/`k8s.namespace.*`). Place it **before** `batch` so `ExcludeFromMetricAttributes` strips uids *before* they get serialized.

## Restricting scope

If you only want to enrich signals from a single demo namespace, override `watch.namespaces` and the RBAC:

```yaml
processors:
  gatewayapi:
    watch:
      namespaces: ["demo"]
```

Namespace-scoped RBAC: replace the `ClusterRole`/`ClusterRoleBinding` in `deploy/40-collector/rbac.yaml` with a `Role`/`RoleBinding` under `demo`. The informer factory picks up the same config.

## Disabling metrics enrichment entirely

High-cardinality paranoia is legitimate — you can turn metrics enrichment off and keep traces/logs enriched:

```yaml
processors:
  gatewayapi:
    enrich:
      traces: true
      logs: true
      metrics: false
```

See [How it works → Cardinality guard](../how-it-works/index.md#cardinality-guard) for the reasoning behind the default exclusions.

## Next

- [Verification](verification.md) — confirm the processor is working.
- [Configuration](../configuration/index.md) — the full field reference.
