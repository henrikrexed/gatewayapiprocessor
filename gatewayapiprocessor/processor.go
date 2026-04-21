package gatewayapiprocessor

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

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

// ambiguousBackendWarnInterval caps the rate of ambiguous-backendRef WARN logs
// so a hot pipeline cannot spam the operator. One log per 30 s is enough to
// surface the signal without flooding; the metric counter still records every
// event.
const ambiguousBackendWarnInterval = 30 * time.Second

// gatewayAPIProcessor is the runtime for all three signal types.
// A single instance is shared across traces/logs/metrics so the informer
// caches and parser chain are built once per collector.
type gatewayAPIProcessor struct {
	cfg    *Config
	logger *zap.Logger
	tel    *telemetryBuilder

	parsers            []parser.Parser
	passthroughAttrKey string // empty if no passthrough parser configured

	tracesNext  consumer.Traces
	logsNext    consumer.Logs
	metricsNext consumer.Metrics

	lookup RouteLookup

	// ambiguousBackendLastWarnNs is the unix-nanos timestamp of the last
	// ambiguous-backendRef WARN we emitted. Accessed atomically. See
	// ambiguousBackendWarnInterval for the sampling contract.
	ambiguousBackendLastWarnNs atomic.Int64

	// startHook is called during Start to warm informers. Tests override it with
	// a no-op; production binds it to newInformers(...).
	startHook func(ctx context.Context) (RouteLookup, func(context.Context) error, error)
	stopFn    func(context.Context) error
}

// shouldWarnAmbiguousBackend returns true at most once per
// ambiguousBackendWarnInterval across the whole processor, regardless of
// (ns, svc). A single global token is enough — the accompanying metric counter
// carries the precise per-outcome volume.
func (p *gatewayAPIProcessor) shouldWarnAmbiguousBackend(now time.Time) bool {
	nowNs := now.UnixNano()
	last := p.ambiguousBackendLastWarnNs.Load()
	if nowNs-last < int64(ambiguousBackendWarnInterval) {
		return false
	}
	return p.ambiguousBackendLastWarnNs.CompareAndSwap(last, nowNs)
}

func newProcessor(set processor.Settings, cfg *Config) (*gatewayAPIProcessor, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gatewayapiprocessor: nil config")
	}
	parsers, passthroughKey, err := buildParserChain(cfg.Parsers)
	if err != nil {
		return nil, err
	}
	tel, err := newTelemetryBuilder(set.TelemetrySettings, set.Logger)
	if err != nil {
		return nil, fmt.Errorf("gatewayapiprocessor: build telemetry: %w", err)
	}
	return &gatewayAPIProcessor{
		cfg:                cfg,
		logger:             set.Logger,
		tel:                tel,
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
	// INFO-level startup summary so operators can see the feature matrix in the
	// Collector log without scraping metrics first.
	p.logger.Info("gatewayapiprocessor starting",
		zap.Bool("enrich.traces", p.cfg.Enrich.Traces),
		zap.Bool("enrich.logs", p.cfg.Enrich.Logs),
		zap.Bool("enrich.metrics", p.cfg.Enrich.Metrics),
		zap.Bool("emit_status_conditions", p.cfg.EmitStatusConds),
		zap.Bool("backend_ref_fallback.enabled", p.cfg.BackendRefFallba.Enabled),
		zap.Int("parsers", len(p.parsers)),
	)

	if p.startHook == nil {
		// Non-Kubernetes mode / tests with static lookup: nothing to warm up.
		return nil
	}
	lookup, stop, err := p.startHook(ctx)
	if err != nil {
		p.logger.Error("gatewayapiprocessor informer start failed", zap.Error(err))
		return fmt.Errorf("gatewayapiprocessor start: %w", err)
	}
	p.lookup = lookup
	p.stopFn = stop
	p.logger.Info("gatewayapiprocessor informers synced")
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
		ctx = p.enrichBatch(ctx, signalTraces, td.SpanCount(), func(ctx context.Context) {
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
						p.enrich(ctx, resourceAttrs, span.Attributes(), signalTraces)
					}
				}
			}
		})
	}
	if p.tracesNext != nil {
		return p.tracesNext.ConsumeTraces(ctx, td)
	}
	return nil
}

