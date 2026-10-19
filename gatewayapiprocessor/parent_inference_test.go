package gatewayapiprocessor

// Tests for the nil-Kind parent-Service inference (ISI-6188 / design D4).
// Done-when coverage:
//   - nil-Kind Service parent inferred post-sync            → TestInferServiceParent_HTTPRoute/inferred_*
//   - NOT inferred during sync                              → .../not_inferred_during_sync
//   - counter increments                                    → TestInferServiceParent_CounterWiring
//   - dangling-Gateway parentRef not falsely meshed         → .../explicit_gateway_* , .../nil_kind_no_service
//   - GRPCRoute parity                                      → TestInferServiceParent_GRPCRoute

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func gwKindPtr(s string) *gwv1.Kind   { k := gwv1.Kind(s); return &k }
func gwGroupPtr(s string) *gwv1.Group { g := gwv1.Group(s); return &g }

// fakeInferrer builds a parentInferrer whose predicate halves are fixed and
// whose record() just counts, so the projection logic can be exercised without
// informers or a real meter. counted is the pointer the record closure bumps.
func fakeInferrer(synced bool, services map[string]struct{}, counted *int) *parentInferrer {
	return &parentInferrer{
		gatewaySynced: func() bool { return synced },
		serviceExists: func(ns, name string) bool {
			_, ok := services[ns+"/"+name]
			return ok
		},
		record: func(ns, name string, kind RouteKind) { *counted++ },
	}
}

func httpRouteWithParent(ns, name string, pr gwv1.ParentReference) *gwv1.HTTPRoute {
	return &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{pr}},
		},
	}
}

func grpcRouteWithParent(ns, name string, pr gwv1.ParentReference) *gwv1.GRPCRoute {
	return &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gwv1.GRPCRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{pr}},
		},
	}
}

