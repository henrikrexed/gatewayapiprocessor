---
title: Passthrough parser
---

# Passthrough parser

The `passthrough` parser is the sink at the end of the chain. When no prior parser matched, passthrough copies the raw `source_attribute` value to a dedicated attribute so operators can still find the record by its opaque route identity.

## Default config

```yaml
parsers:
  - name: passthrough
    source_attribute: "route_name"
    passthrough_attribute: "k8s.gatewayapi.raw_route_name"
```

## Fields

| Field                     | Required   | Default                           | Notes                                                   |
| ------------------------- | ---------- | --------------------------------- | ------------------------------------------------------- |
| `name`                    | yes        | —                                 | Must be `"passthrough"`. Must be **last** in the chain. |
| `source_attribute`        | yes        | `"route_name"`                    | Attribute the parser reads to build the raw string.     |
| `passthrough_attribute`   | no         | `"k8s.gatewayapi.raw_route_name"` | Attribute the parser writes the raw string to.          |

## What gets stamped

On a passthrough match, the processor emits:

| Attribute                          | Value                                          |
| ---------------------------------- | ---------------------------------------------- |
| `k8s.gatewayapi.raw_route_name`    | Whatever string was in `source_attribute`.     |
| `k8s.gatewayapi.parser`            | `passthrough`                                  |

No `k8s.httproute.*` or `k8s.gateway.*` attributes are stamped — the HTTPRoute wasn't identified. If the record then also matches a `backendref_fallback` lookup, the fallback stamps the normalized attributes separately and sets `k8s.gatewayapi.parser=backendref_fallback`.

## Why it must be last

`Config.Validate()` enforces ordering:

```
gatewayapiprocessor: passthrough parser must be last (found at index 0 of 3)
```

If `passthrough` ran first, every record with a non-empty `route_name` would match and short-circuit the Envoy/Linkerd parsers. Keeping it last guarantees the chain tries all structured parsers before falling back.

## Cardinality

`k8s.gatewayapi.raw_route_name` is **stripped from metrics by default** (see [`enrich.exclude_from_metric_attributes`](index.md#enrichexclude_from_metric_attributes)). The raw string is high-cardinality and rarely useful as a metric dimension — it's intended as a breadcrumb on traces and logs.

## When passthrough is a signal

A span landing in your backend with `k8s.gatewayapi.parser=passthrough` is telling you one of:

1. The controller emitting the record isn't covered by your `envoy`/`linkerd` parsers (add a regex).
2. The controller *is* covered, but its `route_name` format doesn't match your regex (update the regex).
3. The record genuinely has no HTTPRoute identity — e.g. a direct pod-to-pod span without a mesh hop.

## See also

- [Envoy-family parser](parser-envoy.md)
- [Linkerd parser](parser-linkerd.md)
- [Troubleshooting → Ambiguous attribution](../troubleshooting/index.md#ambiguous-attribution)
