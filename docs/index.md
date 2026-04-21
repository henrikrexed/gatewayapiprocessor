---
title: gatewayapiprocessor
hide:
  - navigation
---

# gatewayapiprocessor

> An OpenTelemetry Collector processor that turns opaque `route_name` strings into normalized Kubernetes Gateway API attributes — the same shape across Envoy Gateway, Kgateway, Istio ambient, and Linkerd.

`gatewayapiprocessor` runs Kubernetes informers for `Gateway`, `HTTPRoute`, `GRPCRoute`, and `GatewayClass`; parses upstream route identity emitted by the data plane; and stamps normalized `k8s.gateway.*`, `k8s.httproute.*`, and `k8s.gatewayclass.*` attributes on every span, log, and metric data point. It also reads CR status (`HTTPRoute.status.parents[].conditions`) — something `transformprocessor` and OTTL cannot — so a misconfigured `backendRef` shows up directly in your telemetry.

This site is the operator-facing manual. The [repository README](https://github.com/henrikrexed/gatewayapiprocessor#readme) covers the repo layout and demo bring-up; the [processor spec](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec) is the canonical behavioral contract.

---

## When to use it

Use `gatewayapiprocessor` if **any** of these apply:

- Your telemetry carries `route_name` strings from **Envoy Gateway**, **Kgateway**, or **Istio** and you cannot join those strings to your HTTPRoute CRs in the backend.
- You use **Linkerd**, and want its `route_name` / `route_kind` / `route_namespace` labels to be normalized into the same `k8s.httproute.*` namespace you already use for Kubernetes metadata.
- You need **HTTPRoute CR status** (`Accepted`, `ResolvedRefs`) on spans/logs/metrics so `backendRef` typos and `parentRefs` mismatches are observable without leaving your dashboard.
- You operate multiple backends (Tempo + Dynatrace, Loki + Splunk, Prom + OTLP) and want **one attribute schema that survives exporter choice**.

## When not to use it

- Your cluster does not use Gateway API (classic `Ingress` only). Use `k8sattributesprocessor` instead.
- You already have a **vendor-specific** OpenTelemetry exporter that normalizes route identity upstream. Check whether adding this processor introduces conflicting attributes.
- You need to enrich signals from **Inference Extension** (`inference.pool`, `inference.objective`, `target_model_name`). That shape is metrics-only and out of scope for v0 — see [Roadmap](roadmap/index.md).

## The value, in one picture

<div class="grid cards" markdown>

-   :material-help-circle-outline: __Before__

    ---

    `route_name="httproute/demo/api/rule/0/match/0"`<br>
    Panels show 503 spikes on an opaque string. Status of the underlying CR is invisible.

-   :material-check-circle-outline: __After `gatewayapiprocessor`__

    ---

    `k8s.httproute.name="api"`, `k8s.httproute.namespace="demo"`<br>
    `k8s.httproute.accepted=true`, `k8s.httproute.resolved_refs=false`<br>
    The misconfig *is* the attribute.

</div>

## What ships in this repo

- A **Go processor** (`gatewayapiprocessor/`) at `component.StabilityLevelDevelopment`.
- Three parser plug-ins: **Envoy-family**, **Linkerd**, **passthrough**.
- An **OCB builder config** (`builder-config.yaml`) that bakes `gatewayapiprocessor` into a custom `otelcol-gatewayapi` image.
- A **hero demo** under `deploy/` with one live `make break` action and a same-slide `make fix` reversion.
- **Grafana dashboards** and a **Dynatrace DQL notebook** with matching queries — the same query runs against both backends.
- This **MkDocs Material** site.

## Quick links

- [Install via OCB](getting-started/installation.md)
- [Full configuration reference](configuration/index.md)
- [Attribute reference](attribute-reference/index.md)
- [Architecture overview](how-it-works/index.md)
- [Troubleshooting](troubleshooting/index.md)
- [Roadmap](roadmap/index.md)

!!! info "Demo context"
    This processor is a demo artifact for *The Legend of Config: Breath of the Cluster* at ObsSummit North America 2026 ([ISI-661](https://paperclip.isitobservable.com/ISI/issues/ISI-661)). The attribute schema and config shape are tracked in the [processor spec](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec) and are the contract surface — downstream consumers should treat them as stable across patch releases.
