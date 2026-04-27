package gatewayapiprocessor

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.uber.org/zap"

	"github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor/parser"
)

// gatewayAPIProcessor is the runtime for all three signal types.
// A single instance is shared across traces/logs/metrics so the informer
// caches and parser chain are built once per collector.
type gatewayAPIProcessor struct {
	cfg    *Config
	logger *zap.Logger

	parsers            []parser.Parser
	passthroughAttrKey string // empty if no passthrough parser configured

	tracesNext  consumer.Traces
	logsNext    consumer.Logs
	metricsNext consumer.Metrics

	lookup RouteLookup

	// startHook is called during Start to warm informers. Tests override it with
	// a no-op; production binds it to newInformers(...).
	startHook func(ctx context.Context) (RouteLookup, func(context.Context) error, error)
	stopFn    func(context.Context) error
}

func newProcessor(set processor.Settings, cfg *Config) (*gatewayAPIProcessor, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gatewayapiprocessor: nil config")
	}
	parsers, passthroughKey, err := buildParserChain(cfg.Parsers)
	if err != nil {
		return nil, err
	}
	return &gatewayAPIProcessor{
		cfg:                cfg,
		logger:             set.Logger,
		parsers:            parsers,
		passthroughAttrKey: passthroughKey,
		lookup:             newStaticLookup(), // replaced in Start() when informers wire up
	}, nil
}

func buildParserChain(pcs []ParserConfig) ([]parser.Parser, string, error) {
	out := make([]parser.Parser, 0, len(pcs))
	passthroughKey := ""
	for _, pc := range pcs {
		switch pc.Name {
		case "envoy":
			p, err := parser.NewEnvoyParser(pc.Name, pc.SourceAttribute, pc.FormatRegex, pc.Controllers)
			if err != nil {
				return nil, "", fmt.Errorf("envoy parser: %w", err)
			}
			out = append(out, p)
		case "linkerd":
			out = append(out, parser.NewLinkerdParser(pc.Name, pc.Controllers, parser.LinkerdLabelKeys{
				RouteName:      pc.LinkerdLabels.RouteName,
				RouteKind:      pc.LinkerdLabels.RouteKind,
				RouteNamespace: pc.LinkerdLabels.RouteNamespace,
				ParentName:     pc.LinkerdLabels.ParentName,
			}))
		case "passthrough":
			pp := parser.NewPassthroughParser(pc.Name, pc.SourceAttribute, pc.PassthroughAttribute)
			passthroughKey = pp.PassthroughAttribute()
			out = append(out, pp)
		default:
			return nil, "", fmt.Errorf("unknown parser %q", pc.Name)
		}
	}
	return out, passthroughKey, nil
}

// --- component.Component ---

func (p *gatewayAPIProcessor) Start(ctx context.Context, _ component.Host) error {
	if p.startHook == nil {
		// Non-Kubernetes mode / tests with static lookup: nothing to warm up.
		return nil
	}
	lookup, stop, err := p.startHook(ctx)
	if err != nil {
		return fmt.Errorf("gatewayapiprocessor start: %w", err)
	}
	p.lookup = lookup
	p.stopFn = stop
	return nil
}

func (p *gatewayAPIProcessor) Shutdown(ctx context.Context) error {
	if p.stopFn != nil {
		return p.stopFn(ctx)
	}
	return nil
}

