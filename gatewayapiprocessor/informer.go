package gatewayapiprocessor

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"go.uber.org/zap"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gwinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

// newInformers builds a RouteLookup backed by real Gateway API informers.
// Returns a stop function the processor calls on Shutdown.
//
// processor-spec §2.4 "Informer startup":
//  1. Build client-go clientset.
//  2. Start 4 shared informers (Gateway, HTTPRoute, GRPCRoute, GatewayClass).
//  3. Wait for all 4 caches to sync before returning.
//  4. Fail fast on sync timeout.
func newInformers(ctx context.Context, logger *zap.Logger, cfg *Config, tel *telemetryBuilder) (RouteLookup, func(context.Context) error, error) {
	restCfg, err := buildRESTConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build rest config: %w", err)
	}
	client, err := gwclient.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build gateway-api clientset: %w", err)
	}

	resyncPeriod := cfg.Watch.ResyncPeriod
	if resyncPeriod == 0 {
		resyncPeriod = 5 * time.Minute
	}

	// Scoped factory. Empty namespace = watch all namespaces (spec §2.2).
	var factories []gwinformers.SharedInformerFactory
	if len(cfg.Watch.Namespaces) == 0 {
		factories = append(factories, gwinformers.NewSharedInformerFactory(client, resyncPeriod))
	} else {
		for _, ns := range cfg.Watch.Namespaces {
			factories = append(factories,
				gwinformers.NewSharedInformerFactoryWithOptions(client, resyncPeriod, gwinformers.WithNamespace(ns)),
			)
		}
	}

	index := newRouteIndex()
	index.attachTelemetry(tel)
	gwStore := newGatewayStore()
	gcStore := newGatewayClassStore()

	// Collect all shared informers we start so we can wait on their sync.
	var informers []cache.SharedIndexInformer
	type resourceInformer struct {
		resource string
		inf      cache.SharedIndexInformer
	}
	var toAwait []resourceInformer

	for _, f := range factories {
		hrInf := f.Gateway().V1().HTTPRoutes().Informer()
		grInf := f.Gateway().V1().GRPCRoutes().Informer()
		gwInf := f.Gateway().V1().Gateways().Informer()
		gcInf := f.Gateway().V1().GatewayClasses().Informer()

		registerHTTPRouteHandlers(hrInf, index, gwStore, gcStore, cfg, logger, tel)
		registerGRPCRouteHandlers(grInf, index, gwStore, gcStore, cfg, logger, tel)
		registerGatewayHandlers(gwInf, gwStore, logger, tel)
		registerGatewayClassHandlers(gcInf, gcStore, logger, tel)

		informers = append(informers, hrInf, grInf, gwInf, gcInf)
		toAwait = append(toAwait,
			resourceInformer{resource: "HTTPRoute", inf: hrInf},
			resourceInformer{resource: "GRPCRoute", inf: grInf},
			resourceInformer{resource: "Gateway", inf: gwInf},
			resourceInformer{resource: "GatewayClass", inf: gcInf},
		)

		f.Start(ctx.Done())
	}

	syncCtx, cancel := context.WithTimeout(ctx, defaultSyncTimeout(cfg.InformerSyncTimeout))
	defer cancel()
	for _, ri := range toAwait {
		if !cache.WaitForCacheSync(syncCtx.Done(), ri.inf.HasSynced) {
			logger.Error("gatewayapiprocessor: informer cache sync timed out",
				zap.String("resource", ri.resource),
				zap.Duration("timeout", defaultSyncTimeout(cfg.InformerSyncTimeout)),
			)
			return nil, nil, fmt.Errorf("gatewayapiprocessor: %s informer cache sync timed out after %s", ri.resource, defaultSyncTimeout(cfg.InformerSyncTimeout))
		}
		if tel != nil {
			tel.recordInformerEvent(ctx, ri.resource, "sync")
		}
		logger.Info("gatewayapiprocessor: informer cache synced", zap.String("resource", ri.resource))
	}

	stop := func(_ context.Context) error {
		// factories stop when ctx.Done() fires; nothing to do here beyond that.
		return nil
	}
	return index, stop, nil
}

func buildRESTConfig(cfg *Config) (*rest.Config, error) {
	switch cfg.AuthType {
	case AuthTypeKubeConfig:
		return clientcmd.BuildConfigFromFlags("", cfg.KubeConfigPath)
	case AuthTypeNone:
		// Caller opted out of real kube — informers shouldn't have been built.
		return nil, fmt.Errorf("auth_type=none cannot build a rest config")
	default: // serviceAccount (in-cluster)
		return rest.InClusterConfig()
	}
}

func defaultSyncTimeout(v time.Duration) time.Duration {
	if v <= 0 {
		return 30 * time.Second
	}
	return v
}

// --- event handlers ---

