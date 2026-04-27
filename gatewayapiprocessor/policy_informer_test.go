package gatewayapiprocessor

// Unit tests for the dynamic policy informer (ISI-804).
//
// Strategy: exercise policyAdd / policyUpdate / policyDelete and the helper
// projections (policyAccepted, targetsFromUnstructured) directly against an
// in-memory routeIndex. We don't spin up a fake informer / fake clientset —
// those would only test the client-go plumbing, not our logic. The handlers
// themselves are 4 lines of cast + dispatch.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// trafficPolicyU is a small builder for kgateway TrafficPolicy unstructured
// objects. Tests pass a list of target tuples; helper writes them as
// spec.targetRefs[]. status.conditions Accepted=True is on by default — pass
// `accepted=false` to suppress.
type policyOpts struct {
	name    string
	ns      string
	kind    string                // default "TrafficPolicy"
	targets []targetSpec          // spec.targetRefs[]
	status  string                // "accepted", "rejected", "no-status", "ancestor-accepted"
	extra   map[string]any        // extra spec fields
	useSing bool                  // write spec.targetRef (singular) instead
}

type targetSpec struct {
	Group string
	Kind  string
	Name  string
	NS    string
}

func mkPolicy(opts policyOpts) *unstructured.Unstructured {
	if opts.kind == "" {
		opts.kind = "TrafficPolicy"
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("gateway.kgateway.dev/v1alpha1")
	u.SetKind(opts.kind)
	u.SetName(opts.name)
	if opts.ns != "" {
		u.SetNamespace(opts.ns)
	}
	refs := make([]any, 0, len(opts.targets))
	for _, t := range opts.targets {
		ref := map[string]any{
			"group": t.Group,
			"kind":  t.Kind,
			"name":  t.Name,
		}
		if t.NS != "" {
			ref["namespace"] = t.NS
		}
		refs = append(refs, ref)
	}
	if len(refs) > 0 {
		if opts.useSing {
			_ = unstructured.SetNestedMap(u.Object, refs[0].(map[string]any), "spec", "targetRef")
		} else {
			_ = unstructured.SetNestedSlice(u.Object, refs, "spec", "targetRefs")
		}
	}
	switch opts.status {
	case "", "accepted":
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"type": "Accepted", "status": "True"},
		}, "status", "conditions")
	case "rejected":
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{"type": "Accepted", "status": "False"},
		}, "status", "conditions")
	case "ancestor-accepted":
		_ = unstructured.SetNestedSlice(u.Object, []any{
			map[string]any{
				"conditions": []any{
					map[string]any{"type": "Accepted", "status": "True"},
				},
			},
		}, "status", "ancestors")
	case "no-status":
		// no status block at all — optimistic accept
	}
	return u
}

// ---- targetsFromUnstructured ----

func TestTargetsFromUnstructured_HTTPRouteAndGRPCRoute(t *testing.T) {
	u := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"},
			{Group: "gateway.networking.k8s.io", Kind: "GRPCRoute", Name: "checkout", NS: "other"},
		},
	})
	got := targetsFromUnstructured(u)
	require.Len(t, got, 2)
	assert.Equal(t, policyTarget{Kind: RouteKindHTTPRoute, NS: "demo", Name: "api"}, got[0])
	assert.Equal(t, policyTarget{Kind: RouteKindGRPCRoute, NS: "other", Name: "checkout"}, got[1])
}

func TestTargetsFromUnstructured_SkipsOutOfScopeRefs(t *testing.T) {
	u := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{
			{Group: "gateway.networking.k8s.io", Kind: "Gateway", Name: "public"}, // not enriched
			{Group: "", Kind: "Service", Name: "svc"},                             // wrong group
			{Group: "gateway.networking.k8s.io", Kind: "TCPRoute", Name: "tcp"},   // unsupported route kind
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"},  // valid
		},
	})
	got := targetsFromUnstructured(u)
	require.Len(t, got, 1)
	assert.Equal(t, "api", got[0].Name)
}

func TestTargetsFromUnstructured_DefaultsNamespaceToPolicyNS(t *testing.T) {
	u := mkPolicy(policyOpts{
		name: "rl", ns: "policy-ns",
		targets: []targetSpec{
			// No namespace on the targetRef — must default to policy-ns.
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"},
		},
	})
	got := targetsFromUnstructured(u)
	require.Len(t, got, 1)
	assert.Equal(t, "policy-ns", got[0].NS)
}

func TestTargetsFromUnstructured_AcceptsSingularTargetRef(t *testing.T) {
	u := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"},
		},
		useSing: true,
	})
	got := targetsFromUnstructured(u)
	require.Len(t, got, 1, "spec.targetRef (singular) must also be recognised")
	assert.Equal(t, "api", got[0].Name)
}

func TestTargetsFromUnstructured_EmptyAndNil(t *testing.T) {
	assert.Nil(t, targetsFromUnstructured(nil))
	u := &unstructured.Unstructured{}
	u.SetKind("TrafficPolicy")
	u.SetName("empty")
	assert.Empty(t, targetsFromUnstructured(u), "no spec.targetRefs and no spec.targetRef → empty")
}

