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

	// Parent Gateway (resolved from route.spec.parentRefs[0]).
	GatewayName                 string
	GatewayNamespace            string
	GatewayUID                  string
	GatewayListenerName         string
	GatewayClassName            string
	GatewayClassControllerName  string
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
type staticLookup struct {
	routes       map[string]RouteAttributes // key = "<kind>|<ns>/<name>"
	backendIndex map[string]RouteAttributes // key = "<ns>/<service>"
}

func newStaticLookup() *staticLookup {
	return &staticLookup{
		routes:       make(map[string]RouteAttributes),
		backendIndex: make(map[string]RouteAttributes),
	}
}

func (s *staticLookup) put(kind RouteKind, ns, name string, attrs RouteAttributes) {
	s.routes[routeKey(kind, ns, name)] = attrs
}

func (s *staticLookup) putBackend(ns, svc string, attrs RouteAttributes) {
	s.backendIndex[ns+"/"+svc] = attrs
}

func (s *staticLookup) LookupRoute(kind RouteKind, ns, name string) (RouteAttributes, bool) {
	r, ok := s.routes[routeKey(kind, ns, name)]
	return r, ok
}

func (s *staticLookup) LookupByBackendService(ns, service string) (RouteAttributes, bool) {
	r, ok := s.backendIndex[ns+"/"+service]
	return r, ok
}

func routeKey(kind RouteKind, ns, name string) string {
	prefix := "HTTPRoute"
	if kind == RouteKindGRPCRoute {
		prefix = "GRPCRoute"
	}
	return prefix + "|" + ns + "/" + name
}