func TestInferServiceParent_HTTPRoute(t *testing.T) {
	port := gwv1.PortNumber(8080)

	tests := []struct {
		name string
		// parentRef shape
		pr gwv1.ParentReference
		// a Gateway with this ns/name is seeded into gwStore when gatewayName != "".
		gatewayName string
		// inference wiring
		inferrerEnabled bool
		gatewaySynced   bool
		serviceExists   bool
		// expectations
		wantMode      string
		wantParentSvc string
		wantParentKnd string
		wantInferred  bool // record() called exactly once
	}{
		{
			name:            "inferred_post_sync",
			pr:              gwv1.ParentReference{Name: "product-catalog"}, // nil Group, nil Kind
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   true,
			wantMode:        RouteModeMesh,
			wantParentSvc:   "product-catalog",
			wantParentKnd:   "Service",
			wantInferred:    true,
		},
		{
			name:            "inferred_post_sync_with_port",
			pr:              gwv1.ParentReference{Name: "product-catalog", Port: &port},
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   true,
			wantMode:        RouteModeMesh,
			wantParentSvc:   "product-catalog",
			wantParentKnd:   "Service",
			wantInferred:    true,
		},
		{
			name:            "explicit_empty_kind_inferred",
			pr:              gwv1.ParentReference{Kind: gwKindPtr(""), Group: gwGroupPtr(""), Name: "product-catalog"},
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   true,
			wantMode:        RouteModeMesh,
			wantParentSvc:   "product-catalog",
			wantParentKnd:   "Service",
			wantInferred:    true,
		},
		{
			name:            "not_inferred_during_sync",
			pr:              gwv1.ParentReference{Name: "product-catalog"},
			inferrerEnabled: true,
			gatewaySynced:   false, // guardrail 1: never infer during initial sync
			serviceExists:   true,
			wantMode:        RouteModeIngress,
			wantParentSvc:   "",
			wantParentKnd:   "Gateway",
			wantInferred:    false,
		},
		{
			name:            "nil_kind_no_service",
			pr:              gwv1.ParentReference{Name: "product-catalog"},
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   false, // no Service with this ns/name → stays ingress
			wantMode:        RouteModeIngress,
			wantParentSvc:   "",
			wantParentKnd:   "Gateway",
			wantInferred:    false,
		},
		{
			name:            "explicit_gateway_dangling_with_service_present",
			pr:              gwv1.ParentReference{Kind: gwKindPtr("Gateway"), Name: "product-catalog"},
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   true, // even though a Service exists, an explicit Gateway ref must NOT be meshed
			wantMode:        RouteModeIngress,
			wantParentSvc:   "",
			wantParentKnd:   "Gateway",
			wantInferred:    false,
		},
		{
			name:            "non_core_group_not_inferred",
			pr:              gwv1.ParentReference{Group: gwGroupPtr("example.com"), Name: "product-catalog"},
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   true,
			wantMode:        RouteModeIngress,
			wantParentSvc:   "",
			wantParentKnd:   "Gateway",
			wantInferred:    false,
		},
		{
			name:            "nil_kind_but_gateway_exists",
			pr:              gwv1.ParentReference{Name: "product-catalog"},
			gatewayName:     "product-catalog", // a real Gateway with this ns/name → gateway attribution, no inference
			inferrerEnabled: true,
			gatewaySynced:   true,
			serviceExists:   true,
			wantMode:        RouteModeIngress,
			wantParentSvc:   "",
			wantParentKnd:   "Gateway",
			wantInferred:    false,
		},
		{
			name:            "inference_disabled_flag_off",
			pr:              gwv1.ParentReference{Name: "product-catalog"},
			inferrerEnabled: false, // mesh.infer_service_parent=false → inferrer is nil
			gatewaySynced:   true,
			serviceExists:   true,
			wantMode:        RouteModeIngress,
			wantParentSvc:   "",
			wantParentKnd:   "Gateway",
			wantInferred:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gwStore := newGatewayStore()
			if tc.gatewayName != "" {
				gwStore.upsert(&gwv1.Gateway{
					ObjectMeta: metav1.ObjectMeta{Name: tc.gatewayName, Namespace: "otel-demo", UID: "gw-uid"},
					Spec:       gwv1.GatewaySpec{GatewayClassName: "istio"},
				})
			}

			counted := 0
			var inferrer *parentInferrer
			if tc.inferrerEnabled {
				services := map[string]struct{}{}
				if tc.serviceExists {
					services["otel-demo/product-catalog"] = struct{}{}
				}
				inferrer = fakeInferrer(tc.gatewaySynced, services, &counted)
			}

			hr := httpRouteWithParent("otel-demo", "fe", tc.pr)
			ra := httpRouteToAttrs(hr, gwStore, newGatewayClassStore(), defaultMeshCfg(), inferrer)

			assert.Equal(t, tc.wantMode, ra.RouteMode, "route mode")
			assert.Equal(t, tc.wantParentSvc, ra.ParentServiceName, "parent service name")
			assert.Equal(t, tc.wantParentKnd, ra.ParentKind, "parent kind")
			if tc.wantInferred {
				assert.Equal(t, 1, counted, "record() should fire exactly once on inference")
				assert.Equal(t, "otel-demo", ra.ParentServiceNamespace, "parent service namespace")
			} else {
				assert.Equal(t, 0, counted, "record() must not fire")
			}
		})
	}
}

func TestInferServiceParent_HTTPRoute_PortStamped(t *testing.T) {
	port := gwv1.PortNumber(8080)
	counted := 0
	inferrer := fakeInferrer(true, map[string]struct{}{"otel-demo/product-catalog": {}}, &counted)
	hr := httpRouteWithParent("otel-demo", "fe", gwv1.ParentReference{Name: "product-catalog", Port: &port})
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg(), inferrer)
	assert.Equal(t, RouteModeMesh, ra.RouteMode)
	assert.Equal(t, int32(8080), ra.ParentServicePort, "inferred mesh parent carries the parentRef port like the explicit path")
}

func TestInferServiceParent_GRPCRoute(t *testing.T) {
	tests := []struct {
		name          string
		synced        bool
		serviceExists bool
		wantMode      string
		wantInferred  bool
	}{
		{"inferred_post_sync", true, true, RouteModeMesh, true},
		{"not_inferred_during_sync", false, true, RouteModeIngress, false},
		{"no_service_not_inferred", true, false, RouteModeIngress, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			counted := 0
			services := map[string]struct{}{}
			if tc.serviceExists {
				services["otel-demo/checkout"] = struct{}{}
			}
			inferrer := fakeInferrer(tc.synced, services, &counted)
			gr := grpcRouteWithParent("otel-demo", "grpc-fe", gwv1.ParentReference{Name: "checkout"})
			ra := grpcRouteToAttrs(gr, newGatewayStore(), newGatewayClassStore(), defaultMeshCfg(), inferrer)
			assert.Equal(t, tc.wantMode, ra.RouteMode)
			if tc.wantInferred {
				assert.Equal(t, RouteKindGRPCRoute, ra.Kind)
				assert.Equal(t, "checkout", ra.ParentServiceName)
				assert.Equal(t, "Service", ra.ParentKind)
				assert.Equal(t, 1, counted)
			} else {
				assert.Equal(t, 0, counted)
			}
		})
	}
}

