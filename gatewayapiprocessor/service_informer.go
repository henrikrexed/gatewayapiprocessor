package gatewayapiprocessor

// Service-IP reverse-lookup informer (ISI-851).
//
// Why this exists: an audit on ISI-838 showed ~20% of otel-demo spans carry an
// IP-literal `server.address` (a Service ClusterIP, sometimes a PodIP) instead
// of the canonical "<svc>.<ns>.svc.cluster.local" DNS name. The original
// backendref_fallback path can only resolve DNS-shaped values — IP literals
// fall through `splitAddress` as garbage namespaces ("10" / "108" for
// 10.108.x.y) and the bare-hostname fallback skips them because they contain
// dots. The result was 3,128/15,244 spans/2h going un-enriched on the
// observable-gateapiprocess cluster.
//
// What this adds: a core/v1.Service informer that maintains an
// IP -> (namespace, service name) index built from spec.clusterIP +
// spec.clusterIPs (dual-stack). The fallback path (processor.go) consults the
// index *before* trying splitAddress, so an IP-literal `server.address`
// resolves to its real Service tuple and feeds the existing
// LookupByBackendService path unchanged.
//
// Out of scope (Phase 2): EndpointSlice -> PodIP -> service_name index. Pods
// hit by LB-style traffic emit PodIP-shaped server.address values that this
// Service-only index won't catch. ISI-851 description marks EndpointSlices as
// deferred so we ship the ClusterIP win first.

