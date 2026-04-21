package parser

import (
	"regexp"
	"strconv"
)

// EnvoyParser reads the opaque Envoy-family route_name string and decodes it
// into (namespace, name, rule_index, match_index). The format is observed
// across Envoy Gateway, Kgateway, and Istio — it is NOT a stable contract, so
// the regex is configurable (see processor-spec §2.2 `format_regex`).
//
// Canonical format:
//   httproute/<namespace>/<name>/rule/<rule_index>/match/<match_index>
// Trailing /rule and /match segments are optional — TestEnvoyParser_NoRuleNoMatch
// enforces that partial strings still yield (namespace, name).
type EnvoyParser struct {
	name            string
	controllers     []string
	sourceAttribute string
	re              *regexp.Regexp
	nsIdx           int
	nameIdx         int
	ruleIdx         int
	matchIdx        int
}

// NewEnvoyParser compiles the regex and pre-resolves the named-capture indexes.
// The regex must contain named groups 'ns' and 'name'; 'rule' and 'match' are optional.
func NewEnvoyParser(name, sourceAttribute, formatRegex string, controllers []string) (*EnvoyParser, error) {
	re, err := regexp.Compile(formatRegex)
	if err != nil {
		return nil, err
	}
	p := &EnvoyParser{
		name:            name,
		controllers:     controllers,
		sourceAttribute: sourceAttribute,
		re:              re,
		nsIdx:           -1,
		nameIdx:         -1,
		ruleIdx:         -1,
		matchIdx:        -1,
	}
	for i, n := range re.SubexpNames() {
		switch n {
		case "ns":
			p.nsIdx = i
		case "name":
			p.nameIdx = i
		case "rule":
			p.ruleIdx = i
		case "match":
			p.matchIdx = i
		}
	}
	return p, nil
}

func (p *EnvoyParser) Name() string          { return p.name }
func (p *EnvoyParser) Controllers() []string { return p.controllers }

func (p *EnvoyParser) Parse(attrs AttrGetter) Result {
	raw, ok := attrs.Get(p.sourceAttribute)
	if !ok || raw == "" {
		return Result{}
	}
	m := p.re.FindStringSubmatch(raw)
	if m == nil || p.nsIdx < 0 || p.nameIdx < 0 {
		return Result{}
	}
	ns := m[p.nsIdx]
	name := m[p.nameIdx]
	if ns == "" || name == "" {
		return Result{}
	}
	return Result{
		Matched:      true,
		Namespace:    ns,
		Name:         name,
		Kind:         "HTTPRoute",
		RuleIndex:    capturedInt(m, p.ruleIdx),
		MatchIndex:   capturedInt(m, p.matchIdx),
		RawRouteName: raw,
		ParserName:   p.name,
	}
}

// capturedInt returns the int value of an optional named capture group.
// Returns -1 when the group is absent or empty — callers treat -1 as "do not stamp".
func capturedInt(m []string, idx int) int {
	if idx < 0 || idx >= len(m) {
		return -1
	}
	s := m[idx]
	if s == "" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}
