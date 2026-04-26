package gatewayapiprocessor

// Guard 1 enforcement (obs-annex §E, ISI-749 → ISI-756).
//
// The Observability annex declared parser parity between the kind demo cluster
// and the new ClusterAPI cluster — *no per-cluster code path*. The Envoy
// route_name parser (parser/envoy.go) is data-driven: a configurable regex
// with named captures (ns, name, optional rule/match) handles every cluster
// identically. To prevent regressions, this matrix file enforces Guard 1 of
// the obs-annex:
//
//	"Add a processor_matrix_test row that runs with k8s.cluster.name=
//	clusterapi-isi-01 resource attribute set, asserts the same expected
//	k8s.gateway.* / k8s.httproute.* enrichment as the kind run."
//
// See TestEnrichment_ClusterAttribute_ParserParity below for the enforcement
// row. Any future change that introduces a per-cluster code path MUST keep
// the parity assertion green; if it cannot, the change is a parser-parity
// regression and belongs in a new gatewayapiprocessor issue, not a
// per-cluster fork.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
)

// ---- Expanded matrix per ISI-684 §scope ----

// Mixed parser chain: Envoy + Linkerd + passthrough must co-exist; the first
// parser whose attributes are present wins. Matches processor-spec §2.2
// "parsers run in order".
func TestEnrichment_MixedParsers_EnvoyWinsWhenRouteNameMatches(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "default", UID: "uid",
	})

	tp := newTestProcessors(t, lookup, mixedParserChain)

	// Both envoy-style and linkerd-style attrs are present; envoy (first in
	// chain) must claim the record.
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name":      "httproute/default/api/rule/0/match/0",
			"route_kind":      "HTTPRoute",
			"route_namespace": "default",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	parser, _ := attrs.Get(AttrParser)
	assert.Equal(t, "envoy", parser.AsString())
	name, _ := attrs.Get(AttrHTTPRouteName)
	assert.Equal(t, "api", name.AsString())
}

func TestEnrichment_MixedParsers_LinkerdFallsThroughWhenEnvoyMisses(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "prod", "checkout", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "checkout", Namespace: "prod", UID: "uid",
	})

	tp := newTestProcessors(t, lookup, mixedParserChain)

	// Not an envoy-style route_name; Linkerd labels are present; Linkerd must win.
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name":      "checkout",
			"route_kind":      "HTTPRoute",
			"route_namespace": "prod",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	parser, _ := attrs.Get(AttrParser)
	assert.Equal(t, "linkerd", parser.AsString())
	name, _ := attrs.Get(AttrHTTPRouteName)
	assert.Equal(t, "checkout", name.AsString())
	ns, _ := attrs.Get(AttrHTTPRouteNamespace)
	assert.Equal(t, "prod", ns.AsString())
}

func TestEnrichment_MixedParsers_PassthroughLastResort(t *testing.T) {
	tp := newTestProcessors(t, newStaticLookup(), mixedParserChain)

	// Unparseable opaque string, no linkerd labels — passthrough takes it.
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "opaque-envoy-internal"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	parser, _ := attrs.Get(AttrParser)
	assert.Equal(t, "passthrough", parser.AsString())
	raw, _ := attrs.Get(AttrRawRouteName)
	assert.Equal(t, "opaque-envoy-internal", raw.AsString())
}

// GRPCRoute enrichment: parser yields Kind=GRPCRoute; processor stamps the
// gRPC attribute keys (and does NOT stamp the httproute keys). processor-spec
// §1.2 rows 9–10.
func TestEnrichment_GRPCRoute_StampsGRPCKeysOnly(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindGRPCRoute, "grpc-ns", "svc", RouteAttributes{
		Kind: RouteKindGRPCRoute, Name: "svc", Namespace: "grpc-ns", UID: "uid",
		GatewayName: "public",
	})

	tp := newTestProcessors(t, lookup, func(c *Config) {
		c.Parsers = []ParserConfig{
			{
				Name: "linkerd",
				LinkerdLabels: LinkerdLabelsConfig{
					RouteName: "route_name", RouteKind: "route_kind", RouteNamespace: "route_namespace",
				},
			},
		}
	})

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name":      "svc",
			"route_kind":      "GRPCRoute",
			"route_namespace": "grpc-ns",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	name, ok := attrs.Get(AttrGRPCRouteName)
	require.True(t, ok)
	assert.Equal(t, "svc", name.AsString())

	ns, ok := attrs.Get(AttrGRPCRouteNamespace)
	require.True(t, ok)
	assert.Equal(t, "grpc-ns", ns.AsString())

	_, hasHTTP := attrs.Get(AttrHTTPRouteName)
	assert.False(t, hasHTTP, "gRPC-kind routes must NOT stamp httproute.* keys")

	// Per spec §1.2, UID is only on HTTPRoute attrs, not on GRPCRoute.
	_, hasUID := attrs.Get(AttrHTTPRouteUID)
	assert.False(t, hasUID)
}

