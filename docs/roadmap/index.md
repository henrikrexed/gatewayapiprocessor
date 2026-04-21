---
title: Roadmap
---

# Roadmap

`gatewayapiprocessor` is a **demo artifact** built for *The Legend of Config: Breath of the Cluster* at ObsSummit NA 2026. Post-talk, the question of what lands in mainline is governed by the processor spec and the talk's upstream contribution posture ([processor-spec §6](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec)).

Phases below are **provisional**. Nothing on this page is a commitment; they're the shape of follow-up work we've identified.

## Phase 3.0 — v0 (shipping for the talk)

Scope: the current repo.

- [x] Envoy-family, Linkerd, and passthrough parsers.
- [x] Informer-driven `k8s.gateway.*`, `k8s.httproute.*`, `k8s.gatewayclass.*` enrichment.
- [x] HTTPRoute status conditions (`Accepted`, `ResolvedRefs`) on spans/logs/metrics.
- [x] Cardinality guard for metrics (`exclude_from_metric_attributes`).
- [x] BackendRef fallback (probabilistic, opt-out).
- [x] OCB builder + multi-arch GHCR image (`ghcr.io/henrikrexed/otelcol-gatewayapi`).
- [x] This documentation site.

## Phase 3.5 — scope expansion (post-talk)

Directions we expect to take if the community asks for them:

- **Kgateway-specific CRDs.** `HttpListenerPolicy`, `RoutePolicy` — read their bindings and stamp a `kgateway.policy.name` / `kgateway.policy.kind` attribute set so policy misconfigurations become observable.
- **Istio-specific CRDs.** `Telemetry` (spec in the namespace vs. applied to the workload), `WorkloadEntry`, `DestinationRule` — surface the effective policy that shaped a given span.
- **NetworkPolicy correlation.** Stamp a boolean `k8s.networkpolicy.allowed` by joining span source pod + destination service against the cluster's `NetworkPolicy` set. This is a common troubleshoot path and the CR data is already in the cluster.
- **ReferenceGrant awareness.** Stamp `k8s.httproute.parent_ref` with a flag when `ReferenceGrant` is what made the cross-namespace route legal. (Spec §1.5 listed this as out of scope for v0 for security-review reasons — we'd want a formal review before turning it on.)

## Phase 4 — upstream

Two paths:

1. **`opentelemetry-collector-contrib` PR.** The attribute namespace (`k8s.gateway.*`) and the processor's config shape were picked to minimize friction with a contrib PR. Blockers: component review backlog and a stability bump from `Development` to `Beta`.
2. **Semconv PR.** None of these attributes are in published OpenTelemetry semconv yet. A standalone proposal for `k8s.gateway.*` / `k8s.httproute.*` would benefit the whole ecosystem, not just this processor. Stretch goal — see [processor-spec §6.2](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec).

## Things we've considered and *aren't* doing

- **Inference Extension attributes** (`inference.pool`, `inference.objective`, `target_model_name`). Metrics-only; span semconv not stable yet. Out of scope for this processor.
- **NetObserv flow enrichment** — that's a display-layer concern.
- **Rewriting `transformprocessor`.** If you only need flat string manipulation, `transform` + OTTL is still the right tool. This processor earns its keep because it reads CR status, which OTTL cannot.

## How to propose new scope

1. Open a GitHub issue: https://github.com/henrikrexed/gatewayapiprocessor/issues
2. If you have a real-world attribute gap, link a sample input record and the attribute set you'd like to see.
3. The project's cadence is tied to the demo calendar — expect slower review cycles during ObsSummit prep weeks.

## See also

- [Processor spec (ISI-670)](https://paperclip.isitobservable.com/ISI/issues/ISI-670#document-processor-spec)
- [How it works](../how-it-works/index.md)
