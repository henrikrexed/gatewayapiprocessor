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
| `mesh`                    | object &mdash; [MeshConfig](#mesh)     | see below        | GAMMA (service-mesh) enrichment knobs: waypoint recognition and producer/consumer classification. |
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
| `exclude_from_metric_attributes`   | list of string | `[k8s.httproute.uid, k8s.grpcroute.uid, k8s.gateway.uid, k8s.gatewayapi.raw_route_name, k8s.service.parent.port, k8s.httproute.rule.name, k8s.grpcroute.rule.name]` | Attribute keys stripped from metrics before emit. Cardinality guard: keep UID-like, raw-string, and per-rule/per-port GAMMA attributes off the metrics path (ISI-6178 / design D6). |

## backendref_fallback

Best-effort enrichment used when no HTTPRoute match is resolved but a
`backendRef`-style hint is available on the record.

| Key                | Type   | Default          | Description                                                                       |
| ------------------ | ------ | ---------------- | --------------------------------------------------------------------------------- |
| `enabled`          | bool   | `true`           | Enable backendRef fallback.                                                       |
| `source_attribute` | string | `server.address` | Attribute key read for the fallback hint (e.g. the downstream service address).  |

### Mesh + ingress disambiguation (v0.3, ISI-805)

When a single Service is the backend of both a GAMMA mesh-mode route
(`parentRef.kind = Service`) and an ingress-mode route
(`parentRef.kind = Gateway`), the single-candidate index drops the entry as
ambiguous. The processor then runs a binary disambiguator on the candidate
set:

1. **Both candidates same parent kind** (mesh+mesh or ingress+ingress) → no
   stamp. The original "never mis-attribute when ambiguous" safety contract is
   preserved (`TestBackendRefFallback_AmbiguousOwner_NoStamp`).
2. **Three or more candidates** → no stamp. Disambiguation only kicks in for
   the binary mesh+ingress topology — anything beyond that is still treated as
   ambiguous.
3. **Exactly one mesh + one ingress candidate** → the disambiguator inspects
   the span's `k8s.namespace.name` resource attribute:
   - if it matches the mesh route's `parentRef` Service namespace, the **mesh
     route** is stamped (the span is running inside the mesh's east-west
     fabric);
   - otherwise the **ingress route** is stamped (the default — ingress is the
     correct attribution for cross-cluster / north-south spans, including the
     case where a span carries no resource namespace at all).

Worked example — the otel-demo `frontend-proxy` Service is referenced by:

```yaml
# GAMMA mesh route
spec:
  parentRefs:
    - kind: Service
      name: frontend-proxy
  rules:
    - backendRefs:
        - name: frontend-proxy
# Ingress route
spec:
  parentRefs:
    - kind: Gateway
      name: oteldemo-ingress
  rules:
    - backendRefs:
        - name: frontend-proxy
```

A span emitted by a workload running in `otel-demo` and pointed at
`frontend-proxy.otel-demo.svc.cluster.local` is stamped with the mesh route
(`gateway.networking.k8s.io/route-mode = mesh` + `k8s.service.parent.*`). A
span originating outside `otel-demo` (or with no `k8s.namespace.name` resource
attribute) is stamped with the ingress route
(`gateway.networking.k8s.io/route-mode = ingress` + `k8s.gateway.*`).

## mesh

GAMMA (Gateway API for Mesh) enrichment knobs. GAMMA attaches an
`HTTPRoute`/`GRPCRoute` to a **Service** (`parentRef.kind = Service`) for
east-west traffic, or routes through a **waypoint** Gateway whose class is a
mesh data-plane proxy. Added in [ISI-6178](https://paperclip.isitobservable.com/ISI/issues/ISI-6178)
implementing the Architect-approved design decisions D1–D6
([ISI-6175](https://paperclip.isitobservable.com/ISI/issues/ISI-6175)). All of
it is additive — existing `k8s.service.parent.name/namespace` and
`gateway.networking.k8s.io/route-mode` keys are unchanged.

| Key                     | Type           | Default                                           | Description                                                                                                                                                       |
| ----------------------- | -------------- | ------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `waypoint_class_names`  | list of string | `[istio-waypoint, kgateway-waypoint, istio-eastwest]` | gatewayClassName values that identify a mesh waypoint Gateway. A route whose parent Gateway uses one of these classes is classified as `mesh` and stamped with a waypoint implementation. Data-driven: a new mesh impl needs a config change, not a code change. Empty disables class-based waypoint detection (and the associated Service label-cache informer). |
| `classify_route_role`   | bool           | `true`                                            | Stamp `k8s.gatewayapi.route.role` (`producer`/`consumer`) on mesh routes. Set `false` to suppress.                                                                 |
| `infer_service_parent`  | bool           | `true`                                            | Enable nil-Kind parent-Service inference (design D4, [ISI-6188](https://paperclip.isitobservable.com/ISI/issues/ISI-6188)). When a route's `parentRef` omits `Kind` (which per spec defaults to `Gateway`), resolves to no Gateway, yet names a real Service, the route is reclassified as `mesh`. Set `false` to stay strictly spec-literal. See [nil-Kind parent-Service inference](#nil-kind-parent-service-inference) below. |

### GAMMA attribute schema

Stamped on spans/logs (and metrics, except the keys in the D6 strip-list):

| Attribute | Type | Stamped when | Notes |
|---|---|---|---|
| `k8s.httproute.parent.kind` / `k8s.grpcroute.parent.kind` | string | always | `Gateway` or `Service` (defaults to `Gateway` per spec when `parentRef.kind` is omitted). Flips to `Service` when nil-Kind inference reclassifies the parent as mesh — see [below](#nil-kind-parent-service-inference). |
| `k8s.service.parent.name` / `.namespace` | string | mesh (Service parent) | Parent Service identity (shipped before ISI-6178). |
| `k8s.service.parent.port` | int | mesh + `parentRef.port` set | **Stripped from metrics (D6).** |
| `gateway.networking.k8s.io/route-mode` | enum `mesh`\|`ingress` | always | Single source of truth (D2). Flips to `mesh` for a Service parent **or** a waypoint GatewayClass. |
| `k8s.gatewayapi.waypoint.implementation` | enum `istio-ambient`\|`kgateway`\|`istio-eastwest` | waypoint route | From the GatewayClass (class-based) or the parent Service's `istio.io/use-waypoint` label (label-based). |
| `k8s.gatewayapi.waypoint.name` / `.namespace` | string | waypoint resolved | Waypoint Gateway identity (class path) or label value (`istio.io/use-waypoint`, namespace from `istio.io/use-waypoint-namespace`). |
| `k8s.gatewayapi.route.role` | enum `producer`\|`consumer` | mesh + `classify_route_role` | `producer` when the route shares its parent Service's namespace; `consumer` otherwise. |
| `k8s.httproute.rule.name` / `k8s.grpcroute.rule.name` | string | matched rule is named (gateway-api ≥ v1.4) | Resolved from the positional rule index. `k8s.{http,grpc}route.rule_index` remains for unnamed rules. **Stripped from metrics (D6).** |

> **Cilium note:** Cilium's GAMMA implementation supports **producer routes only** — consumer routes are silently ignored by Cilium, so a `k8s.gatewayapi.route.role = consumer` attribute on a Cilium mesh may describe a route that is not actually programmed.

### Waypoint detection & the shared Service informer

Waypoint attribution uses two signals:

1. **Class-based** — the parent Gateway's `gatewayClassName` is in
   `waypoint_class_names`. Resolved at route-projection time, no extra watch.
2. **Label-based** — the parent Service carries `istio.io/use-waypoint`.
   Resolved at stamp time against a Service label-cache.

The label-cache reuses the **existing** core `Service` informer that powers
`backendref_fallback` (one watch, N indexes — design D5). That informer now
starts when `backendref_fallback.enabled` **or** `mesh.waypoint_class_names` is
non-empty **or** `mesh.infer_service_parent` is on, and builds only the indexes
the enabled features need.

### nil-Kind parent-Service inference

Per the Gateway API spec a `parentRef.kind` *defaults* to `Gateway` when
omitted, so a well-formed GAMMA mesh route (parent = a Service) must set
`kind: Service` explicitly. Real-world manifests — and OBI-derived spans that
synthesize `parentRefs` — are inconsistent and frequently omit `Kind`, which
would otherwise misclassify a mesh Service parent as a (missing) Gateway and
drop the route to `ingress` with no attribution.

With `mesh.infer_service_parent: true` (the default) the processor keeps the
spec-correct default but adds a guarded fallback: a nil-Kind **core-group**
`parentRef` that resolves to **no Gateway**, yet whose `ns/name` names a **real
Service**, is reclassified as `mesh` — stamping the same
`k8s.service.parent.*` + `route-mode = mesh` attributes as an explicit Service
parent, and flipping `*.parent.kind` to `Service`. Two guardrails bound it:

1. **Post-sync only.** The fallback fires only *after* the Gateway informer has
   completed its initial list/sync. During sync, "no Gateway with this ns/name"
   is indistinguishable from "the Gateway informer hasn't listed it yet", so
   inferring then would false-positive every ingress route whose Gateway lists
   late. A route projected during the initial sync is cached as `ingress` and
   reclassified on its next re-projection (a route update or the
   `watch.resync_period` relist, default 5m).
2. **No per-span marker.** A reclassification increments the internal
   self-telemetry counter `gatewayapi_parent_inferred_total` and emits a
   debug-log — but stamps **no** marker attribute on the data. The inferred
   mesh attributes are indistinguishable from an explicit Service parent;
   whether they were *inferred* is observable only through the counter + log.

An explicit `kind: Gateway` parentRef is **never** meshed even if it dangles
(no such Gateway) and a same-named Service exists — only an omitted/empty
`Kind` is a candidate.

The Service-existence check uses the Service **name** index — a third index on
the same shared Service informer described above (design D5: one watch, N
indexes), built only when `infer_service_parent` is on.

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
- Every `mesh.waypoint_class_names[*]` entry must be non-empty (non-whitespace).

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
        - k8s.grpcroute.uid
        - k8s.gateway.uid
        - k8s.gatewayapi.raw_route_name
        - k8s.service.parent.port
        - k8s.httproute.rule.name
        - k8s.grpcroute.rule.name
    emit_status_conditions: true
    backendref_fallback:
      enabled: true
      source_attribute: server.address
    mesh:
      waypoint_class_names:
        - istio-waypoint
        - kgateway-waypoint
        - istio-eastwest
      classify_route_role: true
      infer_service_parent: true
    informer_sync_timeout: 30s
```
