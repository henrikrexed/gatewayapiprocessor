package gatewayapiprocessor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ---- httpRouteToAttrs / grpcRouteToAttrs: CR → RouteAttributes projection ----

func TestHTTPRouteToAttrs_WithGatewayAndClass(t *testing.T) {
	gwStore := newGatewayStore()
	gcStore := newGatewayClassStore()

	gwStore.upsert(&gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name: "public", Namespace: "infra", UID: types.UID("gw-uid"),
		},
		Spec: gwv1.GatewaySpec{GatewayClassName: "envoygwc"},
	})
	gcStore.upsert(&gwv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "envoygwc"},
		Spec:       gwv1.GatewayClassSpec{ControllerName: "gateway.envoyproxy.io/gatewayclass-controller"},
	})

	section := gwv1.SectionName("https")
	ns := gwv1.Namespace("infra")
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "demo", UID: types.UID("hr-uid"),
		},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{
					Name:        "public",
					Namespace:   &ns,
					SectionName: &section,
				}},
			},
		},
	}

	cfg := &Config{EmitStatusConds: false}
	ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg)

	assert.Equal(t, "api", ra.Name)
	assert.Equal(t, "demo", ra.Namespace)
	assert.Equal(t, "hr-uid", ra.UID)
	assert.Equal(t, "public", ra.GatewayName)
	assert.Equal(t, "infra", ra.GatewayNamespace)
	assert.Equal(t, "gw-uid", ra.GatewayUID)
	assert.Equal(t, "https", ra.GatewayListenerName)
	assert.Equal(t, "envoygwc", ra.GatewayClassName)
	assert.Equal(t, "gateway.envoyproxy.io/gatewayclass-controller", ra.GatewayClassControllerName)
	assert.Equal(t, "gateway.networking.k8s.io/Gateway/infra/public", ra.ParentRef)
}

// When the parentRef names a Gateway the store hasn't seen, the projection
// must still return the route identity (name/ns/uid) without populating
// Gateway fields. processor-spec §2.4: partial CRD observation is common.
func TestHTTPRouteToAttrs_UnknownGatewayParent_StillStampsRouteIdentity(t *testing.T) {
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo", UID: types.UID("hr")},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Name: "missing-gw"}},
			},
		},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), &Config{})
	assert.Equal(t, "api", ra.Name)
	assert.Empty(t, ra.GatewayName, "unknown parent must not populate gateway fields")
	assert.Equal(t, "gateway.networking.k8s.io/Gateway/demo/missing-gw", ra.ParentRef)
}

func TestHTTPRouteToAttrs_EmitStatusConditions_Stamps(t *testing.T) {
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Status: gwv1.HTTPRouteStatus{RouteStatus: gwv1.RouteStatus{
			Parents: []gwv1.RouteParentStatus{{
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionTrue},
					{Type: "ResolvedRefs", Status: metav1.ConditionFalse},
				},
			}},
		}},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), &Config{EmitStatusConds: true})
	require.NotNil(t, ra.Accepted)
	require.NotNil(t, ra.ResolvedRefs)
	assert.True(t, *ra.Accepted)
	assert.False(t, *ra.ResolvedRefs)
}

func TestHTTPRouteToAttrs_EmitStatusConditions_Off_NoStatus(t *testing.T) {
	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Status: gwv1.HTTPRouteStatus{RouteStatus: gwv1.RouteStatus{
			Parents: []gwv1.RouteParentStatus{{
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionTrue},
				},
			}},
		}},
	}
	ra := httpRouteToAttrs(hr, newGatewayStore(), newGatewayClassStore(), &Config{EmitStatusConds: false})
	assert.Nil(t, ra.Accepted, "emit_status_conditions=false must leave Accepted unpopulated")
	assert.Nil(t, ra.ResolvedRefs)
}

func TestGRPCRouteToAttrs_BasicAndWithGateway(t *testing.T) {
	gwStore := newGatewayStore()
	gcStore := newGatewayClassStore()
	gwStore.upsert(&gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "infra", UID: types.UID("gw")},
		Spec:       gwv1.GatewaySpec{GatewayClassName: "envoygwc"},
	})
	gcStore.upsert(&gwv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "envoygwc"},
		Spec:       gwv1.GatewayClassSpec{ControllerName: "gateway.envoyproxy.io/gatewayclass-controller"},
	})

	ns := gwv1.Namespace("infra")
	gr := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "demo", UID: types.UID("gr-uid")},
		Spec: gwv1.GRPCRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{
				ParentRefs: []gwv1.ParentReference{{Name: "public", Namespace: &ns}},
			},
		},
	}
	ra := grpcRouteToAttrs(gr, gwStore, gcStore, &Config{})

	assert.Equal(t, RouteKindGRPCRoute, ra.Kind)
	assert.Equal(t, "svc", ra.Name)
	assert.Equal(t, "gr-uid", ra.UID)
	assert.Equal(t, "public", ra.GatewayName)
	assert.Equal(t, "envoygwc", ra.GatewayClassName)
}