func (p *gatewayAPIProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

// --- traces ---

func (p *gatewayAPIProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	if p.cfg.Enrich.Traces {
		rss := td.ResourceSpans()
		for i := 0; i < rss.Len(); i++ {
			rs := rss.At(i)
			resourceAttrs := rs.Resource().Attributes()
			sss := rs.ScopeSpans()
			for j := 0; j < sss.Len(); j++ {
				ss := sss.At(j)
				spans := ss.Spans()
				for k := 0; k < spans.Len(); k++ {
					span := spans.At(k)
					p.enrich(resourceAttrs, span.Attributes(), signalTraces)
				}
			}
		}
	}
	if p.tracesNext != nil {
		return p.tracesNext.ConsumeTraces(ctx, td)
	}
	return nil
}

// --- logs ---

func (p *gatewayAPIProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	if p.cfg.Enrich.Logs {
		rls := ld.ResourceLogs()
		for i := 0; i < rls.Len(); i++ {
			rl := rls.At(i)
			resourceAttrs := rl.Resource().Attributes()
			sls := rl.ScopeLogs()
			for j := 0; j < sls.Len(); j++ {
				sl := sls.At(j)
				recs := sl.LogRecords()
				for k := 0; k < recs.Len(); k++ {
					rec := recs.At(k)
					p.enrich(resourceAttrs, rec.Attributes(), signalLogs)
				}
			}
		}
	}
	if p.logsNext != nil {
		return p.logsNext.ConsumeLogs(ctx, ld)
	}
	return nil
}

// --- metrics ---

func (p *gatewayAPIProcessor) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	if p.cfg.Enrich.Metrics {
		rms := md.ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			rm := rms.At(i)
			resourceAttrs := rm.Resource().Attributes()
			sms := rm.ScopeMetrics()
			for j := 0; j < sms.Len(); j++ {
				sm := sms.At(j)
				ms := sm.Metrics()
				for k := 0; k < ms.Len(); k++ {
					m := ms.At(k)
					p.enrichMetric(resourceAttrs, m)
				}
			}
		}
	}
	if p.metricsNext != nil {
		return p.metricsNext.ConsumeMetrics(ctx, md)
	}
	return nil
}

// enrichMetric applies enrichment to every data point of the metric, because
// metric-level attributes live on data points (not on the Metric struct).
func (p *gatewayAPIProcessor) enrichMetric(resourceAttrs pcommon.Map, m pmetric.Metric) {
	//exhaustive:enforce
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	}
}

type signalKind int

const (
	signalTraces signalKind = iota
	signalLogs
	signalMetrics
)

// enrich is the single enrichment path shared by all three signals.
//
// Algorithm (processor-spec §2.4 ConsumeTraces/Logs/Metrics):
//  1. Build a combined attribute view (record > resource).
//  2. Run parser chain; first matched wins.
//  3. If passthrough matched: stamp raw + parser attrs, no CR lookup, done.
//  4. Otherwise: stamp route attrs directly from parser, then enrich with CR
//     metadata via lookup (gateway/gatewayclass/status).
//  5. If backendref_fallback is enabled and no parser matched, try the
//     server.address → HTTPRoute index.
//  6. For signalMetrics, strip exclude_from_metric_attributes keys.
func (p *gatewayAPIProcessor) enrich(resourceAttrs, recordAttrs pcommon.Map, signal signalKind) {
	view := combinedView{record: recordAttrs, resource: resourceAttrs}

	var matched parser.Result
	var any bool
	for _, pr := range p.parsers {
		res := pr.Parse(view)
		if res.Matched {
			matched = res
			any = true
			break
		}
	}

	if !any {
		if p.cfg.BackendRefFallback.Enabled {
			p.applyBackendRefFallback(view, recordAttrs, signal)
		}
		return
	}

	// Stamp raw + parser id for every matched result (envoy/linkerd/passthrough).
	if matched.RawRouteName != "" && p.passthroughAttrKey != "" {
		// passthroughAttrKey is set when a passthrough parser exists; that attr
		// is also the cardinality-sensitive metric strip target, so we only
		// stamp raw routes when it's configured.
		putString(recordAttrs, p.passthroughAttrKey, matched.RawRouteName)
	}
	if matched.ParserName != "" {
		putString(recordAttrs, AttrParser, matched.ParserName)
	}

	// Passthrough doesn't carry (namespace, name) — stop here.
	if matched.Namespace == "" || matched.Name == "" {
		p.maybeStripMetrics(recordAttrs, signal)
		return
	}

	p.stampRouteIdentity(recordAttrs, matched)
	p.stampCRMetadata(recordAttrs, matched)
	p.maybeStripMetrics(recordAttrs, signal)
}

