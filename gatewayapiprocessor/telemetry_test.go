package gatewayapiprocessor

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"

	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// readScopeMetrics collects and returns the current scope metrics view from
// the given ManualReader. Tests use this to assert specific instruments fired.
func readScopeMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ScopeMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name == scopeName {
			return sm
		}
	}
	return metricdata.ScopeMetrics{}
}

// findMetric finds a metric by name inside a scope's metrics. Returns the
// metric zero value when absent so callers can assert on presence.
func findMetric(sm metricdata.ScopeMetrics, name string) (metricdata.Metrics, bool) {
	for _, m := range sm.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return metricdata.Metrics{}, false
}

// sumInt64 adds every data point of an Int64 Sum-typed metric. We ignore label
// sets because tests only care about "was there activity?" totals.
func sumInt64(m metricdata.Metrics) int64 {
	var total int64
	switch v := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range v.DataPoints {
			total += dp.Value
		}
	}
	return total
}

// sumInt64Filtered sums Int64 Sum data points that match every wanted label
// (attribute key -> expected value). Data points missing a key or carrying a
// different value are skipped.
func sumInt64Filtered(m metricdata.Metrics, want map[string]string) int64 {
	var total int64
	v, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
dpLoop:
	for _, dp := range v.DataPoints {
		for k, expected := range want {
			got, ok := dp.Attributes.Value(attribute.Key(k))
			if !ok || got.AsString() != expected {
				continue dpLoop
			}
		}
		total += dp.Value
	}
	return total
}

// sumHistogramCount returns the total count across all buckets/data points of
// a Float64 Histogram. Used to prove "the histogram observed something".
func sumHistogramCount(m metricdata.Metrics) uint64 {
	var total uint64
	if hist, ok := m.Data.(metricdata.Histogram[float64]); ok {
		for _, dp := range hist.DataPoints {
			total += dp.Count
		}
	}
	return total
}

// instrumentedTestProcessors stands up the processor triple with a real SDK
// meter/tracer wired into the factory so every instrument is observable.
type instrumentedTestProcessors struct {
	*testProcessors
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
	logObs *observer.ObservedLogs
}

func newInstrumentedTestProcessors(t *testing.T, lookup RouteLookup, tweak func(*Config)) *instrumentedTestProcessors {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	core, obs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	cfg := createDefaultConfig().(*Config)
	cfg.AuthType = AuthTypeNone
	if tweak != nil {
		tweak(cfg)
	}
	require.NoError(t, cfg.Validate())

	factory := NewFactory()
	ts := new(consumertest.TracesSink)
	ls := new(consumertest.LogsSink)
	ms := new(consumertest.MetricsSink)

	base := processortest.NewNopSettings(factory.Type())
	base.TelemetrySettings.MeterProvider = mp
	base.TelemetrySettings.TracerProvider = tp
	base.Logger = logger
	base.TelemetrySettings.Logger = logger

	tp1, err := factory.CreateTraces(context.Background(), base, cfg, ts)
	require.NoError(t, err)
	lp1, err := factory.CreateLogs(context.Background(), base, cfg, ls)
	require.NoError(t, err)
	mp1, err := factory.CreateMetrics(context.Background(), base, cfg, ms)
	require.NoError(t, err)

	tps := tp1.(*gatewayAPIProcessor)
	lps := lp1.(*gatewayAPIProcessor)
	mps := mp1.(*gatewayAPIProcessor)
	for _, p := range []*gatewayAPIProcessor{tps, lps, mps} {
		p.lookup = lookup
		p.startHook = nil
	}
	require.NoError(t, tps.Start(context.Background(), componentNopHost{}))
	require.NoError(t, lps.Start(context.Background(), componentNopHost{}))
	require.NoError(t, mps.Start(context.Background(), componentNopHost{}))

	return &instrumentedTestProcessors{
		testProcessors: &testProcessors{traces: tps, logs: lps, metrics: mps, ts: ts, ls: ls, ms: ms},
		reader:         reader,
		spans:          spans,
		logObs:         obs,
	}
}

// componentNopHost is a stub because component.Host needs one method. The
// processor Start doesn't use it in any of our paths.
type componentNopHost struct{}

func (componentNopHost) GetExtensions() map[component.ID]component.Component { return nil }

// ---- tests ----

