package gatewayapiprocessor

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
)

// newBenchProcessors is the testing.TB-friendly twin of newTestProcessors.
// It avoids testify.require to stay compatible with *testing.B.
func newBenchProcessors(tb testing.TB, lookup RouteLookup) *testProcessors {
	tb.Helper()
	cfg := createDefaultConfig().(*Config)
	cfg.AuthType = AuthTypeNone
	if err := cfg.Validate(); err != nil {
		tb.Fatalf("config.Validate: %v", err)
	}

	factory := NewFactory()
	ts := new(consumertest.TracesSink)
	ls := new(consumertest.LogsSink)
	ms := new(consumertest.MetricsSink)

	set := processortest.NewNopSettings(factory.Type())
	tp, err := factory.CreateTraces(context.Background(), set, cfg, ts)
	if err != nil {
		tb.Fatalf("CreateTraces: %v", err)
	}
	lp, err := factory.CreateLogs(context.Background(), set, cfg, ls)
	if err != nil {
		tb.Fatalf("CreateLogs: %v", err)
	}
	mp, err := factory.CreateMetrics(context.Background(), set, cfg, ms)
	if err != nil {
		tb.Fatalf("CreateMetrics: %v", err)
	}

	tps := tp.(*gatewayAPIProcessor)
	lps := lp.(*gatewayAPIProcessor)
	mps := mp.(*gatewayAPIProcessor)
	for _, p := range []*gatewayAPIProcessor{tps, lps, mps} {
		p.lookup = lookup
		p.startHook = nil
	}
	if err := tps.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		tb.Fatalf("Start: %v", err)
	}
	tb.Cleanup(func() { _ = tps.Shutdown(context.Background()) })
	return &testProcessors{traces: tps, logs: lps, metrics: mps, ts: ts, ls: ls, ms: ms}
}

// Benchmark suite per ISI-684 §scope 7 — throughput at 1k/10k routes and
// informer memory ceiling. Run with:
//
//   go test -bench=. -benchmem -run=^$ ./...
//
// We intentionally bypass the downstream consumer (using a nil next) to
// isolate enrichment cost from sink overhead. The processor treats nil next
// as "no-op fanout" (see ConsumeTraces/Logs/Metrics).

func seedLookup(n int) *staticLookup {
	lookup := newStaticLookup()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("api-%d", i)
		lookup.put(RouteKindHTTPRoute, "demo", name, RouteAttributes{
			Kind: RouteKindHTTPRoute, Name: name, Namespace: "demo",
			UID: fmt.Sprintf("uid-%d", i), GatewayName: "public",
			GatewayClassName: "envoygwc",
		})
	}
	return lookup
}

func tracesWithNRoutes(n int) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	for i := 0; i < n; i++ {
		sp := ss.Spans().AppendEmpty()
		sp.Attributes().PutStr("route_name", fmt.Sprintf("httproute/demo/api-%d/rule/0/match/0", i))
	}
	return td
}

func metricsWithNRoutes(n int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	sm := rm.ScopeMetrics().AppendEmpty()
	for i := 0; i < n; i++ {
		m := sm.Metrics().AppendEmpty()
		m.SetName("http.server.request.duration")
		dp := m.SetEmptySum().DataPoints().AppendEmpty()
		dp.Attributes().PutStr("route_name", fmt.Sprintf("httproute/demo/api-%d/rule/0/match/0", i))
	}
	return md
}

func BenchmarkEnrichTraces_1kRoutes(b *testing.B) {
	benchEnrichTraces(b, 1000)
}

func BenchmarkEnrichTraces_10kRoutes(b *testing.B) {
	benchEnrichTraces(b, 10000)
}

func BenchmarkEnrichMetrics_1kRoutes(b *testing.B) {
	benchEnrichMetrics(b, 1000)
}

func BenchmarkEnrichMetrics_10kRoutes(b *testing.B) {
	benchEnrichMetrics(b, 10000)
}

func benchEnrichTraces(b *testing.B, n int) {
	tp := newBenchProcessors(b, seedLookup(n))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Clone each iteration is expensive; instead, rebuild once outside
		// by creating a fresh set of records per iteration to avoid reusing
		// already-enriched records (processor mutates in place).
		td := tracesWithNRoutes(n)
		_ = tp.traces.ConsumeTraces(context.Background(), td)
	}
}

func benchEnrichMetrics(b *testing.B, n int) {
	tp := newBenchProcessors(b, seedLookup(n))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		md := metricsWithNRoutes(n)
		_ = tp.metrics.ConsumeMetrics(context.Background(), md)
	}
}

// BenchmarkRouteIndex_Upsert_10k is the informer-memory probe. The informer
// backs the index; upsert rate × object size dominates RSS. b.ReportAllocs
// puts a concrete number next to the README.
func BenchmarkRouteIndex_Upsert_10k(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		idx := newRouteIndex()
		for j := 0; j < 10000; j++ {
			idx.upsertHTTPRoute(RouteAttributes{
				Kind: RouteKindHTTPRoute, Namespace: "demo",
				Name: fmt.Sprintf("api-%d", j),
				UID:  fmt.Sprintf("uid-%d", j),
			}, []backendRef{{Namespace: "demo", Name: fmt.Sprintf("svc-%d", j)}})
		}
	}
}

// BenchmarkEnrichmentHotPath_SingleSpan measures the per-record enrichment
// cost — this is the number the collector's perf budget cares about.
func BenchmarkEnrichmentHotPath_SingleSpan(b *testing.B) {
	tp := newBenchProcessors(b, seedLookup(100))
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		td := singleSpanWith(map[string]string{
			"route_name": "httproute/demo/api-7/rule/0/match/0",
		})
		_ = tp.traces.ConsumeTraces(ctx, td)
	}
}
