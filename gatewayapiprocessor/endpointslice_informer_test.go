package gatewayapiprocessor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---- helpers ----

func ptrBool(b bool) *bool { return &b }

func svcWithClusterIP(ns, name, ip string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.ServiceSpec{ClusterIP: ip, ClusterIPs: []string{ip}},
	}
}

func endpointSlice(ns, name, svc string, addrType discoveryv1.AddressType, eps []discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{},
		},
		AddressType: addrType,
		Endpoints:   eps,
	}
	if svc != "" {
		es.Labels[discoveryv1.LabelServiceName] = svc
	}
	return es
}

// ---- endpointSliceIPSet ----

func TestEndpointSliceIPSet_HonorsIPv4Endpoints(t *testing.T) {
	es := endpointSlice("otel-demo", "ad-abc", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141", "10.244.77.142"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	got := endpointSliceIPSet(es)
	require.Len(t, got, 2)
	_, ok := got["10.244.77.141"]
	assert.True(t, ok)
	_, ok = got["10.244.77.142"]
	assert.True(t, ok)
}

func TestEndpointSliceIPSet_DualStack(t *testing.T) {
	v4 := endpointSlice("otel-demo", "ad-v4", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}},
	})
	v6 := endpointSlice("otel-demo", "ad-v6", "ad", discoveryv1.AddressTypeIPv6, []discoveryv1.Endpoint{
		{Addresses: []string{"fd00::beef"}},
	})
	idx := newPodIPIndex()
	idx.upsertEndpointSlice(v4)
	idx.upsertEndpointSlice(v6)

	ns, name, ok := idx.LookupPodIP("10.244.77.141")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad", name)

	// IPv6 lookup must work in canonicalized form.
	ns, name, ok = idx.LookupPodIP("fd00:0000:0000:0000:0000:0000:0000:beef")
	require.True(t, ok, "IPv6 PodIP must resolve regardless of textual form")
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad", name)
}

func TestEndpointSliceIPSet_SkipsNotReadyEndpoints(t *testing.T) {
	es := endpointSlice("otel-demo", "ad-abc", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
		{Addresses: []string{"10.244.77.200"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(false)}},
	})
	got := endpointSliceIPSet(es)
	require.Len(t, got, 1)
	_, ok := got["10.244.77.141"]
	assert.True(t, ok)
	_, notReady := got["10.244.77.200"]
	assert.False(t, notReady, "Ready=false endpoint must be excluded")
}

func TestEndpointSliceIPSet_ReadyNilTreatedReady(t *testing.T) {
	// API contract: nil Ready means "unknown but treat as ready".
	es := endpointSlice("otel-demo", "ad-abc", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}}, // Conditions zero, Ready=nil
	})
	got := endpointSliceIPSet(es)
	require.Len(t, got, 1)
	_, ok := got["10.244.77.141"]
	assert.True(t, ok)
}

func TestEndpointSliceIPSet_FQDNAddressTypeSkipped(t *testing.T) {
	// FQDN-typed slices store hostnames in Addresses, not IP literals — index
	// would be polluted with non-IP keys. The implementation skips them.
	es := endpointSlice("otel-demo", "external", "external", discoveryv1.AddressTypeFQDN, []discoveryv1.Endpoint{
		{Addresses: []string{"backend.example.com"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	got := endpointSliceIPSet(es)
	assert.Empty(t, got, "FQDN slices must not contribute to the IP index")
}

// ---- serviceOwnerOf ----

func TestServiceOwnerOf_LabelPreferred(t *testing.T) {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{discoveryv1.LabelServiceName: "ad"},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Service", Name: "different"},
			},
		},
	}
	assert.Equal(t, "ad", serviceOwnerOf(es))
}

func TestServiceOwnerOf_FallsBackToOwnerRef(t *testing.T) {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Service", Name: "ad"},
				{Kind: "Pod", Name: "noise"},
			},
		},
	}
	assert.Equal(t, "ad", serviceOwnerOf(es))
}

