# Configuration reference

The processor is registered under the type key `gatewayapi`. All keys below are
YAML mapstructure field names as declared in `gatewayapiprocessor/config.go`.

## Top-level keys

| Key                       | Type                         | Default          | Description                                                                 |
| ------------------------- | ---------------------------- | ---------------- | --------------------------------------------------------------------------- |
| `auth_type`               | enum                         | `serviceAccount` | Kubernetes client auth mode: `serviceAccount`, `kubeConfig`, or `none`.     |
| `kube_config_path`        | string                       | `""`             | Path to a kubeconfig file. **Required** when `auth_type: kubeConfig`.       |
| `watch`                   | object &mdash; [WatchConfig](#watch)   | see below        | Informer scoping.                                                 |
| `parsers`                 | list &mdash; [ParserConfig](#parsers)  | see defaults     | Ordered parser chain. At least one entry required.                |
| `enrich`                  | object &mdash; [EnrichConfig](#enrich) | see below        | Which signals to enrich and metrics cardinality guard.            |
| `emit_status_conditions`  | bool                         | `true`           | When true, stamps `k8s.httproute.accepted` and `k8s.httproute.resolved_refs` from the HTTPRoute status subresource, and `k8s.grpcroute.accepted` and `k8s.grpcroute.resolved_refs` from the GRPCRoute status subresource. |
| `backendref_fallback`     | object &mdash; [BackendRefFallback](#backendref_fallback) | see below | Best-effort enrichment when no HTTPRoute match is found. |
| `informer_sync_timeout`   | duration                     | `30s`            | Upper bound on `Start()` waiting for informer caches to warm up. Must be >= 0. |

## watch

Informer scoping.

| Key             | Type           | Default | Description                                                                           |
| --------------- | -------------- | ------- | ------------------------------------------------------------------------------------- |
| `namespaces`    | list of string | `null`  | Namespaces to watch. `null` or empty means cluster-wide (all namespaces).             |
| `resync_period` | duration       | `5m`    | Informer resync interval. Must be >= 0. `0` disables periodic resyncs.                |
| `policies`      | list of [PolicyGVR](#watchpolicies) | `null` | Gateway API policy CRDs to watch via dynamic informers. When unset, no `k8s.gatewayapi.policy.*` attributes are stamped — the processor behaves exactly as it did before [ISI-804](https://paperclip.isitobservable.com/ISI/issues/ISI-804). |

### watch.policies

Each entry identifies one CRD by its dynamic `(group, version, resource)`
coordinates. The processor builds one shared dynamic informer per entry and
projects every policy whose `spec.targetRefs[]` points at an HTTPRoute or
GRPCRoute (in the `gateway.networking.k8s.io` group) onto the matched route.

| Key        | Type   | Required | Description                                                                  |
| ---------- | ------ | -------- | ---------------------------------------------------------------------------- |
| `group`    | string | yes      | API group of the policy CRD (`gateway.kgateway.dev`, `gateway.envoyproxy.io`, …). May be empty for the core API. |
| `version`  | string | yes      | API version (`v1alpha1`, `v1alpha2`, `v1`, …).                              |
| `resource` | string | yes      | Plural resource name (`trafficpolicies`, `backendconfigpolicies`, …).      |

**Acceptance gate.** A policy is stamped onto its matched routes only when
either:

- `status.conditions[type=Accepted, status=True]` (kgateway-style,
  inherited-attachment shape), or
- any `status.ancestors[*].conditions[type=Accepted, status=True]` (the
  GEP-2648 direct-attachment shape used by Envoy Gateway and others), or
- the CR has no `status` block at all (newly created, not yet reconciled —
  optimistic accept so policies enrich during the controller's reconcile
  window). Once the controller writes a status, the real conditions take
  over.

Otherwise the policy is skipped. A policy that flips from `Accepted=True` to
`Accepted=False` has its prior stamps withdrawn from the index.

**Out-of-scope target refs are skipped silently.** A `targetRef` whose `kind`
is anything other than `HTTPRoute` or `GRPCRoute`, or whose `group` is not
`gateway.networking.k8s.io`, is not enriched. The processor does not stamp
policies onto Gateways, Services, or other route kinds.

**Stamped attributes.** When at least one accepted policy targets the
matched route, the following parallel array attributes are stamped on every
span/log/metric (subject to `enrich.{traces,logs,metrics}` and
`enrich.exclude_from_metric_attributes`):

| Attribute                          | Type            | Description                                                                |
| ---------------------------------- | --------------- | -------------------------------------------------------------------------- |
| `k8s.gatewayapi.policy.names`      | `[]string`      | Policy names, one element per attached policy.                             |
| `k8s.gatewayapi.policy.kinds`      | `[]string`      | Policy CRD kinds (e.g. `TrafficPolicy`, `BackendConfigPolicy`).            |
| `k8s.gatewayapi.policy.namespaces` | `[]string`      | Namespaces of the attached policies.                                       |
| `k8s.gatewayapi.policy.groups`     | `[]string`      | API groups of the policy CRDs.                                             |
| `k8s.gatewayapi.policy.target_kind`| `string`        | `HTTPRoute` or `GRPCRoute` — mirrors the matched route kind.               |

The four list attributes are **element-wise correlated**: index `i` of every
list describes the same policy. Dashboards can group by
`policy.kinds[i]` and select the matching `policy.names[i]`.

There is **no** `policy.uid` attribute by deliberate decision
([ISI-804](https://paperclip.isitobservable.com/ISI/issues/ISI-804)) — keeping
per-span cardinality bounded by policy count, not generation churn. If the
policy attribute set is too cardinal for your metric pipeline, add the four
list attribute keys to `enrich.exclude_from_metric_attributes`; traces and
logs continue to carry the full attribution.

**Example.** Watching kgateway's `TrafficPolicy` and `BackendConfigPolicy`:

```yaml
processors:
  gatewayapi:
    watch:
      namespaces: [otel-demo]
      policies:
        - group: gateway.kgateway.dev
          version: v1alpha1
          resource: trafficpolicies
        - group: gateway.kgateway.dev
          version: v1alpha1
          resource: backendconfigpolicies
```

When a `TrafficPolicy` named `rate-limit-frontend` in `otel-demo`
references an HTTPRoute via `spec.targetRefs[]`, every span on that route
is stamped with:

```
k8s.gatewayapi.policy.names       = ["rate-limit-frontend"]
k8s.gatewayapi.policy.kinds       = ["TrafficPolicy"]
k8s.gatewayapi.policy.namespaces  = ["otel-demo"]
k8s.gatewayapi.policy.groups      = ["gateway.kgateway.dev"]
k8s.gatewayapi.policy.target_kind = "HTTPRoute"
```

## parsers

Each entry is applied in order; the **first parser that yields a non-empty
`(namespace, name)` wins**. The `passthrough` parser, if configured, **MUST
be last** &mdash; this is enforced at config validation time.

| Key                      | Type           | Default  | Description                                                                                                                  |
| ------------------------ | -------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `name`                   | enum           | required | One of `envoy`, `linkerd`, `passthrough`.                                                                                    |
| `controllers`            | list of regex  | `[]`     | Regex patterns tested against `GatewayClass.spec.controllerName`. Empty list means **match any** controller.                 |
| `source_attribute`       | string         | varies   | Signal attribute carrying the opaque route id. Used by `envoy` and `passthrough`.                                            |
| `format_regex`           | string (regex) | varies   | Named-capture regex for `envoy`-family parsers. Must define groups `ns` and `name`; `rule` and `match` are optional.         |
| `linkerd_labels`         | object         | see below| Attribute-key mapping for the `linkerd` parser. See [linkerd_labels](#linkerd_labels).                                       |
| `passthrough_attribute`  | string         | n/a      | Attribute key written by the `passthrough` parser for unparsable strings. Convention: `k8s.gatewayapi.raw_route_name`.       |

### linkerd_labels

Maps semantic roles to Linkerd's source-attribute names.

| Key              | Type   | Default            | Description                                         |
| ---------------- | ------ | ------------------ | --------------------------------------------------- |
| `route_name`     | string | `route_name`       | Attribute carrying the HTTPRoute name.              |
| `route_kind`     | string | `route_kind`       | Attribute carrying the route kind.                  |
| `route_namespace`| string | `route_namespace`  | Attribute carrying the route namespace.             |
| `parent_name`    | string | `parent_name`      | Attribute carrying the parent (Gateway) name.       |

## enrich

Which signals to enrich and the metrics cardinality guard.

| Key                                | Type           | Default                                                                                                              | Description                                                                                                                                     |
| ---------------------------------- | -------------- | -------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `traces`                           | bool           | `true`                                                                                                               | Enable trace enrichment.                                                                                                                        |
| `logs`                             | bool           | `true`                                                                                                               | Enable log enrichment.                                                                                                                          |
| `metrics`                          | bool           | `true`                                                                                                               | Enable metrics enrichment.                                                                                                                      |
| `exclude_from_metric_attributes`   | list of string | `[k8s.httproute.uid, k8s.gateway.uid, k8s.gatewayapi.raw_route_name]`                                                | Attribute keys stripped from metrics before emit. Cardinality guard: keep UID-like and raw-string attributes off the metrics path.              |

## backendref_fallback

Best-effort enrichment used when no HTTPRoute match is resolved but a
`backendRef`-style hint is available on the record.

| Key                | Type   | Default          | Description                                                                       |
| ------------------ | ------ | ---------------- | --------------------------------------------------------------------------------- |
| `enabled`          | bool   | `true`           | Enable backendRef fallback.                                                       |
| `source_attribute` | string | `server.address` | Attribute key read for the fallback hint (e.g. the downstream service address).  |

## Validation rules

Enforced by `Config.Validate()` at component startup:

- `auth_type` must be one of `serviceAccount`, `kubeConfig`, `none`, or empty
  (defaults to `serviceAccount`).
- `auth_type: kubeConfig` requires a non-empty `kube_config_path`.
- At least one parser must be configured.
- The `passthrough` parser, if present, must be the **last** entry in `parsers`.
- Every entry in `controllers` must compile as a Go regex.
- For `name: envoy`, if `format_regex` is set it must compile and **must
  define named groups `ns` and `name`**.
- `watch.resync_period` and `informer_sync_timeout` must be >= 0.
- Every `watch.policies[*]` entry must declare both `version` and `resource`
  (`group` may be empty for core-API CRDs).

## Defaults

The default config emitted by `createDefaultConfig()` is:

```yaml
processors:
  gatewayapi:
    auth_type: serviceAccount
    watch:
      resync_period: 5m
    parsers:
      - name: envoy
        controllers:
          - '^gateway\.envoyproxy\.io/gatewayclass-controller$'
          - '^kgateway\.dev/gatewayclass-controller$'
          - '^istio\.io/gateway-controller$'
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
