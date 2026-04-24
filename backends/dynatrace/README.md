# Dynatrace backend — SECONDARY

Runs alongside Grafana/Tempo to make the backend-portability beat credible
(kills Challenger W1: "this only works with one vendor").

## Token scopes (required)

The Dynatrace API token needs these three scopes:

- `openTelemetryTrace.ingest`
- `metrics.ingest`
- `logs.ingest`

Generate at: `https://<tenant>.live.dynatrace.com/#settings/integration/apitokensettings`

## Setup

The collector deployment (`deploy/40-collector/collector.yaml`) reads the
tenant URL and API token from a Kubernetes Secret named `dynatrace-otlp`
in the `otel-system` namespace.

```bash
kubectl create namespace otel-system --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic dynatrace-otlp \
  --namespace otel-system \
  --from-literal=endpoint="https://<tenant>.live.dynatrace.com/api/v2/otlp" \
  --from-literal=api-token="dt0c01.XXXX.YYYY"
```

Alternatively, the Makefile target `make dynatrace-secret` reads
`DT_TENANT_URL` and `DT_API_TOKEN` from the host env and creates the
Secret in-cluster.

## Notebook

A pre-populated Dynatrace notebook (`notebook.json`) contains the same
before/after queries as the Grafana dashboard. Import it via:

```
Settings → Notebooks → Import
```

## Blocker

Tenant + token are a **BigBoss / @henrik** task — tagged on the parent
issue [ISI-680](https://paperclip.isitobservable.com/ISI/issues/ISI-680).
Until delivered, the Dynatrace exporter is configured but inert (empty
endpoint → collector logs a warning, traces still flow to Grafana).
