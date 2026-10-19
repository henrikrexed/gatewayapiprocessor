package gatewayapiprocessor

// RouteAttributes carries the normalized attributes a route contributes
// when it's attached to a span/log/metric record.
//
// Shape matches processor-spec §1.1 / §1.2. Keep field tagging aligned with
// the attribute schema — any new field should also be stamped in
// processor.stampRouteAttrs.
type RouteAttributes struct {
	// Route identity.
	Kind      RouteKind // HTTPRoute or GRPCRoute
	Name      string
	Namespace string
	UID       string
	ParentRef string // "<group>/<kind>/<ns>/<name>"

	// Status (populated when emit_status_conditions=true).
	Accepted     *bool
	ResolvedRefs *bool

	// Parent Gateway (resolved from route.spec.parentRefs[0] when kind=Gateway).
	GatewayName                string
	GatewayNamespace           string
	GatewayUID                 string
	GatewayListenerName        string
	GatewayClassName           string
	GatewayClassControllerName string

	// ParentKind is the parentRef.kind the route attached to ("Gateway" or
	// "Service"), defaulting to "Gateway" per the Gateway API spec when the
	// parentRef omits Kind. Always populated from the first parentRef so the
	// k8s.{http,grpc}route.parent.kind attribute can be stamped for both
	// ingress and mesh routes (ISI-6178 §4.1).
	ParentKind string

	// RouteMode is "ingress" (parentRef.kind=Gateway) or "mesh"
	// (parentRef.kind=Service, GAMMA). Empty when the parent kind couldn't be
	// determined; downstream stamping treats empty as "ingress" for back-compat.
	RouteMode string

	// Mesh-mode parent Service (GAMMA). Populated only when RouteMode == "mesh".
	ParentServiceName      string
	ParentServiceNamespace string
	// ParentServicePort is the parentRef.port for a Service parent, or 0 when
	// the parentRef omits a port (ISI-6178 §4.1). Nil-safe: 0 means "not set"
	// and is not stamped.
	ParentServicePort int32

	// RouteRole is "producer" or "consumer" for mesh routes (GAMMA, §4.4).
	// Empty for ingress routes and when role classification is disabled.
	RouteRole string

	// Waypoint attribution (GAMMA, §4.3). Populated from the parent Gateway's
	// gatewayClassName when that class is a known waypoint class (class-based
	// path). The label-based path (parent Service istio.io/use-waypoint) is
	// resolved at stamp time against the live Service informer cache, so those
	// fields may be filled there instead — see stampRouteAttrs.
	WaypointImplementation string
	WaypointName           string
	WaypointNamespace      string

	// RuleNames holds the per-rule Name (HTTPRouteRule.Name / GRPCRouteRule.Name,
	// Gateway API v1.1+ experimental / v1.4+ standard) indexed by rule position.
	// A nil/unnamed rule yields an empty string at that index. stampRouteAttrs
	// resolves RuleNames[rule_index] → k8s.{http,grpc}route.rule.name when the
	// parser produced a rule index (ISI-6178 §4.5).
	RuleNames []string

	// Policies holds Gateway API policy references whose targetRefs[*]
	// point at this route. Populated by the policy informer (see
	// policy_informer.go) and read by stampRouteAttrs to write the
	// k8s.gatewayapi.policy.{names,kinds,namespaces,groups} array
	// attributes. Order is informer-discovery order; tests assert
	// ordering-stable output.
	Policies []PolicyRef
}

// PolicyRef is a single Gateway API policy attached to a route. Mirrors the
// fields of a CRD object reference minus the UID — Henrik's direction on
// ISI-804 was that we store the policy by name + CRD kind only, no UID, so
// per-span cardinality stays bounded by policy count.
type PolicyRef struct {
	Name      string
	Namespace string
	Kind      string
	Group     string
}

// RouteKind enumerates the CR kinds the processor enriches from.
type RouteKind int

const (
	RouteKindUnknown RouteKind = iota
	RouteKindHTTPRoute
	RouteKindGRPCRoute
)