// stampRouteIdentity writes attributes that come straight from the parser
// result (no CR lookup required).
func (p *gatewayAPIProcessor) stampRouteIdentity(attrs pcommon.Map, r parser.Result) {
	switch r.Kind {
	case "GRPCRoute":
		putString(attrs, AttrGRPCRouteName, r.Name)
		putString(attrs, AttrGRPCRouteNamespace, r.Namespace)
	default:
		putString(attrs, AttrHTTPRouteName, r.Name)
		putString(attrs, AttrHTTPRouteNamespace, r.Namespace)
		if r.RuleIndex >= 0 {
			attrs.PutInt(AttrHTTPRouteRuleIndex, int64(r.RuleIndex))
		}
		if r.MatchIndex >= 0 {
			attrs.PutInt(AttrHTTPRouteMatchIndex, int64(r.MatchIndex))
		}
	}
}

// stampCRMetadata enriches with gateway/gatewayclass fields read from the
// informer cache. Absent informers (static lookup, cache miss) → no-op; the
// record still carries the parser-derived route identity.
func (p *gatewayAPIProcessor) stampCRMetadata(attrs pcommon.Map, r parser.Result) {
	kind := RouteKindHTTPRoute
	if r.Kind == "GRPCRoute" {
		kind = RouteKindGRPCRoute
	}
	ra, ok := p.lookup.LookupRoute(kind, r.Namespace, r.Name)
	if !ok {
		return
	}
	p.stampRouteAttrs(attrs, ra)
}

// stampRouteAttrs writes all enrichment fields from a RouteAttributes.
// Shared by direct-parse path and backendref_fallback path.
func (p *gatewayAPIProcessor) stampRouteAttrs(attrs pcommon.Map, ra RouteAttributes) {
	if ra.UID != "" {
		if ra.Kind == RouteKindGRPCRoute {
			putString(attrs, AttrGRPCRouteUID, ra.UID)
		} else {
			putString(attrs, AttrHTTPRouteUID, ra.UID)
		}
	}
	if ra.ParentRef != "" {
		if ra.Kind == RouteKindGRPCRoute {
			putString(attrs, AttrGRPCRouteParentRef, ra.ParentRef)
		} else {
			putString(attrs, AttrHTTPRouteParentRef, ra.ParentRef)
		}
	}
	if p.cfg.EmitStatusConds {
		acceptedKey := AttrHTTPRouteAccepted
		resolvedKey := AttrHTTPRouteResolvedRefs
		if ra.Kind == RouteKindGRPCRoute {
			acceptedKey = AttrGRPCRouteAccepted
			resolvedKey = AttrGRPCRouteResolvedRefs
		}
		if ra.Accepted != nil {
			attrs.PutBool(acceptedKey, *ra.Accepted)
		}
		if ra.ResolvedRefs != nil {
			attrs.PutBool(resolvedKey, *ra.ResolvedRefs)
		}
	}
	// Always stamp the route-mode discriminator. Empty defaults to ingress for
	// back-compat with RouteAttributes constructed by older code paths/tests.
	mode := ra.RouteMode
	if mode == "" {
		mode = RouteModeIngress
	}
	putString(attrs, AttrRouteMode, mode)

	if ra.GatewayName != "" {
		putString(attrs, AttrGatewayName, ra.GatewayName)
	}
	if ra.GatewayNamespace != "" {
		putString(attrs, AttrGatewayNamespace, ra.GatewayNamespace)
	}
	if ra.GatewayUID != "" {
		putString(attrs, AttrGatewayUID, ra.GatewayUID)
	}
	if ra.GatewayListenerName != "" {
		putString(attrs, AttrGatewayListenerName, ra.GatewayListenerName)
	}
	if ra.GatewayClassName != "" {
		putString(attrs, AttrGatewayClassName, ra.GatewayClassName)
	}
	if ra.GatewayClassControllerName != "" {
		putString(attrs, AttrGatewayClassController, ra.GatewayClassControllerName)
	}

	// GAMMA mesh-mode: stamp parent Service identity.
	if ra.ParentServiceName != "" {
		putString(attrs, AttrParentServiceName, ra.ParentServiceName)
	}
	if ra.ParentServiceNamespace != "" {
		putString(attrs, AttrParentServiceNamespace, ra.ParentServiceNamespace)
	}

	stampPolicyAttrs(attrs, ra)
}