func TestIsNilKindCoreParent(t *testing.T) {
	tests := []struct {
		name string
		pr   gwv1.ParentReference
		want bool
	}{
		{"nil_group_nil_kind", gwv1.ParentReference{Name: "x"}, true},
		{"empty_group_nil_kind", gwv1.ParentReference{Group: gwGroupPtr(""), Name: "x"}, true},
		{"empty_group_empty_kind", gwv1.ParentReference{Group: gwGroupPtr(""), Kind: gwKindPtr(""), Name: "x"}, true},
		{"explicit_gateway", gwv1.ParentReference{Kind: gwKindPtr("Gateway"), Name: "x"}, false},
		{"explicit_service", gwv1.ParentReference{Kind: gwKindPtr("Service"), Name: "x"}, false},
		{"non_core_group", gwv1.ParentReference{Group: gwGroupPtr("gateway.networking.k8s.io"), Name: "x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNilKindCoreParent(tc.pr))
		})
	}
}

func TestServiceNameIndex(t *testing.T) {
	var nilIdx *serviceNameIndex
	assert.False(t, nilIdx.Exists("ns", "svc"), "nil index is safe and reports absence")

	idx := newServiceNameIndex()
	assert.False(t, idx.Exists("otel-demo", "product-catalog"))

	idx.upsertService(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "product-catalog", Namespace: "otel-demo"}})
	assert.True(t, idx.Exists("otel-demo", "product-catalog"))
	assert.False(t, idx.Exists("otel-demo", "other"), "distinct name absent")
	assert.False(t, idx.Exists("other-ns", "product-catalog"), "namespace is part of the key")

	idx.upsertService(nil) // nil-safe no-op
	idx.deleteService("otel-demo", "product-catalog")
	assert.False(t, idx.Exists("otel-demo", "product-catalog"), "delete removes the entry")
}

func TestNewParentInferrer_DisabledReturnsNil(t *testing.T) {
	off := false
	cfg := &Config{Mesh: MeshConfig{InferServiceParent: &off}}
	inferrer := newParentInferrer(cfg, zap.NewNop(), nil, func() bool { return true }, func(ns, name string) bool { return true })
	assert.Nil(t, inferrer, "inference disabled → nil inferrer (projection path is nil-safe)")

	// Default (nil pointer) → enabled.
	cfgDefault := &Config{}
	assert.NotNil(t, newParentInferrer(cfgDefault, zap.NewNop(), nil, func() bool { return true }, func(ns, name string) bool { return true }))
}

// TestInferServiceParent_CounterWiring drives a real OTel SDK meter through the
// production newParentInferredCounter + newParentInferrer path and asserts that
// a reclassification increments gatewayapi_parent_inferred_total — and that a
// during-sync projection does NOT (guardrail 1).
func TestInferServiceParent_CounterWiring(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	counter := newParentInferredCounter(mp, zap.NewNop())
	require.NotNil(t, counter)

	synced := true
	cfg := &Config{} // InferServiceParent defaults to enabled
	inferrer := newParentInferrer(cfg, zap.NewNop(), counter,
		func() bool { return synced },
		func(ns, name string) bool { return ns == "otel-demo" && name == "product-catalog" },
	)
	require.NotNil(t, inferrer)

	// Post-sync, Service exists → inference fires.
	hr := httpRouteWithParent("otel-demo", "fe", gwv1.ParentReference{Name: "product-catalog"})
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), cfg, inferrer)
	require.Equal(t, RouteModeMesh, ra.RouteMode)

	// During sync → no inference, counter unchanged.
	synced = false
	hr2 := httpRouteWithParent("otel-demo", "fe2", gwv1.ParentReference{Name: "product-catalog"})
	ra2 := httpRouteToAttrs(hr2, newGatewayStore(), newGatewayClassStore(), cfg, inferrer)
	require.Equal(t, RouteModeIngress, ra2.RouteMode)

	assert.Equal(t, int64(1), collectCounter(t, reader, metricParentInferred),
		"counter should be 1 (one inference post-sync, none during sync)")
}

func collectCounter(t *testing.T, reader sdkmetric.Reader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "counter %s should be an int64 Sum", name)
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	t.Fatalf("counter %q not found in collected metrics", name)
	return 0
}
