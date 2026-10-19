package gatewayapiprocessor

// GAMMA mesh enrichment coverage (ISI-6178, workstream B.2) — the six
// enhancements approved in the ISI-6175 design doc (decisions D1–D6):
//   §4.1 first-class parent-Service identity (parent.kind + parent.port)
//   §4.2 mesh/ingress direction from waypoint gatewayClassName
//   §4.3 multi-impl waypoint recognition (class-based + use-waypoint label)
//   §4.4 producer/consumer route role
//   §4.5 named-rule correlation (HTTPRouteRule.Name → k8s.httproute.rule.name)
//   §4.6 GRPCRoute parity
//   D6   high-cardinality mesh keys stripped from metrics by default

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ---- §4.1 parent.kind + parent.port (projection) ----

func TestHTTPRouteToAttrs_ParentKindAndPort_ServiceParent(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	port := gwv1.PortNumber(8080)
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "fe", Namespace: "otel-demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &svcKind, Name: "product-catalog", Port: &port}},
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Equal(t, "Service", ra.ParentKind)
	assert.Equal(t, int32(8080), ra.ParentServicePort)
	assert.Equal(t, RouteModeMesh, ra.RouteMode)
}

func TestHTTPRouteToAttrs_ParentKind_DefaultsGateway(t *testing.T) {
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Name: "public"}}, // nil Kind
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Equal(t, "Gateway", ra.ParentKind, "nil parentRef.kind defaults to Gateway")
	assert.Zero(t, ra.ParentServicePort, "no port for Gateway parent")
}

func TestHTTPRouteToAttrs_NoParentRefs_NoParentKind(t *testing.T) {
	hr := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"}}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Empty(t, ra.ParentKind, "a parentless route stamps no parent.kind")
}

func TestHTTPRouteToAttrs_ServiceParent_NoPort_ZeroPort(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "fe", Namespace: "otel-demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &svcKind, Name: "product-catalog"}},
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Zero(t, ra.ParentServicePort, "omitted parentRef.port projects as 0 (not stamped)")
}

// ---- §4.4 producer/consumer role (projection) ----

func TestRole_Producer_SameNamespace(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "fe", Namespace: "otel-demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &svcKind, Name: "product-catalog"}},
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Equal(t, RouteRoleProducer, ra.RouteRole, "route in parent Service's namespace is a producer route")
}

func TestRole_Consumer_DifferentNamespace(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	parentNS := gwv1.Namespace("otel-demo")
	gr := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "client-override", Namespace: "team-a"},
		Spec: gwv1.GRPCRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &svcKind, Namespace: &parentNS, Name: "product-catalog"}},
			},
		},
	}
	ra := grpcRouteToAttrs(gr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Equal(t, RouteRoleConsumer, ra.RouteRole, "route outside parent Service's namespace is a consumer route")
}

func TestRole_Disabled_NoRole(t *testing.T) {
	svcKind := gwv1.Kind("Service")
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "fe", Namespace: "otel-demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Kind: &svcKind, Name: "product-catalog"}},
			},
		},
	}
	cfg := defaultMeshCfg()
	off := false
	cfg.Mesh.ClassifyRouteRole = &off
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), cfg)
	assert.Empty(t, ra.RouteRole, "classify_route_role=false suppresses role")
}

func TestRole_IngressRoute_NoRole(t *testing.T) {
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "public"}}},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg())
	assert.Empty(t, ra.RouteRole, "ingress routes carry no producer/consumer role")
}

// ---- §4.2 + §4.3 class-based waypoint detection (projection) ----

func TestWaypointClass_FlipsToMeshAndStampsImpl(t *testing.T) {
	cases := []struct {
		class    string
		wantImpl string
	}{
		{"istio-waypoint", WaypointImplIstioAmbient},
		{"kgateway-waypoint", WaypointImplKgateway},
		{"istio-eastwest", WaypointImplIstioEastWest},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			gwStore := newGatewayStore()
			gwStore.upsert(&gwv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "mesh-gw", Namespace: "istio-system", UID: "gw-uid"},
				Spec:       gwv1.GatewaySpec{GatewayClassName: gwv1.ObjectName(tc.class)},
			})
			hr := &gwv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "istio-system"},
				Spec: gwv1.HTTPRouteSpec{
					CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "mesh-gw"}}},
				},
			}
			ra := httpRouteToAttrs(hr, gwStore, newGatewayClassStore(), defaultMeshCfg())
			assert.Equal(t, RouteModeMesh, ra.RouteMode, "a waypoint-class Gateway makes the route mesh")
			assert.Equal(t, tc.wantImpl, ra.WaypointImplementation)
			assert.Equal(t, "mesh-gw", ra.WaypointName)
			assert.Equal(t, "istio-system", ra.WaypointNamespace)
		})
	}
}

