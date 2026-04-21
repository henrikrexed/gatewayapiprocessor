package parser

import "strings"

// LinkerdParser reads Linkerd's already-split route labels.
// Linkerd emits route_name / route_kind / route_namespace as separate labels —
// no parsing needed, just pick them up. See processor-spec §2.2 and the
// Linkerd test cases in §2.5 (TestLinkerdParser_HappyPath).
type LinkerdParser struct {
	name        string
	controllers []string
	labels      LinkerdLabelKeys
}

// LinkerdLabelKeys maps semantic roles to Linkerd label keys. Mirrors
// Config.LinkerdLabelsConfig in the parent package.
type LinkerdLabelKeys struct {
	RouteName      string
	RouteKind      string
	RouteNamespace string
	ParentName     string
}

func NewLinkerdParser(name string, controllers []string, keys LinkerdLabelKeys) *LinkerdParser {
	if keys.RouteName == "" {
		keys.RouteName = "route_name"
	}
	if keys.RouteKind == "" {
		keys.RouteKind = "route_kind"
	}
	if keys.RouteNamespace == "" {
		keys.RouteNamespace = "route_namespace"
	}
	if keys.ParentName == "" {
		keys.ParentName = "parent_name"
	}
	return &LinkerdParser{
		name:        name,
		controllers: controllers,
		labels:      keys,
	}
}

func (p *LinkerdParser) Name() string          { return p.name }
func (p *LinkerdParser) Controllers() []string { return p.controllers }

func (p *LinkerdParser) Parse(attrs AttrGetter) Result {
	name, ok := attrs.Get(p.labels.RouteName)
	if !ok || name == "" {
		return Result{}
	}
	ns, _ := attrs.Get(p.labels.RouteNamespace)
	kind, _ := attrs.Get(p.labels.RouteKind)
	if ns == "" {
		// Can't stamp route attrs without a namespace — surface the raw name so
		// operators can still find the record via the raw route label.
		return Result{}
	}

	// Normalize kind; default to HTTPRoute. Accept common casings.
	switch strings.ToLower(kind) {
	case "grpcroute":
		kind = "GRPCRoute"
	default:
		kind = "HTTPRoute"
	}

	return Result{
		Matched:      true,
		Namespace:    ns,
		Name:         name,
		Kind:         kind,
		RuleIndex:    -1,
		MatchIndex:   -1,
		RawRouteName: name,
		ParserName:   p.name,
	}
}
