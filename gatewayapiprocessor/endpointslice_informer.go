package gatewayapiprocessor

// PodIP reverse-lookup informer (ISI-875, Phase 2 of ISI-851).
//
// Why this exists: the ISI-838 audit and the post-PR-#55 re-audit on
// observable-gateapiprocess showed PodIP-shaped server.address values are the
// largest remaining un-enriched bucket (e.g. 10.244.77.141 ×1,328 spans/2h
// from the otel-demo `ad` Pod). Phase 1 only resolves ClusterIPs via the
// core/v1.Service informer; PodIPs need an EndpointSlice informer to map
// back to the owning Service.
//
// What this adds: a discovery.k8s.io/v1.EndpointSlice informer that
// maintains a PodIP -> (namespace, serviceName) index. The combinedLookup
// now consults the ClusterIP index first and falls back to the PodIP index,
// so the existing applyBackendRefFallback path picks up PodIP-literal
// server.address values without any change to its IP-literal branch.
//
// Owner resolution: EndpointSlices created by the kube EndpointSlice
// controller carry the canonical label `kubernetes.io/service-name`
// (constant `discoveryv1.LabelServiceName`). We prefer that label, then
// fall back to ownerReferences[Service]. EndpointSlices with neither are
// skipped: those are manually-managed slices for headless workloads, mirror
// objects for legacy v1.Endpoints, etc., and lack the Service identity we
// need to enrich.
//
// Out of scope (mirrors Phase 1):
//   - PodIP churn beyond informer's built-in resync (cluster is small).
//   - Endpoint addresses that are FQDN-typed (only "IPv4"/"IPv6" slices
//     contribute to the index — addressType=FQDN slices are skipped).

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// PodIPLookup resolves a Pod IP to the Service that owns the EndpointSlice
// containing it. Returns ok=false for unknown IPs and unparseable input.
//
// Implemented by *podIPIndex (production) and by *staticLookup (tests).
type PodIPLookup interface {
	LookupPodIP(ip string) (namespace, service string, ok bool)
}

// podIPIndex is the IP -> (ns, serviceName) map maintained by the
// EndpointSlice informer. Reads happen on the signal pipeline; writes happen
// on informer goroutines. Mirrors serviceIPIndex's RWMutex pattern so
// go test -race stays clean.
//
// Owner key is the EndpointSlice (ns/name), NOT the owning Service: many
// EndpointSlices can fan out from a single Service, and we want each slice's
// claims to be withdrawable independently when a slice is deleted or its
// address set churns.
type podIPIndex struct {
	mu sync.RWMutex
	// key = canonical net.IP.String(); value = (ns, serviceName) of the
	// Service that owns the EndpointSlice carrying this address.
	m map[string]nsName
	// owner tracks which EndpointSlice currently claims an IP, so on slice
	// update/delete we can withdraw stale claims.
	// key = canonical IP; value = "ns/sliceName".
	owner map[string]string
}

func newPodIPIndex() *podIPIndex {
	return &podIPIndex{
		m:     make(map[string]nsName),
		owner: make(map[string]string),
	}
}

// LookupPodIP satisfies PodIPLookup.
func (p *podIPIndex) LookupPodIP(ip string) (string, string, bool) {
	if ip == "" {
		return "", "", false
	}
	canon := canonicalIP(ip)
	if canon == "" {
		return "", "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if v, ok := p.m[canon]; ok {
		return v.Namespace, v.Name, true
	}
	return "", "", false
}