func TestWaypointClass_NonWaypointGatewayStaysIngress(t *testing.T) {
	gwStore := newGatewayStore()
	gwStore.upsert(&gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "demo"},
		Spec:       gwv1.GatewaySpec{GatewayClassName: "istio"},
	})
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "public"}}},
		},
	}
	ra := httpRouteToAttrs(hr, gwStore, newGatewayClassStore(), defaultMeshCfg())
	assert.Equal(t, RouteModeIngress, ra.RouteMode)
	assert.Empty(t, ra.WaypointImplementation, "a non-waypoint class carries no waypoint attribution")
}

func TestWaypointImplForClass_Mapping(t *testing.T) {
	m := MeshConfig{WaypointClassNames: []string{"istio-waypoint", "kgateway-waypoint", "istio-eastwest"}}
	assert.Equal(t, WaypointImplIstioAmbient, m.waypointImplForClass("istio-waypoint"))
	assert.Equal(t, WaypointImplKgateway, m.waypointImplForClass("kgateway-waypoint"))
	assert.Equal(t, WaypointImplIstioEastWest, m.waypointImplForClass("istio-eastwest"))
	assert.Empty(t, m.waypointImplForClass("istio"), "non-waypoint class returns empty")
	assert.Empty(t, m.waypointImplForClass(""), "empty class returns empty")
	// Not in the configured set → no match even if it embeds a vendor token.
	assert.Empty(t, m.waypointImplForClass("some-random-waypoint"))
}

// ---- §4.5 named-rule projection ----

func TestHTTPRuleNames_Projection(t *testing.T) {
	ruleA := gwv1.SectionName("checkout")
	hr := &gwv1.HTTPRoute{
		Spec: gwv1.HTTPRouteSpec{
			Rules: []gwv1.HTTPRouteRule{
				{Name: &ruleA},
				{}, // unnamed
			},
		},
	}
	names := httpRuleNames(hr)
	require.Len(t, names, 2)
	assert.Equal(t, "checkout", names[0])
	assert.Empty(t, names[1], "unnamed rule yields empty slot")
}

func TestHTTPRuleNames_AllUnnamed_ReturnsNil(t *testing.T) {
	hr := &gwv1.HTTPRoute{Spec: gwv1.HTTPRouteSpec{Rules: []gwv1.HTTPRouteRule{{}, {}}}}
	assert.Nil(t, httpRuleNames(hr), "no named rule → nil slice (no allocation)")
}

func TestGRPCRuleNames_Projection(t *testing.T) {
	ruleB := gwv1.SectionName("stream")
	gr := &gwv1.GRPCRoute{
		Spec: gwv1.GRPCRouteSpec{
			Rules: []gwv1.GRPCRouteRule{{}, {Name: &ruleB}},
		},
	}
	names := grpcRuleNames(gr)
	require.Len(t, names, 2)
	assert.Empty(t, names[0])
	assert.Equal(t, "stream", names[1])
}

// ---- end-to-end enrichment (stamp) ----

func TestEnrichment_MeshParentKindPortRole_Stamped(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "fe", RouteAttributes{
		Kind:                   RouteKindHTTPRoute,
		Name:                   "fe",
		Namespace:              "otel-demo",
		RouteMode:              RouteModeMesh,
		ParentKind:             "Service",
		ParentServiceName:      "product-catalog",
		ParentServiceNamespace: "otel-demo",
		ParentServicePort:      8080,
		RouteRole:              RouteRoleProducer,
	})
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/otel-demo/fe"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])

	kind, ok := attrs.Get(AttrHTTPRouteParentKind)
	require.True(t, ok)
	assert.Equal(t, "Service", kind.AsString())

	port, ok := attrs.Get(AttrParentServicePort)
	require.True(t, ok)
	assert.Equal(t, int64(8080), port.Int())

	role, ok := attrs.Get(AttrRouteRole)
	require.True(t, ok)
	assert.Equal(t, RouteRoleProducer, role.AsString())
}

func TestEnrichment_WaypointLabelResolvedAtStampTime(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "fe", RouteAttributes{
		Kind:                   RouteKindHTTPRoute,
		Name:                   "fe",
		Namespace:              "otel-demo",
		RouteMode:              RouteModeMesh,
		ParentKind:             "Service",
		ParentServiceName:      "product-catalog",
		ParentServiceNamespace: "otel-demo",
		// No projection-time waypoint — resolved from the Service label cache.
	})
	lookup.putWaypoint("otel-demo", "product-catalog", WaypointImplIstioAmbient, "waypoint", "otel-demo")
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/otel-demo/fe"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])

	impl, ok := attrs.Get(AttrWaypointImplementation)
	require.True(t, ok, "label-based waypoint must resolve at stamp time")
	assert.Equal(t, WaypointImplIstioAmbient, impl.AsString())
	name, ok := attrs.Get(AttrWaypointName)
	require.True(t, ok)
	assert.Equal(t, "waypoint", name.AsString())
}

