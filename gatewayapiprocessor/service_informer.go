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
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// startServiceInformer wires the core/v1.Service informer used to populate
// the IP reverse-lookup index. Returns the started informer (so the caller
// can WaitForCacheSync on it alongside the Gateway-API ones) plus the index
// the processor's fallback path will read from.
//
// Like the gateway-api informers, this honors cfg.Watch.Namespaces — empty
// means "watch every namespace"; a list scopes one factory per namespace.
// Resync period mirrors the gateway-api factories so we don't double-pay
// list/relist cost.
func startServiceInformer(
	ctx context.Context,
	logger *zap.Logger,
	restCfg *rest.Config,
	cfg *Config,
) ([]cache.SharedIndexInformer, *serviceIPIndex, error) {
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build kubernetes clientset: %w", err)
	}

	resync := cfg.Watch.ResyncPeriod
	if resync == 0 {
		resync = 5 * time.Minute
	}

	idx := newServiceIPIndex()

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
		registerServiceHandlers(svcInf, idx, logger)
		infs = append(infs, svcInf)
		f.Start(ctx.Done())
	}
	return infs, idx, nil
}

func registerServiceHandlers(inf cache.SharedIndexInformer, idx *serviceIPIndex, logger *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			svc, ok := obj.(*corev1.Service)
			if !ok {
				return
			}
			idx.upsertService(svc)
		},
		UpdateFunc: func(_, newObj any) {
			svc, ok := newObj.(*corev1.Service)
			if !ok {
				return
			}
			idx.upsertService(svc)
		},
		DeleteFunc: func(obj any) {
			svc, ok := obj.(*corev1.Service)
			if !ok {
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if svc2, ok2 := t.Obj.(*corev1.Service); ok2 {
						idx.deleteService(svc2.Namespace, svc2.Name)
						return
					}
				}
				logger.Debug("Service delete: unexpected tombstone type")
				return
			}
			idx.deleteService(svc.Namespace, svc.Name)
		},
	})
}
