package gatewayapiprocessor

import (
	"context"
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

	// indexedLabels carries the last (gateway_class, route_kind) tuple
	// emitted to the routes_indexed UpDownCounter for a given key, so we can
	// emit a matching -1 on delete (or a swap if gateway_class changed on
	// update). Nil when telemetry is not wired in (e.g. unit tests).
	indexedLabels map[string]routeIndexLabel

	// tel is the self-telemetry hook. Nil when the index runs without a
	// wired-up telemetry builder (direct unit-test construction).
	tel *telemetryBuilder
}

// routeIndexLabel is the low-cardinality tuple we attach to routes_indexed.
type routeIndexLabel struct {
	gatewayClass string
	routeKind    string
}

func newRouteIndex() *routeIndex {
	return &routeIndex{
		routes:          make(map[string]RouteAttributes),
		backendIndex:    make(map[string]RouteAttributes),
		claimedBackends: make(map[string]string),
		indexedLabels:   make(map[string]routeIndexLabel),
	}
}

// attachTelemetry wires the telemetry builder for informer event counters and
// the routes_indexed UpDownCounter. Call this right after newRouteIndex() and
// before any upsert — safe to omit (no-op) in tests.
func (r *routeIndex) attachTelemetry(tel *telemetryBuilder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tel = tel
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

// IsAmbiguousBackend returns true when the given (namespace, service) key was
// ever claimed by a route but is no longer resolvable because multiple routes
// referenced the same Service. Used by the self-telemetry path to distinguish
// "ambiguous" from "unresolved" misses on the backendRef fallback.
func (r *routeIndex) IsAmbiguousBackend(ns, svc string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := ns + "/" + svc
	_, claimed := r.claimedBackends[key]
	_, indexed := r.backendIndex[key]
	return claimed && !indexed
}

// upsertHTTPRoute applies a full RouteAttributes + backendRef list from a
// decoded HTTPRoute object. The informer event handlers build the struct and
// call this; tests call it directly to simulate cache state.
func (r *routeIndex) upsertHTTPRoute(ra RouteAttributes, backendRefs []backendRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindHTTPRoute, ra.Namespace, ra.Name)
	_, existed := r.routes[key]
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
	r.updateIndexedLabelLocked(key, "HTTPRoute", ra.GatewayClassName, existed)
}

// upsertGRPCRoute mirrors upsertHTTPRoute for GRPCRoute CRs.
func (r *routeIndex) upsertGRPCRoute(ra RouteAttributes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindGRPCRoute, ra.Namespace, ra.Name)
	_, existed := r.routes[key]
	r.routes[key] = ra
	r.updateIndexedLabelLocked(key, "GRPCRoute", ra.GatewayClassName, existed)
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
	r.removeIndexedLabelLocked(key)
}

func (r *routeIndex) deleteGRPCRoute(ns, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := routeKey(RouteKindGRPCRoute, ns, name)
	delete(r.routes, key)
	r.removeIndexedLabelLocked(key)
}

// updateIndexedLabelLocked maintains the routes_indexed UpDownCounter on
// upsert. Caller must hold the write lock. It emits:
//   - +1 when the key is new
//   - 0 when the labels are unchanged
//   - -1 on the previous label + +1 on the new label when gateway_class shifts
//
// We never encode the route name / UID in the labels — only (gateway_class,
// route_kind). See processor-spec §1.4 cardinality guard.
//
// Context: the callers are informer event handlers (client-go cache callbacks)
// which have no inbound request context, so we pass context.Background(). This
// is an UpDownCounter of cluster state — it has no parent span to link to,
// and sampling/exemplars are not meaningful for it.
func (r *routeIndex) updateIndexedLabelLocked(key, routeKind, gatewayClass string, existed bool) {
	if r.tel == nil {
		return
	}
	newLabel := routeIndexLabel{gatewayClass: gatewayClass, routeKind: routeKind}
	prev, wasIndexed := r.indexedLabels[key]
	if !existed || !wasIndexed {
		r.tel.recordRoutesIndexedDelta(context.Background(), newLabel.gatewayClass, newLabel.routeKind, 1)
		r.indexedLabels[key] = newLabel
		return
	}
	if prev == newLabel {
		return
	}
	r.tel.recordRoutesIndexedDelta(context.Background(), prev.gatewayClass, prev.routeKind, -1)
	r.tel.recordRoutesIndexedDelta(context.Background(), newLabel.gatewayClass, newLabel.routeKind, 1)
	r.indexedLabels[key] = newLabel
}

// removeIndexedLabelLocked emits -1 on the key's last known labels and drops
// the mapping. Caller must hold the write lock.
//
// context.Background() is intentional for the same reason as
// updateIndexedLabelLocked — this is a cluster-state UpDownCounter emitted
// from an informer callback that has no inbound request context.
func (r *routeIndex) removeIndexedLabelLocked(key string) {
	if r.tel == nil {
		return
	}
	prev, ok := r.indexedLabels[key]
	if !ok {
		return
	}
	r.tel.recordRoutesIndexedDelta(context.Background(), prev.gatewayClass, prev.routeKind, -1)
	delete(r.indexedLabels, key)
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
