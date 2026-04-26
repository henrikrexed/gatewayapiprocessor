# Dynatrace integration — clusterapi-isi-01

Bundle that lands the Dynatrace side of the new ClusterAPI cluster:
ingest token spec, K8s Secret, and the gateway-collector exporter +
tail-sampling fragments.

Spec: [ISI-755](https://github.com/henrikrexed/gatewayapiprocessor/issues/755).
Decision context: ISI-749 §6.3 / `obs-annex` §D — separate ingest token
per cluster, reusing the existing Dynatrace tenant.

## Scope (narrowed by board on 2026-04-26)

§6.3 originally called for a per-cluster boundary (Management Zone in
classic terminology). On the Grail-only target tenant
(`https://<env-id>.live.dynatrace.com`) that would have decomposed
into a per-cluster Bucket + Filter Segment + IAM policy + OpenPipeline
route. Board feedback on this issue was unambiguous: **"we don't need
dedicated segments or storage buckets in dynatrace"**. Telemetry from
this cluster lands in the default `logs` / `spans` / `metrics` buckets
and is filtered at query time on the `k8s.cluster.name` resource
attribute.

What the bundle still ships:

- The OTLP **ingest token** runbook (`dt-resources/api-token-spec.md`).
- The K8s **Secret** convention so the gateway collector picks up
  endpoint + token from a single source per cluster.
- The collector-side exporter + tail-sampling fragments under
  `deploy/k8s/collector/gateway/`.
- The agent-side `resource/cluster-id` stamp (so query-time filtering
  on `k8s.cluster.name` actually works in the default buckets).

What the bundle no longer ships (removed in this PR):

- `dt-resources/bucket-logs-clusterapi-isi-01.yaml`
- `dt-resources/filter-segment-clusterapi-isi-01.yaml`
- `dt-resources/iam-policy-clusterapi-isi-01.yaml`
- `dt-resources/openpipeline-route-clusterapi-isi-01.yaml`

## File layout

```
deploy/k8s/dynatrace/
├── 00-namespace.yaml                              gateway-collector ns
├── dynakube.yaml                                  DynaKube CR — scoped OneAgent injection
├── secret-dt-otlp-ingest.example.yaml             Secret shape (no real values)
├── sealedsecret-dt-otlp-ingest.template.yaml      SealedSecret template
├── README.md                                      this file
└── dt-resources/
    └── api-token-spec.md                          token scopes + issuance runbook

deploy/k8s/collector/gateway/
├── exporter-dynatrace.snippet.yaml                gateway-tier OTLP exporter
├── tail-sampling.snippet.yaml                     first-pass sampling policy
└── resource-cluster-id.snippet.yaml               cluster-id stamp (agent-tier)
```

The `*.snippet.yaml` files in `collector/gateway/` are fragments,
consumed by the full agent + gateway `OpenTelemetryCollector` CRs in
[ISI-754](https://github.com/henrikrexed/gatewayapiprocessor/issues/754) /
PR #27 (now merged into this branch).

## Apply order (one-time bring-up)

```bash
# 1. Issue the ingest token (manual — see dt-resources/api-token-spec.md).
#    Copy the token value into a shell var; do NOT echo it into a file
#    that git tracks.

# 2. K8s side (requires kubectl context = clusterapi-isi-01).
kubectl apply -f deploy/k8s/dynatrace/00-namespace.yaml

#    Materialize the Secret. Two options:

#    Option A — sealed-secret (preferred when sealed-secrets controller
#    is installed on the cluster):
#      see sealedsecret-dt-otlp-ingest.template.yaml for the full
#      kubeseal workflow. Commits a *sealed* file to git.

#    Option B — direct kubectl create (no controller; one-time, by
#    a human operator only — bash leading-space trick keeps the token
#    out of shell history):
#       export DT_TENANT_URL='https://<env-id>.live.dynatrace.com'
#       read -rs DT_TOKEN   # paste token, press enter — does not echo
#      kubectl -n gateway-collector create secret generic dt-otlp-ingest \
#        --from-literal=endpoint="$DT_TENANT_URL" \
#        --from-literal=api-token="$DT_TOKEN"
#      unset DT_TOKEN DT_TENANT_URL

# 3. Apply the agent + gateway collectors (ISI-754, already on this
#    branch under deploy/k8s/collector/).
```

## Validation — first trace round-trip

After the gateway collector lands and the Secret is in place, emit a
synthetic trace from inside the cluster and confirm it arrives:

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
- Tenant side: spans should appear in the default `spans` bucket;
  filter on `k8s.cluster.name` to attribute to this cluster.

## DynaKube CR — scoped OneAgent injection

The Dynatrace operator on this cluster is driven by a single
`DynaKube` CR (`dynakube.yaml`). Two design choices matter for
ISI-680:

1. **OneAgent uses `applicationMonitoring` with a `namespaceSelector`
   gated on the label `oneagent=true`** — _opt-in_, not opt-out.
   Demo workload namespaces (`otel-demo`, `gateway-collector`,
   `otel-system`, `obi-system`) intentionally stay unlabeled, so
   they do **not** receive code-module injection.
2. **The OTel SDK apps in `otel-demo` already emit OTLP** through
   `otelcol-agent → otelcol-gateway → Dynatrace`. Injecting OneAgent
   on top would double-instrument every span and metric — exactly the
   regression flagged on ISI-680 board feedback 2026-04-26 15:44Z.

If a future namespace _does_ want OneAgent code-module coverage in
addition to OTLP (e.g. legacy app without SDK instrumentation), the
operator command is:

```bash
kubectl label namespace <ns> oneagent=true
# Existing pods do NOT auto-rotate — the webhook only injects on
# create. Either delete the pods or roll the workload:
kubectl -n <ns> rollout restart deploy/<name>
```

To clear a previously injected pod after switching from
`cloudNativeFullStack` (or after dropping the label), the same
rollout-restart pattern applies — the `dynakube.dynatrace.com/injected`
annotation persists on the pod metadata until the pod is replaced.

ActiveGate is enabled with the `kubernetes-monitoring`,
`metrics-ingest`, `dynatrace-api`, and `routing` capabilities so K8s
API monitoring + smartscape work without OneAgent on every node.

## Open handoffs (blockers — who needs to do what)

| Step | Required scope | Owner |
|---|---|---|
| Issue ingest token | DT token-management permission | ProxOps (one-time, via UI). **Now done — token validated 2026-04-25, see ISI-755 thread.** |
| Create K8s namespace + Secret | kubectl admin on clusterapi-isi-01 | ProxOps |
| Apply agent + gateway collectors | kubectl admin on clusterapi-isi-01 | ProxOps |
| Synthetic trace round-trip validation | me (Observability Agent) | gated on the above |

Heartbeat dtctl token does not carry token-issuance scope — this is the
correct posture; the heartbeat agent should not be able to provision
tenant-level permissions.

## Threat model — one paragraph

The ingest token is the single sensitive secret in this bundle. Its
scope is restricted to ingest paths, so a leak from the gateway
collector compromises ingest-spam capacity for this cluster only — it
cannot read tenant data, list other tokens, or modify settings (except
where the dev token's wider scope deviates from spec — flagged on
ISI-755 thread for a follow-up tightening cycle). The token never
enters the agent namespace; agents reach Dynatrace exclusively via the
gateway service. The Secret is in a dedicated `gateway-collector`
namespace with `istio.io/dataplane-mode: none` so the credential plane
is not multiplexed onto the workload's waypoint mTLS path.