// upsertEndpointSlice rebuilds the IP claims for one EndpointSlice. The
// address set can shrink between revisions (Pod churn, scale-down); we drop
// owner-claimed IPs that aren't in the new set so the index doesn't retain
// stale entries. Slices with no resolvable Service owner, FQDN address type,
// or no Ready endpoints become a no-op (any prior claims for the same owner
// key are still withdrawn so a slice that loses its owner label cleans up).
func (p *podIPIndex) upsertEndpointSlice(es *discoveryv1.EndpointSlice) {
	if es == nil {
		return
	}
	owner := es.Namespace + "/" + es.Name
	svc := serviceOwnerOf(es)
	// A slice that's no longer "indexable" (lost its Service owner, or has
	// no Ready IP-typed endpoints) contributes nothing to the index this
	// revision; force `want` empty so the withdrawal pass below cleans up
	// every claim this owner used to hold.
	var want map[string]struct{}
	if svc != "" {
		want = endpointSliceIPSet(es)
	} else {
		want = map[string]struct{}{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Withdraw stale claims this owner used to hold. With `want` forced
	// empty when svc == "", this branch also handles the "slice lost its
	// Service owner" case.
	for ip, claim := range p.owner {
		if claim != owner {
			continue
		}
		if _, keep := want[ip]; keep {
			continue
		}
		delete(p.owner, ip)
		delete(p.m, ip)
	}
	if svc == "" || len(want) == 0 {
		return
	}
	value := nsName{Namespace: es.Namespace, Name: svc}
	for ip := range want {
		// Take the claim. Two slices owned by different Services pointing at
		// the same Pod IP shouldn't happen in a healthy cluster, but if it
		// does we keep last-writer-wins (mirrors core controller behaviour
		// and lets the most recent informer event reflect ground truth).
		p.owner[ip] = owner
		p.m[ip] = value
	}
}

// deleteEndpointSlice drops every IP this owner currently claims.
func (p *podIPIndex) deleteEndpointSlice(ns, name string) {
	owner := ns + "/" + name
	p.mu.Lock()
	defer p.mu.Unlock()
	for ip, claim := range p.owner {
		if claim == owner {
			delete(p.owner, ip)
			delete(p.m, ip)
		}
	}
}

// endpointSliceIPSet returns the canonicalized IP set this slice contributes.
// Filters: FQDN address type, empty addresses, Ready=false endpoints,
// unparseable IPs. We follow kube-proxy's policy and only index Ready
// endpoints — a not-Ready Pod is not in the routing path, so attributing a
// span to its Service would mislead operators about which workload actually
// served the request.
func endpointSliceIPSet(es *discoveryv1.EndpointSlice) map[string]struct{} {
	out := map[string]struct{}{}
	if es.AddressType != discoveryv1.AddressTypeIPv4 && es.AddressType != discoveryv1.AddressTypeIPv6 {
		return out
	}
	for _, ep := range es.Endpoints {
		// Ready=nil means "unknown but treat as ready" per the API contract.
		if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
			continue
		}
		for _, raw := range ep.Addresses {
			canon := canonicalIP(raw)
			if canon == "" {
				continue
			}
			out[canon] = struct{}{}
		}
	}
	return out
}

// serviceOwnerOf resolves the owning Service name for an EndpointSlice.
// Prefers the canonical kubernetes.io/service-name label (constant
// discoveryv1.LabelServiceName) set by the kube EndpointSlice controller;
// falls back to ownerReferences[Service] for slices created by other
// controllers. Returns "" when no Service owner can be determined — those
// slices are skipped (manually-managed for headless workloads, v1.Endpoints
// mirror objects, etc.). Empty label values are treated as missing because
// kube records the owning Service via the same label format on both
// auto-managed and user-created slices.
func serviceOwnerOf(es *discoveryv1.EndpointSlice) string {
	if v, ok := es.Labels[discoveryv1.LabelServiceName]; ok && v != "" {
		return v
	}
	for _, or := range es.OwnerReferences {
		if or.Kind == "Service" && or.Name != "" {
			return or.Name
		}
	}
	return ""
}

// startEndpointSliceInformer wires the discovery.k8s.io/v1.EndpointSlice
// informer used to populate the PodIP reverse-lookup index. Returns the
// started informers (so the caller can WaitForCacheSync alongside the
// gateway-api / Service ones) plus the index the processor's fallback path
// will read from.
//
// Like the gateway-api informers, this honors cfg.Watch.Namespaces — empty
// means "watch every namespace"; a list scopes one factory per namespace.
// Resync period mirrors the other factories so we don't double-pay
// list/relist cost.
func startEndpointSliceInformer(
	ctx context.Context,
	logger *zap.Logger,
	restCfg *rest.Config,
	cfg *Config,
) ([]cache.SharedIndexInformer, *podIPIndex, error) {
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build kubernetes clientset: %w", err)
	}

	resync := cfg.Watch.ResyncPeriod
	if resync == 0 {
		resync = 5 * time.Minute
	}

	idx := newPodIPIndex()

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
		esInf := f.Discovery().V1().EndpointSlices().Informer()
		registerEndpointSliceHandlers(esInf, idx, logger)
		infs = append(infs, esInf)
		f.Start(ctx.Done())
	}
	return infs, idx, nil
}

func registerEndpointSliceHandlers(inf cache.SharedIndexInformer, idx *podIPIndex, logger *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			es, ok := obj.(*discoveryv1.EndpointSlice)
			if !ok {
				return
			}
			idx.upsertEndpointSlice(es)
		},
		UpdateFunc: func(_, newObj any) {
			es, ok := newObj.(*discoveryv1.EndpointSlice)
			if !ok {
				return
			}
			idx.upsertEndpointSlice(es)
		},
		DeleteFunc: func(obj any) {
			es, ok := obj.(*discoveryv1.EndpointSlice)
			if !ok {
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if es2, ok2 := t.Obj.(*discoveryv1.EndpointSlice); ok2 {
						idx.deleteEndpointSlice(es2.Namespace, es2.Name)
						return
					}
				}
				logger.Debug("EndpointSlice delete: unexpected tombstone type")
				return
			}
			idx.deleteEndpointSlice(es.Namespace, es.Name)
		},
	})
}
