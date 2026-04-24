# Requirements

## OpenTelemetry Collector

| Component                 | Minimum / tested version                                              |
| ------------------------- | --------------------------------------------------------------------- |
| Collector API             | `go.opentelemetry.io/collector/component` v1.56.0                     |
| Collector runtime         | v0.150.0 (OCB dist `otelcol-gatewayapi` in this repo)                 |
| Go toolchain              | Go 1.25 (required by OTel Collector v0.150 modules)                   |

The processor is shipped as a Go module and is intended to be linked into a
custom collector via the OpenTelemetry Collector Builder (OCB). See
[Getting started](getting-started.md) for the full OCB manifest used by this
repo.

## Kubernetes and Gateway API

| Component                         | Minimum / tested version                      |
| --------------------------------- | --------------------------------------------- |
| Kubernetes                        | v1.29+ (tested on v1.32.0 via kind)           |
| Gateway API CRDs                  | v1.3.0 (Standard channel)                     |
| `sigs.k8s.io/gateway-api` module  | v1.5.1                                        |
| `k8s.io/client-go`                | v0.35.x                                       |

The processor uses shared informers against the standard Gateway API CRDs. The
CRDs must be installed in the target cluster.

## Supported Gateway API data planes

Parsers are configurable. The defaults cover:

- **Envoy family**: Envoy Gateway, Kgateway, Istio (ambient and sidecar).
  Matched on `GatewayClass.spec.controllerName` regex, then parsed from an
  opaque `route_name` attribute.
- **Linkerd**: matched on `linkerd.io/gateway-controller`, then parsed from
  discrete label attributes (`route_name`, `route_kind`, `route_namespace`,
  `parent_name`).
- **Passthrough**: catch-all, writes the raw string to
  `k8s.gatewayapi.raw_route_name`.

Any controller whose `GatewayClass.spec.controllerName` matches a parser's
`controllers` regex list will be handled by that parser; all three defaults
may run in the same cluster at once.

## RBAC

The processor authenticates to the Kubernetes API using one of three modes set
via `auth_type`:

- `serviceAccount` (default) &mdash; in-cluster service account token.
- `kubeConfig` &mdash; local kubeconfig (dev only). Requires `kube_config_path`.
- `none` &mdash; no Kubernetes client; parser chain runs in string-only mode.

When running in-cluster (`auth_type: serviceAccount`), the processor's service
account needs **read** access to Gateway API resources and GatewayClasses. A
minimal `ClusterRole` looks like:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gatewayapiprocessor-reader
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources:
      - gateways
      - httproutes
      - grpcroutes
      - gatewayclasses
    verbs: ["get", "list", "watch"]
```

Bind this role to the service account mounted in the collector Pod.

## Pipeline placement

Required order in every pipeline (traces, logs, metrics):

```
... memory_limiter -> k8sattributes -> gatewayapiprocessor -> ... -> batch
```

- `memory_limiter` MUST be first in the processor chain.
- `k8sattributes` must run **before** `gatewayapiprocessor` so the Gateway and
  HTTPRoute lookups can key off Kubernetes metadata already stamped on the
  record.
- `batch` MUST be last.

The metrics pipeline gets the same ordering. UID-like attributes are stripped
from metrics by default (see `enrich.exclude_from_metric_attributes`) to keep
cardinality bounded.

## Network access

The processor only talks to the Kubernetes API server via the informer-backed
clients. No outbound network access is required.