// stampPolicyAttrs writes the k8s.gatewayapi.policy.* array attributes when
// at least one Gateway API policy targets the matched route. ISI-804 contract:
//
//   - names/kinds/namespaces/groups are parallel arrays — index i of every
//     array describes the same policy.
//   - target_kind is a scalar that mirrors the matched route kind.
//   - No policy.uid by deliberate decision (Henrik 2026-04-27): keep
//     per-span cardinality bounded by policy count, not generation churn.
//   - Empty Policies slice → no stamp (clean omission for routes with no
//     attached policy).
func stampPolicyAttrs(attrs pcommon.Map, ra RouteAttributes) {
	if len(ra.Policies) == 0 {
		return
	}
	names := attrs.PutEmptySlice(AttrPolicyNames)
	kinds := attrs.PutEmptySlice(AttrPolicyKinds)
	namespaces := attrs.PutEmptySlice(AttrPolicyNamespaces)
	groups := attrs.PutEmptySlice(AttrPolicyGroups)
	for _, p := range ra.Policies {
		names.AppendEmpty().SetStr(p.Name)
		kinds.AppendEmpty().SetStr(p.Kind)
		namespaces.AppendEmpty().SetStr(p.Namespace)
		groups.AppendEmpty().SetStr(p.Group)
	}
	targetKind := "HTTPRoute"
	if ra.Kind == RouteKindGRPCRoute {
		targetKind = "GRPCRoute"
	}
	putString(attrs, AttrPolicyTargetKind, targetKind)
}

// resourceAttrK8sNamespace is the OTel semantic convention key carrying the
// workload's Kubernetes namespace on the resource. Used by the bare-hostname
// fallback (ISI-802) to qualify spans where server.address is just the bare
// service name (e.g. "cart") — the default OTel SDK auto-instrumentation
// shape for in-cluster traffic.
const resourceAttrK8sNamespace = "k8s.namespace.name"

