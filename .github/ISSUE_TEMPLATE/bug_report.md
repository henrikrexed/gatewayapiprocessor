---
name: Bug report
about: Report a defect in gatewayapiprocessor, the custom collector, or the demo
title: "bug: "
labels: ["bug"]
assignees: []
---

## Summary

<!-- One sentence: what broke? -->

## Environment

- Processor version / commit:
- Collector image tag (`ghcr.io/henrikrexed/gatewayapiprocessor-collector:<tag>`):
- Data plane: <!-- Envoy Gateway / Kgateway / Istio ambient / Linkerd / other -->
- Kubernetes version:
- Gateway API CRDs version:
- Signal type: <!-- traces / logs / metrics -->

## Reproduction

<!-- Minimal steps. Include an OTLP payload (scrubbed) or a kubectl-apply-able manifest if possible. -->

1.
2.
3.

## Expected

## Actual

## Logs / exported attributes

<details>
<summary>Collector logs</summary>

```
```
</details>

<details>
<summary>Emitted attributes on a sample span/log/metric</summary>

```
```
</details>
