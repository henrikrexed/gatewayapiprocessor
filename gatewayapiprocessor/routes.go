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

	// RouteMode is "ingress" (parentRef.kind=Gateway) or "mesh"
	// (parentRef.kind=Service, GAMMA). Empty when the parent kind couldn't be
	// determined; downstream stamping treats empty as "ingress" for back-compat.
	RouteMode string

	// Mesh-mode parent Service (GAMMA). Populated only when RouteMode == "mesh".
	ParentServiceName      string
	ParentServiceNamespace string

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
}

// staticLookup is a trivial RouteLookup used by tests and by the
// no-Kubernetes mode (auth_type=none, no informers).
//
// It also satisfies ServiceIPLookup so the same fixture can drive the
// IP reverse-lookup fallback path (ISI-851) without spinning up informers.
type staticLookup struct {
	routes       map[string]RouteAttributes // key = "<kind>|<ns>/<name>"
	backendIndex map[string]RouteAttributes // key = "<ns>/<service>"
	serviceIPs   map[string]nsName          // key = canonical IP literal
}

func newStaticLookup() *staticLookup {
	return &staticLookup{
		routes:       make(map[string]RouteAttributes),
		backendIndex: make(map[string]RouteAttributes),
		serviceIPs:   make(map[string]nsName),
	}
}

func (s *staticLookup) put(kind RouteKind, ns, name string, attrs RouteAttributes) {
	s.routes[routeKey(kind, ns, name)] = attrs
}

func (s *staticLookup) putBackend(ns, svc string, attrs RouteAttributes) {
	s.backendIndex[ns+"/"+svc] = attrs
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

func (s *staticLookup) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	r, ok := s.routes[routeKey(kind, ns, name)]
	return r, ok
}

func (s *staticLookup) LookupByBackendService(ns, service string) (RouteAttributes, bool) {
	r, ok := s.backendIndex[ns+"/"+service]
	return r, ok
}

// LookupServiceByIP satisfies ServiceIPLookup for tests.
func (s *staticLookup) LookupServiceByIP(ip string) (string, string, bool) {
	canon := canonicalIP(ip)
	if canon == "" {
		return "", "", false
	}
	v, ok := s.serviceIPs[canon]
	if !ok {
		return "", "", false
	}
	return v.Namespace, v.Name, true
}

// combinedLookup glues the route index and the Service-IP index into a single
// RouteLookup-shaped object so the processor can carry one pointer. The
// processor's fallback path type-asserts to ServiceIPLookup before consulting
// the IP index — that lets staticLookup-only tests stay source-compatible.
type combinedLookup struct {
	routes *routeIndex
	ips    *serviceIPIndex
}

func (c *combinedLookup) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	return c.routes.LookupRoute(kind, ns, name)
}

func (c *combinedLookup) LookupByBackendService(ns, service string) (RouteAttributes, bool) {
	return c.routes.LookupByBackendService(ns, service)
}

func (c *combinedLookup) LookupServiceByIP(ip string) (string, string, bool) {
	if c.ips == nil {
		return "", "", false
	}
	return c.ips.LookupServiceByIP(ip)
}

func routeKey(kind RouteKind, ns, name string) string {
	prefix := "HTTPRoute"
	if kind == RouteKindGRPCRoute {
		prefix = "GRPCRoute"
	}
	return prefix + "|" + ns + "/" + name
}