// newTelemetryBuilder returns a builder whose instruments point at a real SDK
// meter when we hand it one — and no-ops when we don't. This matters for the
// "off by default" cost requirement.
func TestTelemetryBuilder_NoopWhenProvidersAbsent(t *testing.T) {
	tb, err := newTelemetryBuilder(component.TelemetrySettings{
		MeterProvider:  metricnoop.NewMeterProvider(),
		TracerProvider: nil,
	}, zap.NewNop())
	require.NoError(t, err)

	// All instruments must be non-nil and safe to call even with a NoopMeter
	// — otherwise a pipeline with service.telemetry.metrics.level=none would
	// NPE on the first enrichment.
	ctx := context.Background()
	require.NotPanics(t, func() {
		tb.recordEnrichment(ctx, "traces", outcomeStamped)
		tb.recordEnrichmentDuration(ctx, "traces", 0.1)
		tb.recordBackendRefFallback(ctx, outcomeResolved)
		tb.recordStatusCondStamped(ctx)
		tb.recordInformerEvent(ctx, "HTTPRoute", "add")
		tb.recordRoutesIndexedDelta(ctx, "envoygwc", "HTTPRoute", 1)
		_, span := tb.startEnrichBatchSpan(ctx, "traces", 0)
		span.End()
	})
}

// TestTelemetry_EnrichmentsTotal_StampedOnMatch seeds a static lookup with a
// route and pushes a span through ConsumeTraces; the counter must tick once
// with labels (signal=traces, outcome=stamped).
func TestTelemetry_EnrichmentsTotal_StampedOnMatch(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "demo", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
		GatewayClassName: "envoygwc",
	})
	p := newInstrumentedTestProcessors(t, lookup, nil)

	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr("route_name", "httproute/demo/api")
	require.NoError(t, p.traces.ConsumeTraces(context.Background(), td))

	sm := readScopeMetrics(t, p.reader)
	m, ok := findMetric(sm, "gatewayapiprocessor_enrichments_total")
	require.True(t, ok, "enrichments_total not emitted")
	require.Equal(t, int64(1), sumInt64Filtered(m, map[string]string{
		"signal": "traces", "outcome": "stamped",
	}))
}

// When no parser matches and backendref_fallback is disabled, we expect
// outcome=dropped. We must also avoid attributes the passthrough parser would
// accept (its default SourceAttribute is route_name), so the record carries
// only unrelated fields.
func TestTelemetry_EnrichmentsTotal_DroppedWhenNoMatch(t *testing.T) {
	lookup := newStaticLookup()
	p := newInstrumentedTestProcessors(t, lookup, func(c *Config) {
		c.BackendRefFallba.Enabled = false
	})

	ld := plog.NewLogs()
	rec := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	rec.Attributes().PutStr("unrelated.attribute", "no-route-signal")
	require.NoError(t, p.logs.ConsumeLogs(context.Background(), ld))

	sm := readScopeMetrics(t, p.reader)
	m, ok := findMetric(sm, "gatewayapiprocessor_enrichments_total")
	require.True(t, ok)
	assert.Equal(t, int64(1), sumInt64Filtered(m, map[string]string{
		"signal": "logs", "outcome": "dropped",
	}))
}

// enrichment_duration must receive at least one observation per ConsumeX call.
func TestTelemetry_EnrichmentDuration_HistogramFires(t *testing.T) {
	lookup := newStaticLookup()
	p := newInstrumentedTestProcessors(t, lookup, nil)

	md := pmetric.NewMetrics()
	dp := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	dp.Attributes().PutStr("foo", "bar")
	require.NoError(t, p.metrics.ConsumeMetrics(context.Background(), md))

	sm := readScopeMetrics(t, p.reader)
	m, ok := findMetric(sm, "gatewayapiprocessor_enrichment_duration")
	require.True(t, ok)
	assert.Greater(t, sumHistogramCount(m), uint64(0), "histogram must record ≥1 observation")
}

// backend_ref_fallback_total must tick with outcome=resolved when the
// fallback path actually finds a route.
func TestTelemetry_BackendRefFallback_Resolved(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("demo", "api-svc", RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
	})
	p := newInstrumentedTestProcessors(t, lookup, nil)

	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr("server.address", "api-svc.demo.svc.cluster.local")
	require.NoError(t, p.traces.ConsumeTraces(context.Background(), td))

	sm := readScopeMetrics(t, p.reader)
	m, ok := findMetric(sm, "gatewayapiprocessor_backend_ref_fallback_total")
	require.True(t, ok)
	assert.Equal(t, int64(1), sumInt64Filtered(m, map[string]string{"outcome": "resolved"}))
}

