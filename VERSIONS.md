# VERSIONS.md

Pinned manifest for the `gatewayapiprocessor` demo stack.

**Date:** 2026-04-21
**Freeze status:** `demo-locked` after rehearsal (see [ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)).
**Update discipline:** version bumps require a PR against this file. Any bump triggers re-record of the pre-recorded fallback clip.

| Component                     | Pinned version | Why pinned                                                          |
| ----------------------------- | -------------- | ------------------------------------------------------------------- |
| Kubernetes (kind node image)  | v1.32.0        | Stable channel on demo day                                          |
| Gateway API CRDs              | v1.3.0         | Standard channel; matches Phase-1 research baseline (ISI-665)       |
| Istio (ambient profile)       | 1.26.0         | Ambient GA; waypoint `Telemetry` API stable                         |
| Kgateway                      | v2.1.0         | CNCF-donated release; `HttpListenerPolicy` available                |
| OTel Operator                 | v0.124.0       | Matches collector version pin                                       |
| OTel Collector (OCB build)    | v0.124.0       | Baseline for contrib processors this quarter                        |
| OBI (opentelemetry-ebpf-...)  | v0.8.0         | Current release; k8s metadata enrichment available                  |
| OTel Demo                     | v2.2.0         | Carries HTTPRoute-ready manifests                                   |
| OpenTelemetry semconv         | 1.40.0         | Baseline for `http.*`, `k8s.*`; no upstream `k8s.gateway.*` yet     |
| Grafana                       | 11.x           | Compose file pin                                                    |
| Tempo                         | 2.6            | Compose file pin                                                    |
| Loki                          | 3.2            | Compose file pin                                                    |
| Prometheus                    | 3.1            | Compose file pin                                                    |
| Dynatrace OTel endpoint       | SaaS           | DT tenant env + token in Makefile from env vars                     |
| Go                            | 1.23           | Matches otel-contrib baseline                                       |

## Image tags

- Custom collector: `ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21`
  - Also tagged `:latest` until next version bump.
  - Multi-arch: `linux/amd64`, `linux/arm64`.

## Update procedure

1. Open a PR against `VERSIONS.md` only (single file per bump).
2. Update matching kustomize/helm values in `deploy/`.
3. CI revalidation workflow runs `make demo` against kind on the bumped versions.
4. On green: merge, re-record fallback clip ([ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)), bump tag in `VERSIONS.md` date line.
5. On red for >72h: re-pin trigger — open an issue, revert VERSIONS.md changes, unblock talk path.