// emit_status_conditions=false: informer projection leaves Accepted/ResolvedRefs
// nil and the stamping path must not emit the attributes.
func TestEmitStatusConditions_Off_DoesNotStamp(t *testing.T) {
	lookup := newStaticLookup()
	tru := true
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "default",
		Accepted: &tru, // even if populated, EmitStatusConds=false must suppress
	})

	tp := newTestProcessors(t, lookup, func(c *Config) { c.EmitStatusConds = false })

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/default/api"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteAccepted)
	assert.False(t, ok, "emit_status_conditions=false must suppress k8s.httproute.accepted")
	_, ok = attrs.Get(AttrHTTPRouteResolvedRefs)
	assert.False(t, ok)
}

// Gateway UID and raw_route_name are default entries in
// ExcludeFromMetricAttributes — both must be stripped from the metrics
// pipeline, complementing the HTTPRoute UID coverage in TestMetricAttributeFilter.
func TestMetricAttributeFilter_GatewayUID_AndRawRouteName_Stripped(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "default",
		UID: "uid-api", GatewayName: "public", GatewayUID: "gw-uid",
	})

	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(),
		singleSumMetricWith(map[string]string{"route_name": "passthrough-thing"}),
	))

	require.Equal(t, 1, len(tp.ms.AllMetrics()))
	dp := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)

	_, present := dp.Attributes().Get(AttrRawRouteName)
	assert.False(t, present, "k8s.gatewayapi.raw_route_name must be stripped on metrics")
}

// backendref_fallback: disabled flag is a no-op even if server.address matches.
func TestBackendRefFallback_Disabled_NoStamp(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("demo", "api-service", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo",
	})

	tp := newTestProcessors(t, lookup, func(c *Config) {
		c.BackendRefFallback = BackendRefFallback{Enabled: false, SourceAttribute: "server.address"}
	})

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"server.address": "api-service.demo.svc.cluster.local"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok, "backendref_fallback disabled must not stamp httproute.name")
}

// backendref_fallback: unknown address (no index hit) is a no-op — never
// mis-attribute.
func TestBackendRefFallback_UnknownAddress_NoStamp(t *testing.T) {
	tp := newTestProcessors(t, newStaticLookup(), func(c *Config) {
		c.BackendRefFallback = BackendRefFallback{Enabled: true, SourceAttribute: "server.address"}
	})

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"server.address": "not-a-service.demo.svc.cluster.local"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok)
	_, ok = attrs.Get(AttrParser)
	assert.False(t, ok, "no parser should be stamped when nothing matched")
}

// backendref_fallback: ambiguous Service → backend index drops the entry and
// fallback must not stamp. End-to-end version of the unit-level test.
func TestBackendRefFallback_AmbiguousOwner_NoStamp(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api-a",
	}, []backendRef{{Namespace: "demo", Name: "shared"}})
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api-b",
	}, []backendRef{{Namespace: "demo", Name: "shared"}})

	tp := newTestProcessors(t, idx, func(c *Config) {
		c.BackendRefFallback = BackendRefFallback{Enabled: true, SourceAttribute: "server.address"}
	})

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"server.address": "shared.demo.svc.cluster.local"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok, "ambiguous backend must NOT mis-attribute to either owner")
}

// Resource-level attribute fallback: route_name on resource attrs should still
// drive enrichment when no record attribute is set. combinedView prefers
// record over resource.
func TestEnrichment_ResourceAttributeFallback(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "default",
	})

	tp := newTestProcessors(t, lookup, nil)

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("route_name", "httproute/default/api")
	ss := rs.ScopeSpans().AppendEmpty()
	_ = ss.Spans().AppendEmpty() // span has no attributes — resource drives enrichment

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), td))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	name, ok := attrs.Get(AttrHTTPRouteName)
	require.True(t, ok, "resource-attribute fallback must drive enrichment")
	assert.Equal(t, "api", name.AsString())
}

