---
title: Linkerd parser
---

# Linkerd parser

Linkerd exposes HTTPRoute identity as **three separate labels** instead of one opaque string. No parsing is needed — the Linkerd parser just picks them up and normalizes them into the same `k8s.httproute.*` namespace.

## Default config

```yaml
parsers:
  - name: linkerd
    controllers:
      - "^linkerd\\.io/gateway-controller$"
    linkerd_labels:
      route_name: "route_name"
      route_kind: "route_kind"
      route_namespace: "route_namespace"
      parent_name: "parent_name"
```

## Fields

| Field                              | Required   | Default              | Notes                                                      |
| ---------------------------------- | ---------- | -------------------- | ---------------------------------------------------------- |
| `name`                             | yes        | —                    | Must be `"linkerd"` to bind to this plug-in.               |
| `controllers`                      | no         | (see above)          | Regex patterns for `GatewayClass.spec.controllerName`.     |
| `linkerd_labels.route_name`        | no         | `"route_name"`       | Source label for the HTTPRoute name.                       |
| `linkerd_labels.route_kind`        | no         | `"route_kind"`       | Source label for `HTTPRoute` vs `GRPCRoute`.               |
| `linkerd_labels.route_namespace`   | no         | `"route_namespace"`  | Source label for the HTTPRoute namespace.                  |
| `linkerd_labels.parent_name`       | no         | `"parent_name"`      | Source label for the parent Gateway's name.                |

## Input / output

Metric with Linkerd labels:

```
route_name="api"
route_kind="HTTPRoute"
route_namespace="demo"
parent_name="public-gw"
```

Becomes:

| Attribute                     | Value                             |
| ----------------------------- | --------------------------------- |
| `k8s.httproute.name`          | `api`                             |
| `k8s.httproute.namespace`     | `demo`                            |
| `k8s.httproute.accepted`      | `true` *(from informer cache)*    |
| `k8s.httproute.resolved_refs` | `true` *(from informer cache)*    |
| `k8s.gateway.name`            | `public-gw`                       |
| `k8s.gatewayclass.name`       | `linkerd-gateway`                 |
| `k8s.gatewayclass.controller` | `linkerd.io/gateway-controller`   |
| `k8s.gatewayapi.parser`       | `linkerd`                         |

If `route_kind="GRPCRoute"` (case-insensitive), the parser stamps `k8s.grpcroute.name` / `k8s.grpcroute.namespace` instead of their HTTPRoute equivalents.

## Caveats

- The parser requires **both** `route_name` and `route_namespace` to be non-empty. If `route_namespace` is missing, the parser returns no match and the record falls through to the next parser (typically `passthrough`).
- Linkerd does not emit `rule_index` or `match_index`, so those attributes are never stamped for Linkerd records.
- `parent_name` is not currently emitted as its own attribute — it's reserved for future use when we need to disambiguate `parentRefs` across gateways.

## See also

- [Envoy-family parser](parser-envoy.md)
- [Passthrough parser](parser-passthrough.md)
