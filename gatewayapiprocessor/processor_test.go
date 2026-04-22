package gatewayapiprocessor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
)

// ---- Test helpers ----

// testProcessors bundles the three per-signal processors plus their downstream
// sinks so tests can drive any signal through the correct instance.
type testProcessors struct {
	traces  *gatewayAPIProcessor
	logs    *gatewayAPIProcessor
	metrics *gatewayAPIProcessor
	ts      *consumertest.TracesSink
	ls      *consumertest.LogsSink
	ms      *consumertest.MetricsSink
}

// newTestProcessors builds the full factory triple wired to the given static
// lookup. No informers, no kube — AuthType is forced to "none" so the default
// start hook is skipped.
func newTestProcessors(t *testing.T, lookup RouteLookup, tweak func(*Config)) *testProcessors {
	t.Helper()
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

	set := processortest.NewNopSettings(factory.Type())
	tp, err := factory.CreateTraces(context.Background(), set, cfg, ts)
	require.NoError(t, err)
	lp, err := factory.CreateLogs(context.Background(), set, cfg, ls)
	require.NoError(t, err)
	mp, err := factory.CreateMetrics(context.Background(), set, cfg, ms)
	require.NoError(t, err)

	tps := tp.(*gatewayAPIProcessor)
	lps := lp.(*gatewayAPIProcessor)
	mps := mp.(*gatewayAPIProcessor)
	for _, p := range []*gatewayAPIProcessor{tps, lps, mps} {
		p.lookup = lookup
		p.startHook = nil
	}
	require.NoError(t, tps.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { _ = tps.Shutdown(context.Background()) })

	return &testProcessors{traces: tps, logs: lps, metrics: mps, ts: ts, ls: ls, ms: ms}
}

// singleSpanWith creates a ptrace.Traces with one span whose attributes are `attrs`.
func singleSpanWith(attrs map[string]string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	sp := ss.Spans().AppendEmpty()
	for k, v := range attrs {
		sp.Attributes().PutStr(k, v)
	}
	return td
}

// singleSumMetricWith creates a pmetric.Metrics with one sum data point carrying attrs.
func singleSumMetricWith(attrs map[string]string) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	sm := rm.ScopeMetrics().AppendEmpty()
	m := sm.Metrics().AppendEmpty()
	m.SetName("http.server.request.duration")
	dp := m.SetEmptySum().DataPoints().AppendEmpty()
	for k, v := range attrs {
		dp.Attributes().PutStr(k, v)
	}
	return md
}

// singleLogWith creates a plog.Logs with one record.
func singleLogWith(attrs map[string]string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	sl := rl.ScopeLogs().AppendEmpty()
	lr := sl.LogRecords().AppendEmpty()
	for k, v := range attrs {
		lr.Attributes().PutStr(k, v)
	}
	return ld
}

func getSpanAttrs(t *testing.T, td ptrace.Traces) pcommon.Map {
	t.Helper()
	require.Equal(t, 1, td.SpanCount())
	return td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes()
}

// ---- Tests 6 & 7: status conditions ----

// Test 6 — HTTPRoute with Accepted=true stamps k8s.httproute.accepted=true.
// processor-spec §2.5: TestStatusConditions_Accepted.
func TestStatusConditions_Accepted(t *testing.T) {
	lookup := newStaticLookup()
	tru := true
	fls := false
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind:         RouteKindHTTPRoute,
		Name:         "api",
		Namespace:    "default",
		UID:          "uid-api",
		Accepted:     &tru,
		ResolvedRefs: &fls,
		GatewayName:  "public",
	})

	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/default/api/rule/0/match/0"}),
	))

	require.Equal(t, 1, len(tp.ts.AllTraces()))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])

	got, ok := attrs.Get(AttrHTTPRouteAccepted)
	require.True(t, ok)
	assert.True(t, got.Bool())

	got, ok = attrs.Get(AttrHTTPRouteResolvedRefs)
	require.True(t, ok)
	assert.False(t, got.Bool(), "ResolvedRefs=false must surface as a false bool, not be omitted")
}

// Test 7 — HTTPRoute with Accepted=false stamps k8s.httproute.accepted=false.
// processor-spec §2.5: TestStatusConditions_Rejected.
func TestStatusConditions_Rejected(t *testing.T) {
	lookup := newStaticLookup()
	fls := false
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "default",
		Accepted:  &fls,
	})

	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/default/api"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	got, ok := attrs.Get(AttrHTTPRouteAccepted)
	require.True(t, ok)
	assert.False(t, got.Bool())
}

// ---- Test 8: metric attribute filter ----

