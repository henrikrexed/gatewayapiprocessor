package gatewayapiprocessor

// Dynamic informer for Gateway API policy attachment CRDs (ISI-804).
//
// Why dynamic, not typed: any CRD that follows the Gateway API "policy
// attachment" pattern carries spec.targetRefs[] of policy targets. We don't
// want to depend on a kgateway/Envoy Gateway/upstream clientset — let the
// user list the GVRs they want watched in cfg.Watch.Policies and use the
// dynamic informer factory to build one informer per GVR.
//
// What lands on the index: PolicyRef{name, namespace, kind, group}. UID is
// deliberately not part of the ref per Henrik's ISI-804 direction — store
// names and CRD identity, not generation churn.
//
// Acceptance gate: only stamp policies whose status condition tree carries
// Accepted=True. Two condition shapes are supported (kgateway uses the first;
// GEP-2648 controllers use the second):
//   1. status.conditions[type=Accepted,status=True]
//   2. status.ancestors[*].conditions[type=Accepted,status=True]
// A policy with no status block at all is optimistically accepted so newly
// created CRs still enrich during the controller's reconcile window —
// otherwise there's a quiet gap where every new policy is invisible.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// policyTarget is a (kind, ns, name) identity of a route a policy targets.
// Kept distinct from routeKey() so target-extraction is testable in isolation
// without dragging in the index lock discipline.
type policyTarget struct {
	Kind RouteKind
	NS   string
	Name string
}

// startPolicyInformers builds one dynamic shared informer per
// cfg.Watch.Policies entry and registers Add/Update/Delete handlers that
// project each policy's spec.targetRefs[] onto the route index.
//
// Returns the started informers so the caller (newInformers) can include
// them in its global cache.WaitForCacheSync. When cfg.Watch.Policies is
// empty, returns (nil, nil) — policy enrichment stays off and the processor
// behaves exactly as it did before ISI-804.
func startPolicyInformers(
	ctx context.Context,
	logger *zap.Logger,
	restCfg *rest.Config,
	cfg *Config,
	idx *routeIndex,
) ([]cache.SharedIndexInformer, error) {
	if len(cfg.Watch.Policies) == 0 {
		return nil, nil
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	resync := cfg.Watch.ResyncPeriod
	if resync == 0 {
		resync = 5 * time.Minute
	}

	// Mirror the route-informer namespace-scoping discipline from informer.go:
	// empty Namespaces → one factory watching all namespaces, otherwise one
	// scoped factory per namespace so we don't fan out a watch we can't see.
	var factories []dynamicinformer.DynamicSharedInformerFactory
	if len(cfg.Watch.Namespaces) == 0 {
		factories = append(factories,
			dynamicinformer.NewDynamicSharedInformerFactory(dyn, resync),
		)
	} else {
		for _, ns := range cfg.Watch.Namespaces {
			factories = append(factories,
				dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, resync, ns, nil),
			)
		}
	}

	var informers []cache.SharedIndexInformer
	for _, f := range factories {
		for _, p := range cfg.Watch.Policies {
			gvr := schema.GroupVersionResource{
				Group:    p.Group,
				Version:  p.Version,
				Resource: p.Resource,
			}
			inf := f.ForResource(gvr).Informer()
			registerPolicyHandlers(inf, idx, p, logger)
			informers = append(informers, inf)
		}
		f.Start(ctx.Done())
	}

	return informers, nil
}

func registerPolicyHandlers(
	inf cache.SharedIndexInformer,
	idx *routeIndex,
	gvr PolicyGVR,
	logger *zap.Logger,
) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			policyAdd(u, gvr, idx)
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldU, _ := oldObj.(*unstructured.Unstructured)
			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			policyUpdate(oldU, newU, gvr, idx)
		},
		DeleteFunc: func(obj any) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if u2, ok2 := t.Obj.(*unstructured.Unstructured); ok2 {
						policyDelete(u2, gvr, idx)
						return
					}
				}
				logger.Debug("policy delete: unexpected tombstone type")
				return
			}
			policyDelete(u, gvr, idx)
		},
	})
}

// policyAdd applies a freshly-discovered policy to the index, gated on
// Accepted=True. Idempotent — applyPolicy dedupes.
func policyAdd(u *unstructured.Unstructured, gvr PolicyGVR, idx *routeIndex) {
	if !policyAccepted(u) {
		return
	}
	ref := policyRefFromUnstructured(u, gvr)
	for _, t := range targetsFromUnstructured(u) {
		idx.applyPolicy(t.Kind, t.NS, t.Name, ref)
	}
}

// policyUpdate diffs old vs new target sets so that:
//   - targets in old but not new are unstamped (route used to have this
//     policy, no longer does — e.g. user removed a targetRef);
//   - targets in new but not old are freshly stamped;
//   - targets in both are left alone (applyPolicy is idempotent anyway).
//
// Acceptance state is independently evaluated for each side: a policy that
// transitioned Accepted=False→True is treated as "old targets empty, new
// targets full" and vice versa. This way a flapping Accepted condition does
// not leave stale stamps when the controller withdraws acceptance.
func policyUpdate(oldU, newU *unstructured.Unstructured, gvr PolicyGVR, idx *routeIndex) {
	ref := policyRefFromUnstructured(newU, gvr)

	var oldTargets []policyTarget
	if oldU != nil && policyAccepted(oldU) {
		oldTargets = targetsFromUnstructured(oldU)
	}
	var newTargets []policyTarget
	if policyAccepted(newU) {
		newTargets = targetsFromUnstructured(newU)
	}

	oldSet := targetSet(oldTargets)
	newSet := targetSet(newTargets)

	for k, t := range oldSet {
		if _, keep := newSet[k]; keep {
			continue
		}
		idx.removePolicy(t.Kind, t.NS, t.Name, ref)
	}
	for k, t := range newSet {
		if _, had := oldSet[k]; had {
			continue
		}
		idx.applyPolicy(t.Kind, t.NS, t.Name, ref)
	}
}

