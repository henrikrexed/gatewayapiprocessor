# Dynatrace ingest token spec — clusterapi-isi-01

> **Status:** Manual issuance required — see _Open handoffs_ in
> `deploy/k8s/dynatrace/README.md`. This file documents the *shape* of
> the token; it does not contain a token.

## Required scopes (least privilege)

The OTLP ingest path needs exactly these three scopes — nothing more:

| Scope | Purpose |
|---|---|
| `openTelemetryTrace.ingest` | Accept OTLP/HTTP traces at `/api/v2/otlp/v1/traces` |
| `metrics.ingest` | Accept OTLP/HTTP metrics at `/api/v2/otlp/v1/metrics` |
| `logs.ingest` | Accept OTLP/HTTP logs at `/api/v2/otlp/v1/logs` |

**Reject** any token that also carries:

- `apiTokens.read` / `apiTokens.write` — token-management belongs on a
  separate admin token, not on the ingest path.
- `entities.read` — read scopes don't help ingest, and a leak of an
  ingest token shouldn't expose tenant inventory.
- `settings.write` — settings mutation belongs to the apply pipeline,
  not the workload.
- `events.ingest` (broad) — narrow to the three above.

If you need to revoke after a leak, delete *just* this token — none of
the demo cluster's exporters depend on it.

## Naming + labelling convention

- Personal access token name: `otlp-ingest-clusterapi-isi-01`
- Description: `OTLP ingest from clusterapi-isi-01 gateway collector — ISI-755`
- Tag: `cluster=clusterapi-isi-01,managed-by=isi-observable,scope=ingest`
- Expiration: **180 days** (matches our token rotation cadence; do not
  set "no expiration").

## Issuance — option A: DT UI (one-time, by ProxOps)

1. DT UI → **Access tokens** (under "Manage" → "Access tokens").
2. **Generate new token** → name + description per above.
3. Select scopes: `openTelemetryTrace.ingest`, `metrics.ingest`,
   `logs.ingest`. Nothing else.
4. Set expiration: 180 days.
5. Copy the value — DT only shows it once. Never paste into a chat,
   PR description, or commit.
6. Apply to the cluster as a Secret using the runbook in
   `deploy/k8s/dynatrace/README.md` § _Secret materialization_.

## Issuance — option B: Account Management API (CI / automation)

For automated rotation, use the Account Management API
(`POST /env/v1/environments/{envId}/tokens`). This is the path a
sealed-secret-rotation CronJob would take — out of scope for ISI-755,
filed as a follow-up under the rotation epic.

## Endpoint URL

The exporter uses:

```
${DT_TENANT_URL}/api/v2/otlp/v1/{traces,metrics,logs}
```

- For the dev tenant referenced in the obs-annex:
  `https://<env-id>.live.dynatrace.com/api/v2/otlp`
- For SaaS Live: `https://<env-id>.live.dynatrace.com/api/v2/otlp`
- For Managed: `https://<cluster>/e/<env-id>/api/v2/otlp`

The collector exporter config in `deploy/40-collector/collector.yaml`
uses `${env:DT_TENANT_URL}` so the same image works across
environments — only the Secret value changes per cluster.
