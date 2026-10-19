package gatewayapiprocessor

import (
	"fmt"
	"regexp"
	"strings"
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

	Watch              WatchConfig        `mapstructure:"watch"`
	Parsers            []ParserConfig     `mapstructure:"parsers"`
	Enrich             EnrichConfig       `mapstructure:"enrich"`
	EmitStatusConds    bool               `mapstructure:"emit_status_conditions"`
	BackendRefFallback BackendRefFallback `mapstructure:"backendref_fallback"`
	Mesh               MeshConfig         `mapstructure:"mesh"`

	// InformerSyncTimeout bounds Start() waiting for informer caches to warm up.
	// Default 30s per processor-spec §2.4.
	InformerSyncTimeout time.Duration `mapstructure:"informer_sync_timeout"`
}

type WatchConfig struct {
	Namespaces   []string      `mapstructure:"namespaces"`
	ResyncPeriod time.Duration `mapstructure:"resync_period"`

	// Policies enumerates the Gateway API policy CRDs to watch via dynamic
	// informers. Each entry stamps k8s.gatewayapi.policy.* attributes on every
	// span/log/metric whose route matches policy.spec.targetRefs[*]. Empty
	// disables policy enrichment — the processor still works exactly as
	// before. The default zero-config policy set is intentionally empty so we
	// never auto-watch CRDs the user didn't ask for; populate this from your
	// collector config when policy attachment exists in the cluster.
	Policies []PolicyGVR `mapstructure:"policies"`
}

// PolicyGVR identifies a Gateway API policy CRD by its dynamic
// (group, version, resource) coordinates. The processor builds one dynamic
// informer per entry. We use GVR (not GVK) so the informer factory can look
// up the resource directly without a discovery round-trip.
type PolicyGVR struct {
	Group    string `mapstructure:"group"`
	Version  string `mapstructure:"version"`
	Resource string `mapstructure:"resource"`
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
	Enabled bool `mapstructure:"enabled"`

	// SourceAttribute is the legacy (single) attribute the fallback reads to
	// resolve a Service DNS name. Kept for backward compatibility — new
	// configs should prefer SourceAttributes (plural).
	SourceAttribute string `mapstructure:"source_attribute"`

	// SourceAttributes is the ordered list of attribute keys the fallback
	// inspects. The first key whose value decodes to "<svc>.<ns>.*" and
	// matches the route index wins. Defaults to ["server.address",
	// "net.peer.name"] so both modern (1.20+) and legacy OTel sem-conv
	// resolve. processor-spec §1.3.
	SourceAttributes []string `mapstructure:"source_attributes"`

	// PodIP toggles the EndpointSlice informer that powers PodIP-literal
	// fallback resolution (ISI-875). nil = default = enabled, so opting
	// into BackendRefFallback.Enabled also opts into PodIP coverage. Set
	// to an explicit false on scale-sensitive clusters where the
	// cluster-wide EndpointSlice watch is too costly. The Phase 1
	// ClusterIP path (Service informer) is always on when Enabled=true;
	// this knob only gates the additional EndpointSlice watch.
	PodIP *bool `mapstructure:"pod_ip"`
}

// MeshConfig holds the GAMMA (Gateway API for Mesh) enrichment knobs added in
// ISI-6178. All of it is additive and defaults to the behaviour described in
// the approved B.1 design doc (ISI-6175, decisions D1–D6).
type MeshConfig struct {
	// WaypointClassNames enumerates the gatewayClassName values that identify a
	// mesh waypoint Gateway. A route whose parent Gateway uses one of these
	// classes is classified as mesh (§4.2) and stamped with a waypoint
	// implementation (§4.3). Data-driven so a new mesh impl needs a config
	// change, not a code change (D2/D3). Empty disables class-based waypoint
	// detection. Default: [istio-waypoint, kgateway-waypoint, istio-eastwest].
	WaypointClassNames []string `mapstructure:"waypoint_class_names"`

	// ClassifyRouteRole toggles producer/consumer role classification on mesh
	// routes (§4.4). nil = default = enabled. Set false to suppress the
	// k8s.gatewayapi.route.role attribute.
	ClassifyRouteRole *bool `mapstructure:"classify_route_role"`

	// InferServiceParent toggles the nil-Kind parent-Service inference fallback
	// (§4.1 / D4). nil = default = enabled. When a core-group parentRef omits
	// Kind and no Gateway with that name exists once caches are warm, the route
	// is reclassified as mesh. The inference machinery itself is tracked
	// separately (follow-up to ISI-6178); this flag reserves the config surface
	// so operators can pre-set it and the default is stable.
	InferServiceParent *bool `mapstructure:"infer_service_parent"`
}