// policyDelete unstamps every target the policy used to point at. Unlike
// add/update, delete does NOT gate by Accepted=True — even an unaccepted
// policy might have been stamped via a prior accepted revision, and we want
// the index to drain cleanly when the CR goes away.
func policyDelete(u *unstructured.Unstructured, gvr PolicyGVR, idx *routeIndex) {
	ref := policyRefFromUnstructured(u, gvr)
	for _, t := range targetsFromUnstructured(u) {
		idx.removePolicy(t.Kind, t.NS, t.Name, ref)
	}
}

func targetSet(ts []policyTarget) map[string]policyTarget {
	m := make(map[string]policyTarget, len(ts))
	for _, t := range ts {
		m[fmt.Sprintf("%d|%s/%s", t.Kind, t.NS, t.Name)] = t
	}
	return m
}

// policyRefFromUnstructured projects a policy CR's metadata into the
// PolicyRef shape the route index stores. Group comes from the configured
// PolicyGVR (not from the CR itself) so the user-supplied configuration is
// the canonical source — this avoids surprises when a CRD reports an
// inconsistent apiVersion.
func policyRefFromUnstructured(u *unstructured.Unstructured, gvr PolicyGVR) PolicyRef {
	return PolicyRef{
		Name:      u.GetName(),
		Namespace: u.GetNamespace(),
		Kind:      u.GetKind(),
		Group:     gvr.Group,
	}
}

// policyAccepted decides whether a policy's status indicates Accepted=True.
// Returns true in three cases:
//  1. status.conditions has Accepted=True (kgateway / inherited-style).
//  2. Any status.ancestors[*].conditions has Accepted=True (GEP-2648 direct
//     attachment, used by Envoy Gateway and others).
//  3. status block is entirely absent — the controller hasn't reconciled
//     the CR yet. Treat as accepted so newly-created policies still enrich
//     during the reconcile window. Once the controller writes a status, the
//     real conditions take over.
//
// Otherwise returns false.
func policyAccepted(u *unstructured.Unstructured) bool {
	if u == nil {
		return false
	}
	_, hasStatus, _ := unstructured.NestedMap(u.Object, "status")
	if !hasStatus {
		return true
	}
	if conds, ok, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); ok {
		if conditionsHaveAcceptedTrue(conds) {
			return true
		}
	}
	if ancs, ok, _ := unstructured.NestedSlice(u.Object, "status", "ancestors"); ok {
		for _, a := range ancs {
			am, isMap := a.(map[string]any)
			if !isMap {
				continue
			}
			c, isSlice := am["conditions"].([]any)
			if !isSlice {
				continue
			}
			if conditionsHaveAcceptedTrue(c) {
				return true
			}
		}
	}
	return false
}

func conditionsHaveAcceptedTrue(conds []any) bool {
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		if t == "Accepted" && s == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}

// targetsFromUnstructured reads spec.targetRefs[] (Gateway API policy
// attachment v1alpha2) and returns only refs in our enrichment scope —
// HTTPRoute and GRPCRoute in the gateway.networking.k8s.io group.
//
// Refs to other resources (Gateway, Service, Backend, etc.) are silently
// skipped; the processor doesn't enrich those today and a future feature
// extension would land them here.
//
// Namespace defaulting: an unset targetRef.namespace defaults to the
// policy's own namespace, per the Gateway API spec.
//
// Some early controllers used spec.targetRef (singular). When targetRefs is
// missing but targetRef is present, we treat it as a one-element slice so a
// kgateway 1.x style CR is still recognised.
func targetsFromUnstructured(u *unstructured.Unstructured) []policyTarget {
	if u == nil {
		return nil
	}
	policyNS := u.GetNamespace()
	refs, found, _ := unstructured.NestedSlice(u.Object, "spec", "targetRefs")
	if !found {
		if single, ok, _ := unstructured.NestedMap(u.Object, "spec", "targetRef"); ok {
			refs = []any{single}
		}
	}
	out := make([]policyTarget, 0, len(refs))
	for _, r := range refs {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		group, _ := rm["group"].(string)
		kind, _ := rm["kind"].(string)
		name, _ := rm["name"].(string)
		ns, _ := rm["namespace"].(string)
		if name == "" {
			continue
		}
		if !strings.EqualFold(group, "gateway.networking.k8s.io") {
			continue
		}
		var rk RouteKind
		switch kind {
		case "HTTPRoute":
			rk = RouteKindHTTPRoute
		case "GRPCRoute":
			rk = RouteKindGRPCRoute
		default:
			continue
		}
		if ns == "" {
			ns = policyNS
		}
		out = append(out, policyTarget{Kind: rk, NS: ns, Name: name})
	}
	return out
}
