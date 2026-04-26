# gatewayapiprocessor — Test Plan

Canonical matrix the processor is validated against before every merge.
Mirrors processor-spec §1–§5 (see `ISI-670#document-processor-spec`) and the
scope expansion called out in `ISI-684`.

## Run it

```bash
cd gatewayapiprocessor
go test ./... -race -cover -count=1
go test ./... -run=^$ -bench=. -benchmem           # benchmarks
```

CI gate: both packages MUST pass with `-race` and coverage ≥ 80%.
Current baseline (as of the ISI-684 NFR-1 gate expansion):

| Package                                                                            | Coverage | Tests |
| ---------------------------------------------------------------------------------- | -------- | ----- |
| `github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor`                   | 82.0%    | 56    |
| `github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor/parser`            | 97.0%    | 12    |

## Matrix overview

Tests are grouped by file. Each file targets a single axis of the contract
so a regression surfaces in a specific file rather than across the suite.

### `parser/parser_test.go` (5 — §2.5 core)

1. `TestEnvoyParser_HappyPath` — canonical `httproute/<ns>/<name>/rule/<i>/match/<j>`.
2. `TestEnvoyParser_NoRuleNoMatch` — partial string still yields `(ns,name)`.
3. `TestEnvoyParser_UnknownFormat` — garbage strings, empty source = no match.
4. `TestLinkerdParser_HappyPath` — split labels → route identity.
5. `TestPassthroughParser_RawAttr` — raw stamping + empty-source guard.

### `parser/parser_extra_test.go` (7 — hardening)

- `TestEnvoyParser_AccessorsAndControllers` — controller regex list round-trips.
- `TestLinkerdParser_AccessorsAndControllers` — Linkerd controller wiring.
- `TestPassthroughParser_Accessors` — passthrough matches any controller.
- `TestNewEnvoyParser_InvalidRegex` — surfaces bad regex at build time.
- `TestNewLinkerdParser_DefaultsLabels` — blank keys default to Linkerd's shipped labels.
- `TestLinkerdParser_GRPCRouteKindNormalised` — `grpcroute` (lower) → `GRPCRoute`.
- `TestNewPassthroughParser_DefaultAttribute` — empty key defaults to `k8s.gatewayapi.raw_route_name`.
- `TestCapturedInt_BadInputs` — non-numeric / OOR / negative → -1.
- `TestMapAttrs_GetMissing` — absent key returns `(_, false)`.

### `processor_test.go` (7 — §2.5 enrichment)

Unchanged from the v0.1 PR — canonical status/metric-filter/fallback path.

- `TestStatusConditions_Accepted` — `k8s.httproute.accepted=true` stamped.
- `TestStatusConditions_Rejected` — `k8s.httproute.accepted=false` stamped.
- `TestMetricAttributeFilter` — `k8s.httproute.uid` stripped on metrics, kept on traces.
- `TestBackendRefFallback` — `server.address` → HTTPRoute lookup.
- `TestInformerSyncTimeout` — Start() fails fast on missing RBAC.
- `TestEnrichment_PassthroughFallback` — unparseable route_name still stamps raw.
- `TestEnrichment_LogsPath` — logs enrich end-to-end (rule/match/uid retained).

### `processor_matrix_test.go` (19 — ISI-684 scope expansion + ISI-785)

Mixed-parser chain and per-signal matrix.

- `TestEnrichment_MixedParsers_EnvoyWinsWhenRouteNameMatches`
- `TestEnrichment_MixedParsers_LinkerdFallsThroughWhenEnvoyMisses`
- `TestEnrichment_MixedParsers_PassthroughLastResort`
- `TestEnrichment_GRPCRoute_StampsGRPCKeysOnly` — gRPC path: only `k8s.grpcroute.*` keys.
- `TestEnrichment_GRPCRoute_StampsStatusConditions` — ISI-785: gRPC path stamps `k8s.grpcroute.accepted` / `k8s.grpcroute.resolved_refs` (and never the httproute siblings).
- `TestEmitStatusConditions_Off_DoesNotStamp` — flag off suppresses `accepted`/`resolved_refs`.
- `TestEmitStatusConditions_Off_GRPCRoute_DoesNotStamp` — ISI-785: flag off suppresses gRPC status keys.
- `TestMetricAttributeFilter_GatewayUID_AndRawRouteName_Stripped` — full `exclude_from_metric_attributes` list.
- `TestBackendRefFallback_Disabled_NoStamp`
- `TestBackendRefFallback_UnknownAddress_NoStamp`
- `TestBackendRefFallback_AmbiguousOwner_NoStamp` — end-to-end ambiguity drop.
- `TestEnrichment_ResourceAttributeFallback` — resource-level `route_name` drives enrichment.
- `TestEnrichMetric_AllMetricTypes` — gauge / sum / histogram / exp-histogram / summary.
- `TestCapabilities_MutatesData` — reports in-place mutation.
- `TestShutdown_InvokesStopFn` — Shutdown drains the informer stop func.
- `TestStart_PropagatesStartHookError` — Start surfaces hook errors.
- `TestFactory_CreatesAllThreeProcessors` — factory smoke for traces/logs/metrics.