// --- logs ---

func (p *gatewayAPIProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	if p.cfg.Enrich.Logs {
		ctx = p.enrichBatch(ctx, signalLogs, ld.LogRecordCount(), func(ctx context.Context) {
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
						p.enrich(ctx, resourceAttrs, rec.Attributes(), signalLogs)
					}
				}
			}
		})
	}
	if p.logsNext != nil {
		return p.logsNext.ConsumeLogs(ctx, ld)
	}
	return nil
}

// --- metrics ---

func (p *gatewayAPIProcessor) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	if p.cfg.Enrich.Metrics {
		ctx = p.enrichBatch(ctx, signalMetrics, md.DataPointCount(), func(ctx context.Context) {
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
						p.enrichMetric(ctx, resourceAttrs, m)
					}
				}
			}
		})
	}
	if p.metricsNext != nil {
		return p.metricsNext.ConsumeMetrics(ctx, md)
	}
	return nil
}

// enrichBatch wraps a single ConsumeX call with a self-telemetry span and
// enrichment_duration histogram observation. It returns the span-scoped ctx so
// the inner enrichment calls inherit the span parentage.
//
// Spec §3.3 "No self-enrichment loop": we stamp `gatewayapiprocessor.self=true`
// on the span so a downstream pipeline that fans spans back in can filter
// them out with a single attribute predicate.
func (p *gatewayAPIProcessor) enrichBatch(ctx context.Context, signal signalKind, items int, body func(context.Context)) context.Context {
	signalStr := signalString(signal)
	start := time.Now()
	ctx, span := p.tel.startEnrichBatchSpan(ctx, signalStr, items)
	defer func() {
		// Record the duration before ending the span so exemplars attached to
		// the histogram observation can link back to this batch span's context.
		elapsed := time.Since(start).Seconds()
		p.tel.recordEnrichmentDuration(ctx, signalStr, elapsed)
		span.End()
	}()
	body(ctx)
	return ctx
}

// signalString maps signalKind → the closed-set label value used on metrics
// and spans. Kept in lockstep with the signal* constants in telemetry.go.
func signalString(s signalKind) string {
	switch s {
	case signalTraces:
		return signalTracesStr
	case signalLogs:
		return signalLogsStr
	case signalMetrics:
		return signalMetricsStr
	}
	return "unknown"
}

