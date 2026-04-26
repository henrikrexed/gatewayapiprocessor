package gatewayapiprocessor

// GAMMA (Gateway API for Service Mesh) coverage — east-west routing where
// HTTPRoute/GRPCRoute attach to a Service rather than a Gateway.
// Issue: ISI-783. Reference: https://gateway-api.sigs.k8s.io/mesh/gamma/

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ---- CR projection: GAMMA mode (parentRef.kind=Service) ----

// A GRPCRoute whose parentRef is a Service must be projected with
// RouteMode=mesh and ParentService* populated, and must NOT carry Gateway
// attribution.
func TestGRPCRouteToAttrs_GAMMAMeshMode(t *testing.T) {
	kind := gwv1.Kind("Service")
	group := gwv1.Group("")
	ns := gwv1.Namespace("otel-demo")
	port := gwv1.PortNumber(8080)

	gr := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "frontend-to-product-catalog", Namespace: "otel-demo", UID: types.UID("gr-uid"),
		},
		Spec: gwv1.GRPCRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{
					Group:     &group,
					Kind:      &kind,
					Namespace: &ns,
					Name:      "product-catalog",
					Port:      &port,
				}},
			},
		},
	}
	ra := grpcRouteToAttrs(gr, newGatewayStore(), newGatewayClassStore(), &Config{})

	assert.Equal(t, RouteModeMesh, ra.RouteMode)
	assert.Equal(t, "product-catalog", ra.ParentServiceName)
	assert.Equal(t, "otel-demo", ra.ParentServiceNamespace)
	assert.Empty(t, ra.GatewayName, "GAMMA route must not populate Gateway fields")
	assert.Empty(t, ra.GatewayUID)
	assert.Equal(t, "/Service/otel-demo/product-catalog", ra.ParentRef)
}

// HTTPRoute parentRef.kind=Service — same shape as the GRPCRoute case.
func TestHTTPRouteToAttrs_GAMMAMeshMode(t *testing.T) {
	kind := gwv1.Kind("Service")
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "frontend-proxy-mesh", Namespace: "otel-demo", UID: types.UID("hr-uid"),
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &kind, Name: "frontend-proxy"}},
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), &Config{})

	assert.Equal(t, RouteModeMesh, ra.RouteMode)
	assert.Equal(t, "frontend-proxy", ra.ParentServiceName)
	assert.Equal(t, "otel-demo", ra.ParentServiceNamespace, "namespace defaults to route's own when parentRef.namespace unset")
	assert.Empty(t, ra.GatewayName)
}

// Ingress-mode (parentRef.kind=Gateway, default) must default RouteMode=ingress.
func TestHTTPRouteToAttrs_IngressModeDefault(t *testing.T) {
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Name: "public"}},
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), &Config{})
	assert.Equal(t, RouteModeIngress, ra.RouteMode, "no kind = Gateway = ingress mode")
}

// ---- isServiceParent ----

func TestIsServiceParent_Matrix(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	gwKind := gwv1.Kind("Gateway")
	emptyGroup := gwv1.Group("")
	customGroup := gwv1.Group("example.com")

	cases := []struct {
		name string
		ref  gwv1.ParentReference
		want bool
	}{
		{"explicit core Service", gwv1.ParentReference{Group: &emptyGroup, Kind: &svcKind}, true},
		{"omitted group Service", gwv1.ParentReference{Kind: &svcKind}, true},
		{"explicit Gateway", gwv1.ParentReference{Kind: &gwKind}, false},
		{"unset kind defaults to Gateway", gwv1.ParentReference{}, false},
		{"non-core group Service-named", gwv1.ParentReference{Group: &customGroup, Kind: &svcKind}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isServiceParent(tc.ref))
		})
	}
}

// ---- backendRefsFromGRPCRoute: GAMMA parent-as-backend indexing ----

// A GAMMA GRPCRoute should appear in the backend index keyed on its parent
// Service (where the spans actually go) AND on any explicit backendRef.
func TestBackendRefsFromGRPCRoute_GAMMA_IncludesParentService(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	gr := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "fe2pc", Namespace: "otel-demo"},
		Spec: gwv1.GRPCRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &svcKind, Name: "product-catalog"}},
			},
			Rules: []gwv1.GRPCRouteRule{{
				BackendRefs: []gwv1.GRPCBackendRef{{
					BackendRef: gwv1.BackendRef{
						BackendObjectReference: gwv1.BackendObjectReference{Name: "product-catalog"},
					},
				}},
			}},
		},
	}
	got := backendRefsFromGRPCRoute(gr)
	// Both parent and backend point to product-catalog → 2 entries (deduping is
	// the index's job via claimedBackends).
	require.Len(t, got, 2)
	assert.Equal(t, "otel-demo", got[0].Namespace)
	assert.Equal(t, "product-catalog", got[0].Name)
}