func TestEnrichment_ProjectionWaypointWinsOverLabel(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "demo", "api", RouteAttributes{
		Kind:                   RouteKindHTTPRoute,
		Name:                   "api",
		Namespace:              "demo",
		RouteMode:              RouteModeMesh,
		WaypointImplementation: WaypointImplKgateway,
		WaypointName:           "mesh-gw",
		WaypointNamespace:      "demo",
		ParentServiceName:      "api-svc",
		ParentServiceNamespace: "demo",
	})
	// A conflicting label entry must be ignored: class-based projection wins.
	lookup.putWaypoint("demo", "api-svc", WaypointImplIstioAmbient, "other", "demo")
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/demo/api"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	impl, ok := attrs.Get(AttrWaypointImplementation)
	require.True(t, ok)
	assert.Equal(t, WaypointImplKgateway, impl.AsString(), "projection-time waypoint takes precedence over the label cache")
}

func TestEnrichment_RuleName_ResolvedFromRuleIndex(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "otel-demo",
		RouteMode: RouteModeIngress,
		RuleNames: []string{"default", "checkout"},
	})
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/otel-demo/api/rule/1"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	name, ok := attrs.Get(AttrHTTPRouteRuleName)
	require.True(t, ok, "rule.name resolves from rule_index 1")
	assert.Equal(t, "checkout", name.AsString())
	// Positional rule_index is retained for back-compat.
	idx, ok := attrs.Get(AttrHTTPRouteRuleIndex)
	require.True(t, ok)
	assert.Equal(t, int64(1), idx.Int())
}

func TestEnrichment_RuleName_UnnamedRule_NotStamped(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "otel-demo",
		RuleNames: []string{"default", ""}, // rule 1 unnamed
	})
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/otel-demo/api/rule/1"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteRuleName)
	assert.False(t, ok, "unnamed rule must not stamp rule.name")
}

func TestEnrichment_RuleName_IndexOutOfRange_NotStamped(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "otel-demo",
		RuleNames: []string{"default"}, // only index 0 exists
	})
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.traces.ConsumeTraces(context.Background(),
		singleSpanWith(map[string]string{"route_name": "httproute/otel-demo/api/rule/5"}),
	))
	attrs := getSpanAttrs(t, tp.ts.AllTraces()[0])
	_, ok := attrs.Get(AttrHTTPRouteRuleName)
	assert.False(t, ok, "out-of-range rule index must not panic or stamp")
}

// ---- D6 metrics strip-list ----

func TestMetricStrip_MeshHighCardinalityKeys(t *testing.T) {
	lookup := newStaticLookup()
	lookup.putBackend("otel-demo", "product-catalog", RouteAttributes{
		Kind:                   RouteKindHTTPRoute,
		Name:                   "fe",
		Namespace:              "otel-demo",
		RouteMode:              RouteModeMesh,
		ParentKind:             "Service",
		ParentServiceName:      "product-catalog",
		ParentServiceNamespace: "otel-demo",
		ParentServicePort:      8080,
		RouteRole:              RouteRoleProducer,
		WaypointImplementation: WaypointImplIstioAmbient,
	})
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(),
		singleSumMetricWith(map[string]string{"server.address": "product-catalog.otel-demo"}),
	))
	dp := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)
	ma := dp.Attributes()

	// Stripped by default (D6): high per-series cardinality.
	_, ok := ma.Get(AttrParentServicePort)
	assert.False(t, ok, "k8s.service.parent.port must be stripped on metrics")

	// Kept on metrics: bounded by cluster topology.
	_, ok = ma.Get(AttrHTTPRouteParentKind)
	assert.True(t, ok, "parent.kind survives on metrics")
	_, ok = ma.Get(AttrParentServiceName)
	assert.True(t, ok, "parent Service name survives on metrics")
	_, ok = ma.Get(AttrRouteRole)
	assert.True(t, ok, "route.role survives on metrics")
	_, ok = ma.Get(AttrWaypointImplementation)
	assert.True(t, ok, "waypoint.implementation survives on metrics")
}

func TestMetricStrip_RuleNameStripped(t *testing.T) {
	lookup := newStaticLookup()
	lookup.put(RouteKindHTTPRoute, "otel-demo", "api", RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "otel-demo",
		RuleNames: []string{"default", "checkout"},
	})
	tp := newTestProcessors(t, lookup, nil)
	require.NoError(t, tp.metrics.ConsumeMetrics(context.Background(),
		singleSumMetricWith(map[string]string{"route_name": "httproute/otel-demo/api/rule/1"}),
	))
	dp := tp.ms.AllMetrics()[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)
	_, ok := dp.Attributes().Get(AttrHTTPRouteRuleName)
	assert.False(t, ok, "k8s.httproute.rule.name must be stripped on metrics (per-rule cardinality)")
}