### `index_test.go` (8 — routeIndex invariants)

- `TestRouteIndex_UpsertAndLookupHTTPRoute`
- `TestRouteIndex_UpsertAndLookupGRPCRoute` — GRPCRoute key does NOT collide with HTTPRoute.
- `TestRouteIndex_DeleteHTTPRoute_ClearsBackendIndex` — deletion ordering re-enables re-claim.
- `TestRouteIndex_DeleteGRPCRoute`
- `TestRouteIndex_BackendConflict_DropsAttribution` — two owners → no match (never mis-attribute).
- `TestRouteIndex_SameOwnerReupsert_KeepsAttribution` — resync idempotency.
- `TestBackendRefsFromHTTPRoute_FiltersNonServiceAndDefaultsNamespace`
- `TestRouteIndex_ConcurrentAccess_NoRaces` — 16 goroutines × 500 iters under `-race`.

### `informer_helpers_test.go` (16 — CR projection unit tests)

- `TestHTTPRouteToAttrs_WithGatewayAndClass` — full (gateway, gatewayclass, controller) chain.
- `TestHTTPRouteToAttrs_UnknownGatewayParent_StillStampsRouteIdentity` — partial-observation safety.
- `TestHTTPRouteToAttrs_EmitStatusConditions_Stamps`
- `TestHTTPRouteToAttrs_EmitStatusConditions_Off_NoStatus`
- `TestGRPCRouteToAttrs_BasicAndWithGateway`
- `TestGRPCRouteToAttrs_EmitStatusConditions_Stamps` — ISI-785: GRPCRoute status → Accepted/ResolvedRefs.
- `TestGRPCRouteToAttrs_EmitStatusConditions_Off_NoStatus` — ISI-785: flag off leaves them nil.
- `TestStatusFlags_BothConditions` / `TestStatusFlags_NoParents`
- `TestFormatParentRef_DefaultsWhenGroupKindUnset`
- `TestFormatParentRef_ExplicitGroupKindAndNamespace`
- `TestDefaultSyncTimeout` — 0 / negative → 30s default; positive passthrough.
- `TestGatewayStore_UpsertGetDelete` / `TestGatewayClassStore_UpsertGetDelete`
- `TestSplitAddress_Matrix` — DNS / short / empty / trailing / leading / double-dot.
- `TestRouteKey_DifferentiatesKinds`

### `informer_integration_test.go` (2 — fake-clientset integration)

Drives real `SharedIndexInformer`s with the Gateway API fake clientset — no
kind cluster required, still exercises `register*Handlers` event dispatch,
cache sync, update + delete propagation, and the tombstone decode branch.

- `TestInformerIntegration_AddUpdateDeleteHTTPRoute` — seeds Gateway +
  GatewayClass, Creates/Updates/Deletes an HTTPRoute, asserts index,
  gatewayStore, and gatewayClassStore state after each transition.
- `TestInformerHandler_HTTPRouteTombstone` — `cache.DeletedFinalStateUnknown`
  path for relist-recovery deletes.

### `config_validate_test.go` (12 — §2.2 validation)

- `TestConfig_Validate_Default` — shipped defaults validate clean.
- `TestConfig_Validate_Matrix` — 9 error branches (bad auth_type; kubeConfig
  missing path; no parsers; passthrough not last; bad controller regex; envoy
  regex missing named groups; envoy regex invalid syntax; negative
  resync_period; negative informer_sync_timeout).
- `TestBuildParserChain_UnknownParser`
- `TestBuildParserChain_EnvoyRegexError`
- `TestNewProcessor_NilConfig`

### `processor_bench_test.go` (6 — throughput + memory ceiling)

All run under `go test -bench=. -benchmem -run=^$`.