import (
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ServiceIPLookup resolves an IP literal (ClusterIP, dual-stack ClusterIPs[*])
// to the Service it belongs to. Returns ok=false for unknown IPs, headless
// Services (clusterIP=None), and unparseable input.
//
// Implemented by *serviceIPIndex (production, populated by the informer) and
// by *staticLookup (tests). The processor's fallback path uses this via a
// type-assertion so an old RouteLookup that doesn't know about IP lookup
// stays source-compatible.
type ServiceIPLookup interface {
	LookupServiceByIP(ip string) (namespace, service string, ok bool)
}

// serviceIPIndex is the IP -> (ns, name) map maintained by the Service
// informer. Reads happen on the signal pipeline; writes happen on informer
// goroutines. RWMutex keeps go test -race clean.
type serviceIPIndex struct {
	mu sync.RWMutex
	// key = canonical net.IP.String(); value = (ns, name) of the owning Service.
	m map[string]nsName
	// owner tracks which (ns, name) currently claims an IP, so on Service
	// update/delete we can withdraw stale claims even if the IP set churned.
	// key = canonical IP; value = owner key "ns/name".
	owner map[string]string
}

type nsName struct {
	Namespace string
	Name      string
}

// Istio ambient-mesh labels that select a waypoint for a Service (GAMMA
// label-based waypoint resolution, ISI-6178 §4.3 / D5). `use-waypoint` names
// the waypoint (value "none" explicitly opts out); `use-waypoint-namespace`
// optionally points at a waypoint in a different namespace.
const (
	labelUseWaypoint    = "istio.io/use-waypoint"
	labelUseWaypointNS  = "istio.io/use-waypoint-namespace"
	useWaypointDisabled = "none"
)

// WaypointLookup resolves the waypoint serving a given Service from the Service
// informer's label cache (D5). Returns ok=false when the Service is unknown or
// carries no (enabled) istio.io/use-waypoint label. Implemented by
// *waypointIndex (production) and *staticLookup (tests); consulted via a
// type-assertion so lookups that predate waypoint support stay
// source-compatible.
type WaypointLookup interface {
	LookupWaypointForService(namespace, name string) (implementation, waypointName, waypointNamespace string, ok bool)
}

// waypointInfo is the resolved waypoint for a Service.
type waypointInfo struct {
	Implementation string
	Name           string
	Namespace      string
}

// waypointIndex maps a Service (ns/name) to the waypoint it selects via the
// istio.io/use-waypoint label. Maintained by the Service informer handler
// alongside serviceIPIndex (one watch, two indexes — D5). Reads happen on the
// signal pipeline; writes on informer goroutines, so an RWMutex keeps
// go test -race clean.
type waypointIndex struct {
	mu sync.RWMutex
	m  map[string]waypointInfo // key = "ns/name"
}

func newWaypointIndex() *waypointIndex {
	return &waypointIndex{m: make(map[string]waypointInfo)}
}

// LookupWaypointForService satisfies WaypointLookup.
func (w *waypointIndex) LookupWaypointForService(ns, name string) (string, string, string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	wi, ok := w.m[ns+"/"+name]
	if !ok {
		return "", "", "", false
	}
	return wi.Implementation, wi.Name, wi.Namespace, true
}

// upsertService (re)derives the waypoint entry for a Service from its labels.
// A Service that drops the label (or sets it to "none") has its entry removed.
func (w *waypointIndex) upsertService(svc *corev1.Service) {
	if svc == nil {
		return
	}
	key := svc.Namespace + "/" + svc.Name
	wp := svc.Labels[labelUseWaypoint]
	if wp == "" || wp == useWaypointDisabled {
		w.mu.Lock()
		delete(w.m, key)
		w.mu.Unlock()
		return
	}
	wpNS := svc.Labels[labelUseWaypointNS]
	if wpNS == "" {
		wpNS = svc.Namespace
	}
	w.mu.Lock()
	w.m[key] = waypointInfo{
		Implementation: WaypointImplIstioAmbient,
		Name:           wp,
		Namespace:      wpNS,
	}
	w.mu.Unlock()
}

func (w *waypointIndex) deleteService(ns, name string) {
	w.mu.Lock()
	delete(w.m, ns+"/"+name)
	w.mu.Unlock()
}

// serviceNameIndex is a set of (ns/name) for every core/v1 Service the informer
// has seen. It is the third index the shared Service informer maintains (one
// watch, N indexes — design D5), added for the nil-Kind parent-Service
// inference (ISI-6188 / D4): the projection path asks "does a Service with this
// ns/name exist?" to decide whether a Gateway-less parentRef is really mesh.
// Reads happen on the signal pipeline; writes on informer goroutines, so an
// RWMutex keeps go test -race clean.
type serviceNameIndex struct {
	mu sync.RWMutex
	m  map[string]struct{} // key = "ns/name"
}

func newServiceNameIndex() *serviceNameIndex {
	return &serviceNameIndex{m: make(map[string]struct{})}
}

// Exists reports whether a Service with this ns/name is in cache. Nil-safe so
// callers that never built the index (inference off) can hold a nil pointer.
func (s *serviceNameIndex) Exists(ns, name string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[ns+"/"+name]
	return ok
}

func (s *serviceNameIndex) upsertService(svc *corev1.Service) {
	if svc == nil {
		return
	}
	s.mu.Lock()
	s.m[svc.Namespace+"/"+svc.Name] = struct{}{}
	s.mu.Unlock()
}

func (s *serviceNameIndex) deleteService(ns, name string) {
	s.mu.Lock()
	delete(s.m, ns+"/"+name)
	s.mu.Unlock()
}

func newServiceIPIndex() *serviceIPIndex {
	return &serviceIPIndex{
		m:     make(map[string]nsName),
		owner: make(map[string]string),
	}
}

// LookupServiceByIP satisfies ServiceIPLookup.
func (s *serviceIPIndex) LookupServiceByIP(ip string) (string, string, bool) {
	if ip == "" {
		return "", "", false
	}
	canon := canonicalIP(ip)
	if canon == "" {
		return "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.m[canon]; ok {
		return v.Namespace, v.Name, true
	}
	return "", "", false
}

// upsertService rebuilds the IP claims for a Service. The Service-IP set can
// shrink between revisions (a Service that lost dual-stack); we explicitly
// drop owner-claimed IPs that aren't in the new set so the index doesn't
// retain stale entries.
//
// Conflict policy NOTE (cross-ref to ISI-875 podIPIndex.upsertEndpointSlice):
// when two Services somehow claim the same IP (a bug or tombstone race —
// kube enforces ClusterIP uniqueness at the API server, so this shouldn't
// happen on a healthy cluster), we drop the entry entirely so we never
// attribute ambiguously. The PodIP index uses last-writer-wins instead,
// because two EndpointSlices owned by different Services pointing at the
// same Pod IP is a more recoverable state (and informer events naturally
// converge to ground truth). The asymmetry between the two indices is
// deliberate, not an oversight.
func (s *serviceIPIndex) upsertService(svc *corev1.Service) {
	if svc == nil {
		return
	}
	want := serviceIPSet(svc)
	owner := svc.Namespace + "/" + svc.Name

	s.mu.Lock()
	defer s.mu.Unlock()

	// Withdraw stale claims this owner used to hold.
	for ip, claim := range s.owner {
		if claim != owner {
			continue
		}
		if _, keep := want[ip]; keep {
			continue
		}
		delete(s.owner, ip)
		delete(s.m, ip)
	}
	// (Re)claim every current IP. If another Service somehow already claims an
	// IP (shouldn't happen in a healthy cluster — kube enforces ClusterIP
	// uniqueness — but tombstone races make it possible), drop the entry so we
	// never attribute ambiguously, mirroring routeIndex.reindexBackends.
	for ip := range want {
		if existing, ok := s.owner[ip]; ok && existing != owner {
			delete(s.owner, ip)
			delete(s.m, ip)
			continue
		}
		s.owner[ip] = owner
		s.m[ip] = nsName{Namespace: svc.Namespace, Name: svc.Name}
	}
}

// deleteService drops every IP this owner currently claims.
func (s *serviceIPIndex) deleteService(ns, name string) {
	owner := ns + "/" + name
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip, claim := range s.owner {
		if claim == owner {
			delete(s.owner, ip)
			delete(s.m, ip)
		}
	}
}

// serviceIPSet returns the canonicalized IP set this Service announces.
// Filters: empty IPs, the literal "None" (headless), and unparseable values.
// Both spec.clusterIP and spec.clusterIPs are honored; dual-stack Services
// list every family in clusterIPs and we want all of them.
func serviceIPSet(svc *corev1.Service) map[string]struct{} {
	out := make(map[string]struct{}, 1+len(svc.Spec.ClusterIPs))
	add := func(raw string) {
		if raw == "" || raw == corev1.ClusterIPNone {
			return
		}
		canon := canonicalIP(raw)
		if canon == "" {
			return
		}
		out[canon] = struct{}{}
	}
	add(svc.Spec.ClusterIP)
	for _, ip := range svc.Spec.ClusterIPs {
		add(ip)
	}
	return out
}

// canonicalIP returns a normalized form of the input IP literal, or empty
// string if the value is not a valid v4 or v6 address. Normalization matters
// for IPv6 ("2001:db8::1" vs "2001:0db8:0000::0001") so the index lookup
// matches no matter which form the SDK emitted.
func canonicalIP(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return ""
	}
	return ip.String()
}