// ---- end-to-end enrichment: GAMMA gRPC span resolves via backendref_fallback ----

// Reproduces the demo scenario in the ticket: a span carrying server.address
// (or net.peer.name) for the destination Service must resolve to the GRPCRoute
// and stamp k8s.grpcroute.* + route-mode=mesh + k8s.service.parent.*.
func TestEnrichment_GAMMA_GRPCRoute_ResolvesViaServerAddress(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("otel-demo", "product-catalog", RouteAttributes{
		Kind:                   RouteKindGRPCRoute,
		Name:                   "frontend-to-product-catalog",
		Namespace:              "otel-demo",
		UID:                    "gr-uid",
		RouteMode:              RouteModeMesh,
		ParentServiceName:      "product-catalog",
		ParentServiceNamespace: "otel-demo",
	})
	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"server.address": "product-catalog.otel-demo.svc.cluster.local"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])

	// Identity: GRPCRoute name/namespace/uid (the issue's load-bearing claim).
	got, ok := attrs.Get(AttrGRPCRouteName)
	require.True(t, ok)
	assert.Equal(t, "frontend-to-product-catalog", got.AsString())
	got, ok = attrs.Get(AttrGRPCRouteNamespace)
	require.True(t, ok)
	assert.Equal(t, "otel-demo", got.AsString())
	got, ok = attrs.Get(AttrGRPCRouteUID)
	require.True(t, ok)
	assert.Equal(t, "gr-uid", got.AsString())

	// Mode discriminator.
	got, ok = attrs.Get(AttrRouteMode)
	require.True(t, ok)
	assert.Equal(t, RouteModeMesh, got.AsString())

	// Parent Service identity.
	got, ok = attrs.Get(AttrParentServiceName)
	require.True(t, ok)
	assert.Equal(t, "product-catalog", got.AsString())
	got, ok = attrs.Get(AttrParentServiceNamespace)
	require.True(t, ok)
	assert.Equal(t, "otel-demo", got.AsString())

	// Must NOT stamp Gateway attribution on a mesh-mode route.
	_, ok = attrs.Get(AttrGatewayName)
	assert.False(t, ok, "mesh-mode route must not carry k8s.gateway.name")

	// Must NOT stamp HTTPRoute attributes when matched route is GRPCRoute.
	_, ok = attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok)
}

// Legacy net.peer.name (pre-1.20 sem-conv) must also resolve when modern
// server.address isn't present. The ticket's demo apps emit net.peer.name —
// without this fallback, GAMMA spans miss enrichment entirely.
func TestEnrichment_GAMMA_LegacyNetPeerName_Resolves(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("otel-demo", "product-catalog", RouteAttributes{
		Kind:                   RouteKindGRPCRoute,
		Name:                   "frontend-to-product-catalog",
		Namespace:              "otel-demo",
		RouteMode:              RouteModeMesh,
		ParentServiceName:      "product-catalog",
		ParentServiceNamespace: "otel-demo",
	})
	tp := newTestProcessors(t, lookup, nil) // default fallback walks both attrs

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"net.peer.name": "product-catalog.otel-demo"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	got, ok := attrs.Get(AttrGRPCRouteName)
	require.True(t, ok, "fallback must walk net.peer.name when server.address absent")
	assert.Equal(t, "frontend-to-product-catalog", got.AsString())
}

// Ingress-mode (current behavior) must continue to produce k8s.gateway.* and
// route-mode=ingress — the talk's parity claim ("same CRD, ingress AND mesh").
func TestEnrichment_Ingress_StampsGatewayAndModeIngress(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "demo", "api", RouteAttributes{
		Kind:        RouteKindHTTPRoute,
		Name:        "api",
		Namespace:   "demo",
		UID:         "uid-api",
		RouteMode:   RouteModeIngress,
		GatewayName: "public",
	})
	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/demo/api/rule/0/match/0"}),
	))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	mode, ok := attrs.Get(AttrRouteMode)
	require.True(t, ok)
	assert.Equal(t, RouteModeIngress, mode.AsString())
	gw, ok := attrs.Get(AttrGatewayName)
	require.True(t, ok)
	assert.Equal(t, "public", gw.AsString())
}

// ---- multi-source-attribute fallback ordering ----

func TestBackendRefFallback_EffectiveSourceAttrs(t *testing.T) {
	// Plural beats singular; singular is appended deduped.
	b := BackendRefFallback{SourceAttributes: []string{"server.address", "net.peer.name"}, SourceAttribute: "server.address"}
	got := b.effectiveSourceAttrs()
	assert.Equal(t, []string{"server.address", "net.peer.name"}, got)

	// Only singular set → list of one.
	b = BackendRefFallback{SourceAttribute: "server.address"}
	assert.Equal(t, []string{"server.address"}, b.effectiveSourceAttrs())

	// Both empty → nil.
	b = BackendRefFallback{}
	assert.Nil(t, b.effectiveSourceAttrs())

	// Singular not in plural list → appended at end.
	b = BackendRefFallback{SourceAttributes: []string{"server.address"}, SourceAttribute: "destination.address"}
	assert.Equal(t, []string{"server.address", "destination.address"}, b.effectiveSourceAttrs())
}

