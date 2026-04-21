package gatewayapiprocessor

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"go.opentelemetry.io/collector/component"
)

// scopeName is the OTel instrumentation scope shared by all self-telemetry
// emitted by this processor. The Collector's `service.telemetry.metrics.level`
// stanza decides whether these instruments are real or no-op implementations —
// we never branch on a homegrown flag.
const scopeName = "github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor"

// selfAttrKey marks any telemetry this processor itself emits so that if the
// Collector pipeline fans its own output back into a gatewayapi-enriched
// pipeline, we can short-circuit instead of re-enriching our own data.
// processor-spec §3.3 "no self-enrichment loop".
const selfAttrKey = "gatewayapiprocessor.self"

// Enrichment outcome label values — kept as constants to guarantee a closed
// set (cardinality guard: labels are finite and controlled by this file).
const (
	outcomeStamped        = "stamped"
	outcomeDropped        = "dropped"
	outcomeAmbiguousOwner = "ambiguous_owner"
	outcomeResolved       = "resolved"
	outcomeAmbiguous      = "ambiguous"
	outcomeUnresolved     = "unresolved"
)

const (
	signalMetricsStr = "metrics"
	signalTracesStr  = "traces"
	signalLogsStr    = "logs"
)

// telemetryBuilder owns the processor's self-telemetry instruments and tracer.
// Built once per processor factory call from TelemetrySettings, so a single
// instance is shared across traces/logs/metrics signals.
type telemetryBuilder struct {
	logger *zap.Logger
	tracer trace.Tracer

	routesIndexed              metric.Int64UpDownCounter
	enrichmentsTotal           metric.Int64Counter
	informerEventsTotal        metric.Int64Counter
	enrichmentDuration         metric.Float64Histogram
	backendRefFallbackTotal    metric.Int64Counter
	statusConditionsStampedTot metric.Int64Counter
}

// newTelemetryBuilder wires all instruments off the given TelemetrySettings.
// Passing a nil TelemetrySettings or nil providers is safe — callers (tests,
// standalone use) get a no-op builder whose increments compile away.
func newTelemetryBuilder(ts component.TelemetrySettings, logger *zap.Logger) (*telemetryBuilder, error) {
	mp := ts.MeterProvider
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	tp := ts.TracerProvider
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	meter := mp.Meter(scopeName)
	tb := &telemetryBuilder{
		logger: logger,
		tracer: tp.Tracer(scopeName),
	}

	var err error
	if tb.routesIndexed, err = meter.Int64UpDownCounter(
		"gatewayapiprocessor_routes_indexed",
		metric.WithDescription("Number of routes currently held in the in-memory index, split by gateway_class and route_kind."),
		metric.WithUnit("{route}"),
	); err != nil {
		return nil, fmt.Errorf("routes_indexed: %w", err)
	}
	if tb.enrichmentsTotal, err = meter.Int64Counter(
		"gatewayapiprocessor_enrichments_total",
		metric.WithDescription("Number of enrichment attempts on incoming telemetry, labelled by signal and outcome."),
		metric.WithUnit("{enrichment}"),
	); err != nil {
		return nil, fmt.Errorf("enrichments_total: %w", err)
	}
	if tb.informerEventsTotal, err = meter.Int64Counter(
		"gatewayapiprocessor_informer_events_total",
		metric.WithDescription("Number of Kubernetes informer events processed, labelled by resource and event."),
		metric.WithUnit("{event}"),
	); err != nil {
		return nil, fmt.Errorf("informer_events_total: %w", err)
	}
	if tb.enrichmentDuration, err = meter.Float64Histogram(
		"gatewayapiprocessor_enrichment_duration",
		metric.WithDescription("End-to-end latency of a single ConsumeX enrichment batch."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("enrichment_duration: %w", err)
	}
	if tb.backendRefFallbackTotal, err = meter.Int64Counter(
		"gatewayapiprocessor_backend_ref_fallback_total",
		metric.WithDescription("Outcomes of the server.address → HTTPRoute backendRef fallback path."),
		metric.WithUnit("{lookup}"),
	); err != nil {
		return nil, fmt.Errorf("backend_ref_fallback_total: %w", err)
	}
	if tb.statusConditionsStampedTot, err = meter.Int64Counter(
		"gatewayapiprocessor_status_conditions_stamped_total",
		metric.WithDescription("Number of records whose route status conditions were stamped. Gated by emit_status_conditions=true."),
		metric.WithUnit("{record}"),
	); err != nil {
		return nil, fmt.Errorf("status_conditions_stamped_total: %w", err)
	}

	return tb, nil
}

// recordEnrichment adds 1 to the enrichments_total counter.
func (t *telemetryBuilder) recordEnrichment(ctx context.Context, signal, outcome string) {
	t.enrichmentsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("signal", signal),
		attribute.String("outcome", outcome),
	))
}

// recordEnrichmentDuration adds a single observation to the duration histogram.
func (t *telemetryBuilder) recordEnrichmentDuration(ctx context.Context, signal string, seconds float64) {
	t.enrichmentDuration.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("signal", signal),
	))
}

// recordBackendRefFallback adds 1 to the fallback outcome counter.
func (t *telemetryBuilder) recordBackendRefFallback(ctx context.Context, outcome string) {
	t.backendRefFallbackTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome),
	))
}

// recordStatusCondStamped bumps the status-conditions counter — caller must
// check cfg.EmitStatusConds before invoking so the counter is not stamped on
// records whose conditions were not actually emitted.
func (t *telemetryBuilder) recordStatusCondStamped(ctx context.Context) {
	t.statusConditionsStampedTot.Add(ctx, 1)
}

// recordInformerEvent bumps informer_events_total with the (resource, event)
// tuple. resource ∈ HTTPRoute|GRPCRoute|Gateway|GatewayClass; event ∈
// add|update|delete|sync.
func (t *telemetryBuilder) recordInformerEvent(ctx context.Context, resource, event string) {
	t.informerEventsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("resource", resource),
		attribute.String("event", event),
	))
}

// recordRoutesIndexedDelta bumps routes_indexed by delta for the given
// (gateway_class, route_kind) tuple. Use +1 on upsert of a new key, -1 on
// delete. Intentionally does NOT include route UID or name — see
// processor-spec §1.4 cardinality guard.
func (t *telemetryBuilder) recordRoutesIndexedDelta(ctx context.Context, gatewayClass, routeKind string, delta int64) {
	if gatewayClass == "" {
		gatewayClass = "unknown"
	}
	t.routesIndexed.Add(ctx, delta, metric.WithAttributes(
		attribute.String("gateway_class", gatewayClass),
		attribute.String("route_kind", routeKind),
	))
}

// startEnrichBatchSpan opens the per-batch internal span. Callers must End()
// the returned span, typically via defer. Returns a nil-safe no-op span when
// the tracer provider is NoopTracerProvider.
func (t *telemetryBuilder) startEnrichBatchSpan(ctx context.Context, signal string, items int) (context.Context, trace.Span) {
	ctx, span := t.tracer.Start(ctx, "gatewayapiprocessor.EnrichBatch",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("signal", signal),
			attribute.Int("items", items),
			attribute.Bool(selfAttrKey, true),
		),
	)
	return ctx, span
}