// ---- config defaults ----

func TestDefaultConfig_MeshDefaults(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	assert.Equal(t, []string{"istio-waypoint", "kgateway-waypoint", "istio-eastwest"}, cfg.Mesh.WaypointClassNames)
	assert.True(t, cfg.Mesh.ClassifyRouteRoleEnabled(), "role classification defaults on")
	assert.True(t, cfg.Mesh.InferServiceParentEnabled(), "nil-Kind inference flag defaults on")
	assert.True(t, cfg.Mesh.WaypointEnabled())
	assert.Contains(t, cfg.Enrich.ExcludeFromMetricAttributes, AttrParentServicePort)
	assert.Contains(t, cfg.Enrich.ExcludeFromMetricAttributes, AttrHTTPRouteRuleName)
	assert.Contains(t, cfg.Enrich.ExcludeFromMetricAttributes, AttrGRPCRouteRuleName)
	require.NoError(t, cfg.Validate())
}

func TestValidate_RejectsEmptyWaypointClassName(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.Mesh.WaypointClassNames = []string{"istio-waypoint", "  "}
	assert.Error(t, cfg.Validate(), "blank waypoint_class_names entry must be rejected")
}

// ---- D5 waypointIndex (Service use-waypoint label cache) ----

func TestWaypointIndex_UseWaypointLabel(t *testing.T) {
	idx := newWaypointIndex()
	idx.upsertService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "product-catalog",
			Namespace: "otel-demo",
			Labels:    map[string]string{labelUseWaypoint: "waypoint"},
		},
	})
	impl, name, ns, ok := idx.LookupWaypointForService("otel-demo", "product-catalog")
	require.True(t, ok)
	assert.Equal(t, WaypointImplIstioAmbient, impl)
	assert.Equal(t, "waypoint", name)
	assert.Equal(t, "otel-demo", ns, "waypoint namespace defaults to the Service's namespace")
}

func TestWaypointIndex_CrossNamespaceWaypoint(t *testing.T) {
	idx := newWaypointIndex()
	idx.upsertService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc",
			Namespace: "team-a",
			Labels: map[string]string{
				labelUseWaypoint:   "shared-wp",
				labelUseWaypointNS: "mesh-system",
			},
		},
	})
	_, name, ns, ok := idx.LookupWaypointForService("team-a", "svc")
	require.True(t, ok)
	assert.Equal(t, "shared-wp", name)
	assert.Equal(t, "mesh-system", ns, "use-waypoint-namespace overrides the Service namespace")
}

func TestWaypointIndex_NoneDisablesAndRemoves(t *testing.T) {
	idx := newWaypointIndex()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "demo", Labels: map[string]string{labelUseWaypoint: "wp"},
		},
	}
	idx.upsertService(svc)
	_, _, _, ok := idx.LookupWaypointForService("demo", "svc")
	require.True(t, ok)

	// Flipping the label to "none" must remove the entry (explicit opt-out).
	svc.Labels[labelUseWaypoint] = useWaypointDisabled
	idx.upsertService(svc)
	_, _, _, ok = idx.LookupWaypointForService("demo", "svc")
	assert.False(t, ok, `use-waypoint="none" removes the waypoint entry`)
}

func TestWaypointIndex_DeleteService(t *testing.T) {
	idx := newWaypointIndex()
	idx.upsertService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "demo", Labels: map[string]string{labelUseWaypoint: "wp"}},
	})
	idx.deleteService("demo", "svc")
	_, _, _, ok := idx.LookupWaypointForService("demo", "svc")
	assert.False(t, ok)
}

func TestWaypointIndex_UnlabeledService_NoEntry(t *testing.T) {
	idx := newWaypointIndex()
	idx.upsertService(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "demo"}})
	_, _, _, ok := idx.LookupWaypointForService("demo", "svc")
	assert.False(t, ok, "a Service with no use-waypoint label has no waypoint entry")
}

// combinedLookup with a nil waypoint index (waypoint attribution off) must not
// panic and must report ok=false.
func TestCombinedLookup_NilWaypointIndex(t *testing.T) {
	c := &combinedLookup{routes: newRouteIndex()}
	_, _, _, ok := c.LookupWaypointForService("demo", "svc")
	assert.False(t, ok)
}

// defaultMeshCfg returns a Config whose Mesh block mirrors createDefaultConfig
// so projection unit tests exercise the shipped defaults (waypoint classes +
// role classification on) rather than a zero Config.
func defaultMeshCfg() *Config {
	return &Config{
		Mesh: MeshConfig{
			WaypointClassNames: []string{"istio-waypoint", "kgateway-waypoint", "istio-eastwest"},
		},
	}
}
