package gatewayapiprocessor

// Attribute keys stamped by the processor. These are the contract surface;
// bumping any of these is a breaking change. processor-spec §1.1/§1.2 is
// the canonical source — keep that in sync with this file.
const (
	AttrGatewayName            = "k8s.gateway.name"
	AttrGatewayNamespace       = "k8s.gateway.namespace"
	AttrGatewayUID             = "k8s.gateway.uid"
	AttrGatewayListenerName    = "k8s.gateway.listener.name"
	AttrGatewayClassName       = "k8s.gatewayclass.name"
	AttrGatewayClassController = "k8s.gatewayclass.controller"

	AttrHTTPRouteName         = "k8s.httproute.name"
	AttrHTTPRouteNamespace    = "k8s.httproute.namespace"
	AttrHTTPRouteUID          = "k8s.httproute.uid"
	AttrHTTPRouteRuleIndex    = "k8s.httproute.rule_index"
	AttrHTTPRouteMatchIndex   = "k8s.httproute.match_index"
	AttrHTTPRouteParentRef    = "k8s.httproute.parent_ref"
	AttrHTTPRouteAccepted     = "k8s.httproute.accepted"
	AttrHTTPRouteResolvedRefs = "k8s.httproute.resolved_refs"

	AttrGRPCRouteName         = "k8s.grpcroute.name"
	AttrGRPCRouteNamespace    = "k8s.grpcroute.namespace"
	AttrGRPCRouteUID          = "k8s.grpcroute.uid"
	AttrGRPCRouteParentRef    = "k8s.grpcroute.parent_ref"
	AttrGRPCRouteAccepted     = "k8s.grpcroute.accepted"
	AttrGRPCRouteResolvedRefs = "k8s.grpcroute.resolved_refs"

	// AttrRouteMode discriminates the two parent shapes of Gateway API routes:
	// "ingress" (parentRef.kind=Gateway, north-south) vs "mesh" (parentRef.kind=Service, GAMMA east-west).
	// Always stamped when route enrichment matched, so dashboards can split modes
	// without parsing names.
	AttrRouteMode = "gateway.networking.k8s.io/route-mode"

	// Mesh-mode parent Service identity. Mirrors AttrGateway* for ingress mode.
	AttrParentServiceName      = "k8s.service.parent.name"
	AttrParentServiceNamespace = "k8s.service.parent.namespace"

	AttrRawRouteName = "k8s.gatewayapi.raw_route_name"
	AttrParser       = "k8s.gatewayapi.parser"

	// Policy attachment attributes (ISI-804). Written when a watched policy
	// CRD's spec.targetRefs[*] points at the matched HTTPRoute or GRPCRoute.
	//
	// All four list attributes are element-wise correlated: index `i` of every
	// list describes the same policy. AttrPolicyTargetKind is scalar because
	// it always equals the route kind the span was attributed to.
	//
	// Per ISI-804 we deliberately do NOT stamp policy.uid — store names + kinds
	// only so per-span cardinality is bounded by policy count, not policy
	// generation churn.
	AttrPolicyNames      = "k8s.gatewayapi.policy.names"
	AttrPolicyKinds      = "k8s.gatewayapi.policy.kinds"
	AttrPolicyNamespaces = "k8s.gatewayapi.policy.namespaces"
	AttrPolicyGroups     = "k8s.gatewayapi.policy.groups"
	AttrPolicyTargetKind = "k8s.gatewayapi.policy.target_kind"
)

// RouteMode values for AttrRouteMode.
const (
	RouteModeIngress = "ingress"
	RouteModeMesh    = "mesh"
)
