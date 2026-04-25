# Split-topology OTel Collector — clusterapi-isi-01

This bundle deploys the agent (DaemonSet) + gateway (Deployment, 2 replicas)
collector topology required by [ISI-754](https://github.com/henrikrexed/gatewayapiprocessor/issues/754)
on the `observable-gateapiprocess` ClusterAPI cluster (observability identity:
`clusterapi-isi-01`).

The kind cluster (`deploy/40-collector/`) keeps its **gateway-only** mode for
the demo flow — that is intentional and is **not** what this bundle replaces.

## Why split (not gateway-only)

Per `obs-annex` §C / ISI-749 §6.2:

| Concern | Why it forces the split |
|---|---|
| `gatewayapiprocessor` informer cache | Cluster-scoped LIST/WATCH on Gateway/HTTPRoute. Per-node ⇒ N× kube-apiserver load. Lives at the gateway tier only. |
| `k8sattributesprocessor` enrichment | Cheapest at the node — local kubelet metadata, no cross-node API calls. Lives at the agent tier only. |
| Tail sampling | Needs full-trace assembly. Per-node sees one span at a time. Gateway with trace-ID affinity (loadbalancing exporter at agent) makes the decision window correct. |

## File layout

```
deploy/k8s/collector/
├── README.md                                this file
├── 00-namespaces.yaml                       otel-system (gateway-collector ns lives in deploy/k8s/dynatrace/00-namespace.yaml)
├── agent/
│   ├── rbac.yaml                            SA + ClusterRole (k8sattributes) + Role (gateway endpoints)
│   └── otelcol-agent.yaml                   OpenTelemetryCollector mode=daemonset
├── gateway/
│   ├── rbac.yaml                            SA + ClusterRole (Gateway API LIST/WATCH)
│   ├── otelcol-gateway.yaml                 OpenTelemetryCollector mode=deployment, replicas=2
│   ├── exporter-dynatrace.snippet.yaml      docs-only; logic inlined in otelcol-gateway.yaml
│   ├── tail-sampling.snippet.yaml           docs-only; logic inlined in otelcol-gateway.yaml
│   └── resource-cluster-id.snippet.yaml     docs-only; inlined in otelcol-agent.yaml (cluster-id stamped at agent)
```

## Pipeline invariants (do not move)

- `memory_limiter` is the **first** processor on every pipeline (both tiers).
- `batch` is the **last** processor on every pipeline (both tiers).
- `gatewayapiprocessor` runs **only** at the gateway. Never at agents.
- `k8sattributesprocessor` runs **only** at the agent. Never at the gateway.
- `resourcedetectionprocessor` is **not** used. It targets VM/bare-metal — this
  cluster is pure K8s.
- Agent → gateway transport uses **two `loadbalancing` exporter instances**, both
  resolving the same gateway Service (`otelcol-gateway-collector.gateway-collector`)
  via the `k8s` resolver, but **keyed differently per signal**:
  - `loadbalancing` (`routing_key: traceID`) — used by the **traces** and
    **metrics** pipelines. Trace-ID affinity is required so each trace's spans
    converge on a single gateway replica (precondition for gateway-side tail
    sampling).
  - `loadbalancing/logs` (`routing_key: service`) — used by the **logs** pipeline
    only. filelog records have no trace context; reusing the traceID-keyed
    exporter would hash every record to the empty key and converge all log
    volume onto a single gateway replica. Service-keyed routing spreads logs by
    `service.name` while keeping the same Service / RBAC. Revisit this if any
    future gateway processor needs full per-service log assembly.

## Image / version pin

Both tiers run `ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21`. This is
the OCB build pinned in `gatewayapiprocessor/VERSIONS.md` (Collector v0.150.0,
Operator v0.150.0). Bumping the image requires a PR against `VERSIONS.md` and a
re-record of the demo fallback clip.

A Kyverno or Gatekeeper policy enforcing the image pin is a follow-up — see the
ISI-754 PR comment for the open-question on which admission stack lands first.

## Prerequisites

These MUST be satisfied before applying. The bundle no-ops or crash-loops if
they are not.

| Prereq | How to verify | Owner |
|---|---|---|
| OTel Operator v0.150.0 installed (CRDs `OpenTelemetryCollector`, `Instrumentation`) | `kubectl get crd opentelemetrycollectors.opentelemetry.io -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].name}'` returns `v1beta1` and the operator pod runs v0.150.0 | ProxOps (workload cluster) |
| cert-manager (operator dependency) | `kubectl get deploy -n cert-manager cert-manager` is Available | ProxOps |
| Gateway API CRDs v1.3.0 (`gateways`, `httproutes`, `gatewayclasses`) | `kubectl get crd gateways.gateway.networking.k8s.io` exists and matches the v1.3.0 schema | ProxOps |
| Namespace `gateway-collector` and `Secret/dt-otlp-ingest` (with keys `endpoint`, `api-token`) | `kubectl -n gateway-collector get secret dt-otlp-ingest -o jsonpath='{.data.endpoint}' \| base64 -d` matches the DT tenant URL | ISI-755 / ProxOps |
| Workload cluster nodes Ready, kernel ≥ 6.x | `kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.kernelVersion}'` all start with `6.` | ProxOps (ISI-753 / ISI-738) |
| Custom collector image pullable from harbor mirror or upstream ghcr.io | `crane manifest ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21` resolves | Build pipeline |

## Apply order

```bash
# 0. Re-confirm prerequisites listed above. Do NOT proceed past any unmet row.

# 1. Namespaces.
kubectl apply -f deploy/k8s/dynatrace/00-namespace.yaml   # gateway-collector
kubectl apply -f deploy/k8s/collector/00-namespaces.yaml  # otel-system

# 2. Dynatrace ingest Secret (one-time; see deploy/k8s/dynatrace/README.md
#    for sealed-secret vs. direct-create paths). MUST exist before the
#    gateway pods start, otherwise the Deployment crash-loops on missing env.

# 3. RBAC.
kubectl apply -f deploy/k8s/collector/agent/rbac.yaml
kubectl apply -f deploy/k8s/collector/gateway/rbac.yaml

# 4. Gateway tier first — agents need its service to exist before they
#    start trying to load-balance traceID.
kubectl apply -f deploy/k8s/collector/gateway/otelcol-gateway.yaml
kubectl -n gateway-collector rollout status deploy/otelcol-gateway-collector --timeout=180s

# 5. Agent tier.
kubectl apply -f deploy/k8s/collector/agent/otelcol-agent.yaml
kubectl -n otel-system rollout status ds/otelcol-agent-collector --timeout=180s
```

## Verify

```bash
# Two CRs, one each tier, gateway shows replicas=2.
kubectl get otelcol -A
# NAMESPACE           NAME              MODE        VERSION   READY   AGE   IMAGE
# gateway-collector   otelcol-gateway   deployment  0.150.0   2/2     1m    ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21
# otel-system         otelcol-agent     daemonset   0.150.0   N/N     1m    ghcr.io/isi-observable/otelcol-gatewayapi:2026-04-21

# Synthetic span from inside the cluster.
kubectl run -n default --rm -it --restart=Never tg --image=ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.150.0 -- \
  traces --otlp-insecure \
  --otlp-endpoint=otelcol-gateway-collector.gateway-collector.svc.cluster.local:4317 \
  --traces 1 --service smoke-test \
  --otlp-attributes 'k8s.cluster.name="clusterapi-isi-01"'

# In Dynatrace (~60s after emission):
#   fetch spans
#   | filter k8s.cluster.name == "clusterapi-isi-01"
#   | filter service.name == "smoke-test"
#   | sort timestamp desc
#   | limit 5
```

The "Done when" criteria from ISI-754:

- [ ] `kubectl get otelcol -A` shows two CRs (one daemonset, one deployment with 2 replicas).
- [ ] Agent → gateway OTLP traffic visible in Dynatrace within 60s of synthetic emit.
- [ ] A synthetic span emitted at the gateway carries enriched `k8s.gateway.*`
      and `k8s.httproute.*` attributes (requires at least one Gateway/HTTPRoute
      in the cluster — exercised by the demo workload).

## Open follow-ups

- **Image-pin enforcement**: pick Kyverno or Gatekeeper, ship a `ClusterPolicy`
  that rejects any `OpenTelemetryCollector` whose `spec.image` does not match
  the pinned tag. Tracking on a follow-up issue once admission stack is
  decided.
- **Tempo exporter**: env-driven (`TEMPO_ENDPOINT`) but currently unset, so the
  exporter is wired in config but not in any pipeline. When ISI lands a Tempo
  target, add `otlphttp/tempo` to the `traces` pipeline and document the
  endpoint in this README.
- **`emit_status_conditions` consumer**: `gatewayapiprocessor` writes back to
  Gateway/HTTPRoute `.status` — confirm with the platform team that this is
  acceptable on a shared cluster, otherwise toggle off.
