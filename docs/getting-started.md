# Getting started

Install `gatewayapiprocessor` into a custom collector and run it against a kind
cluster in under five minutes. The repo already ships a `make demo` target
that does the full end-to-end walk for the ObsSummit 2026 demo stack; this
page covers just the processor.

## Prerequisites

- [OpenTelemetry Collector Builder (OCB)](https://opentelemetry.io/docs/collector/custom-collector/) &mdash; `go install go.opentelemetry.io/collector/cmd/builder@latest`.
- Go 1.25+ on `PATH`.
- A Kubernetes cluster with Gateway API v1.3+ CRDs installed. For local
  experimentation, `kind` works fine.

## 1. Build a custom collector with the processor

This repo ships an OCB manifest at `builder-config.yaml`. The key line is the
processor gomod entry:

```yaml
processors:
  - gomod: github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor v0.1.0
```

Build:

```bash
# from the repo root
make build-collector
# or directly:
builder --config builder-config.yaml
```

This produces `./_build/otelcol-gatewayapi`.

The pre-built multi-arch image is also available on GHCR:

```
ghcr.io/henrikrexed/gatewayapiprocessor:0.1.0
```

## 2. Minimal collector config

Save this as `collector.yaml`. It wires the processor into the traces
pipeline. Pipeline placement is intentional: `memory_limiter` first,
`k8sattributes` before `gatewayapiprocessor`, `batch` last.

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
  gatewayapi: {}     # all defaults
  batch: {}

exporters:
  debug:
    verbosity: detailed

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters: [debug]
    logs:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      processors: [memory_limiter, k8sattributes, gatewayapi, batch]
      exporters: [debug]
```

## 3. RBAC

When running in-cluster, the collector's service account needs read access to
Gateway API resources:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gatewayapiprocessor-reader
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: [gateways, httproutes, grpcroutes, gatewayclasses]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: gatewayapiprocessor-reader
subjects:
  - kind: ServiceAccount
    name: otelcol
    namespace: observability
roleRef:
  kind: ClusterRole
  name: gatewayapiprocessor-reader
  apiGroup: rbac.authorization.k8s.io
```

Substitute your collector's service account name and namespace.

## 4. Run locally against a kubeconfig

For a first smoke test you can skip cluster-side RBAC and run the collector
binary locally with a kubeconfig:

```bash
./_build/otelcol-gatewayapi --config=collector.yaml
```

Update `collector.yaml` to use `auth_type: kubeConfig` during local dev:

```yaml
processors:
  gatewayapi:
    auth_type: kubeConfig
    kube_config_path: /home/you/.kube/config
```

Send one OTLP trace with a `route_name` attribute matching your cluster's
HTTPRoute:

```bash
# Example: send a single span via grpcurl / otel-cli with an attribute:
#   route_name = "httproute/default/api/rule/0/match/0"
```

Watch the `debug` exporter output. You should see the span coming out with
`k8s.httproute.name=api`, `k8s.httproute.namespace=default`,
`k8s.gateway.name=<parent>`, and so on.

## 5. Verify the parser won

Every enriched record carries `k8s.gatewayapi.parser` with the name of the
parser that matched. Use this to confirm chain routing:

```
k8s.gatewayapi.parser = "envoy"
```

If you see `raw_route_name` set instead of structured HTTPRoute attributes,
the parser chain did not match &mdash; see
[Architecture &mdash; Parser chain](architecture.md#parser-chain).

## 6. Move to a real pipeline

From here:

- Add real exporters (OTLP to Tempo/Loki/Prometheus, or Dynatrace OTel, etc.).
- Switch `auth_type` to `serviceAccount` and deploy via the operator or a
  DaemonSet.
- Scope `watch.namespaces` if the collector service account does not have
  cluster-wide read access.
- Tighten `enrich.exclude_from_metric_attributes` if your metrics backend is
  cost-sensitive on cardinality.

See the [Configuration reference](configuration.md) for every knob and
[Examples](examples.md) for patterns.

## Full ObsSummit demo stack

To bring up the full pinned demo stack (kind + ambient + kgateway + OBI +
OTel Demo + the custom collector) used in the 2026 ObsSummit talk:

```bash
git clone https://github.com/henrikrexed/gatewayapiprocessor
cd gatewayapiprocessor
make demo
```

See the repo's [README](https://github.com/henrikrexed/gatewayapiprocessor#gatewayapiprocessor)
for the full break/fix demo flow.