// RouteLookup is the read-side contract the enrichment path depends on.
//
// The informer-backed implementation lives in index.go; tests swap in
// an in-memory fake to exercise the 10-case matrix without a real API server.
type RouteLookup interface {
	LookupRoute(kind RouteKind, namespace, name string) (RouteAttributes, bool)
	// LookupByBackendService resolves a single best candidate HTTPRoute that
	// references the given Service. Used by the backendref_fallback path.
	// Returns (_, false) when no unambiguous match is available.
	LookupByBackendService(namespace, service string) (RouteAttributes, bool)
	// LookupByBackendServiceWithParents returns every route that claims the
	// given backend Service, including the ambiguous case where the
	// single-candidate index would have dropped the entry. Used by the
	// binary disambiguator on the backendref_fallback path (ISI-805) to
	// recover legitimate mesh + ingress dual-mode topologies that share a
	// backend Service.
	LookupByBackendServiceWithParents(namespace, service string) ([]RouteAttributes, bool)
}

// staticLookup is a trivial RouteLookup used by tests and by the
// no-Kubernetes mode (auth_type=none, no informers).
//
// It also satisfies ServiceIPLookup so the same fixture can drive the
// IP reverse-lookup fallback path (ISI-851) without spinning up informers.
type staticLookup struct {
	routes              map[string]RouteAttributes   // key = "<kind>|<ns>/<name>"
	backendIndex        map[string]RouteAttributes   // key = "<ns>/<service>"
	backendIndexAll     map[string][]RouteAttributes // key = "<ns>/<service>" — all candidates (ISI-805)
	backendIndexDropped map[string]struct{}          // key = "<ns>/<service>" — sticky once-dropped
	serviceIPs          map[string]nsName            // key = canonical IP literal — Phase 1 ClusterIP (ISI-851)
	podIPs              map[string]nsName            // key = canonical IP literal — Phase 2 PodIP (ISI-875)
	waypoints           map[string]waypointInfo      // key = "<ns>/<service>" — use-waypoint label cache (ISI-6178)
}

func newStaticLookup() *staticLookup {
	return &staticLookup{
		routes:              make(map[string]RouteAttributes),
		backendIndex:        make(map[string]RouteAttributes),
		backendIndexAll:     make(map[string][]RouteAttributes),
		backendIndexDropped: make(map[string]struct{}),
		serviceIPs:          make(map[string]nsName),
		podIPs:              make(map[string]nsName),
		waypoints:           make(map[string]waypointInfo),
	}
}

// putWaypoint seeds a Service -> waypoint mapping. Tests use this to simulate a
// Service carrying the istio.io/use-waypoint label without a real informer.
func (s *staticLookup) putWaypoint(ns, svc, impl, wpName, wpNS string) {
	s.waypoints[ns+"/"+svc] = waypointInfo{Implementation: impl, Name: wpName, Namespace: wpNS}
}

// LookupWaypointForService satisfies WaypointLookup for tests.
func (s *staticLookup) LookupWaypointForService(ns, name string) (string, string, string, bool) {
	wi, ok := s.waypoints[ns+"/"+name]
	if !ok {
		return "", "", "", false
	}
	return wi.Implementation, wi.Name, wi.Namespace, true
}

func (s *staticLookup) put(kind RouteKind, ns, name string, attrs RouteAttributes) {
	s.routes[routeKey(kind, ns, name)] = attrs
}

// putBackend seeds the single-candidate backend index. Mirrors routeIndex
// semantics: a second putBackend for the same (ns, svc) drops the index entry
// (ambiguous) and once dropped it stays dropped — even on a 3rd or later
// putBackend — so the staticLookup matches the real index's behaviour where
// `claimedBackends` keeps the first owner pinned and any later owner is
// rejected from the single-candidate path. The candidate is still retained in
// backendIndexAll so the disambiguator can see all owners.
func (s *staticLookup) putBackend(ns, svc string, attrs RouteAttributes) {
	bkey := ns + "/" + svc
	s.backendIndexAll[bkey] = append(s.backendIndexAll[bkey], attrs)
	if _, dropped := s.backendIndexDropped[bkey]; dropped {
		// Sticky drop — once ambiguous, always ambiguous in the single-candidate
		// view. Matches routeIndex.reindexBackends, which keeps the first owner
		// in `claimedBackends` and drops `backendIndex[bkey]` for any 2nd+ owner.
		return
	}
	if _, exists := s.backendIndex[bkey]; exists {
		delete(s.backendIndex, bkey)
		s.backendIndexDropped[bkey] = struct{}{}
		return
	}
	s.backendIndex[bkey] = attrs
}

// putServiceIP seeds an IP -> (ns, svc) mapping. Tests use this to simulate a
// Service informer cache for the ISI-851 fallback path. The IP is normalized
// before storing so callers can pass either canonical or non-canonical forms.
func (s *staticLookup) putServiceIP(ip, ns, svc string) {
	canon := canonicalIP(ip)
	if canon == "" {
		return
	}
	s.serviceIPs[canon] = nsName{Namespace: ns, Name: svc}
}

