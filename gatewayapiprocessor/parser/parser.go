// Package parser implements the pluggable parser chain for gatewayapiprocessor.
//
// A Parser extracts route identity from incoming signal attributes. Parsers are
// consulted in order; the first one that returns Matched=true wins. The
// passthrough parser MUST be last — it always matches.
//
// See processor-spec §2.2 for the config schema and §2.5 for the test matrix.
package parser

// AttrGetter is the minimal attribute accessor the parsers need.
// It lets parsers stay independent of pdata so unit tests can pass plain maps.
type AttrGetter interface {
	Get(key string) (string, bool)
}

// MapAttrs is a trivial AttrGetter over a string map.
type MapAttrs map[string]string

func (m MapAttrs) Get(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// Result is what a parser returns when it matches a record.
type Result struct {
	// Matched is true when this parser claims the record.
	Matched bool

	// Namespace + Name identify the HTTPRoute or GRPCRoute CR.
	// Required when Matched=true.
	Namespace string
	Name      string

	// Kind is "HTTPRoute" or "GRPCRoute". Defaults to "HTTPRoute" when unset.
	Kind string

	// RuleIndex / MatchIndex are -1 when absent (envoy variants without
	// rule/match suffix — see processor-spec §2.5 TestEnvoyParser_NoRuleNoMatch).
	RuleIndex  int
	MatchIndex int

	// RawRouteName is the original opaque route id, passed through from the
	// source attribute. Populated for both envoy and passthrough parsers.
	RawRouteName string

	// ParserName is the parser id ("envoy", "linkerd", "passthrough").
	ParserName string
}

// Parser is the pluggable route-identity parser.
type Parser interface {
	// Name is the parser id ("envoy", "linkerd", "passthrough").
	Name() string
	// Controllers is the list of gatewayclass controllerName regex patterns
	// this parser claims. Empty = any controller (e.g. passthrough).
	Controllers() []string
	// Parse reads the input attributes and returns a Result.
	// Matched=false means "not mine, try the next parser".
	Parse(attrs AttrGetter) Result
}
