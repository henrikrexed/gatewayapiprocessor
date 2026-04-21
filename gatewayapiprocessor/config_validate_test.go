package gatewayapiprocessor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/processor/processortest"
)

// TestConfig_Validate_Default exercises the shipped defaults end-to-end so a
// regression to createDefaultConfig() gets caught here.
func TestConfig_Validate_Default(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	require.NoError(t, cfg.Validate())
}

// validateMatrix covers every error branch in Config.Validate so the validator
// stays faithful to processor-spec §2.2.
func TestConfig_Validate_Matrix(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "invalid auth_type",
			mutate:  func(c *Config) { c.AuthType = AuthType("bogus") },
			wantErr: "invalid auth_type",
		},
		{
			name: "kubeConfig without path",
			mutate: func(c *Config) {
				c.AuthType = AuthTypeKubeConfig
				c.KubeConfigPath = ""
			},
			wantErr: "requires kube_config_path",
		},
		{
			name:    "no parsers",
			mutate:  func(c *Config) { c.Parsers = nil },
			wantErr: "at least one parser",
		},
		{
			name: "passthrough not last",
			mutate: func(c *Config) {
				c.Parsers = []ParserConfig{
					{Name: "passthrough", SourceAttribute: "route_name"},
					{Name: "envoy", SourceAttribute: "route_name", FormatRegex: `^(?P<ns>[^/]+)/(?P<name>[^/]+)`},
				}
			},
			wantErr: "passthrough parser must be last",
		},
		{
			name: "bad controller regex",
			mutate: func(c *Config) {
				c.Parsers[0].Controllers = []string{"["}
			},
			wantErr: "controller regex",
		},
		{
			name: "envoy format_regex missing named groups",
			mutate: func(c *Config) {
				c.Parsers[0] = ParserConfig{
					Name:            "envoy",
					SourceAttribute: "route_name",
					FormatRegex:     `^([^/]+)/([^/]+)`, // no named groups
				}
			},
			wantErr: "must define named groups",
		},
		{
			name: "envoy format_regex invalid syntax",
			mutate: func(c *Config) {
				c.Parsers[0] = ParserConfig{
					Name:            "envoy",
					SourceAttribute: "route_name",
					FormatRegex:     "[",
				}
			},
			wantErr: "format_regex",
		},
		{
			name:    "negative resync_period",
			mutate:  func(c *Config) { c.Watch.ResyncPeriod = -1 * time.Second },
			wantErr: "resync_period",
		},
		{
			name:    "negative informer_sync_timeout",
			mutate:  func(c *Config) { c.InformerSyncTimeout = -1 * time.Second },
			wantErr: "informer_sync_timeout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := createDefaultConfig().(*Config)
			tc.mutate(cfg)

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// buildParserChain refuses unknown parser names — config.Validate doesn't
// cover this path, it's caught at factory construction.
func TestBuildParserChain_UnknownParser(t *testing.T) {
	_, _, err := buildParserChain([]ParserConfig{{Name: "unknown"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown parser")
}

// buildParserChain also guards envoy regex compilation at factory time; the
// Validate step ran earlier, but we double-check here for defense-in-depth.
func TestBuildParserChain_EnvoyRegexError(t *testing.T) {
	_, _, err := buildParserChain([]ParserConfig{
		{Name: "envoy", SourceAttribute: "route_name", FormatRegex: "["},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "envoy parser")
}

// newProcessor must reject nil config — guards a misuse that would otherwise
// NPE on cfg.Parsers.
func TestNewProcessor_NilConfig(t *testing.T) {
	set := processortest.NewNopSettings(NewFactory().Type())
	_, err := newProcessor(set, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil config")
}