// startServiceInformer wires the core/v1.Service informer that populates the
// Service-derived indexes. Returns the informers (so the caller can
// WaitForCacheSync on them alongside the Gateway-API ones), the factories (so
// the caller can Start them in the unified start phase), and whichever indexes
// the enabled features need.
//
// The caller passes a shared kubernetes.Interface so the Service informer
// and the EndpointSlice informer (ISI-875) share one HTTP connection pool
// to the API server instead of each opening their own.
//
// Like the gateway-api informers, this honors cfg.Watch.Namespaces — empty
// means "watch every namespace"; a list scopes one factory per namespace.
// Resync period mirrors the gateway-api factories so we don't double-pay
// list/relist cost.
//
// The three indexes are built independently so the informer only pays for what
// the enabled features need (D5): buildIP toggles the ClusterIP reverse-lookup
// index (backendref_fallback), buildWaypoint toggles the use-waypoint label
// cache (GAMMA waypoint attribution), and buildName toggles the Service name
// index (nil-Kind parent-Service inference, ISI-6188 / D4). Any of the three
// may be nil in the returned tuple when its feature is off; a non-nil set
// shares one Service watch.
//
// The informer is created and its handlers registered, but f.Start is NOT
// called here — the caller (newInformers) starts every factory in one phase
// after all handlers are wired, so the Service name index is observable before
// the Gateway-API route projections begin inferring against it.
func startServiceInformer(
	logger *zap.Logger,
	client kubernetes.Interface,
	cfg *Config,
	buildIP bool,
	buildWaypoint bool,
	buildName bool,
) ([]cache.SharedIndexInformer, []informers.SharedInformerFactory, *serviceIPIndex, *waypointIndex, *serviceNameIndex, error) {
	resync := cfg.Watch.ResyncPeriod
	if resync == 0 {
		resync = 5 * time.Minute
	}

	var ipIdx *serviceIPIndex
	if buildIP {
		ipIdx = newServiceIPIndex()
	}
	var wpIdx *waypointIndex
	if buildWaypoint {
		wpIdx = newWaypointIndex()
	}
	var nameIdx *serviceNameIndex
	if buildName {
		nameIdx = newServiceNameIndex()
	}

	var factories []informers.SharedInformerFactory
	if len(cfg.Watch.Namespaces) == 0 {
		factories = append(factories, informers.NewSharedInformerFactory(client, resync))
	} else {
		for _, ns := range cfg.Watch.Namespaces {
			factories = append(factories,
				informers.NewSharedInformerFactoryWithOptions(client, resync, informers.WithNamespace(ns)),
			)
		}
	}

	var infs []cache.SharedIndexInformer
	for _, f := range factories {
		svcInf := f.Core().V1().Services().Informer()
		registerServiceHandlers(svcInf, ipIdx, wpIdx, nameIdx, logger)
		infs = append(infs, svcInf)
	}
	return infs, factories, ipIdx, wpIdx, nameIdx, nil
}