// ---- policyAccepted ----

func TestPolicyAccepted_TopLevelConditionsAcceptedTrue(t *testing.T) {
	u := mkPolicy(policyOpts{name: "p", ns: "demo", status: "accepted"})
	assert.True(t, policyAccepted(u))
}

func TestPolicyAccepted_TopLevelConditionsAcceptedFalse(t *testing.T) {
	u := mkPolicy(policyOpts{name: "p", ns: "demo", status: "rejected"})
	assert.False(t, policyAccepted(u))
}

func TestPolicyAccepted_AncestorAcceptedTrue(t *testing.T) {
	u := mkPolicy(policyOpts{name: "p", ns: "demo", status: "ancestor-accepted"})
	assert.True(t, policyAccepted(u),
		"GEP-2648-style status.ancestors[].conditions must be honored")
}

func TestPolicyAccepted_NoStatusBlockOptimisticAccept(t *testing.T) {
	u := mkPolicy(policyOpts{name: "p", ns: "demo", status: "no-status"})
	assert.True(t, policyAccepted(u),
		"newly-created policies without status must enrich during reconcile window")
}

// ---- policyAdd / policyUpdate / policyDelete ----

func TestPolicyAdd_AppliesAcceptedPolicy(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo", UID: "uid",
	}, nil)

	u := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"}},
	})
	policyAdd(u, PolicyGVR{Group: "gateway.kgateway.dev", Version: "v1alpha1", Resource: "trafficpolicies"}, idx)

	ra, ok := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.True(t, ok)
	require.Len(t, ra.Policies, 1)
	assert.Equal(t, "rl", ra.Policies[0].Name)
	assert.Equal(t, "TrafficPolicy", ra.Policies[0].Kind)
	assert.Equal(t, "gateway.kgateway.dev", ra.Policies[0].Group,
		"PolicyRef.Group must come from configured GVR, not the CR's apiVersion")
}

func TestPolicyAdd_SkipsRejectedPolicy(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{
		Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo",
	}, nil)
	u := mkPolicy(policyOpts{
		name: "rl", ns: "demo", status: "rejected",
		targets: []targetSpec{{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"}},
	})
	policyAdd(u, PolicyGVR{Group: "gateway.kgateway.dev"}, idx)

	ra, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Empty(t, ra.Policies, "rejected policy must not stamp")
}

func TestPolicyUpdate_DiffsTargetSet(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo"}, nil)
	idx.upsertHTTPRoute(RouteAttributes{Kind: RouteKindHTTPRoute, Name: "checkout", Namespace: "demo"}, nil)

	gvr := PolicyGVR{Group: "gateway.kgateway.dev"}

	// v1: targets api + checkout
	v1 := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"},
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "checkout"},
		},
	})
	policyAdd(v1, gvr, idx)

	ra, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Len(t, ra.Policies, 1)
	ra, _ = idx.LookupRoute(RouteKindHTTPRoute, "demo", "checkout")
	assert.Len(t, ra.Policies, 1)

	// v2: targets api only — checkout must lose its policy stamp
	v2 := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{
			{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"},
		},
	})
	policyUpdate(v1, v2, gvr, idx)

	ra, _ = idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Len(t, ra.Policies, 1, "api still targeted — keeps stamp")
	ra, _ = idx.LookupRoute(RouteKindHTTPRoute, "demo", "checkout")
	assert.Empty(t, ra.Policies, "checkout removed from targetRefs — stamp must drop")
}

func TestPolicyUpdate_AcceptedFlapWithdrawsStamps(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo"}, nil)
	gvr := PolicyGVR{Group: "gateway.kgateway.dev"}

	old := mkPolicy(policyOpts{
		name: "rl", ns: "demo", // accepted
		targets: []targetSpec{{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"}},
	})
	policyAdd(old, gvr, idx)

	// Same targets, but Accepted flips to False — must withdraw the stamp.
	newU := mkPolicy(policyOpts{
		name: "rl", ns: "demo", status: "rejected",
		targets: []targetSpec{{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"}},
	})
	policyUpdate(old, newU, gvr, idx)

	ra, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Empty(t, ra.Policies, "Accepted flipping to False must withdraw the stamp")
}

func TestPolicyDelete_RemovesUnconditionally(t *testing.T) {
	idx := newRouteIndex()
	idx.upsertHTTPRoute(RouteAttributes{Kind: RouteKindHTTPRoute, Name: "api", Namespace: "demo"}, nil)
	gvr := PolicyGVR{Group: "gateway.kgateway.dev"}

	u := mkPolicy(policyOpts{
		name: "rl", ns: "demo",
		targets: []targetSpec{{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "api"}},
	})
	policyAdd(u, gvr, idx)
	ra, _ := idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	require.Len(t, ra.Policies, 1)

	// Delete must remove the stamp even if the deleted CR is somehow
	// "unaccepted" at the moment of deletion — drains the index cleanly.
	policyDelete(u, gvr, idx)
	ra, _ = idx.LookupRoute(RouteKindHTTPRoute, "demo", "api")
	assert.Empty(t, ra.Policies)
}