// putPodIP seeds a PodIP -> (ns, svc) mapping. Tests use this to simulate
// the EndpointSlice informer cache for the ISI-875 fallback path. The IP
// is normalized so callers can pass either canonical or non-canonical
// forms.
func (s *staticLookup) putPodIP(ip, ns, svc string) {
	canon := canonicalIP(ip)
	if canon == "" {
		return
	}
	s.podIPs[canon] = nsName{Namespace: ns, Name: svc}
}

func (s *staticLookup) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	r, ok := s.routes[routeKey(kind, ns, name)]
	return r, ok
}

func (s *staticLookup) LookupByBackendService(ns, service string) (RouteAttributes, bool) {
	r, ok := s.backendIndex[ns+"/"+service]
	return r, ok
}

// LookupByBackendServiceWithParents satisfies RouteLookup.
func (s *staticLookup) LookupByBackendServiceWithParents(ns, service string) ([]RouteAttributes, bool) {
	rs, ok := s.backendIndexAll[ns+"/"+service]
	if !ok || len(rs) == 0 {
		return nil, false
	}
	return rs, true
}

// LookupServiceByIP satisfies ServiceIPLookup for tests. ClusterIP entries
// (putServiceIP) take precedence over PodIP entries (putPodIP) when the same
// canonical IP is registered in both indices, mirroring combinedLookup's
// production order: a ClusterIP claim is the more specific signal.
func (s *staticLookup) LookupServiceByIP(ip string) (string, string, bool) {
	canon := canonicalIP(ip)
	if canon == "" {
		return "", "", false
	}
	if v, ok := s.serviceIPs[canon]; ok {
		return v.Namespace, v.Name, true
	}
	if v, ok := s.podIPs[canon]; ok {
		return v.Namespace, v.Name, true
	}
	return "", "", false
}

// combinedLookup glues the route index, the Service-IP index, and the
// PodIP index into a single RouteLookup-shaped object so the processor can
// carry one pointer. The processor's fallback path type-asserts to
// ServiceIPLookup before consulting the IP indices — that lets
// staticLookup-only tests stay source-compatible.
type combinedLookup struct {
	routes    *routeIndex
	ips       *serviceIPIndex // ISI-851 ClusterIP index
	pods      *podIPIndex     // ISI-875 EndpointSlice→PodIP index (may be nil)
	waypoints *waypointIndex  // ISI-6178 use-waypoint label cache (may be nil)
}

func (c *combinedLookup) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	return c.routes.LookupRoute(kind, ns, name)
}

func (c *combinedLookup) LookupByBackendService(ns, service string) (RouteAttributes, bool) {
	return c.routes.LookupByBackendService(ns, service)
}

func (c *combinedLookup) LookupByBackendServiceWithParents(ns, service string) ([]RouteAttributes, bool) {
	return c.routes.LookupByBackendServiceWithParents(ns, service)
}

// LookupServiceByIP resolves an IP literal to its (namespace, service) tuple
// by consulting the ClusterIP index first and then the PodIP index. This
// order matters: a Service ClusterIP is a more specific signal than a Pod
// happening to live behind a Service, and in the (rare) case where both
// indices know an IP we should attribute traffic to the Service-level
// claim, not to one of its endpoints. Returns ok=false when neither index
// has the IP or when neither index is configured.
func (c *combinedLookup) LookupServiceByIP(ip string) (string, string, bool) {
	if c.ips != nil {
		if ns, svc, ok := c.ips.LookupServiceByIP(ip); ok {
			return ns, svc, true
		}
	}
	if c.pods != nil {
		return c.pods.LookupPodIP(ip)
	}
	return "", "", false
}

// LookupWaypointForService satisfies WaypointLookup (ISI-6178 §4.3). Returns
// ok=false when no waypoint index is configured (waypoint attribution off) or
// the Service carries no use-waypoint label.
func (c *combinedLookup) LookupWaypointForService(ns, name string) (string, string, string, bool) {
	if c.waypoints == nil {
		return "", "", "", false
	}
	return c.waypoints.LookupWaypointForService(ns, name)
}

func routeKey(kind RouteKind, ns, name string) string {
	prefix := "HTTPRoute"
	if kind == RouteKindGRPCRoute {
		prefix = "GRPCRoute"
	}
	return prefix + "|" + ns + "/" + name
}
