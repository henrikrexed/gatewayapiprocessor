---
title: Getting started
---

# Getting started

This section walks you through the 3-minute path from a fresh cluster to a collector that stamps normalized Gateway API attributes on every signal.

| Step                                                     | What you do                                                                      |
| -------------------------------------------------------- | -------------------------------------------------------------------------------- |
| [Installation](installation.md)                          | Build (or pull) the custom collector image; apply RBAC.                          |
| [Minimum viable pipeline](minimum-viable-pipeline.md)    | Drop `gatewayapi` into your collector config — defaults cover most clusters.     |
| [Verification](verification.md)                          | Apply a demo `HTTPRoute`, watch the processor log informer sync, inspect output. |

Prerequisites:

- A Kubernetes cluster with the **Gateway API v1 CRDs** installed (`kubectl api-resources | grep gateway.networking.k8s.io`).
- At least one `GatewayClass` installed by Envoy Gateway, Kgateway, Istio, or Linkerd.
- The ability to run `helm`, `kubectl`, and (for the full hero demo) `kind` and `docker`.
- Go 1.23 and the OTel Collector Builder (`ocb`) if you want to build the image yourself.
