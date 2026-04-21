package gatewayapiprocessor

import (
	"fmt"
	"regexp"
	"time"
)

// AuthType is the Kubernetes client auth mode.
type AuthType string

const (
	AuthTypeServiceAccount AuthType = "serviceAccount"
	AuthTypeKubeConfig     AuthType = "kubeConfig"
	AuthTypeNone           AuthType = "none"
)

// Config is the gatewayapiprocessor processor config.
//
// Keep this shape aligned with processor-spec §2.2 (ISI-670#document-processor-spec).
// YAML field names reuse the mapstructure tags.
type Config struct {
	AuthType       AuthType `mapstructure:"auth_type"`
	KubeConfigPath string   `mapstructure:"kube_config_path"`

	Watch            WatchConfig        `mapstructure:"watch"`
	Parsers          []ParserConfig     `mapstructure:"parsers"`
	Enrich           EnrichConfig       `mapstructure:"enrich"`
	EmitStatusConds  bool               `mapstructure:"emit_status_conditions"`
	BackendRefFallba BackendRefFallback `mapstructure:"backendref_fallback"`

	// InformerSyncTimeout bounds Start() waiting for informer caches to warm up.
	// Default 30s per processor-spec §2.4.
	InformerSyncTimeout time.Duration `mapstructure:"informer_sync_timeout"`
}

type WatchConfig struct {
	Namespaces   []string      `mapstructure:"namespaces"`
	ResyncPeriod time.Duration `mapstructure:"resync_period"`
}

// ParserConfig is a single entry in the parser plug-in chain.
// Parsers run in order; the first one that yields a non-empty (namespace,name)
// wins. "passthrough" MUST be last.
type ParserConfig struct {
	Name string `mapstructure:"name"`

	// Controllers is a list of regex patterns applied against
	// GatewayClass.spec.controllerName. Empty = match any.
	Controllers []string `mapstructure:"controllers"`

	// SourceAttribute is the signal attribute carrying the opaque route id
	// (envoy/passthrough parsers).
	SourceAttribute string `mapstructure:"source_attribute"`

	// FormatRegex is the named-capture regex for envoy-family parsers.
	// Expected named groups: ns, name, rule (optional), match (optional).
	FormatRegex string `mapstructure:"format_regex"`

	// LinkerdLabels maps semantic roles to the Linkerd source-attribute names.
	LinkerdLabels LinkerdLabelsConfig `mapstructure:"linkerd_labels"`

	// PassthroughAttribute is the attribute key written for unparsable strings.
	PassthroughAttribute string `mapstructure:"passthrough_attribute"`
}

type LinkerdLabelsConfig struct {
	RouteName      string `mapstructure:"route_name"`
	RouteKind      string `mapstructure:"route_kind"`
	RouteNamespace string `mapstructure:"route_namespace"`
	ParentName     string `mapstructure:"parent_name"`
}

type EnrichConfig struct {
	Traces  bool `mapstructure:"traces"`
	Logs    bool `mapstructure:"logs"`
	Metrics bool `mapstructure:"metrics"`

	// ExcludeFromMetricAttributes is the cardinality guard — attributes listed
	// here are stripped before the record is emitted on the metrics pipeline.
	// processor-spec §1.4 flags these as the Istio Telemetry footgun.
	ExcludeFromMetricAttributes []string `mapstructure:"exclude_from_metric_attributes"`
}

type BackendRefFallback struct {
	Enabled         bool   `mapstructure:"enabled"`
	SourceAttribute string `mapstructure:"source_attribute"`
}

// Validate enforces processor-spec invariants at config load time.
func (c *Config) Validate() error {
	switch c.AuthType {
	case "", AuthTypeServiceAccount, AuthTypeKubeConfig, AuthTypeNone:
	default:
		return fmt.Errorf("gatewayapiprocessor: invalid auth_type %q (want serviceAccount|kubeConfig|none)", c.AuthType)
	}

	if c.AuthType == AuthTypeKubeConfig && c.KubeConfigPath == "" {
		return fmt.Errorf("gatewayapiprocessor: auth_type=kubeConfig requires kube_config_path")
	}

	if len(c.Parsers) == 0 {
		return fmt.Errorf("gatewayapiprocessor: at least one parser must be configured")
	}

	// processor-spec §2.2: passthrough MUST be last in the chain.
	for i, p := range c.Parsers {
		if p.Name == "passthrough" && i != len(c.Parsers)-1 {
			return fmt.Errorf("gatewayapiprocessor: passthrough parser must be last (found at index %d of %d)", i, len(c.Parsers))
		}
		for _, pat := range p.Controllers {
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("gatewayapiprocessor: parser %q controller regex %q: %w", p.Name, pat, err)
			}
		}
		if p.Name == "envoy" && p.FormatRegex != "" {
			re, err := regexp.Compile(p.FormatRegex)
			if err != nil {
				return fmt.Errorf("gatewayapiprocessor: parser %q format_regex: %w", p.Name, err)
			}
			names := re.SubexpNames()
			hasNs, hasName := false, false
			for _, n := range names {
				if n == "ns" {
					hasNs = true
				}
				if n == "name" {
					hasName = true
				}
			}
			if !hasNs || !hasName {
				return fmt.Errorf("gatewayapiprocessor: parser %q format_regex must define named groups 'ns' and 'name'", p.Name)
			}
		}
	}

	if c.Watch.ResyncPeriod < 0 {
		return fmt.Errorf("gatewayapiprocessor: watch.resync_period must be >= 0")
	}

	if c.InformerSyncTimeout < 0 {
		return fmt.Errorf("gatewayapiprocessor: informer_sync_timeout must be >= 0")
	}

	return nil
}
