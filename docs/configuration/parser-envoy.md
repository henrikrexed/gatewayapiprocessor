---
title: Envoy-family parser
---

# Envoy-family parser

The `envoy` parser decodes the opaque `route_name` string emitted by **Envoy Gateway**, **Kgateway**, and **Istio** into a `(namespace, name, rule_index, match_index)` tuple. The underlying format is not a stable contract across these projects, so the regex is configurable.

## Canonical format

```
httproute/<namespace>/<name>/rule/<rule_index>/match/<match_index>
```

Trailing `/rule/<N>` and `/match/<M>` are optional — a record with only `httproute/<ns>/<name>` still yields `k8s.httproute.name` and `k8s.httproute.namespace` but leaves the rule/match indexes unstamped.

## Default config

```yaml
parsers:
  - name: envoy
    controllers:
      - "^gateway\\.envoyproxy\\.io/gatewayclass-controller$"   # Envoy Gateway
      - "^kgateway\\.dev/gatewayclass-controller$"              # Kgateway
      - "^istio\\.io/gateway-controller$"                       # Istio
    source_attribute: "route_name"
    format_regex: "^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)(?:/rule/(?P<rule>\\d+))?(?:/match/(?P<match>\\d+))?"
```

## Fields

| Field              | Required   | Default                 | Notes                                                                                  |
| ------------------ | ---------- | ----------------------- | -------------------------------------------------------------------------------------- |
| `name`             | yes        | —                       | Must be `"envoy"` to bind to this plug-in.                                             |
| `controllers`      | no         | (see above)             | List of regex patterns matched against `GatewayClass.spec.controllerName`.             |
| `source_attribute` | yes        | `"route_name"`          | The signal attribute the parser reads.                                                 |
| `format_regex`     | yes        | (see above)             | Named groups `ns` and `name` are **required**; `rule` and `match` are optional.        |

## Input shapes we've observed

Envoy Gateway (v1.x) — canonical:

```
route_name="httproute/default/api/rule/0/match/0"
```

Kgateway v2 — same shape, different controller name:

```
route_name="httproute/demo/cart/rule/2/match/1"
```

Istio ambient waypoint — canonical:

```
route_name="httproute/demo/api/rule/0/match/0"
```

Envoy Gateway with no rule/match index (rare, control-plane-driven):

```
route_name="httproute/default/api"
```

## Customizing the regex

If a controller you use emits a different shape, provide your own regex — just keep the named groups:

```yaml
parsers:
  - name: envoy
    controllers:
      - "^my-custom-controller$"
    source_attribute: "x-my-route-id"
    format_regex: "^(?P<ns>[^.]+)\\.(?P<name>[^.]+)\\.(?P<rule>\\d+)$"
```

`Config.Validate()` will reject a `format_regex` that doesn't expose named groups `ns` and `name`.

## What gets stamped

Example span with `route_name="httproute/demo/api/rule/2/match/1"` under `gateway.envoyproxy.io/gatewayclass-controller`:

| Attribute                           | Value                                                |
| ----------------------------------- | ---------------------------------------------------- |
| `k8s.httproute.name`                | `api`                                                |
| `k8s.httproute.namespace`           | `demo`                                               |
| `k8s.httproute.rule_index`          | `2`                                                  |
| `k8s.httproute.match_index`         | `1`                                                  |
| `k8s.httproute.parent_ref`          | `gateway.networking.k8s.io/Gateway/demo/public-gw`   |
| `k8s.httproute.accepted`            | `true` *(from informer cache)*                       |
| `k8s.httproute.resolved_refs`       | `true` *(from informer cache)*                       |
| `k8s.gateway.name`                  | `public-gw`                                          |
| `k8s.gatewayclass.name`             | `envoy-gateway`                                      |
| `k8s.gatewayclass.controller`       | `gateway.envoyproxy.io/gatewayclass-controller`      |
| `k8s.gatewayapi.parser`             | `envoy`                                              |

## Caveats

- If two parsers match the same record (e.g. you configure both `envoy` and a custom Envoy-compatible parser), the **first one in the list wins**.
- The regex is applied against the value of `source_attribute` as a string — if your proxy emits `route_name` as an array or an object, you'll need a receiver-side transform to flatten it first.
- The parser does not peek at the record's `service.name` or `k8s.pod.name`. Only `source_attribute` plus the informer cache are used.

## See also

- [Linkerd parser](parser-linkerd.md)
- [Passthrough parser](parser-passthrough.md)
- [Attribute reference](../attribute-reference/index.md)