// registerServiceHandlers wires Service add/update/delete into whichever of the
// three indexes are enabled. All index args are nil-safe so a feature that is
// off simply skips its index write.
func registerServiceHandlers(inf cache.SharedIndexInformer, ipIdx *serviceIPIndex, wpIdx *waypointIndex, nameIdx *serviceNameIndex, logger *zap.Logger) {
	upsert := func(svc *corev1.Service) {
		if ipIdx != nil {
			ipIdx.upsertService(svc)
		}
		if wpIdx != nil {
			wpIdx.upsertService(svc)
		}
		if nameIdx != nil {
			nameIdx.upsertService(svc)
		}
	}
	del := func(ns, name string) {
		if ipIdx != nil {
			ipIdx.deleteService(ns, name)
		}
		if wpIdx != nil {
			wpIdx.deleteService(ns, name)
		}
		if nameIdx != nil {
			nameIdx.deleteService(ns, name)
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			svc, ok := obj.(*corev1.Service)
			if !ok {
				return
			}
			upsert(svc)
		},
		UpdateFunc: func(_, newObj any) {
			svc, ok := newObj.(*corev1.Service)
			if !ok {
				return
			}
			upsert(svc)
		},
		DeleteFunc: func(obj any) {
			svc, ok := obj.(*corev1.Service)
			if !ok {
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if svc2, ok2 := t.Obj.(*corev1.Service); ok2 {
						del(svc2.Namespace, svc2.Name)
						return
					}
				}
				logger.Debug("Service delete: unexpected tombstone type")
				return
			}
			del(svc.Namespace, svc.Name)
		},
	})
}
