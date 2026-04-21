---
title: Installation
---

# Installation

`gatewayapiprocessor` is not yet part of `opentelemetry-collector-contrib`, so you need a **custom collector build** that includes it. Two paths: pull the pre-built image from GHCR, or build your own via OCB.

## Option A — pull the pre-built image

The project CI publishes multi-arch images to GHCR on every `VERSIONS.md` bump:

```bash
docker pull ghcr.io/henrikrexed/otelcol-gatewayapi:2026-04-21
# also tagged :latest until the next bump
```

Platforms: `linux/amd64`, `linux/arm64`.

Image tag matches the `VERSIONS.md` date so you can audit exactly which pinned manifest the binary was compiled against. See the repo's `.github/workflows/build.yaml` for the publish workflow.

## Option B — build from source (OCB)

OCB (`ocb` or `builder`, depending on your install) reads [`builder-config.yaml`](https://github.com/henrikrexed/gatewayapiprocessor/blob/main/builder-config.yaml) and emits a fully linked collector binary.

```bash
git clone https://github.com/henrikrexed/gatewayapiprocessor
cd gatewayapiprocessor

make ocb-install              # installs go.opentelemetry.io/collector/cmd/builder@v0.124.0
make build-collector          # produces ./_build/otelcol-gatewayapi
```

To build a multi-arch image locally, use the `push-collector` target (requires `docker buildx` and QEMU):

```bash
IMAGE_REGISTRY=ghcr.io/YOUR_USER \
IMAGE_NAME=otelcol-gatewayapi \
COLLECTOR_TAG=$(date +%Y-%m-%d) \
make push-collector
```

## Deploying on Kubernetes

### 1. Apply the ClusterRole and RoleBinding

The processor needs read access to `gateways`, `httproutes`, `grpcroutes`, and `gatewayclasses` (cluster-scoped when you watch all namespaces). The `backendref_fallback` also needs `services` read.

```yaml title="deploy/40-collector/rbac.yaml"
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gatewayapiprocessor
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways", "httproutes", "grpcroutes", "gatewayclasses"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["services"]   # required when backendref_fallback.enabled=true
    verbs: ["get", "list", "watch"]
```

Bind this role to the ServiceAccount your collector pod runs as. The `make demo` target ships a reference `ClusterRoleBinding` in the same file.

### 2. Deploy the custom collector

If you use the OpenTelemetry Operator, point `OpenTelemetryCollector.spec.image` at your built image:

```yaml
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: otelcol-gatewayapi
  namespace: otel-system
spec:
  image: ghcr.io/henrikrexed/otelcol-gatewayapi:2026-04-21
  serviceAccount: otelcol-gatewayapi
  mode: deployment
  config: |
    # see Minimum viable pipeline
```

If you deploy by hand, any standard `Deployment` spec with that image + ServiceAccount works — the processor only needs a kubeconfig it can authenticate with.

## Next

- [Minimum viable pipeline](minimum-viable-pipeline.md) — drop the processor into your collector config.
- [Verification](verification.md) — confirm the informers are synced and the attributes are stamped.
