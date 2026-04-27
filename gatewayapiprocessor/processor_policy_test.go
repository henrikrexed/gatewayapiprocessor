package gatewayapiprocessor

// Policy attachment enrichment (ISI-804).
//
// Tests cover the contract Henrik approved on 2026-04-27:
//   - Multi-policy stamping uses parallel array attributes (one element per
//     policy, element-wise correlated by index).
//   - No policy.uid is stamped — only name + kind + namespace + group.
//   - Routes with no attached policy emit no policy.* attributes at all.
//   - target_kind mirrors the matched route kind (HTTPRoute or GRPCRoute).
//   - Stamping is shared between the direct parser path and the
//     backendref_fallback path (the matrix below exercises both via the
//     existing route lookup wiring).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

func TestEnrichment_Policy_NoPolicyOnRoute(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "default", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "default",
		UID:       "uid",
		// Policies intentionally nil — the v1 baseline.
	})

	tp := newTestProcessors(t, lookup, mixedParserChain)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name": "httproute/default/api/rule/0/match/0",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	assertNoPolicyAttrs(t, attrs)
}

func TestEnrichment_Policy_SinglePolicyStampsArrays(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "frontend-to-cart", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "frontend-to-cart",
		Namespace: "otel-demo",
		UID:       "uid-frontend",
		Policies: []PolicyRef{
			{Name: "rate-limit-frontend", Namespace: "otel-demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev"},
		},
	})

	tp := newTestProcessors(t, lookup, mixedParserChain)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name": "httproute/otel-demo/frontend-to-cart/rule/0/match/0",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	assert.Equal(t, []string{"rate-limit-frontend"}, sliceAttr(t, attrs, AttrPolicyNames))
	assert.Equal(t, []string{"TrafficPolicy"}, sliceAttr(t, attrs, AttrPolicyKinds))
	assert.Equal(t, []string{"otel-demo"}, sliceAttr(t, attrs, AttrPolicyNamespaces))
	assert.Equal(t, []string{"gateway.kgateway.dev"}, sliceAttr(t, attrs, AttrPolicyGroups))

	target, ok := attrs.Get(AttrPolicyTargetKind)
	require.True(t, ok, "target_kind must be stamped when policies are present")
	assert.Equal(t, "HTTPRoute", target.AsString())

	// Henrik's direction: no policy.uid attribute. Guard against accidental
	// re-introduction.
	_, hasUID := attrs.Get("k8s.gatewayapi.policy.uid")
	assert.False(t, hasUID, "policy.uid must never be stamped (ISI-804 cardinality decision)")
}

func TestEnrichment_Policy_MultiPolicyArraysAreElementWise(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "frontend-to-cart", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "frontend-to-cart",
		Namespace: "otel-demo",
		UID:       "uid-frontend",
		Policies: []PolicyRef{
			{Name: "rate-limit-frontend", Namespace: "otel-demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev"},
			{Name: "retries", Namespace: "otel-demo", Kind: "BackendConfigPolicy", Group: "gateway.kgateway.dev"},
		},
	})

	tp := newTestProcessors(t, lookup, mixedParserChain)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name": "httproute/otel-demo/frontend-to-cart/rule/0/match/0",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])

	names := sliceAttr(t, attrs, AttrPolicyNames)
	kinds := sliceAttr(t, attrs, AttrPolicyKinds)
	namespaces := sliceAttr(t, attrs, AttrPolicyNamespaces)
	groups := sliceAttr(t, attrs, AttrPolicyGroups)

	require.Len(t, names, 2)
	require.Len(t, kinds, 2)
	require.Len(t, namespaces, 2)
	require.Len(t, groups, 2)

	// Element-wise correlation contract: index 0 is the first policy, index 1
	// the second, in informer-discovery order. Dashboards rely on this so a
	// query like `policy.kinds[i] == 'TrafficPolicy'` selects the matching
	// policy.names[i].
	assert.Equal(t, []string{"rate-limit-frontend", "retries"}, names)
	assert.Equal(t, []string{"TrafficPolicy", "BackendConfigPolicy"}, kinds)
	assert.Equal(t, []string{"otel-demo", "otel-demo"}, namespaces)
	assert.Equal(t, []string{"gateway.kgateway.dev", "gateway.kgateway.dev"}, groups)
}

