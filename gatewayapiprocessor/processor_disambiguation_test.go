package gatewayapiprocessor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

// ISI-805 — parentRef-aware disambiguation when multiple HTTPRoutes/GRPCRoutes
// share a backend Service.
//
// Test matrix (per the plan at /ISI/issues/ISI-805#document-plan):
//
//	┌──────────────────────────────────────────────────────────┬────────────────────┐
//	│ scenario                                                 │ expectation        │
//	├──────────────────────────────────────────────────────────┼────────────────────┤
//	│ 1 backend, mesh+ingress, span ns matches mesh parent ns  │ mesh stamped       │
//	│ 1 backend, mesh+ingress, span ns differs from mesh ns    │ ingress stamped    │
//	│ 1 backend, mesh+ingress, span has no resource ns         │ ingress stamped    │
//	│ 1 backend, mesh+mesh                                     │ no stamp           │
//	│ 1 backend, ingress+ingress (legacy AmbiguousOwner case)  │ no stamp           │
//	│ 1 backend, mesh+ingress+mesh (3 candidates)              │ no stamp           │
//	└──────────────────────────────────────────────────────────┴────────────────────┘
//
// All cases drive end-to-end through ConsumeTraces so the disambiguator and
// the stamping path are exercised together.

const (
	frontendProxy = "frontend-proxy"
	otelDemo      = "otel-demo"
)

// disambigMeshRoute returns the GAMMA mesh-mode HTTPRoute candidate used by
// the disambiguation matrix. parent kind = Service, RouteMode = mesh,
// ParentService* points at the dual-mode Service.
func disambigMeshRoute() RouteAttributes {
	return RouteAttributes{
		Kind:                   RouteKindHTTPRoute,
		Name:                   "frontend-proxy-mesh",
		Namespace:              otelDemo,
		UID:                    "uid-mesh",
		RouteMode:              RouteModeMesh,
		ParentServiceName:      frontendProxy,
		ParentServiceNamespace: otelDemo,
	}
}

// disambigIngressRoute returns the ingress-mode HTTPRoute candidate. parent
// kind = Gateway, RouteMode = ingress, GatewayName populated so the stamping
// path stays end-to-end visible.
func disambigIngressRoute() RouteAttributes {
	return RouteAttributes{
		Kind:        RouteKindHTTPRoute,
		Name:        "oteldemo-ingress-route",
		Namespace:   otelDemo,
		UID:         "uid-ingress",
		RouteMode:   RouteModeIngress,
		GatewayName: "oteldemo-ingress",
	}
}

// dualModeFallbackProcessors wires both candidates against the shared backend
// Service in a real routeIndex, then returns the test processor triple.
func dualModeFallbackProcessors(t *testing.T, candidates ...RouteAttributes) *testProcessors {
	t.Helper()
	idx := newRouteIndex()
	for _, ra := range candidates {
		switch ra.Kind {
		case RouteKindGRPCRoute:
			idx.upsertGRPCRoute(ra, []backendRef{{Namespace: otelDemo, Name: frontendProxy}})
		default:
			idx.upsertHTTPRoute(ra, []backendRef{{Namespace: otelDemo, Name: frontendProxy}})
		}
	}
	// Sanity: with 2+ candidates, the legacy single-candidate index must have
	// dropped its entry — otherwise the disambiguator would never run.
	if len(candidates) >= 2 {
		_, ok := idx.LookupByBackendService(otelDemo, frontendProxy)
		require.False(t, ok, "single-candidate index must be ambiguous-dropped before disambiguator runs")
		got, ok := idx.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
		require.True(t, ok)
		require.Len(t, got, len(candidates), "all candidates must survive in backendOwners")
	}
	return newTestProcessors(t, idx, func(c *Config) {
		c.BackendRefFallback = BackendRefFallback{Enabled: true, SourceAttribute: "server.address"}
	})
}

// spanWithResourceNS produces a span carrying server.address pointing at the
// dual-mode Service plus a resource k8s.namespace.name attribute (or none).
func spanWithResourceNS(resourceNS string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	if resourceNS != "" {
		rs.Resource().Attributes().PutStr("k8s.namespace.name", resourceNS)
	}
	ss := rs.ScopeSpans().AppendEmpty()
	sp := ss.Spans().AppendEmpty()
	sp.Attributes().PutStr("server.address", frontendProxy+"."+otelDemo+".svc.cluster.local")
	return td
}