// applyBackendRefFallback tries to resolve a route via the
// server.address / net.peer.name → route index when no parser matched.
//
// Walks p.cfg.BackendRefFallback.effectiveSourceAttrs() in order; first
// non-empty value that decodes to <svc>.<ns>.* and matches the index wins.
// Supports both modern sem-conv (server.address, 1.20+) and legacy OTel
// (net.peer.name) so auto-instrumentation that hasn't migrated still resolves.
func (p *gatewayAPIProcessor) applyBackendRefFallback(view combinedView, recordAttrs pcommon.Map, signal signalKind) {
	keys := p.cfg.BackendRefFallback.effectiveSourceAttrs()
	if len(keys) == 0 {
		return
	}
	var ra RouteAttributes
	var matched bool
	for _, key := range keys {
		raw, ok := view.Get(key)
		if !ok || raw == "" {
			continue
		}
		addr := normalizeSourceAddr(key, raw)
		if addr == "" {
			continue
		}
		ns, svc := splitAddress(addr)
		if ns == "" || svc == "" {
			// Bare hostname (e.g. "cart") — the OTel SDK auto-instrumentation
			// emits these for in-cluster traffic. Fall back to the resource's
			// k8s.namespace.name and treat addr as the bare service name.
			// processor-spec §1.3 (ISI-802).
			if !strings.ContainsRune(addr, '.') {
				if resNs, ok := view.Get(resourceAttrK8sNamespace); ok && resNs != "" {
					ns, svc = resNs, addr
				}
			}
			if ns == "" || svc == "" {
				continue
			}
		}
		if r, ok := p.lookup.LookupByBackendService(ns, svc); ok {
			ra = r
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	// Synthesize a parser result so we can share the stamping path.
	kind := "HTTPRoute"
	if ra.Kind == RouteKindGRPCRoute {
		kind = "GRPCRoute"
	}
	pr := parser.Result{
		Matched:    true,
		Namespace:  ra.Namespace,
		Name:       ra.Name,
		Kind:       kind,
		RuleIndex:  -1,
		MatchIndex: -1,
		ParserName: "backendref_fallback",
	}
	p.stampRouteIdentity(recordAttrs, pr)
	p.stampRouteAttrs(recordAttrs, ra)
	putString(recordAttrs, AttrParser, pr.ParserName)
	p.maybeStripMetrics(recordAttrs, signal)
}

// maybeStripMetrics removes cardinality-sensitive attributes on metrics only.
// processor-spec §1.4: *.uid must never land on metric pipelines.
func (p *gatewayAPIProcessor) maybeStripMetrics(attrs pcommon.Map, signal signalKind) {
	if signal != signalMetrics {
		return
	}
	for _, k := range p.cfg.Enrich.ExcludeFromMetricAttributes {
		attrs.Remove(k)
	}
}

// combinedView reads first from record attributes, then falls back to
// resource attributes. Most data planes put route_name on record; resource
// fallback covers collectors that lift labels to the resource.
type combinedView struct {
	record   pcommon.Map
	resource pcommon.Map
}

func (v combinedView) Get(key string) (string, bool) {
	if val, ok := v.record.Get(key); ok {
		return val.AsString(), true
	}
	if val, ok := v.resource.Get(key); ok {
		return val.AsString(), true
	}
	return "", false
}

func putString(m pcommon.Map, key, val string) {
	m.PutStr(key, val)
}

// normalizeSourceAddr takes the raw value of a source attribute and returns
// just the host component, suitable for splitAddress / bare-hostname lookup.
//
// Modern sem-conv attributes (server.address, net.peer.name) are returned
// as-is after stripping a trailing :port if present (spec says these should
// not include a port, but legacy SDKs sometimes embed one).
//
// Legacy URL-bearing attributes (http.url, url.full) are parsed as URLs;
// the URL host is returned (without the port). http.host is treated as
// host[:port] per the legacy convention.
//
// Returns "" when the value cannot be parsed into a usable host. ISI-802
// follow-up: required to enrich routes for SDKs that only emit http.url
// (e.g. otel-demo's load-generator and Envoy ingress spans).
func normalizeSourceAddr(key, raw string) string {
	switch key {
	case "http.url", "url.full":
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return ""
		}
		return u.Hostname()
	case "http.host":
		return stripPort(raw)
	default:
		// server.address, net.peer.name, and any user-configured attribute:
		// treat as host[:port]. Strip the port defensively.
		return stripPort(raw)
	}
}

// stripPort removes a trailing ":<port>" from a host string. IPv6 addresses
// (which contain colons) are left untouched — splitAddress already returns
// empty for them, and the bare-hostname fallback skips values containing
// dots (which IPv6 strings do via "::" or hex form).
func stripPort(host string) string {
	// IPv6 literal in URL-style brackets: "[::1]:8080" → "[::1]"
	if strings.HasPrefix(host, "[") {
		if i := strings.Index(host, "]"); i > 0 {
			return host[1:i]
		}
		return host
	}
	// Don't try to strip from raw IPv6 (more than one ':')
	if strings.Count(host, ":") > 1 {
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		return host[:i]
	}
	return host
}

// splitAddress extracts (namespace, service) from a Kubernetes Service DNS
// name like "api-service.demo.svc.cluster.local" or "api-service.demo".
// Returns ("","") for addresses we can't decode (raw IPs, external hosts).
func splitAddress(addr string) (string, string) {
	svc, ns := "", ""
	// parse dot-separated segments; need at least <svc>.<ns>
	dot := -1
	for i := 0; i < len(addr); i++ {
		if addr[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(addr)-1 {
		return "", ""
	}
	svc = addr[:dot]
	rest := addr[dot+1:]
	// namespace is the next segment before the next dot (or to end-of-string)
	nsEnd := len(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i] == '.' {
			nsEnd = i
			break
		}
	}
	ns = rest[:nsEnd]
	if ns == "" {
		return "", ""
	}
	return ns, svc
}