// enrichMetric applies enrichment to every data point of the metric, because
// metric-level attributes live on data points (not on the Metric struct).
func (p *gatewayAPIProcessor) enrichMetric(ctx context.Context, resourceAttrs pcommon.Map, m pmetric.Metric) {
	//exhaustive:enforce
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(ctx, resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(ctx, resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(ctx, resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(ctx, resourceAttrs, dps.At(i).Attributes(), signalMetrics)
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			p.enrich(ctx, resourceAttrs, dps.At(i).Attributes(), signalMetrics)
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
func (p *gatewayAPIProcessor) enrich(ctx context.Context, resourceAttrs, recordAttrs pcommon.Map, signal signalKind) {
	signalStr := signalString(signal)
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
		if p.cfg.BackendRefFallba.Enabled {
			p.applyBackendRefFallback(ctx, view, recordAttrs, signal)
			return
		}
		p.tel.recordEnrichment(ctx, signalStr, outcomeDropped)
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
		p.tel.recordEnrichment(ctx, signalStr, outcomeStamped)
		return
	}

	p.stampRouteIdentity(recordAttrs, matched)
	p.stampCRMetadata(ctx, recordAttrs, matched)
	p.maybeStripMetrics(recordAttrs, signal)
	p.tel.recordEnrichment(ctx, signalStr, outcomeStamped)
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
func (p *gatewayAPIProcessor) stampCRMetadata(ctx context.Context, attrs pcommon.Map, r parser.Result) {
	kind := RouteKindHTTPRoute
	if r.Kind == "GRPCRoute" {
		kind = RouteKindGRPCRoute
	}
	ra, ok := p.lookup.LookupRoute(kind, r.Namespace, r.Name)
	if !ok {
		return
	}
	p.stampRouteAttrs(ctx, attrs, ra)
}

// stampRouteAttrs writes all enrichment fields from a RouteAttributes.
// Shared by direct-parse path and backendref_fallback path.
func (p *gatewayAPIProcessor) stampRouteAttrs(ctx context.Context, attrs pcommon.Map, ra RouteAttributes) {
	// UID is stamped under the HTTPRoute key only; gRPC routes keep just
	// name/namespace per processor-spec §1.2 table row 9-10.
	if ra.UID != "" && ra.Kind != RouteKindGRPCRoute {
		putString(attrs, AttrHTTPRouteUID, ra.UID)
	}
	if ra.ParentRef != "" && ra.Kind != RouteKindGRPCRoute {
		putString(attrs, AttrHTTPRouteParentRef, ra.ParentRef)
	}
	if p.cfg.EmitStatusConds && ra.Kind != RouteKindGRPCRoute {
		stamped := false
		if ra.Accepted != nil {
			attrs.PutBool(AttrHTTPRouteAccepted, *ra.Accepted)
			stamped = true
		}
		if ra.ResolvedRefs != nil {
			attrs.PutBool(AttrHTTPRouteResolvedRefs, *ra.ResolvedRefs)
			stamped = true
		}
		if stamped {
			p.tel.recordStatusCondStamped(ctx)
		}
	}
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
}

// applyBackendRefFallback tries to resolve a route via the server.address →
// HTTPRoute index when no parser matched.
func (p *gatewayAPIProcessor) applyBackendRefFallback(ctx context.Context, view combinedView, recordAttrs pcommon.Map, signal signalKind) {
	signalStr := signalString(signal)
	key := p.cfg.BackendRefFallba.SourceAttribute
	if key == "" {
		p.tel.recordBackendRefFallback(ctx, outcomeUnresolved)
		p.tel.recordEnrichment(ctx, signalStr, outcomeDropped)
		return
	}
	addr, ok := view.Get(key)
	if !ok || addr == "" {
		p.tel.recordBackendRefFallback(ctx, outcomeUnresolved)
		p.tel.recordEnrichment(ctx, signalStr, outcomeDropped)
		return
	}
	ns, svc := splitAddress(addr)
	if ns == "" || svc == "" {
		p.tel.recordBackendRefFallback(ctx, outcomeUnresolved)
		p.tel.recordEnrichment(ctx, signalStr, outcomeDropped)
		return
	}
	ra, ok := p.lookup.LookupByBackendService(ns, svc)
	if !ok {
		// A miss is either "no owner" or "index dropped it because multiple
		// routes claimed the same Service". The informer-backed index exposes
		// IsAmbiguousBackend to tell the two apart; the static test lookup
		// does not implement it and falls through to "unresolved".
		outcome := outcomeUnresolved
		enrichOutcome := outcomeDropped
		if c, ok2 := p.lookup.(interface {
			IsAmbiguousBackend(ns, svc string) bool
		}); ok2 && c.IsAmbiguousBackend(ns, svc) {
			outcome = outcomeAmbiguous
			enrichOutcome = outcomeAmbiguousOwner
			if p.shouldWarnAmbiguousBackend(time.Now()) {
				p.logger.Warn("gatewayapiprocessor: ambiguous backendRef ownership (sampled)",
					zap.String("namespace", ns),
					zap.String("service", svc),
					zap.String("source_attribute", key),
					zap.String("signal", signalStr),
					zap.Duration("sample_interval", ambiguousBackendWarnInterval),
				)
			}
		}
		p.tel.recordBackendRefFallback(ctx, outcome)
		p.tel.recordEnrichment(ctx, signalStr, enrichOutcome)
		return
	}
	// Synthesize a parser result so we can share the stamping path.
	pr := parser.Result{
		Matched:    true,
		Namespace:  ra.Namespace,
		Name:       ra.Name,
		Kind:       "HTTPRoute",
		RuleIndex:  -1,
		MatchIndex: -1,
		ParserName: "backendref_fallback",
	}
	p.stampRouteIdentity(recordAttrs, pr)
	p.stampRouteAttrs(ctx, recordAttrs, ra)
	putString(recordAttrs, AttrParser, pr.ParserName)
	p.maybeStripMetrics(recordAttrs, signal)
	p.tel.recordBackendRefFallback(ctx, outcomeResolved)
	p.tel.recordEnrichment(ctx, signalStr, outcomeStamped)
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
