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

	AttrGRPCRouteName      = "k8s.grpcroute.name"
	AttrGRPCRouteNamespace = "k8s.grpcroute.namespace"

	AttrRawRouteName = "k8s.gatewayapi.raw_route_name"
	AttrParser       = "k8s.gatewayapi.parser"
)