// When neither direct parse nor backendRef resolves, fallback counter ticks
// outcome=unresolved AND enrichments_total ticks outcome=dropped.
func TestTelemetry_BackendRefFallback_Unresolved(t *testing.T) {
	lookup := newStaticLookup()
	p := newInstrumentedTestProcessors(t, lookup, nil)

	td := ptrace.NewTraces()
	td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	require.NoError(t, p.traces.ConsumeTraces(context.Background(), td))

	sm := readScopeMetrics(t, p.reader)
	fbm, ok := findMetric(sm, "gatewayapiprocessor_backend_ref_fallback_total")
	require.True(t, ok)
	assert.Equal(t, int64(1), sumInt64Filtered(fbm, map[string]string{"outcome": "unresolved"}))

	em, ok := findMetric(sm, "gatewayapiprocessor_enrichments_total")
	require.True(t, ok)
	assert.Equal(t, int64(1), sumInt64Filtered(em, map[string]string{"signal": "traces", "outcome": "dropped"}))
}

// Status-conditions counter is gated: when EmitStatusConds=true and the route
// has Accepted/ResolvedRefs populated, we must tick once per stamped record.
func TestTelemetry_StatusConditionsStamped(t *testing.T) {
	accepted := true
	resolved := true
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "demo", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
		Accepted: &accepted, ResolvedRefs: &resolved,
	})
	p := newInstrumentedTestProcessors(t, lookup, func(c *Config) {
		c.EmitStatusConds = true
	})

	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr("route_name", "httproute/demo/api")
	require.NoError(t, p.traces.ConsumeTraces(context.Background(), td))

	sm := readScopeMetrics(t, p.reader)
	m, ok := findMetric(sm, "gatewayapiprocessor_status_conditions_stamped_total")
	require.True(t, ok)
	assert.Equal(t, int64(1), sumInt64(m))
}

// Setting EmitStatusConds=false must short-circuit the counter even when a
// route carries Accepted/ResolvedRefs.
func TestTelemetry_StatusConditionsStamped_GatedOff(t *testing.T) {
	accepted := true
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "demo", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
		Accepted: &accepted,
	})
	p := newInstrumentedTestProcessors(t, lookup, func(c *Config) {
		c.EmitStatusConds = false
	})

	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr("route_name", "httproute/demo/api")
	require.NoError(t, p.traces.ConsumeTraces(context.Background(), td))

	sm := readScopeMetrics(t, p.reader)
	m, ok := findMetric(sm, "gatewayapiprocessor_status_conditions_stamped_total")
	if ok {
		assert.Equal(t, int64(0), sumInt64(m), "counter must stay 0 when gated off")
	}
}

// routes_indexed tracks the number of keys in the index, labelled by
// gateway_class + route_kind. Upsert-new → +1; delete → -1. Update that
// changes gateway_class → swap -1/+1.
func TestTelemetry_RoutesIndexed_DeltaOnUpsertDelete(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tb, err := newTelemetryBuilder(component.TelemetrySettings{MeterProvider: mp}, zap.NewNop())
	require.NoError(t, err)

	idx := newRouteIndex()
	idx.attachTelemetry(tb)

	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
		GatewayClassName: "envoygwc",
	}, nil)
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
		GatewayClassName: "kgatewaygwc",
	}, nil)
	idx.deleteHTTPRoute("demo", "api")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var sm metricdata.ScopeMetrics
	for _, s := range rm.ScopeMetrics {
		if s.Scope.Name == scopeName {
			sm = s
		}
	}
	m, ok := findMetric(sm, "gatewayapiprocessor_routes_indexed")
	require.True(t, ok)
	// After +1 (envoygwc) then swap (-1 envoygwc, +1 kgatewaygwc) then -1
	// (kgatewaygwc), net is 0 for both labels.
	assert.Equal(t, int64(0), sumInt64FilteredUDC(m, map[string]string{"gateway_class": "envoygwc"}))
	assert.Equal(t, int64(0), sumInt64FilteredUDC(m, map[string]string{"gateway_class": "kgatewaygwc"}))
}

// sumInt64FilteredUDC reads an Int64 UpDownCounter (Sum with
// IsMonotonic=false) and sums data points matching the given labels.
func sumInt64FilteredUDC(m metricdata.Metrics, want map[string]string) int64 {
	var total int64
	v, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0
	}