// GRPCRoute status conditions: with emit_status_conditions=true, the projection
// must lift Accepted/ResolvedRefs out of gr.Status.Parents. ISI-785.
func TestGRPCRouteToAttrs_EmitStatusConditions_Stamps(t *testing.T) {
	gr := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "grpc-ns"},
		Status: gwv1.GRPCRouteStatus{RouteStatus: gwv1.RouteStatus{
			Parents: []gwv1.RouteParentStatus{{
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionTrue},
					{Type: "ResolvedRefs", Status: metav1.ConditionFalse},
				},
			}},
		}},
	}
	ra := grpcRouteToAttrs(gr, newGatewayStore(), newGatewayClassStore(), &Config{EmitStatusConds: true})
	require.NotNil(t, ra.Accepted)
	require.NotNil(t, ra.ResolvedRefs)
	assert.True(t, *ra.Accepted)
	assert.False(t, *ra.ResolvedRefs)
}

func TestGRPCRouteToAttrs_EmitStatusConditions_Off_NoStatus(t *testing.T) {
	gr := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "grpc-ns"},
		Status: gwv1.GRPCRouteStatus{RouteStatus: gwv1.RouteStatus{
			Parents: []gwv1.RouteParentStatus{{
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionTrue},
				},
			}},
		}},
	}
	ra := grpcRouteToAttrs(gr, newGatewayStore(), newGatewayClassStore(), &Config{EmitStatusConds: false})
	assert.Nil(t, ra.Accepted, "emit_status_conditions=false must leave Accepted unpopulated on GRPCRoute")
	assert.Nil(t, ra.ResolvedRefs)
}

// ---- statusFlags: Accepted/ResolvedRefs extraction ----

func TestStatusFlags_BothConditions(t *testing.T) {
	a, r := statusFlags([]gwv1.RouteParentStatus{{
		Conditions: []metav1.Condition{
			{Type: "Accepted", Status: metav1.ConditionTrue},
			{Type: "ResolvedRefs", Status: metav1.ConditionFalse},
			{Type: "Unknown", Status: metav1.ConditionTrue},
		},
	}})
	require.NotNil(t, a)
	require.NotNil(t, r)
	assert.True(t, *a)
	assert.False(t, *r)
}

func TestStatusFlags_NoParents(t *testing.T) {
	a, r := statusFlags(nil)
	assert.Nil(t, a)
	assert.Nil(t, r)
}

// ---- formatParentRef ----

func TestFormatParentRef_DefaultsWhenGroupKindUnset(t *testing.T) {
	ref := gwv1.ParentReference{Name: "public"}
	assert.Equal(t,
		"gateway.networking.k8s.io/Gateway/owner-ns/public",
		formatParentRef(ref, "owner-ns"),
	)
}

func TestFormatParentRef_ExplicitGroupKindAndNamespace(t *testing.T) {
	grp := gwv1.Group("custom.example.com")
	kind := gwv1.Kind("CustomParent")
	ns := gwv1.Namespace("infra")
	ref := gwv1.ParentReference{
		Group:     &grp,
		Kind:      &kind,
		Namespace: &ns,
		Name:      "gw",
	}
	assert.Equal(t, "custom.example.com/CustomParent/infra/gw", formatParentRef(ref, "owner-ns"))
}

// ---- defaultSyncTimeout ----

func TestDefaultSyncTimeout(t *testing.T) {
	assert.Equal(t, 30*time.Second, defaultSyncTimeout(0))
	assert.Equal(t, 30*time.Second, defaultSyncTimeout(-5*time.Second))
	assert.Equal(t, 7*time.Second, defaultSyncTimeout(7*time.Second))
}

// ---- gatewayStore / gatewayClassStore ----

func TestGatewayStore_UpsertGetDelete(t *testing.T) {
	store := newGatewayStore()
	gw := &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "infra"}}
	store.upsert(gw)

	got, ok := store.get("infra", "public")
	require.True(t, ok)
	assert.Same(t, gw, got)

	store.delete("infra", "public")
	_, ok = store.get("infra", "public")
	assert.False(t, ok)
}

func TestGatewayClassStore_UpsertGetDelete(t *testing.T) {
	store := newGatewayClassStore()
	gc := &gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "envoygwc"}}
	store.upsert(gc)

	got, ok := store.get("envoygwc")
	require.True(t, ok)
	assert.Same(t, gc, got)

	store.delete("envoygwc")
	_, ok = store.get("envoygwc")
	assert.False(t, ok)
}

// ---- splitAddress edge cases (processor.go helper) ----

func TestSplitAddress_Matrix(t *testing.T) {
	cases := []struct {
		in     string
		wantNS string
		wantS  string
	}{
		{"api-service.demo.svc.cluster.local", "demo", "api-service"},
		{"api-service.demo", "demo", "api-service"},
		{"svc-only", "", ""},
		{"", "", ""},
		{"trailing.", "", ""}, // dot is last char → invalid
		{".leading", "", ""},  // dot at start → invalid
		{"svc..double", "", ""},
	}
	for _, tc := range cases {
		ns, svc := splitAddress(tc.in)
		assert.Equalf(t, tc.wantNS, ns, "ns for %q", tc.in)
		assert.Equalf(t, tc.wantS, svc, "svc for %q", tc.in)
	}
}

// routeKey prefix must differentiate GRPCRoute from HTTPRoute — the index
// stores both in the same map.
func TestRouteKey_DifferentiatesKinds(t *testing.T) {
	assert.NotEqual(t,
		routeKey(RouteKindHTTPRoute, "demo", "svc"),
		routeKey(RouteKindGRPCRoute, "demo", "svc"),
	)
	// Unknown kind defaults to HTTPRoute prefix.
	assert.Equal(t,
		routeKey(RouteKindHTTPRoute, "demo", "svc"),
		routeKey(RouteKindUnknown, "demo", "svc"),
	)
}
