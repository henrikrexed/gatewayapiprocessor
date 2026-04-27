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
	}, nil)

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
	idx.upsertGRPCRoute(RouteAttributes{Kind: RouteKindGRPCRoute, Namespace: "demo", Name: "svc"}, nil)
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

// ---- Policy attachment index (ISI-804) ----

func TestRouteIndex_ApplyPolicy_OverlaysOnLookup(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo", UID: "uid-api",
	}, nil)

	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", PolicyRef{
		Name: "rate-limit", Namespace: "demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev",
	})

	ra, ok := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.True(t, ok)
	require.Len(t, ra.Policies, 1)
	assert.Equal(t, "rate-limit", ra.Policies[0].Name)
	assert.Equal(t, "TrafficPolicy", ra.Policies[0].Kind)
}

func TestRouteIndex_ApplyPolicy_IsIdempotent(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo", UID: "uid",
	}, nil)
	p := PolicyRef{Name: "rl", Namespace: "demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev"}

	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", p)
	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", p)
	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", p)

	ra, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Len(t, ra.Policies, 1, "duplicate applyPolicy calls must dedupe")
}

func TestRouteIndex_ApplyPolicy_BeforeRouteExists(t *testing.T) {
	// The policy informer often races ahead of the route informer. The index
	// must accept the PolicyRef even when the matched route is unknown so that
	// the later route upsert sees the policies on first lookup.
	idx := newRouteIndex()
	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", PolicyRef{
		Name: "rl", Namespace: "demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev",
	})

	_, ok := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.False(t, ok, "lookup must return false until the route exists")

	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo", UID: "uid",
	}, nil)

	ra, ok := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.True(t, ok)
	require.Len(t, ra.Policies, 1, "policy applied before route must surface on later lookup")
}

func TestRouteIndex_RemovePolicy_DropsAndCleansEmptyEntries(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo", UID: "uid",
	}, nil)
	a := PolicyRef{Name: "a", Namespace: "demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev"}
	b := PolicyRef{Name: "b", Namespace: "demo", Kind: "BackendConfigPolicy", Group: "gateway.kgateway.dev"}
	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", a)
	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", b)

	idx.removePolicy(RouteKindHTTPRoute, "demo", "api", a)
	ra, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.Len(t, ra.Policies, 1)
	assert.Equal(t, "b", ra.Policies[0].Name)

	idx.removePolicy(RouteKindHTTPRoute, "demo", "api", b)
	ra, _ = idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Empty(t, ra.Policies)

	// Internal cleanup: when the last policy is removed, the per-route entry
	// must be deleted so a long-running collector with high policy churn
	// doesn't leak map entries.
	idx.mu.RLock()
	_, has := idx.policies[routeKey(RouteKindHTTPRoute, "demo", "api")]
	idx.mu.RUnlock()
	assert.False(t, has, "policies map must drop empty per-route entries")
}

func TestRouteIndex_PolicyOverlay_DefensivelyCopies(t *testing.T) {
	// A caller mutating the returned slice must NOT corrupt the index.
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo", UID: "uid",
	}, nil)
	idx.applyPolicy(RouteKindHTTPRoute, "demo", "api", PolicyRef{
		Name: "a", Namespace: "demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev",
	})

	ra1, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.Len(t, ra1.Policies, 1)
	ra1.Policies[0].Name = "MUTATED"

	ra2, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.Len(t, ra2.Policies, 1)
	assert.Equal(t, "a", ra2.Policies[0].Name, "lookup overlay must not alias the index storage")
}

func TestRouteIndex_PolicyOverlay_BackendServiceLookup(t *testing.T) {
	// The backendref_fallback path must also see attached policies — the demo's
	// GAMMA gRPC spans resolve via this path.
	idx := newRouteIndex()
	idx.upsertGRPCRoute(RouteAttributes{
		Kind: RouteKindGRPCRoute, Name: "checkout", Namespace: "demo", UID: "uid-checkout",
	}, []backendRef{{Namespace: "demo", Name: "checkout-svc"}})
	idx.applyPolicy(RouteKindGRPCRoute, "demo", "checkout", PolicyRef{
		Name: "rl", Namespace: "demo", Kind: "TrafficPolicy", Group: "gateway.kgateway.dev",
	})

	ra, ok := idx.LookupByBackendService("demo", "checkout-svc")
	require.True(t, ok)
	require.Len(t, ra.Policies, 1)
	assert.Equal(t, "rl", ra.Policies[0].Name)
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
