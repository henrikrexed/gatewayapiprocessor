# Dynatrace integration — clusterapi-isi-01

Bundle that lands the Dynatrace side of the new ClusterAPI cluster:
ingest token, per-cluster bucket, filter segment, IAM policy, K8s
Secret, and the gateway-collector exporter + tail-sampling fragments.

Spec: [ISI-755](https://github.com/henrikrexed/gatewayapiprocessor/issues/755).
Decision context: ISI-749 §6.3 / `obs-annex` §D — separate ingest token
+ per-cluster scope, reusing the existing Dynatrace tenant.

## Why "Management Zone" is split into three resources here

ISI-749 §6.3 calls for a **Management Zone** scoped to
`k8s.cluster.name=clusterapi-isi-01`. On a Grail-only Dynatrace tenant
(which this is — confirmed against
`https://oat05854.dev.apps.dynatracelabs.com`), the classic Management
Zone schema is not available. Its boundary semantics decompose into
three Grail-native resources:

| Boundary | Grail resource | File |
|---|---|---|
| Storage | Bucket `logs_clusterapi_isi_01` | `dt-resources/bucket-logs-clusterapi-isi-01.yaml` |
| UI / query view | Filter Segment `clusterapi-isi-01` | `dt-resources/filter-segment-clusterapi-isi-01.yaml` |
| Access (RBAC) | IAM policy `clusterapi-isi-01-readonly` | `dt-resources/iam-policy-clusterapi-isi-01.yaml` |
| Routing (logs) | OpenPipeline rule | `dt-resources/openpipeline-route-clusterapi-isi-01.yaml` |

All four together satisfy the §6.3 boundary. Applying only the bucket
gives you storage scope but everyone can still read it; applying only
the filter segment gives you UI scope but logs still land in
`default_logs`. **Apply all four or none.**

## File layout

```
deploy/k8s/dynatrace/
├── 00-namespace.yaml                              gateway-collector ns
├── secret-dt-otlp-ingest.example.yaml             Secret shape (no real values)
├── sealedsecret-dt-otlp-ingest.template.yaml      SealedSecret template
├── README.md                                      this file
└── dt-resources/
    ├── api-token-spec.md                          token scopes + issuance runbook
    ├── bucket-logs-clusterapi-isi-01.yaml         per-cluster log bucket
    ├── filter-segment-clusterapi-isi-01.yaml      UI/query scope
    ├── iam-policy-clusterapi-isi-01.yaml          access scope (RBAC)
    └── openpipeline-route-clusterapi-isi-01.yaml  logs → cluster bucket

deploy/k8s/collector/gateway/
├── exporter-dynatrace.snippet.yaml                gateway-tier OTLP exporter
├── tail-sampling.snippet.yaml                     first-pass sampling policy
└── resource-cluster-id.snippet.yaml               cluster-id stamp (agent-tier)
```

The `*.snippet.yaml` files in `collector/gateway/` are fragments. They
are **not** standalone deployments — ISI-754 wires them into the full
agent + gateway OpenTelemetryCollector CRs.

## Apply order (one-time bring-up)

```bash
# 0. Pre-flight — confirm dtctl context is the right tenant.
dtctl ctx                                 # expect dynatrace-dev / clusterapi-isi-01 tenant

# 1. Dynatrace side (requires a token with bucket-write,
#    settings:objects:write, and IAM admin scope — NOT the ingest token).
dtctl apply -f deploy/k8s/dynatrace/dt-resources/bucket-logs-clusterapi-isi-01.yaml
dtctl apply -f deploy/k8s/dynatrace/dt-resources/openpipeline-route-clusterapi-isi-01.yaml
dtctl apply -f deploy/k8s/dynatrace/dt-resources/filter-segment-clusterapi-isi-01.yaml
# IAM policy via DT UI or Account Management API (not yet a dtctl
# first-class verb in v0.23) — see iam-policy-clusterapi-isi-01.yaml.

# 2. Issue the ingest token (manual — see dt-resources/api-token-spec.md).
#    Copy the token value into a shell var; do NOT echo it into a file
#    that git tracks.

# 3. K8s side (requires kubectl context = clusterapi-isi-01).
kubectl apply -f deploy/k8s/dynatrace/00-namespace.yaml

#    Materialize the Secret. Two options:

#    Option A — sealed-secret (preferred when sealed-secrets controller
#    is installed on the cluster):
#      see sealedsecret-dt-otlp-ingest.template.yaml for the full
#      kubeseal workflow. Commits a *sealed* file to git.

#    Option B — direct kubectl create (no controller; one-time, by
#    a human operator only):
#      kubectl -n gateway-collector create secret generic dt-otlp-ingest \
#        --from-literal=endpoint="$DT_TENANT_URL" \
#        --from-literal=api-token="$DT_TOKEN"

# 4. Collector side — wire ISI-754's full agent + gateway CRs to use
#    the snippets in deploy/k8s/collector/gateway/. Verify in DT UI
#    that the per-cluster bucket starts receiving logs and the filter
#    segment dashboards populate.
```

## Validation — first trace round-trip

After ISI-754's gateway collector lands and the Secret is in place,
emit a synthetic trace from inside the cluster and confirm it arrives:

```bash
# Inside the cluster (any pod with telemetrygen):
telemetrygen traces --otlp-insecure \
  --otlp-endpoint=otelcol-gateway.gateway-collector.svc.cluster.local:4317 \
  --traces 1 --service smoke-test \
  --otlp-attributes 'k8s.cluster.name="clusterapi-isi-01"'

# In DT (within ~60s of emission):
dtctl query 'fetch spans, scanLimitGBytes:1
             | filter k8s.cluster.name == "clusterapi-isi-01"
             | filter service.name == "smoke-test"
             | sort timestamp desc
             | limit 5'
```

If the query returns 0 rows after 2 minutes:

- Agent side: check `kubectl logs -n otel-system ds/otelcol-agent` for
  `loadbalancing` exporter errors.
- Gateway side: check `kubectl logs -n gateway-collector deploy/otelcol-gateway`
  for `otlphttp/dynatrace` 401 (token bad scope) or 429 (ingest cap).
- Tenant side: `dtctl get bucket logs_clusterapi_isi_01` should show
  `records > 0` after the first batch flush.

## Open handoffs (blockers — who needs to do what)

This bundle is committable as IaC, but several apply steps require
permissions the heartbeat-context dtctl token (`dynatrace-dev`) does
not carry:

| Step | Required scope | Owner |
|---|---|---|
| Apply bucket | `storage:bucket-definitions:write` | ProxOps or Architect (Winston) — admin token |
| Apply OpenPipeline route | `settings:objects:write` on `builtin:openpipeline.logs.pipelines` | ProxOps |
| Apply filter segment | `settings:objects:write` on `builtin:dt.segmentation.segments` | ProxOps |
| Apply IAM policy | Account Management API admin | ProxOps |
| Issue ingest token | DT token-management permission | ProxOps (one-time, via UI) |
| Create K8s namespace + Secret | kubectl admin on clusterapi-isi-01 | ProxOps |

Heartbeat dtctl token observed `access denied to create bucket` — this
is the correct posture; the heartbeat agent should not be able to
provision tenant-level RBAC or storage. Escalation routes through
[ISI-755](https://github.com/henrikrexed/gatewayapiprocessor/issues/755)
to ProxOps for execution, with Architect (Winston) as approver.

## Threat model — one paragraph

The ingest token is the single sensitive secret in this bundle. Its
scope is restricted to ingest only (no read, no admin), so a leak from
the gateway collector compromises ingest-spam capacity for this cluster
only — it cannot read tenant data, list other tokens, or modify
settings. The token never enters the agent namespace; agents reach
Dynatrace exclusively via the gateway service. The Secret is in a
dedicated `gateway-collector` namespace with `istio.io/dataplane-mode:
none` so the credential plane is not multiplexed onto the workload's
waypoint mTLS path. Rotation cadence: 180 days, tracked separately
from this bring-up issue.
