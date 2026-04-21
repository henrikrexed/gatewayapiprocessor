package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Accessor coverage — Name() / Controllers() pin the values wired into the
// config. They're small but carry the controller-match story per
// processor-spec §2.2.

func TestEnvoyParser_AccessorsAndControllers(t *testing.T) {
	ctrls := []string{
		`^gateway\.envoyproxy\.io/gatewayclass-controller$`,
		`^kgateway\.dev/gatewayclass-controller$`,
		`^istio\.io/gateway-controller$`,
	}
	p, err := NewEnvoyParser("envoy", "route_name", envoyRegex, ctrls)
	require.NoError(t, err)
	assert.Equal(t, "envoy", p.Name())
	assert.Equal(t, ctrls, p.Controllers())
}

func TestLinkerdParser_AccessorsAndControllers(t *testing.T) {
	ctrls := []string{`^linkerd\.io/gateway-controller$`}
	p := NewLinkerdParser("linkerd", ctrls, LinkerdLabelKeys{})
	assert.Equal(t, "linkerd", p.Name())
	assert.Equal(t, ctrls, p.Controllers())
}

func TestPassthroughParser_Accessors(t *testing.T) {
	p := NewPassthroughParser("passthrough", "route_name", "k8s.gatewayapi.raw_route_name")
	assert.Equal(t, "passthrough", p.Name())
	assert.Nil(t, p.Controllers(), "passthrough matches any controller")
}

// NewEnvoyParser surfaces regex compile errors — mirrors what
// Config.Validate guards at load time.
func TestNewEnvoyParser_InvalidRegex(t *testing.T) {
	_, err := NewEnvoyParser("envoy", "route_name", "[", nil)
	require.Error(t, err)
}

// NewLinkerdParser defaults all label keys when left blank so operators who
// run vanilla Linkerd don't need to spell out every key.
func TestNewLinkerdParser_DefaultsLabels(t *testing.T) {
	p := NewLinkerdParser("linkerd", nil, LinkerdLabelKeys{})
	r := p.Parse(MapAttrs{
		"route_name":      "api",
		"route_kind":      "HTTPRoute",
		"route_namespace": "default",
	})
	require.True(t, r.Matched)
	assert.Equal(t, "api", r.Name)
	assert.Equal(t, "HTTPRoute", r.Kind)
}

// Linkerd parser recognises GRPCRoute with case-insensitive normalisation
// ("grpcroute" → "GRPCRoute"). processor-spec §1.2 canonicalises on the
// processor side so gRPC and HTTP routes stamp their own keyspace.
func TestLinkerdParser_GRPCRouteKindNormalised(t *testing.T) {
	p := NewLinkerdParser("linkerd", nil, LinkerdLabelKeys{})
	r := p.Parse(MapAttrs{
		"route_name":      "svc",
		"route_kind":      "grpcroute", // lowercase — still accepted
		"route_namespace": "demo",
	})
	require.True(t, r.Matched)
	assert.Equal(t, "GRPCRoute", r.Kind)
}

// NewPassthroughParser defaults PassthroughAttribute to the spec key when the
// caller passes "".
func TestNewPassthroughParser_DefaultAttribute(t *testing.T) {
	p := NewPassthroughParser("passthrough", "route_name", "")
	assert.Equal(t, "k8s.gatewayapi.raw_route_name", p.PassthroughAttribute())
}

// capturedInt corner cases — non-numeric and out-of-range indexes both map
// to -1 so the processor skips rule/match stamping.
func TestCapturedInt_BadInputs(t *testing.T) {
	m := []string{"full", "default", "api", "not-a-number", ""}
	assert.Equal(t, -1, capturedInt(m, 3))  // non-numeric
	assert.Equal(t, -1, capturedInt(m, 4))  // empty
	assert.Equal(t, -1, capturedInt(m, 99)) // out-of-range
	assert.Equal(t, -1, capturedInt(m, -1))
}

// MapAttrs.Get returns (empty, false) for missing keys so parsers can cheaply
// distinguish "absent" from "present but empty".
func TestMapAttrs_GetMissing(t *testing.T) {
	a := MapAttrs{"present": "yes"}
	v, ok := a.Get("missing")
	assert.False(t, ok)
	assert.Empty(t, v)
}