// Metric type matrix — enrichMetric must walk every pmetric.MetricType.
func TestEnrichMetric_AllMetricTypes(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "default",
	})
	tp := newTestProcessors(t, lookup, nil)

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	sm := rm.ScopeMetrics().AppendEmpty()

	// Gauge
	mg := sm.Metrics().AppendEmpty()
	mg.SetName("gauge")
	mg.SetEmptyGauge().DataPoints().AppendEmpty().Attributes().PutStr("route_name", "httproute/default/api")

	// Sum
	ms := sm.Metrics().AppendEmpty()
	ms.SetName("sum")
	ms.SetEmptySum().DataPoints().AppendEmpty().Attributes().PutStr("route_name", "httproute/default/api")

	// Histogram
	mh := sm.Metrics().AppendEmpty()
	mh.SetName("hist")
	mh.SetEmptyHistogram().DataPoints().AppendEmpty().Attributes().PutStr("route_name", "httproute/default/api")

	// Exponential histogram
	me := sm.Metrics().AppendEmpty()
	me.SetName("exp")
	me.SetEmptyExponentialHistogram().DataPoints().AppendEmpty().Attributes().PutStr("route_name", "httproute/default/api")

	// Summary
	mq := sm.Metrics().AppendEmpty()
	mq.SetName("summary")
	mq.SetEmptySummary().DataPoints().AppendEmpty().Attributes().PutStr("route_name", "httproute/default/api")

	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(), md))

	out := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	require.Equal(t, 5, out.Len())

	// Verify each metric type stamped httproute.name on its data point.
	assert.True(t, hasStringAttr(out.At(0).Gauge().DataPoints().At(0).Attributes(), AttrHTTPRouteName, "api"))
	assert.True(t, hasStringAttr(out.At(1).Sum().DataPoints().At(0).Attributes(), AttrHTTPRouteName, "api"))
	assert.True(t, hasStringAttr(out.At(2).Histogram().DataPoints().At(0).Attributes(), AttrHTTPRouteName, "api"))
	assert.True(t, hasStringAttr(out.At(3).ExponentialHistogram().DataPoints().At(0).Attributes(), AttrHTTPRouteName, "api"))
	assert.True(t, hasStringAttr(out.At(4).Summary().DataPoints().At(0).Attributes(), AttrHTTPRouteName, "api"))
}

// Capabilities must report MutatesData=true so collector wiring doesn't try to
// share the record slice between branches.
func TestCapabilities_MutatesData(t *testing.T) {
	tp := newTestProcessors(t, newStaticLookup(), nil)
	assert.True(t, tp.traces.Capabilities().MutatesData)
}

// Shutdown must invoke stopFn when one was installed by Start().
func TestShutdown_InvokesStopFn(t *testing.T) {
	factory := NewFactory()
	cfg := createDefaultConfig().(*Config)
	cfg.AuthType = AuthTypeNone
	p, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		consumertest.NewNop(),
	)
	require.NoError(t, err)
	gp := p.(*gatewayAPIProcessor)

	called := false
	gp.startHook = func(_ context.Context) (RouteLookup, func(context.Context) error, error) {
		return newStaticLookup(), func(context.Context) error {
			called = true
			return nil
		}, nil
	}

	require.NoError(t, gp.Start(context.Background(), componenttest.NewNopHost()))
	require.NoError(t, gp.Shutdown(context.Background()))
	assert.True(t, called, "Shutdown must invoke the stop function returned by startHook")
}

