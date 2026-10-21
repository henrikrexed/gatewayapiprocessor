# VERSIONS.md

Pinned manifest for the `gatewayapiprocessor` demo stack.

**Date:** 2026-10-21
**Freeze status:** `demo-locked` after rehearsal (see [ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)); OTel stack rebased onto v0.163 per the [ISI-6164](https://paperclip.isitobservable.com/ISI/issues/ISI-6164) dependency release ([ISI-6386](https://paperclip.isitobservable.com/ISI/issues/ISI-6386) — k8s v0.37.1 + gateway-api v1.6.3, down-pins dropped; prior v0.154 train [ISI-6241](https://paperclip.isitobservable.com/ISI/issues/ISI-6241), v0.150 rebase [ISI-687](https://paperclip.isitobservable.com/ISI/issues/ISI-687)). Image tag `:2026-10-21` published on the `ocb-2026-10-21` tag push ([ISI-6177](https://paperclip.isitobservable.com/ISI/issues/ISI-6177)).
**Update discipline:** version bumps require a PR against this file. Any bump triggers re-record of the pre-recorded fallback clip.

| Component                     | Pinned version | Why pinned                                                          |
| ----------------------------- | -------------- | ------------------------------------------------------------------- |
| Kubernetes (kind node image)  | v1.32.0        | Stable channel on demo day                                          |
| Gateway API CRDs              | v1.3.0         | Standard channel; matches Phase-1 research baseline (ISI-665)       |
| Istio (ambient profile)       | 1.26.0         | Ambient GA; waypoint `Telemetry` API stable                         |
| Kgateway                      | v2.1.0         | CNCF-donated release; `HttpListenerPolicy` available                |
| OTel Operator                 | v0.149.0       | Latest upstream operator release at demo cut; CRD `v1beta1` schema identical to v0.150.0 (no upstream v0.150 operator yet — see ISI-754) |
| OTel Collector (OCB build)    | v0.163.0       | Bumped from v0.154 (ISI-6386); stable modules at v1.69.0. k8sattributesprocessor moved to stable component versioning — pinned at v1.2.0, which adopted client-go v0.37.1 (train v0.163.0, verified on the Go module proxy 2026-10-21); k8s.io/* down-pins dropped, module on k8s v0.37.1 + gateway-api v1.6.3 (supersedes dependabot #89) |
| OBI (opentelemetry-ebpf-...)  | v0.8.0         | Current release; k8s metadata enrichment available                  |
| OTel Demo                     | v2.2.0         | Carries HTTPRoute-ready manifests                                   |
| OpenTelemetry semconv         | 1.40.0         | Baseline for `http.*`, `k8s.*`; no upstream `k8s.gateway.*` yet     |
| Grafana                       | 11.x           | Compose file pin                                                    |
| Tempo                         | 2.6            | Compose file pin                                                    |
| Loki                          | 3.2            | Compose file pin                                                    |
| Prometheus                    | 3.1            | Compose file pin                                                    |
| Dynatrace OTel endpoint       | SaaS           | DT tenant env + token in Makefile from env vars                     |
| Go                            | 1.26           | Forced by `k8s.io/apimachinery@v0.37` + `client-go@v0.37` `go 1.26.0` directive (ISI-844; previously 1.25 / ISI-687) |

## Cluster requirements

The kind demo cluster runs on dev-laptop kernels (6.x). To preserve OBI parser
parity and HTTP/2 visibility on the ProxOps-managed ClusterAPI cluster
([ISI-680](https://paperclip.isitobservable.com/ISI/issues/ISI-680) /
[observable-gateapiprocess](https://github.com/henrikrexed/proxmox-clusters/tree/main/clusters/observable-gateapiprocess)),
the workload-cluster nodes are pinned to **Ubuntu 24.04 LTS (kernel 6.8.x)**.

OBI v0.8.0 (`opentelemetry-ebpf-instrumentation`) floor requirements:

| OBI capability                                    | Min kernel |
| ------------------------------------------------- | ---------- |
| BTF / CO-RE                                       | 5.8        |
| HTTP/2 framing reconstruction (gRPC visibility)   | 5.14       |
| ringbuf event channel                             | 5.8        |
| `BPF_PROG_TYPE_TRACING` / fentry                  | 5.5        |

**Pinned**: Ubuntu 24.04 LTS, kernel 6.8.x — Proxmox VM template
`ubuntu-2404-kube-v1.35.3` (templateID 130 on `homelab3`). The pin is enforced
in `_templates/control-plane.tmpl.yaml` and `_templates/workers.tmpl.yaml` in
the [proxmox-clusters](https://github.com/henrikrexed/proxmox-clusters) repo,
and surfaced as `observable.demo/os-image` / `observable.demo/kernel-floor`
labels on every `KubeadmControlPlane`, `MachineDeployment`, and
`ProxmoxMachineTemplate`.

**Rejected** for this cluster:

- **Ubuntu 22.04 (kernel 5.15)** — works, but degrades HTTP/2 framing
  reconstruction so gRPC-side spans diverge from the kind demo cluster.
- **Talos** — different kernel cadence; OBI compatibility matrix is thinner
  and would couple talk-day visibility to upstream Talos kernel timing.

**Verification** (run after every cluster rebuild on the OBI demo path):

```bash
kubectl --kubeconfig <observable-gateapiprocess.kubeconfig> get nodes \
  -o jsonpath='{.items[*].status.nodeInfo.kernelVersion}'
```

Every value returned MUST start with `6.` (we expect `6.8.x`).

A bump (e.g. 24.04 → 26.04, or kernel 6.8 → 6.x+N) requires a coordinated PR
against this section AND against the proxmox-clusters templates above —
freshly built golden VM template, then re-render of every consumer cluster.
Source: [ISI-753](https://paperclip.isitobservable.com/ISI/issues/ISI-753).

## Image tags

- Custom collector: `ghcr.io/henrikrexed/otelcol-gatewayapi:2026-10-21`
  - Also tagged `:latest` until next version bump (semver path; published by `release.yml`).
  - Date-stamped pins (e.g. `:2026-04-21`) are published by `.github/workflows/publish-ocb-image.yml` on `ocb-YYYY-MM-DD` tag pushes. That workflow runs the same cosign signing + Trivy scan + CycloneDX SBOM steps as the semver `release.yml` path.
  - Multi-arch: `linux/amd64`, `linux/arm64`.

### Published digests

An independently-verifiable pin: the immutable `@sha256:` digest behind each
mutable date tag. Both the canonical `gatewayapiprocessor-collector` image and
the `otelcol-gatewayapi` alias (the name every `deploy/` manifest pins) are
pushed from one manifest, so they share a digest — `cosign verify` must name
whichever repo you actually pull from. `publish-ocb-image.yml` prints the
ready-to-paste line (both names) to its run summary (step "Print published image
refs + record digest"); record it here after each publish.

Both packages are **public** on GHCR, so a consumer can resolve and verify the
digest anonymously (`crane digest ghcr.io/henrikrexed/otelcol-gatewayapi:<tag>`).
Real publish tags carry an ISI suffix (`ocb-YYYY-MM-DD-isiNNN` → tag
`YYYY-MM-DD-isiNNN`); the current demo pin is the bare-date `2026-04-21`.

| Date tag     | Digest (shared by `…/gatewayapiprocessor-collector` and `…/otelcol-gatewayapi`) |
| ------------ | ------------------------------------------------------------------------------- |
| `2026-04-21` | `sha256:c5b0c418e15886b1a17576be0bc6d5ade3f4a900238f952d5fb7039b3ae7dd6a`        |

## Update procedure

1. Open a PR against `VERSIONS.md` only (single file per bump).
2. Update matching kustomize/helm values in `deploy/`.
3. CI revalidation workflow runs `make demo` against kind on the bumped versions.
4. On green: merge, re-record fallback clip ([ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)), bump tag in `VERSIONS.md` date line.
5. On red for >72h: re-pin trigger — open an issue, revert VERSIONS.md changes, unblock talk path.