func TestServiceOwnerOf_EmptyLabelTreatedAsMissing(t *testing.T) {
	// Some controllers set the label but leave the value empty; that
	// should not pin the slice to a Service named "" — treat as missing
	// and fall through to ownerRefs.
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{discoveryv1.LabelServiceName: ""},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Service", Name: "fallback-svc"},
			},
		},
	}
	assert.Equal(t, "fallback-svc", serviceOwnerOf(es))
}

func TestServiceOwnerOf_UnownedReturnsEmpty(t *testing.T) {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{},
	}
	assert.Equal(t, "", serviceOwnerOf(es))
}

// ---- podIPIndex CRUD ----

func TestPodIPIndex_UpsertLookupDelete(t *testing.T) {
	idx := newPodIPIndex()
	es := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	idx.upsertEndpointSlice(es)

	ns, name, ok := idx.LookupPodIP("10.244.77.141")
	require.True(t, ok, "freshly upserted PodIP must resolve to the owning Service")
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad", name)

	idx.deleteEndpointSlice("otel-demo", "ad-xyz")
	_, _, ok = idx.LookupPodIP("10.244.77.141")
	assert.False(t, ok, "delete must withdraw all slice-claimed IPs")
}

// IP set shrinkage between revisions: a slice that loses an address (Pod
// scale-down, churn) must not retain stale claims.
func TestPodIPIndex_UpdateChurnDropsStaleIPs(t *testing.T) {
	idx := newPodIPIndex()
	first := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141", "10.244.77.142"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	idx.upsertEndpointSlice(first)

	// Pod 142 went away.
	second := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	idx.upsertEndpointSlice(second)

	_, _, ok := idx.LookupPodIP("10.244.77.142")
	assert.False(t, ok, "withdrawn PodIP must not resolve after slice update")
	ns, name, ok := idx.LookupPodIP("10.244.77.141")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad", name)
}

// A slice that loses its Service owner label entirely (rare, but we
// shouldn't strand its claims) — the upsert must withdraw everything.
func TestPodIPIndex_UpdateLosesOwner(t *testing.T) {
	idx := newPodIPIndex()
	first := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	idx.upsertEndpointSlice(first)

	stripped := endpointSlice("otel-demo", "ad-xyz", "" /* no svc */, discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	stripped.OwnerReferences = nil
	idx.upsertEndpointSlice(stripped)

	_, _, ok := idx.LookupPodIP("10.244.77.141")
	assert.False(t, ok, "claims must be withdrawn when slice loses its Service owner")
}

func TestPodIPIndex_SkipsUnownedSliceFromTheStart(t *testing.T) {
	idx := newPodIPIndex()
	es := endpointSlice("otel-demo", "manual", "" /* no svc */, discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	idx.upsertEndpointSlice(es)
	_, _, ok := idx.LookupPodIP("10.244.77.141")
	assert.False(t, ok, "unowned EndpointSlice must not contribute to the index")
}

func TestPodIPIndex_DeleteWithdrawsAllOwnerClaims(t *testing.T) {
	idx := newPodIPIndex()
	es := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141", "10.244.77.142", "10.244.77.143"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	idx.upsertEndpointSlice(es)
	for _, ip := range []string{"10.244.77.141", "10.244.77.142", "10.244.77.143"} {
		_, _, ok := idx.LookupPodIP(ip)
		require.True(t, ok, "pre-delete: %s must resolve", ip)
	}

	idx.deleteEndpointSlice("otel-demo", "ad-xyz")
	for _, ip := range []string{"10.244.77.141", "10.244.77.142", "10.244.77.143"} {
		_, _, ok := idx.LookupPodIP(ip)
		assert.Falsef(t, ok, "post-delete: %s must not resolve", ip)
	}
}

func TestPodIPIndex_NilAndJunkFailClosed(t *testing.T) {
	idx := newPodIPIndex()
	// nil slice must not panic.
	idx.upsertEndpointSlice(nil)
	_, _, ok := idx.LookupPodIP("")
	assert.False(t, ok)
	_, _, ok = idx.LookupPodIP("not-an-ip")
	assert.False(t, ok)
}

// ---- combinedLookup precedence (ISI-875 §combinedLookup extension) ----

func TestCombinedLookup_PreferServiceIPOverPodIP(t *testing.T) {
	// In the (rare) case where the same canonical IP lives in both the
	// ClusterIP index and the PodIP index, ClusterIP must win — Service-
	// level identity is the more specific signal.
	svcIdx := newServiceIPIndex()
	svcIdx.upsertService(svcWithClusterIP("otel-demo", "ad-svc", "10.244.77.141"))

	podIdx := newPodIPIndex()
	es := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	podIdx.upsertEndpointSlice(es)

	c := &combinedLookup{ips: svcIdx, pods: podIdx}
	ns, name, ok := c.LookupServiceByIP("10.244.77.141")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad-svc", name, "ClusterIP claim must take precedence over PodIP")
}

func TestCombinedLookup_PodIPFallback(t *testing.T) {
	// ClusterIP miss → PodIP hit.
	svcIdx := newServiceIPIndex()
	podIdx := newPodIPIndex()
	es := endpointSlice("otel-demo", "ad-xyz", "ad", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{"10.244.77.141"}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(true)}},
	})
	podIdx.upsertEndpointSlice(es)

	c := &combinedLookup{ips: svcIdx, pods: podIdx}
	ns, name, ok := c.LookupServiceByIP("10.244.77.141")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad", name)
}

