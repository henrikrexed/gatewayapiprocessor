# Changelog

All notable changes to `gatewayapiprocessor` are tracked here. This file follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2026-10-21] — dependency release (ISI-6164 / ISI-6177)

First image cut since `2026-04-21`:
`ghcr.io/henrikrexed/otelcol-gatewayapi:2026-10-21` (OCB dist `0.163.0-isi.1`,
multi-arch `linux/amd64,linux/arm64`, published by `publish-ocb-image.yml` on the
`ocb-2026-10-21` tag). Rolls up the accumulated dependency train (OTel Collector
v0.150 → v0.154 → v0.163, k8s v0.37.1, gateway-api v1.6.3; workstreams A.1–A.3)
plus the GAMMA mesh-enrichment and OBI network-flow feature work that landed on
`main` since the last cut.

### Added

- **GAMMA mesh enrichments D1–D6** (ISI-6178). Mesh-mode (`parentRef.kind=Service`)
  span enrichments for the GAMMA topology — see the ISI-6178 PR (#93) and
  `docs/configuration.md`.
- **nil-Kind parent-Service inference (GAMMA D4)** (ISI-6188, #97). When a
  `parentRef` omits `kind`, the processor infers a parent `Service` (GAMMA
  mesh-mode default) instead of dropping the enrichment.
- **parentRef-aware disambiguation when a Service is shared by mesh and ingress
  routes** (ISI-805). `routeIndex.LookupByBackendServiceWithParents` exposes the
  full owner set when the single-candidate `backendIndex` would otherwise drop
  an entry as ambiguous. `applyBackendRefFallback` runs a binary disambiguator
  on the candidate set: if exactly one mesh-mode (`parentRef.kind=Service`) and
  one ingress-mode (`parentRef.kind=Gateway`) route share the backend, the
  span's `k8s.namespace.name` resource attribute decides which to stamp —
  matching the mesh route's parent Service namespace stamps mesh, otherwise
  ingress. Same-mode pairs (mesh+mesh, ingress+ingress) and >2 candidates
  preserve the existing no-stamp safety contract
  (`TestBackendRefFallback_AmbiguousOwner_NoStamp`). Recovers the otel-demo
  `frontend-proxy` topology where one HTTPRoute is mesh and another is ingress
  for the same backend Service. Fully test-covered by the 6-case matrix in
  `processor_disambiguation_test.go`. See
  [docs/configuration.md#mesh--ingress-disambiguation-v03-isi-805](docs/configuration.md).

### Changed

- **OTel Collector — v0.154.0 → v0.163.0** (ISI-6386, #99; the authoritative
  step of the ISI-6164 dependency sweep). Stable modules (`component`,
  `consumer`, `pdata`, `processor`, `featuregate`, `pipeline`) `v1.60.0` →
  `v1.69.0`; v0-line modules `v0.154.0` → `v0.163.0`. `builder-config.yaml`
  `gomod` lines, `dist.version` (`0.163.0-isi.1`), `Makefile OCB_VERSION`, and
  the `VERSIONS.md` OTel Collector row rolled in lockstep — a mixed train breaks
  the OCB build (ambiguous `collector/internal/statusutil` import, which split
  into its own module at v0.163).
- **k8s.io/\* down-pins dropped; k8s v0.37.1 + gateway-api v1.6.3** (ISI-6386,
  #99, supersedes dependabot #89). Contrib's `k8sattributesprocessor` moved to
  stable component versioning (`v1.2.0`) and adopted `client-go v0.37.1` on the
  v0.163 train (verified on the Go module proxy 2026-10-21), so the
  `k8s.io/* => v0.35.4` `replaces` block in `builder-config.yaml` (ISI-844) is
  removed. The processor module now tracks `k8s.io/client-go v0.37.1` /
  `k8s.io/apimachinery v0.37.1` / `sigs.k8s.io/gateway-api v1.6.3`.
- **OTel Collector — v0.150.0 → v0.154.0** (ISI-6241, #92). Stable modules
  `v1.56.0` → `v1.60.0`; v0-line modules `v0.150.0` → `v0.154.0`. The
  intermediate train, kept with the k8s down-pins before ISI-6386 cleared them.
- **Go toolchain — 1.25 → 1.26**, `go.mod` `go 1.26.0` (ISI-844), with the
  pinned toolchain raised to **go1.26.9** to clear govulncheck (ISI-6281, #95).
  CI, release, and `publish-ocb-image` workflows pin Go `1.26` via
  `actions/setup-go`, and `go.mod` carries `toolchain go1.26.9`. (The
  `Dockerfile` is a distroless `COPY`-only runtime image with no Go build stage,
  so it has no toolchain pin of its own.)
- **golang.org/x/net — → v0.60.0** (ISI-6281, #95) to clear the govulncheck
  advisory; `go mod tidy` strips the toolchain directive, so it is re-pinned
  explicitly.
- **stretchr/testify — v1.11.1 → v1.12.1** (ISI-6241, #92).
- **go.opentelemetry.io/otel SDK — bumped** (dependabot #91).
- **OBI deploy — enriched network-flow metrics + service graph enabled**
  (ISI-1014, #78). Deploy-manifest change under `deploy/` — not a processor-code
  change — but ships in the same demo-stack cut.
- **OTel Collector upgrade — v0.124.0 → v0.150.0** (ISI-687, parent ISI-679).
  - Stable modules (`component`, `consumer`, `pdata`, `processor`, `featuregate`,
    `pipeline`): `v1.30.0` → `v1.56.0`.
  - v0-line modules (`consumertest`, `processortest`, `componenttest`,
    `componentstatus`, `pdata/pprofile`, `pdata/testdata`, `xconsumer`,
    `xprocessor`, `internal/componentalias`): `v0.124.0` → `v0.150.0`.
  - OTel Go SDK (`otel`, `otel/metric`, `otel/trace`, `otel/sdk`,
    `otel/sdk/metric`): `v1.35.0` → `v1.43.0`.
  - OTel auto-instrumentation SDK: `v1.1.0` → `v1.2.1`.
  - `stretchr/testify`: `v1.10.0` → `v1.11.1` (pulled in by processor test deps).
  - `go.uber.org/zap`: `v1.27.0` → `v1.27.1`.
- **Go toolchain — 1.24 → 1.25.** Required by collector v0.150 modules (minimum
  Go bumped in v0.146.0 upstream). `go.mod` `go` directive set to `1.25.0`; CI
  workflow matrix (`.github/workflows/ci.yml`) and release workflow
  (`.github/workflows/release.yml`) pin `1.25` via `actions/setup-go`.
  `.golangci.yml` `run.go` bumped to match.
- **OCB manifest — `builder-config.yaml`.** All receiver / processor / exporter /
  extension `gomod` lines bumped to `v0.150.0`. `dist.version` rolled to
  `0.150.0-isi.1`. OCB binary installed by CI derives from this file, so release
  workflow automatically pulls `go.opentelemetry.io/collector/cmd/builder@v0.150.0`.
- **golangci-lint action version.** Pinned to `v1.64.8` (last v1-line release) to
  preserve the current `.golangci.yml` schema. Migration to golangci-lint v2.x
  is tracked as a separate follow-up to keep this PR scoped to the OTEL bump.
- **VERSIONS.md.** OTel Operator, OTel Collector (OCB build), and Go rows
  updated to reflect new pins; date header notes the ISI-687 rebase.

### Validation

- `go test ./... -race -covermode=atomic -coverprofile=coverage.out -timeout=5m`
  green against the ISI-684 expanded matrix (65 unit tests + 6 benches).
- Coverage: `gatewayapiprocessor` **81.7%** of statements,
  `gatewayapiprocessor/parser` **97.0%** — above the 80% warn / 70% fail gate
  configured in `.github/workflows/ci.yml`.
- No processor API surface changes required: `processor.NewFactory`,
  `processor.WithTraces/Logs/Metrics`, `processor.Settings`, and the
  `consumer.Traces/Logs/Metrics` contracts are unchanged between v0.124 and
  v0.150 for our call sites.

### Notes

- Upstream tagged `v0.150.0` as the latest 0.150-line release at time of bump;
  `v0.150.1` does not exist on the opentelemetry-collector / -contrib module
  registries. Tracking `v0.150.0` satisfies the ISI-687 scope requirement to
  target the 0.150 release train.
- Downstream coordination:
  [DevOps Engineer](https://paperclip.isitobservable.com/ISI/agents/devops-engineer)
  owns the CI Go toolchain rollout; [Testing Architect](https://paperclip.isitobservable.com/ISI/agents/testing-architect)
  will rerun the expanded matrix on the upgraded deps before ISI-684 merges.
