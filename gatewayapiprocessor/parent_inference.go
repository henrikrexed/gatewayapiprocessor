package gatewayapiprocessor

// nil-Kind parent-Service inference (ISI-6188 / design D4).
//
// Why this exists: per the Gateway API spec a route's parentRef.kind *defaults*
// to "Gateway" when omitted, so a well-formed GAMMA mesh route (parent = a
// Service) must set `kind: Service` explicitly. Real-world manifests — and
// OBI-derived spans that synthesize parentRefs — are inconsistent and routinely
// omit Kind. The projection path (applyParentRef) therefore misclassifies a
// nil-Kind Service parent as a (missing) Gateway, dropping it to ingress with
// no attribution.
//
// D4 keeps the spec-correct default but adds a guarded fallback: a nil-Kind
// core-group parentRef that resolves to no Gateway, yet whose ns/name names a
// real Service, is reclassified as mesh. Two guardrails bound the blast radius
// (design §4.1, Architect sign-off 2026-10-18):
//
//  1. Fire only *after* the Gateway informer HasSynced. During the initial
//     list/sync "no Gateway with this ns/name" is indistinguishable from "the
//     Gateway informer hasn't listed it yet", so inferring then would
//     false-positive every ingress route whose Gateway lists late.
//  2. On reclassify, increment the internal self-telemetry counter
//     gatewayapi_parent_inferred_total and debug-log — but stamp NO per-span
//     marker attribute. The resulting mesh attributes are the normal GAMMA
//     schema; the fact that they were *inferred* is observable only through
//     the counter + log, never on the data itself.
//
// Convergence note: the projection is cached in routeIndex at event time, so a
// route projected during the initial sync is cached as ingress (guardrail 1)
// and only reclassified on its next re-projection — a route update or the
// informer resync (cfg.Watch.ResyncPeriod, default 5m). This eventual
// convergence is the deliberate trade-off guardrail 1 accepts to avoid
// false positives during sync.

import (
	"context"

	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// inferScopeName is the instrumentation scope for the processor's internal
// self-telemetry meter.
const inferScopeName = "github.com/henrikrexed/gatewayapiprocessor/gatewayapiprocessor"

// metricParentInferred is the self-telemetry counter incremented each time D4
// reclassifies a nil-Kind parentRef as a mesh Service parent (guardrail 2).
const metricParentInferred = "gatewayapi_parent_inferred_total"

// parentInferrer carries the cross-informer dependencies the D4 inference needs
// at route-projection time. A nil *parentInferrer disables inference entirely —
// the state in no-Kubernetes mode, in projection unit tests that don't exercise
// D4, and whenever mesh.infer_service_parent is false. All methods are
// nil-safe so the projection path can hold an untyped nil.
type parentInferrer struct {
	// gatewaySynced reports whether every Gateway informer has finished its
	// initial list/sync (guardrail 1).
	gatewaySynced func() bool
	// serviceExists reports whether a core/v1 Service with this ns/name is in
	// the Service informer's name index (the second half of the D4 predicate).
	serviceExists func(ns, name string) bool
	// record increments gatewayapi_parent_inferred_total and debug-logs the
	// reclassification (guardrail 2). Never stamps a span attribute.
	record func(ns, name string, kind RouteKind)
}

// maybeInferServiceParent reports whether a parentRef that resolved to no
// Gateway should be reclassified as a mesh Service parent, and records the
// inference when it fires. It is the single D4 decision point. Returns false
// (a no-op) when inference is disabled, the ref isn't a nil-Kind core-group
// ref, the Gateway informer hasn't synced, or no Service with the ref's
// ns/name exists.
func (inf *parentInferrer) maybeInferServiceParent(pr gwv1.ParentReference, parentNS string, kind RouteKind) bool {
	if inf == nil {
		return false
	}
	if !isNilKindCoreParent(pr) {
		return false
	}
	if !inf.gatewaySynced() {
		return false
	}
	if !inf.serviceExists(parentNS, string(pr.Name)) {
		return false
	}
	inf.record(parentNS, string(pr.Name), kind)
	return true
}

// isNilKindCoreParent reports whether a ParentReference omits Kind while
// staying in the core API group — the exact "nil-Kind trap" shape that
// parentKindOf defaults to "Gateway" and isServiceParent rejects. An explicit
// Kind (even "Gateway") is NOT a candidate: a dangling explicit-Gateway
// parentRef must stay ingress and never be meshed (design D4 done-when). Both a
// nil Group and an explicit empty Group are the core API group, mirroring
// isServiceParent / formatParentRef.
func isNilKindCoreParent(pr gwv1.ParentReference) bool {
	if pr.Group != nil && *pr.Group != "" {
		return false
	}
	return pr.Kind == nil || *pr.Kind == ""
}

// newParentInferrer wires a parentInferrer from the live informer caches and a
// self-telemetry counter. Returns nil when mesh.infer_service_parent is off so
// callers can pass the nil straight into the projection path.
func newParentInferrer(
	cfg *Config,
	logger *zap.Logger,
	counter metric.Int64Counter,
	gatewaySynced func() bool,
	serviceExists func(ns, name string) bool,
) *parentInferrer {
	if !cfg.Mesh.InferServiceParentEnabled() {
		return nil
	}
	return &parentInferrer{
		gatewaySynced: gatewaySynced,
		serviceExists: serviceExists,
		record: func(ns, name string, kind RouteKind) {
			if counter != nil {
				counter.Add(context.Background(), 1)
			}
			logger.Debug("inferred mesh Service parent for nil-Kind parentRef",
				zap.String("namespace", ns),
				zap.String("service", name),
				zap.String("route_kind", kind.String()),
			)
		},
	}
}

// newParentInferredCounter builds the gatewayapi_parent_inferred_total counter
// from the processor's MeterProvider. A nil MeterProvider or a creation error
// yields a nil counter; callers treat nil as "metric disabled" and inference
// still works (debug-log only). Processor startup never blocks on a metric
// error.
func newParentInferredCounter(mp metric.MeterProvider, logger *zap.Logger) metric.Int64Counter {
	if mp == nil {
		return nil
	}
	c, err := mp.Meter(inferScopeName).Int64Counter(
		metricParentInferred,
		metric.WithDescription("Number of nil-Kind route parentRefs reclassified as mesh Service parents by the infer_service_parent fallback (ISI-6188 / GAMMA D4)."),
		metric.WithUnit("{inference}"),
	)
	if err != nil {
		logger.Warn("failed to create self-telemetry counter; inference metric disabled",
			zap.String("counter", metricParentInferred), zap.Error(err))
		return nil
	}
	return c
}
