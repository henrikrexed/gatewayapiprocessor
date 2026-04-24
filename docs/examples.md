# Example configurations

All examples below go under the collector's `processors:` block and must be
wired into a pipeline (see [Getting started](getting-started.md) for a
complete collector config).

## Minimal

Accept all defaults. The processor runs with the default parser chain
(envoy + linkerd + passthrough), enriches all three signals, and reads
in-cluster credentials.

```yaml
processors:
  gatewayapi: {}
```

## Namespace-scoped watch

Scope informers to a list of namespaces. Useful when the collector's service
account does not have cluster-wide Gateway API read access.

```yaml
processors:
  gatewayapi:
    watch:
      namespaces:
        - default
        - apps
      resync_period: 10m
```

## Dev / local kubeconfig

Point the processor at a local kubeconfig for development. Not for production.

```yaml
processors:
  gatewayapi:
    auth_type: kubeConfig
    kube_config_path: /home/you/.kube/config
```

## String-only mode (no Kubernetes client)

Use when you want the parser chain to run but do not have cluster access (for
example, in integration tests). Status conditions and Gateway/HTTPRoute
lookups are skipped; only attributes that can be derived from the signal
record itself are written.

```yaml
processors:
  gatewayapi:
    auth_type: none
    emit_status_conditions: false
    parsers:
      - name: envoy
        source_attribute: route_name
        format_regex: '^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)'
      - name: passthrough
        source_attribute: route_name
        passthrough_attribute: k8s.gatewayapi.raw_route_name
```

## Traces-only enrichment

Enrich traces but skip logs and metrics. Handy when you want to keep the
metrics pipeline completely free of Gateway API attributes.

```yaml
processors:
  gatewayapi:
    enrich:
      traces: true
      logs: false
      metrics: false
```

## Tighter metrics cardinality guard

Strip more attributes from the metrics pipeline. The default guard already
excludes UID-like attributes; this variant also drops match/rule indices.

```yaml
processors:
  gatewayapi:
    enrich:
      metrics: true
      exclude_from_metric_attributes:
        - k8s.httproute.uid
        - k8s.gateway.uid
        - k8s.gatewayapi.raw_route_name
        - k8s.httproute.rule_index
        - k8s.httproute.match_index
```

## Custom envoy format regex

Override the default regex to parse a non-standard `route_name` format. The
regex must still define the named groups `ns` and `name`.

```yaml
processors:
  gatewayapi:
    parsers:
      - name: envoy
        controllers:
          - '^example\.com/gateway-controller$'
        source_attribute: route_name
        format_regex: '^route:(?P<ns>[^:]+):(?P<name>[^:]+):r(?P<rule>\d+)$'
      - name: passthrough
        source_attribute: route_name
        passthrough_attribute: k8s.gatewayapi.raw_route_name
```

## Linkerd-only

Only run the Linkerd parser. Non-Linkerd traffic falls through to passthrough.

```yaml
processors:
  gatewayapi:
    parsers:
      - name: linkerd
        controllers:
          - '^linkerd\.io/gateway-controller$'
        linkerd_labels:
          route_name: route_name
          route_kind: route_kind
          route_namespace: route_namespace
          parent_name: parent_name
      - name: passthrough
        source_attribute: route_name
        passthrough_attribute: k8s.gatewayapi.raw_route_name
```

## BackendRef fallback disabled

Turn off the backendRef best-effort fallback when you only want enrichment
backed by a real HTTPRoute match.

```yaml
processors:
  gatewayapi:
    backendref_fallback:
      enabled: false
```

## Faster informer sync timeout

Shorter `informer_sync_timeout` trades startup strictness for faster failure:
if the Gateway API CRDs are missing, the collector fails its readiness probe
sooner.

```yaml
processors:
  gatewayapi:
    informer_sync_timeout: 10s
```

## Full, all-keys example

Every key set to a non-default value for reference.

```yaml
processors:
  gatewayapi:
    auth_type: serviceAccount
    watch:
      namespaces:
        - default
        - apps
      resync_period: 2m
    parsers:
      - name: envoy
        controllers:
          - '^gateway\.envoyproxy\.io/gatewayclass-controller$'
          - '^kgateway\.dev/gatewayclass-controller$'
        source_attribute: route_name
        format_regex: '^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)(?:/rule/(?P<rule>\d+))?(?:/match/(?P<match>\d+))?'
      - name: linkerd
        controllers:
          - '^linkerd\.io/gateway-controller$'
        linkerd_labels:
          route_name: route_name
          route_kind: route_kind
          route_namespace: route_namespace
          parent_name: parent_name
      - name: passthrough
        source_attribute: route_name
        passthrough_attribute: k8s.gatewayapi.raw_route_name
    enrich:
      traces: true
      logs: true
      metrics: true
      exclude_from_metric_attributes:
        - k8s.httproute.uid
        - k8s.gateway.uid
        - k8s.gatewayapi.raw_route_name
    emit_status_conditions: true
    backendref_fallback:
      enabled: true
      source_attribute: server.address
    informer_sync_timeout: 30s
```