func registerHTTPRouteHandlers(inf cache.SharedIndexInformer, idx *routeIndex, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, logger *zap.Logger, tel *telemetryBuilder) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			hr, ok := obj.(*gwv1.HTTPRoute)
			if !ok {
				return
			}
			ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg)
			idx.upsertHTTPRoute(ra, backendRefsFromHTTPRoute(hr))
			if tel != nil {
				tel.recordInformerEvent(context.Background(), "HTTPRoute", "add")
			}
		},
		UpdateFunc: func(_, newObj any) {
			hr, ok := newObj.(*gwv1.HTTPRoute)
			if !ok {
				return
			}
			ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg)
			idx.upsertHTTPRoute(ra, backendRefsFromHTTPRoute(hr))
			if tel != nil {
				tel.recordInformerEvent(context.Background(), "HTTPRoute", "update")
			}
		},
		DeleteFunc: func(obj any) {
			hr, ok := obj.(*gwv1.HTTPRoute)
			if !ok {
				// tombstone path
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if hr2, ok2 := t.Obj.(*gwv1.HTTPRoute); ok2 {
						idx.deleteHTTPRoute(hr2.Namespace, hr2.Name)
						if tel != nil {
							tel.recordInformerEvent(context.Background(), "HTTPRoute", "delete")
						}
						return
					}
				}
				logger.Debug("HTTPRoute delete: unexpected tombstone type")
				return
			}
			idx.deleteHTTPRoute(hr.Namespace, hr.Name)
			if tel != nil {
				tel.recordInformerEvent(context.Background(), "HTTPRoute", "delete")
			}
		},
	})
}

func registerGRPCRouteHandlers(inf cache.SharedIndexInformer, idx *routeIndex, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, logger *zap.Logger, tel *telemetryBuilder) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			gr, ok := obj.(*gwv1.GRPCRoute)
			if !ok {
				return
			}
			idx.upsertGRPCRoute(grpcRouteToAttrs(gr, gwStore, gcStore, cfg))
			if tel != nil {
				tel.recordInformerEvent(context.Background(), "GRPCRoute", "add")
			}
		},
		UpdateFunc: func(_, newObj any) {
			gr, ok := newObj.(*gwv1.GRPCRoute)
			if !ok {
				return
			}
			idx.upsertGRPCRoute(grpcRouteToAttrs(gr, gwStore, gcStore, cfg))
			if tel != nil {
				tel.recordInformerEvent(context.Background(), "GRPCRoute", "update")
			}
		},
		DeleteFunc: func(obj any) {
			gr, ok := obj.(*gwv1.GRPCRoute)
			if !ok {
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if gr2, ok2 := t.Obj.(*gwv1.GRPCRoute); ok2 {
						idx.deleteGRPCRoute(gr2.Namespace, gr2.Name)
						if tel != nil {
							tel.recordInformerEvent(context.Background(), "GRPCRoute", "delete")
						}
						return
					}
				}
				logger.Debug("GRPCRoute delete: unexpected tombstone type")
				return
			}
			idx.deleteGRPCRoute(gr.Namespace, gr.Name)
			if tel != nil {
				tel.recordInformerEvent(context.Background(), "GRPCRoute", "delete")
			}
		},
	})
}

func registerGatewayHandlers(inf cache.SharedIndexInformer, store *gatewayStore, _ *zap.Logger, tel *telemetryBuilder) {
	bump := func(event string) {
		if tel != nil {
			tel.recordInformerEvent(context.Background(), "Gateway", event)
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			store.upsert(obj.(*gwv1.Gateway))
			bump("add")
		},
		UpdateFunc: func(_, n any) {
			store.upsert(n.(*gwv1.Gateway))
			bump("update")
		},
		DeleteFunc: func(obj any) {
			if gw, ok := obj.(*gwv1.Gateway); ok {
				store.delete(gw.Namespace, gw.Name)
				bump("delete")
			}
		},
	})
}

func registerGatewayClassHandlers(inf cache.SharedIndexInformer, store *gatewayClassStore, _ *zap.Logger, tel *telemetryBuilder) {
	bump := func(event string) {
		if tel != nil {
			tel.recordInformerEvent(context.Background(), "GatewayClass", event)
		}
	}
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			store.upsert(obj.(*gwv1.GatewayClass))
			bump("add")
		},
		UpdateFunc: func(_, n any) {
			store.upsert(n.(*gwv1.GatewayClass))
			bump("update")
		},
		DeleteFunc: func(obj any) {
			if gc, ok := obj.(*gwv1.GatewayClass); ok {
				store.delete(gc.Name)
				bump("delete")
			}
		},
	})
}

// --- projections from CR -> RouteAttributes ---