func TestCombinedLookup_NilPodsIsHarmless(t *testing.T) {
	// Operators who set backendref_fallback.pod_ip=false leave c.pods nil;
	// LookupServiceByIP must still work for the ClusterIP path.
	svcIdx := newServiceIPIndex()
	svcIdx.upsertService(svcWithClusterIP("otel-demo", "currency", "10.110.190.183"))

	c := &combinedLookup{ips: svcIdx, pods: nil}
	ns, name, ok := c.LookupServiceByIP("10.110.190.183")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "currency", name)

	_, _, ok = c.LookupServiceByIP("10.244.77.141")
	assert.False(t, ok, "no pod index → PodIP literals miss cleanly")
}

// ---- staticLookup PodIP support ----

func TestStaticLookup_PutPodIPAndLookup(t *testing.T) {
	s := newStaticLookup()
	s.putPodIP("10.244.77.141", "otel-demo", "ad")
	ns, name, ok := s.LookupServiceByIP("10.244.77.141")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "ad", name)
}

func TestStaticLookup_ServiceIPBeatsPodIP(t *testing.T) {
	// Mirrors combinedLookup precedence so processor matrix tests stay
	// consistent with production behaviour.
	s := newStaticLookup()
	s.putServiceIP("10.244.77.141", "otel-demo", "ad-svc")
	s.putPodIP("10.244.77.141", "otel-demo", "ad")
	_, name, ok := s.LookupServiceByIP("10.244.77.141")
	require.True(t, ok)
	assert.Equal(t, "ad-svc", name, "Service IP must shadow PodIP for the same literal")
}

// ---- BackendRefFallback.PodIPEnabled ----

func TestBackendRefFallback_PodIPDefaultsTrue(t *testing.T) {
	b := BackendRefFallback{Enabled: true}
	assert.True(t, b.PodIPEnabled(), "nil PodIP must default enabled")
}

func TestBackendRefFallback_PodIPExplicitFalse(t *testing.T) {
	f := false
	b := BackendRefFallback{Enabled: true, PodIP: &f}
	assert.False(t, b.PodIPEnabled())
}

func TestBackendRefFallback_PodIPExplicitTrue(t *testing.T) {
	tr := true
	b := BackendRefFallback{Enabled: true, PodIP: &tr}
	assert.True(t, b.PodIPEnabled())
}