| Benchmark                             | What it measures                                        |
| ------------------------------------- | ------------------------------------------------------- |
| `BenchmarkEnrichTraces_1kRoutes`      | Per-batch enrichment cost for 1000 spans                |
| `BenchmarkEnrichTraces_10kRoutes`     | Linear scaling probe (10× payload)                      |
| `BenchmarkEnrichMetrics_1kRoutes`     | Metrics path cost (strip + dp walk)                     |
| `BenchmarkEnrichMetrics_10kRoutes`    | Metrics linear scaling                                  |
| `BenchmarkRouteIndex_Upsert_10k`      | Informer memory ceiling — bytes/ops per 10k upserts     |
| `BenchmarkEnrichmentHotPath_SingleSpan` | Steady-state hot-path latency per record              |

Reference baseline (non-race, go1.23.6, QEMU linux/amd64) from the initial
ISI-684 run:

```
BenchmarkEnrichTraces_1kRoutes-6        1   2.54ms/op    1.77MB/op    20791 allocs/op
BenchmarkEnrichTraces_10kRoutes-6       1   21.6ms/op    16.5MB/op   209802 allocs/op
BenchmarkEnrichMetrics_1kRoutes-6       1   2.85ms/op    1.71MB/op    24779 allocs/op
BenchmarkEnrichMetrics_10kRoutes-6      1   21.0ms/op    16.6MB/op   249797 allocs/op
BenchmarkRouteIndex_Upsert_10k-6        1   9.40ms/op    7.77MB/op    99496 allocs/op
BenchmarkEnrichmentHotPath_SingleSpan-6 1   17.1µs/op    39952B/op    34 allocs/op
```

Single-span hot path sits at ~17µs — plenty of headroom under the collector's
default per-batch budget. Linear scaling holds from 1k→10k, so the informer
memory ceiling is bounded by the upsert benchmark: ~7.7MB per 10k routes.

### `processor_nfr_test.go` (3 — PRD NFR-1 hard gate)

These are regular Go tests (not benchmarks) that measure enrichment cost via
`time.Since` and FAIL the build if the processor violates the PRD
[ISI-690](/ISI/issues/ISI-690) §6 NFR-1 budget. Benchmarks above are
reporting-only probes; this file is the assertion gate CI runs.

| Test                                                               | Budget                     |
| ------------------------------------------------------------------ | -------------------------- |
| `TestNFR1_EnrichmentLatency_p95_10kRoutes_LE_100us`                | traces p95 ≤ 100µs/record  |
| `TestNFR1_MetricsEnrichmentLatency_p95_10kRoutes_LE_100us`         | metrics p95 ≤ 100µs/record |
| `TestNFR1_Throughput_10kRoutesCache_GE_9500rps`                    | ≥ 9500 records/sec sustained |

Throughput budget derivation: the PRD allows ≤ 5% pipeline-throughput
regression at 10k spans/sec when the processor is enabled. A 5% budget on
10_000 rps is 500 rps, so the processor must sustain at least 9500 rps on its
own before it becomes the pipeline bottleneck.

Reference headroom from the ISI-684 NFR-1 run (go1.25, linux/amd64):

```
NFR-1 enrichment latency @ 10000-route cache: p50=1.2µs p95=4.0µs p99=10.9µs (budget p95 ≤ 100µs, 5000 samples)
NFR-1 metrics enrichment latency @ 10000-route cache: p50=1.1µs p95=3.4µs p99=5.1µs (budget p95 ≤ 100µs, 5000 samples)
NFR-1 throughput @ 10000-route cache: ~498000 rps (budget ≥ 9500 rps, 1s window)
```

The gate self-skips under `-short` and when `NFR1_SKIP=1` so fast dev loops
stay green. CI runs the full suite without `-short`, so PR builds always
enforce the budget.

## Coverage targets

| Package                                  | Target | Actual |
| ---------------------------------------- | ------ | ------ |
| `gatewayapiprocessor` (main)             | ≥ 80%  | 82.0%  |
| `gatewayapiprocessor/parser`             | ≥ 80%  | 97.0%  |

Remaining uncovered lines live in `informer.go` (`newInformers`,
`buildRESTConfig`) — these require a real `rest.Config` and are validated at
deploy time in the collector builder image, not in unit tests.

## CI expectations

- `go test ./... -race -cover` — 0 failures, coverage ≥ 80% on both packages.
- NFR-1 threshold-assertion gate (`TestNFR1_*` in `processor_nfr_test.go`)
  runs as part of the default suite; no `-short`, no `NFR1_SKIP`.
- Benchmarks not gated on CI time; run on demand for reporting.
- No `kind` / no cluster dependency: every test uses a static lookup or the
  Gateway API fake clientset. CI image only needs the Go toolchain.