dpLoop:
	for _, dp := range v.DataPoints {
		for k, expected := range want {
			got, ok := dp.Attributes.Value(attribute.Key(k))
			if !ok || got.AsString() != expected {
				continue dpLoop
			}
		}
		total += dp.Value
	}
	return total
}

// informer_events_total must tick for each synthetic upsert we push through
// the registered handler wrappers.
func TestTelemetry_InformerEvents_RecordedPerEvent(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tb, err := newTelemetryBuilder(component.TelemetrySettings{MeterProvider: mp}, zap.NewNop())
	require.NoError(t, err)

	ctx := context.Background()
	tb.recordInformerEvent(ctx, "HTTPRoute", "add")
	tb.recordInformerEvent(ctx, "HTTPRoute", "update")
	tb.recordInformerEvent(ctx, "HTTPRoute", "delete")
	tb.recordInformerEvent(ctx, "Gateway", "sync")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	var sm metricdata.ScopeMetrics
	for _, s := range rm.ScopeMetrics {
		if s.Scope.Name == scopeName {
			sm = s
		}
	}
	m, ok := findMetric(sm, "gatewayapiprocessor_informer_events_total")
	require.True(t, ok)
	assert.Equal(t, int64(4), sumInt64(m))
	// Labels must carry the resource + event tuple verbatim.
	assert.Equal(t, int64(1), sumInt64Filtered(m, map[string]string{"resource": "HTTPRoute", "event": "add"}))
	assert.Equal(t, int64(1), sumInt64Filtered(m, map[string]string{"resource": "Gateway", "event": "sync"}))
}

// The EnrichBatch span must be emitted and carry the self-marker so a fan-in
// loop can drop it. Span name + attributes are the contract.
func TestTelemetry_EnrichBatchSpan_CarriesSelfMarker(t *testing.T) {
	lookup := newStaticLookup()
	p := newInstrumentedTestProcessors(t, lookup, nil)

	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.Attributes().PutStr("route_name", "not-a-real-route")
	require.NoError(t, p.traces.ConsumeTraces(context.Background(), td))

	ended := p.spans.Ended()
	require.NotEmpty(t, ended, "EnrichBatch span must end")
	var found bool
	for _, sp := range ended {
		if sp.Name() != "gatewayapiprocessor.EnrichBatch" {
			continue
		}
		found = true
		var marker bool
		for _, attr := range sp.Attributes() {
			if string(attr.Key) == selfAttrKey {
				marker = attr.Value.AsBool()
			}
		}
		assert.True(t, marker, "self-telemetry guard attribute missing from EnrichBatch span")
	}
	assert.True(t, found, "EnrichBatch span not found in recorded spans")
}

// Startup logs must include the enrichment feature matrix at INFO so ops can
// verify the runtime config from the Collector log alone.
func TestTelemetry_StartupInfoLog_IncludesFeatureMatrix(t *testing.T) {
	lookup := newStaticLookup()
	p := newInstrumentedTestProcessors(t, lookup, nil)
	_ = p // Start was already called inside newInstrumentedTestProcessors

	starts := p.logObs.FilterMessage("gatewayapiprocessor starting").All()
	require.NotEmpty(t, starts, "expected 'gatewayapiprocessor starting' INFO log")
	// At least one start log per factory call — we stand up traces+logs+metrics
	// so 3 messages are expected, but we only assert ≥1 so the test isn't
	// brittle if we later dedupe.
	one := starts[0]
	haveKeys := map[string]bool{}
	for _, f := range one.Context {
		haveKeys[f.Key] = true
	}
	for _, k := range []string{"enrich.traces", "enrich.logs", "enrich.metrics", "emit_status_conditions"} {
		assert.True(t, haveKeys[k], "startup log missing field: %s", k)
	}
}

// Self-telemetry must stamp the selfAttrKey on every span so a feedback loop
// is detectable by a downstream processor. Also guards against us accidentally
// renaming the constant without updating docs.
func TestTelemetry_SelfMarker_ConstantStable(t *testing.T) {
	assert.Equal(t, "gatewayapiprocessor.self", selfAttrKey)
	assert.True(t, strings.Contains(scopeName, "gatewayapiprocessor"))
}

// Unused SDK span sanity guard — prevents the linter from complaining about
// unused imports when test bodies evolve.
var _ = sdktrace.NewTracerProvider