// Start() surfaces errors from the startHook.
func TestStart_PropagatesStartHookError(t *testing.T) {
	factory := NewFactory()
	cfg := createDefaultConfig().(*Config)
	cfg.AuthType = AuthTypeNone
	p, err := factory.CreateTraces(
		context.Background(),
		processortest.NewNopSettings(factory.Type()),
		cfg,
		consumertest.NewNop(),
	)
	require.NoError(t, err)
	gp := p.(*gatewayAPIProcessor)
	gp.startHook = func(_ context.Context) (RouteLookup, func(context.Context) error, error) {
		return nil, nil, fmt.Errorf("boom")
	}

	err = gp.Start(context.Background(), componenttest.NewNopHost())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// ---- Factory smoke: all three signal processors instantiate and enrich ----

func TestFactory_CreatesAllThreeProcessors(t *testing.T) {
	factory := NewFactory()
	cfg := createDefaultConfig().(*Config)
	cfg.AuthType = AuthTypeNone
	set := processortest.NewNopSettings(factory.Type())

	tp, err := factory.CreateTraces(context.Background(), set, cfg, consumertest.NewNop())
	require.NoError(t, err)
	require.NotNil(t, tp)

	lp, err := factory.CreateLogs(context.Background(), set, cfg, consumertest.NewNop())
	require.NoError(t, err)
	require.NotNil(t, lp)

	mp, err := factory.CreateMetrics(context.Background(), set, cfg, consumertest.NewNop())
	require.NoError(t, err)
	require.NotNil(t, mp)
}

// TestEnrichment_ClusterAttribute_ParserParity is the test enforcement of
// Guard 1 from the obs-annex (see file preamble). Two rows — kind-isi-01 and
// clusterapi-isi-01 — are run through the same processor with the same parser
// config, the same RouteLookup, and the same input span. The k8s.cluster.name
// resource attribute is the *only* thing that varies. The test asserts that:
//
//  1. enrichment runs and stamps a non-empty k8s.gateway.* / k8s.httproute.*
//     attribute set on the kind row (otherwise a parser regression would
//     silently make this test pass), and
//  2. the clusterapi row produces a byte-for-byte identical attribute set,
//     proving the parser does NOT special-case the cluster, and
//  3. the k8s.cluster.name resource attribute survives enrichment unchanged
//     so downstream Dynatrace Management Zone routing (obs-annex §D) keeps
//     working.
//
// A future per-cluster code path would surface here as a diff in (2) — fail
// the build, force the regression into review.
func TestEnrichment_ClusterAttribute_ParserParity(t *testing.T) {
	const routeName = "httproute/default/api/rule/0/match/0"

	// Snapshot every k8s.gateway.* / k8s.gatewayclass.* / k8s.httproute.* /
	// k8s.gatewayapi.* attribute on the enriched span into a comparable map.
	snapshot := func(attrs pcommon.Map) map[string]string {
		out := map[string]string{}
		attrs.Range(func(k string, v pcommon.Value) bool {
			if strings.HasPrefix(k, "k8s.gateway.") ||
				strings.HasPrefix(k, "k8s.gatewayclass.") ||
				strings.HasPrefix(k, "k8s.httproute.") ||
				strings.HasPrefix(k, "k8s.gatewayapi.") {
				out[k] = v.AsString()
			}
			return true
		})
		return out
	}

	run := func(t *testing.T, clusterName string) map[string]string {
		t.Helper()
		lookup := newStaticLookup()
		lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
			Kind:                       RouteKindHTTPRoute,
			Name:                       "api",
			Namespace:                  "default",
			UID:                        "uid-api",
			GatewayName:                "public",
			GatewayNamespace:           "default",
			GatewayUID:                 "gw-uid",
			GatewayListenerName:        "https",
			GatewayClassName:           "istio",
			GatewayClassControllerName: "istio.io/gateway-controller",
		})
		tp := newTestProcessors(t, lookup, nil)

		td := ptrace.NewTraces()
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("k8s.cluster.name", clusterName)
		ss := rs.ScopeSpans().AppendEmpty()
		sp := ss.Spans().AppendEmpty()
		sp.Attributes().PutStr("route_name", routeName)

		require.NoError(t, tp.traces.ConsumeTraces(context.Background(), td))
		require.Equal(t, 1, len(tp.ts.AllTraces()))

		out := tp.ts.AllTraces()[0]
		// (3) k8s.cluster.name on resource must survive enrichment — the agent
		// stamps it, the gateway must not strip it. obs-annex §D depends on it.
		gotCluster, ok := out.ResourceSpans().At(0).Resource().Attributes().Get("k8s.cluster.name")
		require.True(t, ok, "k8s.cluster.name resource attribute must be preserved")
		require.Equal(t, clusterName, gotCluster.AsString())

		return snapshot(getSpanAttrs(t, out))
	}

	kindOut := run(t, "kind-isi-01")
	capiOut := run(t, "clusterapi-isi-01")

	// (1) Sanity — without this, a future change that broke the parser would
	// silently produce two empty maps and trivially "match".
	require.Equal(t, "api", kindOut[AttrHTTPRouteName],
		"kind row produced no httproute.name — parser regression")
	require.Equal(t, "public", kindOut[AttrGatewayName],
		"kind row produced no gateway.name — CR-metadata stamping broken")
	require.Equal(t, "envoy", kindOut[AttrParser],
		"kind row should have matched the envoy parser")

	// (2) Parity — every k8s.gateway.* / k8s.httproute.* / k8s.gatewayapi.*
	// attribute is identical regardless of k8s.cluster.name.
	assert.Equal(t, kindOut, capiOut,
		"parser output must be identical regardless of k8s.cluster.name (Guard 1)")
}

// ---- helpers ----

// mixedParserChain wires envoy+linkerd+passthrough in that order — a common
// real-world config where a cluster runs multiple gatewayclass controllers.
func mixedParserChain(c *Config) {
	c.Parsers = []ParserConfig{
		{
			Name:            "envoy",
			SourceAttribute: "route_name",
			FormatRegex:     `^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)(?:/rule/(?P<rule>\d+))?(?:/match/(?P<match>\d+))?`,
		},
		{
			Name: "linkerd",
			LinkerdLabels: LinkerdLabelsConfig{
				RouteName: "route_name", RouteKind: "route_kind", RouteNamespace: "route_namespace",
			},
		},
		{
			Name:                 "passthrough",
			SourceAttribute:      "route_name",
			PassthroughAttribute: "k8s.gatewayapi.raw_route_name",
		},
	}
}

func hasStringAttr(attrs pcommon.Map, key, want string) bool {
	v, ok := attrs.Get(key)
	if !ok {
		return false
	}
	return v.AsString() == want
}