// ClassifyRouteRoleEnabled reports whether producer/consumer classification is
// on. Defaults to true (nil pointer) per D4-adjacent §4.4.
func (m MeshConfig) ClassifyRouteRoleEnabled() bool {
	return m.ClassifyRouteRole == nil || *m.ClassifyRouteRole
}

// InferServiceParentEnabled reports whether nil-Kind inference is on. Defaults
// to true (nil pointer) per D4.
func (m MeshConfig) InferServiceParentEnabled() bool {
	return m.InferServiceParent == nil || *m.InferServiceParent
}

// WaypointEnabled reports whether any waypoint detection is configured. Drives
// the decoupled Service-informer start gate (D5): the waypoint label-cache is
// only built when at least one waypoint class is configured.
func (m MeshConfig) WaypointEnabled() bool {
	return len(m.WaypointClassNames) > 0
}

// waypointImplForClass maps a gatewayClassName to its waypoint implementation
// enum, or "" when the class is not a configured waypoint class. The mapping is
// name-substring based so operator-defined classes that embed the vendor token
// (e.g. "prod-istio-waypoint") still resolve; exact configured names always
// match. Precedence: istio-eastwest before istio (east-west is the more
// specific Istio form), then kgateway, then generic istio-ambient.
func (m MeshConfig) waypointImplForClass(className string) string {
	if className == "" {
		return ""
	}
	matched := false
	for _, c := range m.WaypointClassNames {
		if c == className {
			matched = true
			break
		}
	}
	if !matched {
		return ""
	}
	switch {
	case strings.Contains(className, "eastwest") || strings.Contains(className, "east-west"):
		return WaypointImplIstioEastWest
	case strings.Contains(className, "kgateway"):
		return WaypointImplKgateway
	default:
		// Any other configured waypoint class is treated as an Istio ambient
		// waypoint (istio-waypoint is the canonical GAMMA class name).
		return WaypointImplIstioAmbient
	}
}

// PodIPEnabled reports whether the EndpointSlice informer that drives the
// PodIP fallback path should be started. Defaults to true so the feature
// is on as soon as BackendRefFallback.Enabled flips on; an explicit
// `pod_ip: false` in YAML disables it.
func (b BackendRefFallback) PodIPEnabled() bool {
	return b.PodIP == nil || *b.PodIP
}

// effectiveSourceAttrs returns the ordered attribute keys this config uses
// for the backendref_fallback path. Combines SourceAttributes and the legacy
// singular SourceAttribute (in that order, deduped). Returns nil only when
// both are empty.
func (b BackendRefFallback) effectiveSourceAttrs() []string {
	if len(b.SourceAttributes) == 0 && b.SourceAttribute == "" {
		return nil
	}
	out := make([]string, 0, len(b.SourceAttributes)+1)
	seen := map[string]struct{}{}
	add := func(k string) {
		if k == "" {
			return
		}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	for _, k := range b.SourceAttributes {
		add(k)
	}
	add(b.SourceAttribute)
	return out
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

	for i, p := range c.Watch.Policies {
		if p.Version == "" || p.Resource == "" {
			return fmt.Errorf("gatewayapiprocessor: watch.policies[%d] requires version and resource (group may be empty for core API)", i)
		}
	}

	for i, name := range c.Mesh.WaypointClassNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("gatewayapiprocessor: mesh.waypoint_class_names[%d] must not be empty", i)
		}
	}

	return nil
}
