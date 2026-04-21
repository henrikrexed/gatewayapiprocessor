package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const envoyRegex = `^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)(?:/rule/(?P<rule>\d+))?(?:/match/(?P<match>\d+))?`

// Test 1 — Envoy parser happy path.
// processor-spec §2.5: TestEnvoyParser_HappyPath.
func TestEnvoyParser_HappyPath(t *testing.T) {
	p, err := NewEnvoyParser("envoy", "route_name", envoyRegex, nil)
	require.NoError(t, err)

	r := p.Parse(MapAttrs{"route_name": "httproute/default/api/rule/0/match/0"})
	require.True(t, r.Matched)
	assert.Equal(t, "default", r.Namespace)
	assert.Equal(t, "api", r.Name)
	assert.Equal(t, 0, r.RuleIndex)
	assert.Equal(t, 0, r.MatchIndex)
	assert.Equal(t, "HTTPRoute", r.Kind)
	assert.Equal(t, "envoy", r.ParserName)
	assert.Equal(t, "httproute/default/api/rule/0/match/0", r.RawRouteName)
}

// Test 2 — Envoy parser accepts routes with no rule/match segments.
// processor-spec §2.5: TestEnvoyParser_NoRuleNoMatch.
func TestEnvoyParser_NoRuleNoMatch(t *testing.T) {
	p, err := NewEnvoyParser("envoy", "route_name", envoyRegex, nil)
	require.NoError(t, err)

	r := p.Parse(MapAttrs{"route_name": "httproute/default/api"})
	require.True(t, r.Matched)
	assert.Equal(t, "default", r.Namespace)
	assert.Equal(t, "api", r.Name)
	assert.Equal(t, -1, r.RuleIndex, "absent rule must yield -1 so the processor skips the attribute")
	assert.Equal(t, -1, r.MatchIndex)
}

// Test 3 — Envoy parser rejects unknown format (garbage string).
// processor-spec §2.5: TestEnvoyParser_UnknownFormat.
func TestEnvoyParser_UnknownFormat(t *testing.T) {
	p, err := NewEnvoyParser("envoy", "route_name", envoyRegex, nil)
	require.NoError(t, err)

	for _, input := range []string{
		"unknown-format-string",
		"grpc/something/else",
		"",
	} {
		r := p.Parse(MapAttrs{"route_name": input})
		assert.False(t, r.Matched, "input %q should not match envoy format", input)
	}
}

// Test 4 — Linkerd parser reads split labels.
// processor-spec §2.5: TestLinkerdParser_HappyPath.
func TestLinkerdParser_HappyPath(t *testing.T) {
	p := NewLinkerdParser("linkerd", nil, LinkerdLabelKeys{
		RouteName:      "route_name",
		RouteKind:      "route_kind",
		RouteNamespace: "route_namespace",
	})

	r := p.Parse(MapAttrs{
		"route_name":      "api",
		"route_kind":      "HTTPRoute",
		"route_namespace": "default",
	})
	require.True(t, r.Matched)
	assert.Equal(t, "api", r.Name)
	assert.Equal(t, "default", r.Namespace)
	assert.Equal(t, "HTTPRoute", r.Kind)

	// Missing namespace → don't match (we can't stamp without ns).
	r = p.Parse(MapAttrs{"route_name": "api"})
	assert.False(t, r.Matched)
}

// Test 5 — Passthrough writes the raw route_name under the configured key.
// processor-spec §2.5: TestPassthrough_RawAttr.
func TestPassthroughParser_RawAttr(t *testing.T) {
	p := NewPassthroughParser("passthrough", "route_name", "k8s.gatewayapi.raw_route_name")
	r := p.Parse(MapAttrs{"route_name": "something-unparseable"})
	require.True(t, r.Matched)
	assert.Equal(t, "something-unparseable", r.RawRouteName)
	assert.Equal(t, "k8s.gatewayapi.raw_route_name", p.PassthroughAttribute())

	// Passthrough still doesn't invent (ns,name).
	assert.Empty(t, r.Namespace)
	assert.Empty(t, r.Name)

	// Empty source = no match.
	r = p.Parse(MapAttrs{"route_name": ""})
	assert.False(t, r.Matched)
	r = p.Parse(MapAttrs{})
	assert.False(t, r.Matched)
}
