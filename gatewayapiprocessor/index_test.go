package gatewayapiprocessor

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ---- routeIndex lifecycle (ISI-684 §scope 3: informer deletion ordering) ----

func TestRouteIndex_UpsertAndLookupHTTPRoute(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "demo",
		UID:       "uid-api",
	}, []backendRef{{Namespace: "demo", Name: "api-svc"}})

	ra, ok := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.True(t, ok)
	assert.Equal(t, "uid-api", ra.UID)

	got, ok := idx.LookupByBackendService("demo", "api-svc")
	require.True(t, ok)
	assert.Equal(t, "api", got.Name)
}

func TestRouteIndex_UpsertAndLookupGRPCRoute(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertGRPCRoute(RouteAttributes{
		Kind:      RouteKindGRPCRoute,
		Name:      "svc",
		Namespace: "demo",
		UID:       "uid-grpc",
	})

	ra, ok := idx.LookupRoute(RouteKindGRPCRoute, "demo", "svc")
	require.True(t, ok)
	assert.Equal(t, "uid-grpc", ra.UID)

	_, ok = idx.LookupRoute(RouteKindHTTPRoute, "demo", "svc")
	assert.False(t, ok, "GRPCRoute key must not collide with HTTPRoute key")
}

// Henrik scope-expansion on ISI-679: deletion ordering must clear backend
// attribution so a subsequent HTTPRoute claiming the same Service owns it
// cleanly — no ambiguous-drop carryover.
func TestRouteIndex_DeleteHTTPRoute_ClearsBackendIndex(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api",
		Namespace: "demo",
	}, []backendRef{{Namespace: "demo", Name: "api-svc"}})

	idx.deleteHTTPRoute("demo", "api")

	_, ok := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.False(t, ok, "route must be removed from primary map")

	_, ok = idx.LookupByBackendService("demo", "api-svc")
	assert.False(t, ok, "backend index must be purged when its owner is deleted")

	// Re-claim the same backend with a different owner — must succeed.
	idx.upsertHTTPRoute(RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      "api-v2",
		Namespace: "demo",
	}, []backendRef{{Namespace: "demo", Name: "api-svc"}})
	ra, ok := idx.LookupByBackendService("demo", "api-svc")
	require.True(t, ok)
	assert.Equal(t, "api-v2", ra.Name)
}

func TestRouteIndex_DeleteGRPCRoute(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertGRPCRoute(RouteAttributes{Kind: RouteKindGRPCRoute, Namespace: "demo", Name: "svc"})
	idx.deleteGRPCRoute("demo", "svc")

	_, ok := idx.LookupRoute(RouteKindGRPCRoute, "demo", "svc")
	assert.False(t, ok)
}

// processor-spec §1.3 / ISI-684: "backendRef fallback chain with ambiguous
// owner (drop, do not mis-attribute)". Two routes claiming the same Service
// must cause the backend index entry to disappear.
func TestRouteIndex_BackendConflict_DropsAttribution(t *testing.T) {
	idx := newRouteIndex()

	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api-a",
	}, []backendRef{{Namespace: "demo", Name: "shared-svc"}})

	// Sanity: first route owns the backend.
	ra, ok := idx.LookupByBackendService("demo", "shared-svc")
	require.True(t, ok)
	assert.Equal(t, "api-a", ra.Name)

	// Second route with the same backend — attribution must drop.
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api-b",
	}, []backendRef{{Namespace: "demo", Name: "shared-svc"}})

	_, ok = idx.LookupByBackendService("demo", "shared-svc")
	assert.False(t, ok, "ambiguous backend must yield no match (never mis-attribute)")

	// Both routes themselves are still retrievable by (ns,name).
	_, ok = idx.LookupRoute(RouteKindHTTPRoute, "demo", "api-a")
	assert.True(t, ok)
	_, ok = idx.LookupRoute(RouteKindHTTPRoute, "demo", "api-b")
	assert.True(t, ok)
}

// An idempotent re-upsert of the same (kind,ns,name) must NOT self-conflict.
// This is the "same owner re-applies backend" path — a common case when the
// informer resyncs and re-fires AddFunc on an object already present.
func TestRouteIndex_SameOwnerReupsert_KeepsAttribution(t *testing.T) {
	idx := newRouteIndex()

	for i := 0; i < 3; i++ {
		idx.upsertHTTPRoute(RouteAttributes{
			Kind: RouteKindHTTPRoute, Namespace: "demo", Name: "api",
		}, []backendRef{{Namespace: "demo", Name: "svc"}})
	}

	ra, ok := idx.LookupByBackendService("demo", "svc")
	require.True(t, ok, "same-owner re-upsert must keep attribution (not a conflict)")
	assert.Equal(t, "api", ra.Name)
}

func TestBackendRefsFromHTTPRoute_FiltersNonServiceAndDefaultsNamespace(t *testing.T) {
	group := gwv1.Group("extensions.example.com")
	kindSvc := gwv1.Kind("Service")
	kindOther := gwv1.Kind("SuperBackend")
	otherNS := gwv1.Namespace("other")

	hr := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec: gwv1.HTTPRouteSpec{
			Rules: []gwv1.HTTPRouteRule{
				{
					BackendRefs: []gwv1.HTTPBackendRef{
						{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
							Name: "local-svc",
							Kind: &kindSvc,
						}}},
						{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
							Name:      "cross-svc",
							Kind:      &kindSvc,
							Namespace: &otherNS,
						}}},
						// Must be filtered: non-empty group (custom extension).
						{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
							Name:  "ext",
							Group: &group,
							Kind:  &kindSvc,
						}}},
						// Must be filtered: non-Service kind.
						{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
							Name: "other",
							Kind: &kindOther,
						}}},
					},
				},
			},
		},
	}

	refs := backendRefsFromHTTPRoute(hr)
	require.Len(t, refs, 2)
	assert.Contains(t, refs, backendRef{Namespace: "demo", Name: "local-svc"})
	assert.Contains(t, refs, backendRef{Namespace: "other", Name: "cross-svc"})
}

// Concurrent upsert / lookup / delete must not race. Go's race detector asserts
// this — the RWMutex in routeIndex is the only guarantee.
func TestRouteIndex_ConcurrentAccess_NoRaces(t *testing.T) {
	idx := newRouteIndex()
	const workers = 16
	const iters = 500

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				ns := "demo"
				name := "api"
				idx.upsertHTTPRoute(RouteAttributes{
					Kind: RouteKindHTTPRoute, Namespace: ns, Name: name,
					UID: string(types.UID("uid-")),
				}, []backendRef{{Namespace: ns, Name: "svc"}})
				_, _ = idx.LookupRoute(RouteKindHTTPRoute, ns, name)
				_, _ = idx.LookupByBackendService(ns, "svc")
				if (id+i)%17 == 0 {
					idx.deleteHTTPRoute(ns, name)
				}
			}
		}(w)
	}
	wg.Wait()
}