func httpRouteToAttrs(hr *gwv1.HTTPRoute, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config) RouteAttributes {
	ra := RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      hr.Name,
		Namespace: hr.Namespace,
		UID:       string(hr.UID),
	}
	if len(hr.Spec.ParentRefs) > 0 {
		pr := hr.Spec.ParentRefs[0]
		ra.ParentRef = formatParentRef(pr, hr.Namespace)
		ns := hr.Namespace
		if pr.Namespace != nil && *pr.Namespace != "" {
			ns = string(*pr.Namespace)
		}
		if gw, ok := gwStore.get(ns, string(pr.Name)); ok {
			ra.GatewayName = gw.Name
			ra.GatewayNamespace = gw.Namespace
			ra.GatewayUID = string(gw.UID)
			if pr.SectionName != nil {
				ra.GatewayListenerName = string(*pr.SectionName)
			}
			ra.GatewayClassName = string(gw.Spec.GatewayClassName)
			if gc, ok2 := gcStore.get(ra.GatewayClassName); ok2 {
				ra.GatewayClassControllerName = string(gc.Spec.ControllerName)
			}
		}
	}
	if cfg.EmitStatusConds {
		ra.Accepted, ra.ResolvedRefs = statusFlags(hr.Status.Parents)
	}
	return ra
}

func grpcRouteToAttrs(gr *gwv1.GRPCRoute, gwStore *gatewayStore, gcStore *gatewayClassStore, _ *Config) RouteAttributes {
	ra := RouteAttributes{
		Kind:      RouteKindGRPCRoute,
		Name:      gr.Name,
		Namespace: gr.Namespace,
		UID:       string(gr.UID),
	}
	if len(gr.Spec.ParentRefs) > 0 {
		pr := gr.Spec.ParentRefs[0]
		ra.ParentRef = formatParentRef(pr, gr.Namespace)
		ns := gr.Namespace
		if pr.Namespace != nil && *pr.Namespace != "" {
			ns = string(*pr.Namespace)
		}
		if gw, ok := gwStore.get(ns, string(pr.Name)); ok {
			ra.GatewayName = gw.Name
			ra.GatewayNamespace = gw.Namespace
			ra.GatewayUID = string(gw.UID)
			if pr.SectionName != nil {
				ra.GatewayListenerName = string(*pr.SectionName)
			}
			ra.GatewayClassName = string(gw.Spec.GatewayClassName)
			if gc, ok2 := gcStore.get(ra.GatewayClassName); ok2 {
				ra.GatewayClassControllerName = string(gc.Spec.ControllerName)
			}
		}
	}
	return ra
}

func statusFlags(parents []gwv1.RouteParentStatus) (*bool, *bool) {
	var accepted, resolved *bool
	for _, ps := range parents {
		for _, c := range ps.Conditions {
			switch c.Type {
			case "Accepted":
				v := c.Status == metav1.ConditionTrue
				accepted = &v
			case "ResolvedRefs":
				v := c.Status == metav1.ConditionTrue
				resolved = &v
			}
		}
	}
	return accepted, resolved
}

func formatParentRef(pr gwv1.ParentReference, ownerNS string) string {
	group := "gateway.networking.k8s.io"
	if pr.Group != nil && *pr.Group != "" {
		group = string(*pr.Group)
	}
	kind := "Gateway"
	if pr.Kind != nil && *pr.Kind != "" {
		kind = string(*pr.Kind)
	}
	ns := ownerNS
	if pr.Namespace != nil && *pr.Namespace != "" {
		ns = string(*pr.Namespace)
	}
	return group + "/" + kind + "/" + ns + "/" + string(pr.Name)
}

// --- small typed stores for Gateway / GatewayClass ---
//
// Both stores are written by informer event handlers (running on informer
// goroutines) and read by the enrichment path + route projection (on signal
// pipeline goroutines). A RWMutex keeps go test -race clean.

type gatewayStore struct {
	mu sync.RWMutex
	m  map[string]*gwv1.Gateway
}

func newGatewayStore() *gatewayStore { return &gatewayStore{m: map[string]*gwv1.Gateway{}} }

func (s *gatewayStore) upsert(gw *gwv1.Gateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[gw.Namespace+"/"+gw.Name] = gw
}

func (s *gatewayStore) get(ns, name string) (*gwv1.Gateway, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.m[ns+"/"+name]
	return g, ok
}
func (s *gatewayStore) delete(ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ns+"/"+name)
}

type gatewayClassStore struct {
	mu sync.RWMutex
	m  map[string]*gwv1.GatewayClass
}

func newGatewayClassStore() *gatewayClassStore {
	return &gatewayClassStore{m: map[string]*gwv1.GatewayClass{}}
}

func (s *gatewayClassStore) upsert(gc *gwv1.GatewayClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[gc.Name] = gc
}
func (s *gatewayClassStore) get(name string) (*gwv1.GatewayClass, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.m[name]
	return g, ok
}
func (s *gatewayClassStore) delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}