// Case 1 — mesh + ingress, span resource ns == mesh parent Service ns → mesh.
func TestBackendRefFallback_Disambiguate_MeshWhenSpanInsideMesh(t *testing.T) {
	tp := dualModeFallbackProcessors(t, disambigMeshRoute(), disambigIngressRoute())

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), spanWithResourceNS(otelDemo)))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	mode, ok := attrs.Get(AttrRouteMode)
	require.True(t, ok)
	assert.Equal(t, RouteModeMesh, mode.AsString(), "span inside mesh ns → mesh route stamped")
	name, ok := attrs.Get(AttrHTTPRouteName)
	require.True(t, ok)
	assert.Equal(t, "frontend-proxy-mesh", name.AsString())
	parser, ok := attrs.Get(AttrParser)
	require.True(t, ok)
	assert.Equal(t, "backendref_fallback", parser.AsString())
	parentSvc, ok := attrs.Get(AttrParentServiceName)
	require.True(t, ok)
	assert.Equal(t, frontendProxy, parentSvc.AsString())
}

// Case 2 — mesh + ingress, span resource ns differs from mesh parent ns → ingress.
func TestBackendRefFallback_Disambiguate_IngressWhenSpanOutsideMesh(t *testing.T) {
	tp := dualModeFallbackProcessors(t, disambigMeshRoute(), disambigIngressRoute())

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), spanWithResourceNS("ingress-system")))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	mode, ok := attrs.Get(AttrRouteMode)
	require.True(t, ok)
	assert.Equal(t, RouteModeIngress, mode.AsString())
	name, ok := attrs.Get(AttrHTTPRouteName)
	require.True(t, ok)
	assert.Equal(t, "oteldemo-ingress-route", name.AsString())
	gw, ok := attrs.Get(AttrGatewayName)
	require.True(t, ok)
	assert.Equal(t, "oteldemo-ingress", gw.AsString())
	// Mesh-only attributes must NOT leak onto the ingress-stamped span.
	_, ok = attrs.Get(AttrParentServiceName)
	assert.False(t, ok, "ingress-stamped span must not carry k8s.service.parent.name")
}

// Case 3 — mesh + ingress, no span resource ns → ingress (default).
func TestBackendRefFallback_Disambiguate_DefaultsToIngressWhenNoResourceNS(t *testing.T) {
	tp := dualModeFallbackProcessors(t, disambigMeshRoute(), disambigIngressRoute())

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), spanWithResourceNS("")))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	mode, ok := attrs.Get(AttrRouteMode)
	require.True(t, ok)
	assert.Equal(t, RouteModeIngress, mode.AsString(),
		"missing k8s.namespace.name resource ns must default to ingress (the safe fallback for cross-cluster traffic)")
}

// Case 4 — both candidates mesh: within-kind ambiguity, no stamp.
func TestBackendRefFallback_Disambiguate_BothMesh_NoStamp(t *testing.T) {
	meshA := disambigMeshRoute()
	meshA.Name = "mesh-a"
	meshB := disambigMeshRoute()
	meshB.Name = "mesh-b"

	tp := dualModeFallbackProcessors(t, meshA, meshB)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), spanWithResourceNS(otelDemo)))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok, "mesh+mesh ambiguity must not stamp (preserve safety contract)")
	_, ok = attrs.Get(AttrParser)
	assert.False(t, ok)
}

// Case 5 — both candidates ingress: legacy AmbiguousOwner contract preserved.
func TestBackendRefFallback_Disambiguate_BothIngress_NoStamp(t *testing.T) {
	ingA := disambigIngressRoute()
	ingA.Name = "ingress-a"
	ingB := disambigIngressRoute()
	ingB.Name = "ingress-b"

	tp := dualModeFallbackProcessors(t, ingA, ingB)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), spanWithResourceNS(otelDemo)))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok, "ingress+ingress ambiguity must not stamp — matches TestBackendRefFallback_AmbiguousOwner_NoStamp")
}

// Case 6 — three candidates (mesh + ingress + mesh): existing >2-owner safety
// kicks in, no stamp.
func TestBackendRefFallback_Disambiguate_ThreeCandidates_NoStamp(t *testing.T) {
	meshA := disambigMeshRoute()
	meshA.Name = "mesh-a"
	meshB := disambigMeshRoute()
	meshB.Name = "mesh-b"
	ing := disambigIngressRoute()

	tp := dualModeFallbackProcessors(t, meshA, ing, meshB)

	require.NoError(t, tp.traces.ConsumeTraces(context.Background(), spanWithResourceNS(otelDemo)))

	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteName)
	assert.False(t, ok, ">2 candidates must preserve the existing no-stamp safety semantics")
}

