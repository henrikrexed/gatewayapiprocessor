package gatewayapiprocessor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---- canonicalIP ----

func TestCanonicalIP_Matrix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// IPv4 — already canonical.
		{"10.108.2.156", "10.108.2.156"},
		{"127.0.0.1", "127.0.0.1"},
		// IPv6 canonicalization: net.IP.String() collapses the leading zero
		// run, so two textual forms of the same address must round-trip equal.
		{"2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		// Junk and edge cases must fail closed.
		{"", ""},
		{"not-an-ip", ""},
		{"10.108.2", ""},
		{"None", ""},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, canonicalIP(tc.in), "canonicalIP(%q)", tc.in)
	}
}

// ---- serviceIPSet ----

func TestServiceIPSet_HonorsClusterIPAndDualStack(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec: corev1.ServiceSpec{
			ClusterIP:  "10.108.2.156",
			ClusterIPs: []string{"10.108.2.156", "fd00::dead:beef"},
		},
	}
	got := serviceIPSet(svc)
	require.Len(t, got, 2)
	_, ok := got["10.108.2.156"]
	assert.True(t, ok, "v4 ClusterIP must appear")
	_, ok = got["fd00::dead:beef"]
	assert.True(t, ok, "v6 dual-stack ClusterIP must appear (canonical form)")
}

// Headless Services (clusterIP=None) and empty / unparseable values must not
// pollute the index — those would create false-positive lookups.
func TestServiceIPSet_SkipsHeadlessAndJunk(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "headless", Namespace: "demo"},
		Spec: corev1.ServiceSpec{
			ClusterIP:  corev1.ClusterIPNone,
			ClusterIPs: []string{"None", "", "garbage"},
		},
	}
	assert.Empty(t, serviceIPSet(svc))
}

// ---- serviceIPIndex CRUD ----

func TestServiceIPIndex_UpsertLookupDelete(t *testing.T) {
	idx := newServiceIPIndex()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "currency", Namespace: "otel-demo"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.110.190.183"},
	}
	idx.upsertService(svc)

	ns, name, ok := idx.LookupServiceByIP("10.110.190.183")
	require.True(t, ok, "freshly upserted ClusterIP must resolve")
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "currency", name)

	idx.deleteService("otel-demo", "currency")
	_, _, ok = idx.LookupServiceByIP("10.110.190.183")
	assert.False(t, ok, "delete must withdraw all owner-claimed IPs")
}

// IP set shrinkage between revisions: a Service that loses one of its
// dual-stack IPs (e.g., switched from PreferDualStack -> SingleStack v4) must
// not retain stale IPv6 claims. processor-spec invariant: index never points
// at an IP a Service no longer announces.
func TestServiceIPIndex_UpdateDropsStaleIPs(t *testing.T) {
	idx := newServiceIPIndex()
	first := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cart", Namespace: "otel-demo"},
		Spec: corev1.ServiceSpec{
			ClusterIP:  "10.102.38.35",
			ClusterIPs: []string{"10.102.38.35", "fd00::cafe"},
		},
	}
	idx.upsertService(first)

	// Re-upsert with v6 dropped.
	second := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cart", Namespace: "otel-demo"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.102.38.35", ClusterIPs: []string{"10.102.38.35"}},
	}
	idx.upsertService(second)

	_, _, ok := idx.LookupServiceByIP("fd00::cafe")
	assert.False(t, ok, "withdrawn dual-stack IP must not resolve after update")
	ns, name, ok := idx.LookupServiceByIP("10.102.38.35")
	require.True(t, ok)
	assert.Equal(t, "otel-demo", ns)
	assert.Equal(t, "cart", name)
}

// IPv6 lookup must work no matter which textual form the SDK sent (canonical
// vs. zero-padded). Index normalizes both sides so writers and readers agree.
func TestServiceIPIndex_IPv6FormCanonicalization(t *testing.T) {
	idx := newServiceIPIndex()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "v6svc", Namespace: "demo"},
		Spec:       corev1.ServiceSpec{ClusterIPs: []string{"2001:0db8:0000:0000:0000:0000:0000:0001"}},
	}
	idx.upsertService(svc)
	ns, name, ok := idx.LookupServiceByIP("2001:db8::1")
	require.True(t, ok)
	assert.Equal(t, "demo", ns)
	assert.Equal(t, "v6svc", name)
}

// nil Service must be a safe no-op so a panicking informer event handler
// can't take down the collector.
func TestServiceIPIndex_NilUpsertSafe(t *testing.T) {
	idx := newServiceIPIndex()
	idx.upsertService(nil) // must not panic
	assert.Empty(t, idx.m)
}

// ---- combinedLookup ----

// combinedLookup must transparently delegate every method, including the
// nil-ips defensive path so a future caller that builds it with a nil IP
// index doesn't panic.
func TestCombinedLookup_DelegatesAndHandlesNilIPIndex(t *testing.T) {
	routes := newRouteIndex()
	ips := newServiceIPIndex()
	ips.upsertService(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "demo"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.5"},
	})

	c := &combinedLookup{routes: routes, ips: ips}
	ns, name, ok := c.LookupServiceByIP("10.0.0.5")
	require.True(t, ok)
	assert.Equal(t, "demo/api", ns+"/"+name)

	// Defensive nil — the production wiring always passes a real index, but
	// callers (or tests) that construct combinedLookup themselves shouldn't
	// crash if they leave it zero-valued.
	c2 := &combinedLookup{routes: routes}
	_, _, ok = c2.LookupServiceByIP("10.0.0.5")
	assert.False(t, ok)
}
