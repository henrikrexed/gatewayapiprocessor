package gatewayapiprocessor

import (
	"sync"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// routeIndex is the in-memory projection of the informer caches that the
// enrichment path reads. It is maintained by event handlers registered on the
// Gateway / HTTPRoute / GRPCRoute / GatewayClass informers.
//
// Lock discipline: a single RWMutex guards all maps. This is deliberately
// coarse — the enrichment path only reads, and write churn is bounded by CR
// change rate (seconds at most). A finer lock would not pay for itself.
type routeIndex struct {
	mu sync.RWMutex

	// Primary route store. Key = "<kind>|<ns>/<name>".
	routes map[string]RouteAttributes

	// backendIndex keys HTTPRoutes by their backendRef Service DNS name.
	// Key = "<ns>/<service>". Single-candidate writes only — the first
	// upsert wins; multiple routes referencing the same Service drop the
	// second one so we never stamp ambiguous attribution.
	backendIndex map[string]RouteAttributes

	// claimedBackends records which (ns/service) keys already have an owner,
	// so we can detect ambiguity on subsequent updates.
	claimedBackends map[string]string // key -> "<kind>|<ns>/<name>" owner
}

func newRouteIndex() *routeIndex {
	return &routeIndex{
		routes:          make(map[string]RouteAttributes),
		backendIndex:    make(map[string]RouteAttributes),
		claimedBackends: make(map[string]string),
	}
}

// LookupRoute satisfies RouteLookup.
func (r *routeIndex) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ra, ok := r.routes[routeKey(kind, ns, name)]
	return ra, ok
}

// LookupByBackendService satisfies RouteLookup.
func (r *routeIndex) LookupByBackendService(ns, svc string) (RouteAttributes, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ra, ok := r.backendIndex[ns+"/"+svc]
	return ra, ok
}

// upsertHTTPRoute applies a full RouteAttributes + backendRef list from a
// decoded HTTPRoute object. The informer event handlers build the struct and
// call this; tests call it directly to simulate cache state.
func (r *routeIndex) upsertHTTPRoute(ra RouteAttributes, backendRefs []backendRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindHTTPRoute, ra.Namespace, ra.Name)
	r.routes[key] = ra
	owner := key
	for _, b := range backendRefs {
		bkey := b.Namespace + "/" + b.Name
		if existing, ok := r.claimedBackends[bkey]; ok && existing != owner {
			// Multiple routes reference this service — drop the index entry so
			// backendref_fallback never attributes ambiguously.
			delete(r.backendIndex, bkey)
			continue
		}
		r.claimedBackends[bkey] = owner
		r.backendIndex[bkey] = ra
	}
}

// upsertGRPCRoute mirrors upsertHTTPRoute for GRPCRoute CRs.
func (r *routeIndex) upsertGRPCRoute(ra RouteAttributes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[routeKey(RouteKindGRPCRoute, ra.Namespace, ra.Name)] = ra
}

// deleteHTTPRoute removes a route and its backendRef attribution entries.
func (r *routeIndex) deleteHTTPRoute(ns, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindHTTPRoute, ns, name)
	delete(r.routes, key)
	for bkey, owner := range r.claimedBackends {
		if owner == key {
			delete(r.claimedBackends, bkey)
			delete(r.backendIndex, bkey)
		}
	}
}

func (r *routeIndex) deleteGRPCRoute(ns, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, routeKey(RouteKindGRPCRoute, ns, name))
}

// backendRef is the narrowed projection of HTTPRouteRule.BackendRefs we keep
// in the index. We only care about (namespace, service-name) — enough to
// resolve server.address lookups.
type backendRef struct {
	Namespace string
	Name      string
}

// backendRefsFromHTTPRoute flattens an HTTPRoute's rules into our narrow struct.
// Filters to core-group Services (empty group, kind=Service). Cross-namespace
// references fall back to the route's own namespace if unset (consistent with
// the Gateway API defaulting rules).
func backendRefsFromHTTPRoute(route *gwv1.HTTPRoute) []backendRef {
	out := make([]backendRef, 0)
	for _, rule := range route.Spec.Rules {
		for _, br := range rule.BackendRefs {
			ref := br.BackendObjectReference
			// Skip non-Service backends (e.g. Gateway extensions).
			if ref.Group != nil && *ref.Group != "" {
				continue
			}
			if ref.Kind != nil && *ref.Kind != "Service" {
				continue
			}
			ns := route.Namespace
			if ref.Namespace != nil && *ref.Namespace != "" {
				ns = string(*ref.Namespace)
			}
			out = append(out, backendRef{Namespace: ns, Name: string(ref.Name)})
		}
	}
	return out
}