func TestEnrichment_Policy_GRPCRouteTargetKind(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindGRPCRoute, "otel-demo", "checkout-grpc", RouteAttributes{
		Kind:      RouteKindGRPCRoute,
		Name:      "checkout-grpc",
		Namespace: "otel-demo",
		UID:       "uid-checkout",
		Policies: []PolicyRef{
			{Name: "rate-limit-checkout", Namespace: "otel-demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev"},
		},
	})

	tp := newTestProcessors(t, lookup, mixedParserChain)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name":      "checkout-grpc",
			"route_kind":      "GRPCRoute",
			"route_namespace": "otel-demo",
		}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	target, ok := attrs.Get(AttrPolicyTargetKind)
	require.True(t, ok)
	assert.Equal(t, "GRPCRoute", target.AsString(),
		"target_kind must mirror the matched route kind (GRPCRoute here)")
}

// Cardinality discipline: when the user excludes policy.* from metric
// pipelines, those attrs are stripped before the metric is forwarded — the
// policy attribution still rides on traces/logs unchanged.
func TestEnrichment_Policy_MetricsExcludeStripsPolicyAttrs(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "frontend-to-cart", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "frontend-to-cart",
		Namespace: "otel-demo",
		UID:       "uid-frontend",
		Policies: []PolicyRef{
			{Name: "rate-limit-frontend", Namespace: "otel-demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev"},
		},
	})

	tweak := func(c *Config) {
		mixedParserChain(c)
		c.Enrich.Metrics = true
		c.Enrich.ExcludeFromMetricAttributes = []string{
			AttrPolicyNames, AttrPolicyKinds, AttrPolicyNamespaces, AttrPolicyGroups,
		}
	}
	tp := newTestProcessors(t, lookup, tweak)

	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(),
		singleSumMetricWith(map[string]string{
			"route_name": "httproute/otel-demo/frontend-to-cart/rule/0/match/0",
		}),
	))

	require.Equal(t, 1, len(tp.ms.AllMetrics()))
	dp := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)
	for _, k := range []string{AttrPolicyNames, AttrPolicyKinds, AttrPolicyNamespaces, AttrPolicyGroups} {
		_, has := dp.Attributes().Get(k)
		assert.Falsef(t, has, "metric pipeline must strip %q per ExcludeFromMetricAttributes", k)
	}
	// target_kind is left in place — it's a low-cardinality scalar and useful
	// on metrics. Users who want to strip it can add it to the exclude list.
	tk, has := dp.Attributes().Get(AttrPolicyTargetKind)
	require.True(t, has)
	assert.Equal(t, "HTTPRoute", tk.AsString())

	// And on traces, the same enrichment runs unmodified.
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{
			"route_name": "httproute/otel-demo/frontend-to-cart/rule/0/match/0",
		}),
	))
	spanAttrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	assert.Equal(t, []string{"rate-limit-frontend"}, sliceAttr(t, spanAttrs, AttrPolicyNames))
}

// ---- helpers ----

// sliceAttr extracts a string array attribute. Fails the test if the key is
// missing or not a slice — both indicate a stamping bug.
func sliceAttr(t *testing.T, attrs pcommon.Map, key string) []string {
	t.Helper()
	v, ok := attrs.Get(key)
	require.Truef(t, ok, "attribute %q missing", key)
	require.Equalf(t, pcommon.ValueTypeSlice, v.Type(), "attribute %q is not a slice", key)
	s := v.Slice()
	out := make([]string, 0, s.Len())
	for i := 0; i < s.Len(); i++ {
		out = append(out, s.At(i).AsString())
	}
	return out
}

// assertNoPolicyAttrs verifies the policy.* contract isn't stamped when no
// policy targets the route. Catches regressions where stampPolicyAttrs is
// called unconditionally.
func assertNoPolicyAttrs(t *testing.T, attrs pcommon.Map) {
	t.Helper()
	for _, k := range []string{
		AttrPolicyNames,
		AttrPolicyKinds,
		AttrPolicyNamespaces,
		AttrPolicyGroups,
		AttrPolicyTargetKind,
	} {
		_, has := attrs.Get(k)
		assert.Falsef(t, has, "policy attribute %q must not be stamped when no policy targets the route", k)
	}
}