// formatParentRef must be consistent with isServiceParent: a parentRef with
// Kind=Service and nil Group classifies as GAMMA (mesh) upstream, so the
// parent_ref string must use the core API group ("") — NOT
// "gateway.networking.k8s.io/Service/...". Regression guard for PR#37 review.
func TestFormatParentRef_NilGroupServiceKind_UsesCoreGroup(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	ref := gwv1.ParentReference{Kind: &svcKind, Name: "product-catalog"}
	// Sanity: this is the same input isServiceParent classifies as mesh.
	require.True(t, isServiceParent(ref))
	assert.Equal(t,
		"/Service/otel-demo/product-catalog",
		formatParentRef(ref, "otel-demo"),
		"nil Group + Kind=Service must format with core group, matching isServiceParent",
	)

	// Explicit-empty-Group form must produce the same string.
	emptyGroup := gwv1.Group("")
	ref2 := gwv1.ParentReference{Group: &emptyGroup, Kind: &svcKind, Name: "product-catalog"}
	assert.Equal(t, formatParentRef(ref, "otel-demo"), formatParentRef(ref2, "otel-demo"))
}

// upsert{HTTP,GRPC}Route on Update must release backend keys the route no
// longer claims, otherwise stale fallback attribution lingers when a route's
// backendRefs change. Regression guard for PR#37 review.
func TestRouteIndex_UpsertHTTPRoute_ReleasesStaleBackendsOnUpdate(t *testing.T) {
	idx := newRouteIndex()
	ra := RouteAttributes{Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo"}
	idx.upsertHTTPRoute(ra, []backendRef{{Namespace: "demo", Name: "api-v1"}})
	got, ok := idx.LookupByBackendService("demo", "api-v1")
	require.True(t, ok)
	assert.Equal(t, "api", got.Name)

	// Route now points at a different backend Service.
	idx.upsertHTTPRoute(ra, []backendRef{{Namespace: "demo", Name: "api-v2"}})

	_, ok = idx.LookupByBackendService("demo", "api-v1")
	assert.False(t, ok, "stale backend claim from prior upsert must be released")
	got, ok = idx.LookupByBackendService("demo", "api-v2")
	require.True(t, ok)
	assert.Equal(t, "api", got.Name)
}

func TestRouteIndex_UpsertGRPCRoute_ReleasesStaleBackendsOnUpdate(t *testing.T) {
	idx := newRouteIndex()
	ra := RouteAttributes{Kind: RouteKindGRPCRoute, Name: "fe2pc", Namespace: "otel-demo"}
	idx.upsertGRPCRoute(ra, []backendRef{{Namespace: "otel-demo", Name: "product-catalog"}})

	// GAMMA parent Service swapped to a different Service.
	idx.upsertGRPCRoute(ra, []backendRef{{Namespace: "otel-demo", Name: "ad-service"}})

	_, ok := idx.LookupByBackendService("otel-demo", "product-catalog")
	assert.False(t, ok, "stale GAMMA parent Service claim must be released on Update")
	got, ok := idx.LookupByBackendService("otel-demo", "ad-service")
	require.True(t, ok)
	assert.Equal(t, "fe2pc", got.Name)
}

// Metric strip list must include k8s.grpcroute.uid — same cardinality risk
// as the http variant.
func TestMetricStrip_GRPCRouteUID(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("otel-demo", "product-catalog", RouteAttributes{
		Kind:      RouteKindGRPCRoute,
		Name:      "fe2pc",
		Namespace: "otel-demo",
		UID:       "gr-uid",
		RouteMode: RouteModeMesh,
	})
	tp := newTestProcessors(t, lookup, nil)

	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(),
		singleSumMetricWith(map[string]string{"server.address": "product-catalog.otel-demo"}),
	))
	dp := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)
	_, present := dp.Attributes().Get(AttrGRPCRouteUID)
	assert.False(t, present, "k8s.grpcroute.uid must be stripped on metrics (cardinality)")
	// Name + route-mode survive.
	name, ok := dp.Attributes().Get(AttrGRPCRouteName)
	require.True(t, ok)
	assert.Equal(t, "fe2pc", name.AsString())
	mode, ok := dp.Attributes().Get(AttrRouteMode)
	require.True(t, ok)
	assert.Equal(t, RouteModeMesh, mode.AsString())
}
