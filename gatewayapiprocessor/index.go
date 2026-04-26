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
//
// Update semantics: if the route previously claimed a backend that no longer
// appears in backendRefs (route's spec changed), the stale claim is cleared
// before re-indexing. Without this, a renamed backendRef would leave the old
// service pointing at this route forever.
func (r *routeIndex) upsertHTTPRoute(ra RouteAttributes, backendRefs []backendRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindHTTPRoute, ra.Namespace, ra.Name)
	r.routes[key] = ra
	r.reindexBackends(key, ra, backendRefs)
}

// upsertGRPCRoute mirrors upsertHTTPRoute for GRPCRoute CRs. Indexes by
// backendRef so GAMMA mesh-mode gRPC spans (which carry server.address /
// net.peer.name pointing at the backend Service) can resolve via the
// backendref_fallback path. Update semantics match upsertHTTPRoute — see
// that doc.
func (r *routeIndex) upsertGRPCRoute(ra RouteAttributes, backendRefs []backendRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindGRPCRoute, ra.Namespace, ra.Name)
	r.routes[key] = ra
	r.reindexBackends(key, ra, backendRefs)
}

// reindexBackends releases previously-owned backend keys that are no longer
// in backendRefs, then re-claims the current set. Caller must hold r.mu.
func (r *routeIndex) reindexBackends(owner string, ra RouteAttributes, backendRefs []backendRef) {
	wanted := make(map[string]struct{}, len(backendRefs))
	for _, b := range backendRefs {
		wanted[b.Namespace+"/"+b.Name] = struct{}{}
	}
	// Drop stale claims belonging to this owner.
	for bkey, claim := range r.claimedBackends {
		if claim != owner {
			continue
		}
		if _, keep := wanted[bkey]; keep {
			continue
		}
		delete(r.claimedBackends, bkey)
		delete(r.backendIndex, bkey)
	}
	// (Re)claim current backends.
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
	key := routeKey(RouteKindGRPCRoute, ns, name)
	delete(r.routes, key)
	for bkey, owner := range r.claimedBackends {
		if owner == key {
			delete(r.claimedBackends, bkey)
			delete(r.backendIndex, bkey)
		}
	}
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
	// GAMMA: parent Service is the destination in mesh mode. Including it in
	// the backend index lets spans hitting that Service resolve via the
	// backendref_fallback path even when the route declares no explicit
	// backendRef (uncommon but legal).
	for _, pr := range route.Spec.ParentRefs {
		if !isServiceParent(pr) {
			continue
		}
		ns := route.Namespace
		if pr.Namespace != nil && *pr.Namespace != "" {
			ns = string(*pr.Namespace)
		}
		out = append(out, backendRef{Namespace: ns, Name: string(pr.Name)})
	}
	return out
}

// backendRefsFromGRPCRoute mirrors backendRefsFromHTTPRoute for GRPCRoute.
// Required so GAMMA gRPC spans (server.address resolves to the backend
// Service) can match via the backendref_fallback path. Also includes the
// parent Service for GAMMA routes — it IS the destination in mesh mode and
// often equals the only backendRef, but indexing both is harmless.
func backendRefsFromGRPCRoute(route *gwv1.GRPCRoute) []backendRef {
	out := make([]backendRef, 0)
	for _, rule := range route.Spec.Rules {
		for _, br := range rule.BackendRefs {
			ref := br.BackendObjectReference
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
	for _, pr := range route.Spec.ParentRefs {
		if !isServiceParent(pr) {
			continue
		}
		ns := route.Namespace
		if pr.Namespace != nil && *pr.Namespace != "" {
			ns = string(*pr.Namespace)
		}
		out = append(out, backendRef{Namespace: ns, Name: string(pr.Name)})
	}
	return out
}
