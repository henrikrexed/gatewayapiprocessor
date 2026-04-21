# Changelog

All notable changes to `gatewayapiprocessor` are tracked here. This file follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

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
  workflow matrix (`.github/workflows/ci.yml`), release workflow
  (`.github/workflows/release.yml`), and `Dockerfile` `GO_VERSION` arg all pin
  `1.25`. `.golangci.yml` `run.go` bumped to match.
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
