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

	// policies maps a route key to the deduplicated PolicyRefs whose
	// CRD spec.targetRefs[*] points at it. Lives separately from `routes`
	// so route lifecycle (informer add/update/delete) and policy lifecycle
	// (a different informer) don't fight for the same write. LookupRoute
	// merges policies into the returned RouteAttributes value at read time.
	// Key = "<kind>|<ns>/<name>".
	policies map[string][]PolicyRef
}

func newRouteIndex() *routeIndex {
	return &routeIndex{
		routes:          make(map[string]RouteAttributes),
		backendIndex:    make(map[string]RouteAttributes),
		claimedBackends: make(map[string]string),
		policies:        make(map[string][]PolicyRef),
	}
}

// LookupRoute satisfies RouteLookup.
func (r *routeIndex) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := routeKey(kind, ns, name)
	ra, ok := r.routes[key]
	if !ok {
		return ra, false
	}
	r.overlayPoliciesLocked(key, &ra)
	return ra, true
}

// LookupByBackendService satisfies RouteLookup.
func (r *routeIndex) LookupByBackendService(ns, svc string) (RouteAttributes, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ra, ok := r.backendIndex[ns+"/"+svc]
	if !ok {
		return ra, false
	}
	r.overlayPoliciesLocked(routeKey(ra.Kind, ra.Namespace, ra.Name), &ra)
	return ra, true
}

// overlayPoliciesLocked attaches the current policy snapshot for the given
// route key onto ra.Policies. Caller MUST hold r.mu (read or write). The
// overlay is a defensive copy so callers can mutate the returned slice
// without racing other readers — the index map is the source of truth.
func (r *routeIndex) overlayPoliciesLocked(key string, ra *RouteAttributes) {
	pols := r.policies[key]
	if len(pols) == 0 {
		return
	}
	ra.Policies = append([]PolicyRef(nil), pols...)
}

// applyPolicy records that a Gateway API policy CRD's spec.targetRefs[*]
// points at the route identified by (kind, ns, name). Idempotent: if an
// equivalent PolicyRef already exists for this route, the call is a no-op.
//
// Called from the dynamic policy informer's Add/Update handlers; safe to call
// before the matched route has been ingested by the route informer (the
// policy ref simply waits in the index until the route appears).
func (r *routeIndex) applyPolicy(kind RouteKind, ns, name string, p PolicyRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(kind, ns, name)
	for _, existing := range r.policies[key] {
		if policyRefsEqual(existing, p) {
			return
		}
	}
	r.policies[key] = append(r.policies[key], p)
}

// removePolicy drops a previously-applied PolicyRef from the route. Called
// from the dynamic policy informer's Update (for targets that disappeared
// between revisions) and Delete handlers. Last-policy removal also clears the
// per-route entry so memory does not leak after a fully-detached policy.
func (r *routeIndex) removePolicy(kind RouteKind, ns, name string, p PolicyRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(kind, ns, name)
	pols := r.policies[key]
	for i, existing := range pols {
		if policyRefsEqual(existing, p) {
			r.policies[key] = append(pols[:i], pols[i+1:]...)
			if len(r.policies[key]) == 0 {
				delete(r.policies, key)
			}
			return
		}
	}
}

// policyRefsEqual compares two refs by their identity tuple. UID is
// deliberately not part of the tuple — see ISI-804.
func policyRefsEqual(a, b PolicyRef) bool {
	return a.Name == b.Name && a.Namespace == b.Namespace && a.Kind == b.Kind && a.Group == b.Group
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
