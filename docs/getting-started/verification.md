---
title: Verification
---

# Verification

Three checks confirm the processor is working end-to-end.

## 1. Informer cache sync

On startup the processor starts four shared informers (`Gateway`, `HTTPRoute`, `GRPCRoute`, `GatewayClass`) and waits for all of them to sync before the collector pipelines accept data. If RBAC is wrong the processor fails `Start()` within `informer_sync_timeout` (default 30s) with a clear error.

```bash
kubectl logs -n otel-system deploy/otelcol-gatewayapi | grep gatewayapi
```

Expected lines (field names may vary by collector version):

```
INFO    gatewayapiprocessor/informer.go informers synced
```

If you see `failed to sync informer caches` or a `Forbidden` error, check the `ClusterRole`/`RoleBinding` from [Installation](installation.md#1-apply-the-clusterrole-and-rolebinding).

## 2. HTTPRoute Accepted

Apply a demo HTTPRoute and wait for the controller to accept it:

```bash
kubectl apply -f deploy/30-demo/httproute-api.yaml
kubectl wait --for=condition=Accepted=True httproute/api --namespace demo --timeout=120s
```

`Accepted=True` is the signal that the processor's informer has both the HTTPRoute and its parent Gateway — from this point on, spans carrying the matching `route_name` will be enriched.

## 3. Attribute stamping

Send a test span with a recognizable `route_name` attribute (simulating an Envoy-family proxy):

```bash
otlp-cli trace \
  --attr service.name=api \
  --attr route_name=httproute/demo/api/rule/0/match/0 \
  --endpoint https://otelcol-gatewayapi.otel-system.svc:4318
```

The span received at your backend should now carry:

| Attribute                     | Value                                |
| ----------------------------- | ------------------------------------ |
| `k8s.httproute.name`          | `api`                                |
| `k8s.httproute.namespace`     | `demo`                               |
| `k8s.httproute.rule_index`    | `0`                                  |
| `k8s.httproute.match_index`   | `0`                                  |
| `k8s.httproute.accepted`      | `true`                               |
| `k8s.httproute.resolved_refs` | `true`                               |
| `k8s.gateway.name`            | *name of the HTTPRoute's parent Gateway* |
| `k8s.gatewayclass.name`       | *name of the parent Gateway's class*     |
| `k8s.gatewayclass.controller` | e.g. `gateway.envoyproxy.io/gatewayclass-controller` |
| `k8s.gatewayapi.parser`       | `envoy`                              |

If the span is missing `k8s.httproute.*` but has `k8s.gatewayapi.raw_route_name` and `k8s.gatewayapi.parser=passthrough`, your controller isn't matched by the envoy parser's `controllers` regex. See [Configuration → Envoy-family parser](../configuration/parser-envoy.md).

## 4. (Optional) Hero demo

The repo ships a scripted hero demo you can run in any kind-compatible host:

```bash
make demo        # full stack: kind + ambient + kgateway + OBI + OTel Demo
make break       # 503 spike on k8s.httproute.name=api, resolved_refs=false
make fix         # restores backendRef, green within ~30s
make clean
```

The same queries render in Grafana (`backends/grafana/dashboards/before-after.json`) and Dynatrace (`backends/dynatrace/notebook.json`).

## Next

- [Configuration](../configuration/index.md) — tune parsers and enrichment scope.
- [Troubleshooting](../troubleshooting/index.md) — common bring-up issues.