// Test 8 — UID attributes are stripped on metrics but retained on traces/logs.
// processor-spec §2.5: TestMetricAttributeFilter.
// processor-spec §1.4 pins this as the Istio Telemetry footgun.
func TestMetricAttributeFilter(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "default",
		UID:       "uid-api",
	})
	tp := newTestProcessors(t, lookup, nil)

	// Traces: UID MUST be retained.
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/default/api/rule/0/match/0"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteUID)
	assert.True(t, ok, "k8s.httproute.uid must be present on traces (high cardinality is acceptable)")

	// Metrics: UID MUST be stripped.
	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(),
		singleSumMetricWith(map[string]string{"route_name": "httproute/default/api/rule/0/match/0"}),
	))
	require.Equal(t, 1, len(tp.ms.AllMetrics()))
	dp := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)
	_, present := dp.Attributes().Get(AttrHTTPRouteUID)
	assert.False(t, present, "k8s.httproute.uid must be stripped on metrics (Istio Telemetry footgun per spec §1.4)")

	// But the route name MUST survive on metrics — it's what the dashboard joins on.
	routeName, ok := dp.Attributes().Get(AttrHTTPRouteName)
	require.True(t, ok)
	assert.Equal(t, "api", routeName.AsString())
}

// ---- Test 9: backendRef fallback ----

// Test 9 — Span carrying only server.address resolves to the correct HTTPRoute.
// processor-spec §2.5: TestBackendRefFallback.
func TestBackendRefFallback(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("demo", "api-service", RouteAttributes{
		Kind:        RouteKindHTTPRoute,
		Name:        "api",
		Namespace:   "demo",
		UID:         "uid-api",
		GatewayName: "public",
	})

	tp := newTestProcessors(t, lookup, func(c *Config) {
		c.BackendRefFallback = BackendRefFallback{Enabled: true, SourceAttribute: "server.address"}
	})

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"server.address": "api-service.demo.svc.cluster.local"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	name, ok := attrs.Get(AttrHTTPRouteName)
	require.True(t, ok, "backendref_fallback must stamp k8s.httproute.name from server.address")
	assert.Equal(t, "api", name.AsString())

	ns, ok := attrs.Get(AttrHTTPRouteNamespace)
	require.True(t, ok)
	assert.Equal(t, "demo", ns.AsString())
}

// ---- Test 10: informer sync timeout ----

// Test 10 — Missing RBAC / unreachable API causes Start() to fail within the
// configured timeout. processor-spec §2.5: TestInformerSyncTimeout.
func TestInformerSyncTimeout(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.InformerSyncTimeout = 200 * time.Millisecond

	factory := NewFactory()
	p, err := factory.CreateTraces(context.Background(), processortest.NewNopSettings(factory.Type()), cfg, consumertest.NewNop())
	require.NoError(t, err)

	// Override startHook with one that never syncs — simulates missing RBAC
	// where WaitForCacheSync times out before HasSynced returns true.
	gp := p.(*gatewayAPIProcessor)
	gp.startHook = func(ctx context.Context) (RouteLookup, func(context.Context) error, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(cfg.InformerSyncTimeout)
		}
		<-time.After(time.Until(deadline) + 10*time.Millisecond)
		return nil, nil, fmt.Errorf("gatewayapiprocessor: informer cache sync timed out after %s", cfg.InformerSyncTimeout)
	}

	// Bound Start with the configured timeout so this test can't hang CI.
	startCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = p.Start(startCtx, componenttest.NewNopHost())
	require.Error(t, err, "Start() must fail when informer caches do not sync")
	assert.Contains(t, err.Error(), "sync timed out")
}

// ---- Bonus coverage (not part of the 10-case matrix but guards hot paths) ----

// TestEnrichment_PassthroughFallback — an unparseable route_name still gets
// the raw attribute stamped and the parser id labeled.
func TestEnrichment_PassthroughFallback(t *testing.T) {
	tp := newTestProcessors(t, newStaticLookup(), nil)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "weird-format/xyz"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	raw, ok := attrs.Get(AttrRawRouteName)
	require.True(t, ok)
	assert.Equal(t, "weird-format/xyz", raw.AsString())

	parser, ok := attrs.Get(AttrParser)
	require.True(t, ok)
	assert.Equal(t, "passthrough", parser.AsString())

	// No HTTPRoute name is stamped — we didn't identify the route.
	_, hasName := attrs.Get(AttrHTTPRouteName)
	assert.False(t, hasName)
}

// TestEnrichment_LogsPath — logs signal is enriched end-to-end.
func TestEnrichment_LogsPath(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "default",
		UID:       "uid-api",
	})
	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.logs.ConsumeLogs(context.Background(),
		singleLogWith(map[string]string{"route_name": "httproute/default/api/rule/1/match/2"}),
	))

	require.Equal(t, 1, len(tp.ls.AllLogs()))
	rec := tp.ls.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)

	name, ok := rec.Attributes().Get(AttrHTTPRouteName)
	require.True(t, ok)
	assert.Equal(t, "api", name.AsString())

	rule, ok := rec.Attributes().Get(AttrHTTPRouteRuleIndex)
	require.True(t, ok)
	assert.Equal(t, int64(1), rule.Int())

	match, ok := rec.Attributes().Get(AttrHTTPRouteMatchIndex)
	require.True(t, ok)
	assert.Equal(t, int64(2), match.Int())

	uid, ok := rec.Attributes().Get(AttrHTTPRouteUID)
	require.True(t, ok, "logs should retain UID (only metrics strip it)")
	assert.Equal(t, "uid-api", uid.AsString())
}
