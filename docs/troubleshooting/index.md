---
title: Troubleshooting
---

# Troubleshooting

Common issues and how to diagnose them.

## Informer sync timeout

**Symptom:** Collector pod fails to become Ready. Logs show:

```
ERROR  gatewayapiprocessor/informer.go  failed to sync informer caches   {"cache":"httproute"}
```

**Cause:** RBAC. The ServiceAccount the collector runs as cannot `list`/`watch` one of `gateways`, `httproutes`, `grpcroutes`, `gatewayclasses`, or (if `backendref_fallback.enabled=true`) `services`.

**Fix:** Apply the reference `ClusterRole` + `ClusterRoleBinding` in [`deploy/40-collector/rbac.yaml`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/deploy/40-collector/rbac.yaml), making sure `subjects[].name` matches your collector's ServiceAccount. If you're running namespace-scoped, swap to `Role`/`RoleBinding` under the namespace and set `watch.namespaces` accordingly.

**Verify:**

```bash
kubectl auth can-i list httproutes.gateway.networking.k8s.io \
  --as=system:serviceaccount:otel-system:otelcol-gatewayapi
```

## Ambiguous attribution

**Symptom:** Records carry `k8s.gatewayapi.parser=backendref_fallback`, but the `k8s.httproute.name` attributed doesn't match what you expect.

**Cause:** Two or more HTTPRoutes share the same `backendRefs[]` (they both point to the same Service). The fallback picks the first indexed match.

**Fix options:**

1. Disable the fallback in strict clusters:

    ```yaml
    processors:
      gatewayapi:
        backendref_fallback:
          enabled: false
    ```

2. Ensure your mesh/gateway emits `route_name` upstream so the `envoy` or `linkerd` parser matches first. Once a structured parser matches, the fallback never runs.
3. Disambiguate at the workload level by giving each HTTPRoute a distinct backend Service (even if they route to the same pods).

## Missing HTTPRoute status attributes

**Symptom:** `k8s.httproute.name` and `k8s.httproute.namespace` are stamped, but `k8s.httproute.accepted` and `k8s.httproute.resolved_refs` are not.

**Cause:** Either `emit_status_conditions=false` is set, or the HTTPRoute's controller has never written status conditions (fresh HTTPRoute, or misbehaving controller).

**Fix:** Check:

```bash
kubectl get httproute api -n demo -o jsonpath='{.status.parents}' | jq .
```

If `status.parents` is empty or missing `Accepted`/`ResolvedRefs`, the controller is the problem, not the processor. Verify the `parentRefs` point at an installed Gateway.

## Istio ambient gotchas

- **Ambient waypoint controller name.** The default `envoy` parser matches `^istio\.io/gateway-controller$` — that's the classic (sidecar) Istio controller. Ambient waypoints use the same controller name as of Istio 1.26, but if you've set `spec.gatewayClassName` on a `waypoint`-profile Gateway to a custom class, add the custom controller's regex to the `controllers` list. The demo pins Istio 1.26.0, so stock installs of that version work out of the box.
- **Telemetry API interaction.** Istio's Telemetry API applies dimensions at the proxy. If you configure Istio Telemetry to emit UIDs as dimensions *and* keep this processor's UID enrichment on metrics (overriding the default guard), you will duplicate UIDs as metric labels from two sources. Keep the default `exclude_from_metric_attributes` unless you've audited both sources.

## Custom `format_regex` rejected

**Symptom:** Collector fails to start with:

```
gatewayapiprocessor: parser "envoy" format_regex must define named groups 'ns' and 'name'
```

**Cause:** `Config.Validate()` requires both named groups on the envoy parser's regex.

**Fix:** Ensure your regex uses `(?P<ns>...)` and `(?P<name>...)`:

```yaml
format_regex: "^route/(?P<ns>[^/]+)/(?P<name>[^/]+)$"
```

`rule` and `match` named groups are optional — omit them if your upstream format doesn't carry those indexes.

## Passthrough dominating the output

**Symptom:** Every record lands with `k8s.gatewayapi.parser=passthrough`; no records get `k8s.httproute.name`.

**Cause:** One of:

1. The `envoy` parser's `controllers` regex doesn't match your `GatewayClass.spec.controllerName`. Check:

    ```bash
    kubectl get gatewayclass -o jsonpath='{.items[*].spec.controllerName}'
    ```

    If the controller name you see isn't in the default regex list, add it to `controllers`.

2. Your proxy doesn't emit the canonical `httproute/<ns>/<name>/...` format on the configured `source_attribute`. Check a raw record:

    ```bash
    kubectl logs deploy/otelcol-gatewayapi -n otel-system | grep route_name
    ```

    Adjust `format_regex` to match, or change `source_attribute` to a different key your data plane uses.

## High metric cardinality after enabling the processor

**Symptom:** Your metrics backend's cardinality alarm fires shortly after turning the processor on.

**Cause:** Most likely you overrode `exclude_from_metric_attributes` to an empty list, which stamps UIDs on every metric series.

**Fix:** Restore the default cardinality guard:

```yaml
processors:
  gatewayapi:
    enrich:
      exclude_from_metric_attributes:
        - "k8s.httproute.uid"
        - "k8s.gateway.uid"
        - "k8s.gatewayapi.raw_route_name"
```

Or disable metrics enrichment entirely:

```yaml
processors:
  gatewayapi:
    enrich:
      metrics: false
```

## Backpressure / memory pressure

**Symptom:** Collector pod OOMKilled under load after enabling the processor.

**Cause:** Your `memory_limiter` is either not first in the pipeline or not tuned for the extra informer cache memory (typically 1–5 MB in practice).

**Fix:** Ensure pipeline order starts with `memory_limiter`, set `limit_percentage: 80` / `spike_limit_percentage: 25` (standard defaults), and re-check pod memory limits. The processor itself does not hold a buffer of records — enrichment is synchronous per-record.

## Filing a bug

If none of the above helps, file an issue at https://github.com/henrikrexed/gatewayapiprocessor/issues with:

- Your full `gatewayapi` config block.
- A sample input record (raw `route_name` / Linkerd labels / `server.address` value).
- The output record you expected vs. what you got.
- `kubectl get gatewayclass -o yaml` for the class in question.

## See also

- [Configuration](../configuration/index.md)
- [How it works](../how-it-works/index.md)