// Index-level test: LookupByBackendServiceWithParents must surface every owner
// even when the single-candidate index has dropped its entry — and must drop
// its entries cleanly when the route is deleted.
func TestRouteIndex_LookupByBackendServiceWithParents(t *testing.T) {
	idx := newRouteIndex()

	idx.upsertHTTPRoute(disambigMeshRoute(),
		[]backendRef{{Namespace: otelDemo, Name: frontendProxy}})
	idx.upsertHTTPRoute(disambigIngressRoute(),
		[]backendRef{{Namespace: otelDemo, Name: frontendProxy}})

	// Single-candidate fast path is dropped.
	_, ok := idx.LookupByBackendService(otelDemo, frontendProxy)
	assert.False(t, ok)

	// With-parents lookup returns both.
	got, ok := idx.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	require.True(t, ok)
	require.Len(t, got, 2)
	names := []string{got[0].Name, got[1].Name}
	assert.Contains(t, names, "frontend-proxy-mesh")
	assert.Contains(t, names, "oteldemo-ingress-route")

	// Deleting the mesh route removes it from the multi-owner set.
	idx.deleteHTTPRoute(otelDemo, "frontend-proxy-mesh")
	got, ok = idx.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	require.True(t, ok)
	require.Len(t, got, 1)
	assert.Equal(t, "oteldemo-ingress-route", got[0].Name)

	// Deleting the last route empties the entry.
	idx.deleteHTTPRoute(otelDemo, "oteldemo-ingress-route")
	_, ok = idx.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	assert.False(t, ok)
}

// combinedLookup is the production-wired RouteLookup. It must delegate the
// new with-parents method to the underlying routeIndex.
func TestCombinedLookup_DelegatesWithParents(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(disambigMeshRoute(),
		[]backendRef{{Namespace: otelDemo, Name: frontendProxy}})
	idx.upsertHTTPRoute(disambigIngressRoute(),
		[]backendRef{{Namespace: otelDemo, Name: frontendProxy}})

	cl := &combinedLookup{routes: idx, ips: nil}
	got, ok := cl.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	require.True(t, ok)
	require.Len(t, got, 2)
	// Single-candidate path returns nothing in the ambiguous case.
	_, ok = cl.LookupByBackendService(otelDemo, frontendProxy)
	assert.False(t, ok)
	// LookupRoute still resolves both routes individually.
	_, ok = cl.LookupRoute(RouteKindHTTPRoute, otelDemo, "frontend-proxy-mesh")
	assert.True(t, ok)
}

// staticLookup exposes the same multi-owner contract for tests that don't
// spin up a full routeIndex (e.g. fixtures driving the disambiguator from
// outside this package).
func TestStaticLookup_LookupByBackendServiceWithParents(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend(otelDemo, frontendProxy, disambigMeshRoute())
	lookup.putBackend(otelDemo, frontendProxy, disambigIngressRoute())

	// Single-candidate index drops on second putBackend (matches real index).
	_, ok := lookup.LookupByBackendService(otelDemo, frontendProxy)
	assert.False(t, ok)

	// With-parents lookup keeps both.
	got, ok := lookup.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	require.True(t, ok)
	require.Len(t, got, 2)

	// Unknown backend returns (nil, false).
	_, ok = lookup.LookupByBackendServiceWithParents("missing", "nope")
	assert.False(t, ok)
}

// Update path: when a route's backendRefs change to no longer claim a Service,
// the multi-owner set must release the stale entry.
func TestRouteIndex_LookupByBackendServiceWithParents_ReleasesStaleOnUpdate(t *testing.T) {
	idx := newRouteIndex()
	mesh := disambigMeshRoute()

	idx.upsertHTTPRoute(mesh, []backendRef{{Namespace: otelDemo, Name: frontendProxy}})
	got, ok := idx.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	require.True(t, ok)
	require.Len(t, got, 1)

	// Same route, different backendRef — old key must be released.
	idx.upsertHTTPRoute(mesh, []backendRef{{Namespace: otelDemo, Name: "checkout"}})

	_, ok = idx.LookupByBackendServiceWithParents(otelDemo, frontendProxy)
	assert.False(t, ok, "stale multi-owner entry must be released when the owner stops claiming the backend")

	got, ok = idx.LookupByBackendServiceWithParents(otelDemo, "checkout")
	require.True(t, ok)
	require.Len(t, got, 1)
}
