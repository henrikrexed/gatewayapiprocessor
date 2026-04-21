package parser

// PassthroughParser is the sink at the end of the parser chain. It always
// matches when the configured source attribute is present, copying the raw
// opaque route id to the configured passthrough attribute.
//
// processor-spec §1.3 pins the fallback attributes:
//
//	k8s.gatewayapi.raw_route_name
//	k8s.gatewayapi.parser
//
// passthrough emits Matched=true with Namespace/Name empty — the processor
// will skip HTTPRoute lookup and still stamp the raw+parser attributes.
type PassthroughParser struct {
	name                 string
	sourceAttribute      string
	passthroughAttribute string
}

func NewPassthroughParser(name, sourceAttribute, passthroughAttribute string) *PassthroughParser {
	if passthroughAttribute == "" {
		passthroughAttribute = "k8s.gatewayapi.raw_route_name"
	}
	return &PassthroughParser{
		name:                 name,
		sourceAttribute:      sourceAttribute,
		passthroughAttribute: passthroughAttribute,
	}
}

func (p *PassthroughParser) Name() string          { return p.name }
func (p *PassthroughParser) Controllers() []string { return nil }

// PassthroughAttribute exposes the configured passthrough attribute key so the
// processor can stamp it alongside k8s.gatewayapi.parser.
func (p *PassthroughParser) PassthroughAttribute() string { return p.passthroughAttribute }

func (p *PassthroughParser) Parse(attrs AttrGetter) Result {
	raw, ok := attrs.Get(p.sourceAttribute)
	if !ok || raw == "" {
		return Result{}
	}
	return Result{
		Matched:      true,
		Namespace:    "",
		Name:         "",
		RawRouteName: raw,
		ParserName:   p.name,
		RuleIndex:    -1,
		MatchIndex:   -1,
	}
}
