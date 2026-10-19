package gatewayapiprocessor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

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
func newInformers(ctx context.Context, logger *zap.Logger, cfg *Config) (RouteLookup, func(context.Context) error, error) {
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
	gwStore := newGatewayStore()
	gcStore := newGatewayClassStore()

	// Collect all shared informers we start so we can wait on their sync.
	var informers []cache.SharedIndexInformer

	for _, f := range factories {
		hrInf := f.Gateway().V1().HTTPRoutes().Informer()
		grInf := f.Gateway().V1().GRPCRoutes().Informer()
		gwInf := f.Gateway().V1().Gateways().Informer()
		gcInf := f.Gateway().V1().GatewayClasses().Informer()

		registerHTTPRouteHandlers(hrInf, index, gwStore, gcStore, cfg, logger)
		registerGRPCRouteHandlers(grInf, index, gwStore, gcStore, cfg, logger)
		registerGatewayHandlers(gwInf, gwStore, logger)
		registerGatewayClassHandlers(gcInf, gcStore, logger)

		informers = append(informers, hrInf, grInf, gwInf, gcInf)

		f.Start(ctx.Done())
	}

	// Policy informers (ISI-804). Only fires when cfg.Watch.Policies is
	// configured; routes & enrichment continue to work unchanged for users
	// who haven't opted in.
	policyInformers, err := startPolicyInformers(ctx, logger, restCfg, cfg, index)
	if err != nil {
		return nil, nil, fmt.Errorf("start policy informers: %w", err)
	}
	informers = append(informers, policyInformers...)

	// Service-IP informer (ISI-851). Gated on backendref_fallback.enabled —
	// the index is only consulted from applyBackendRefFallback, which itself
	// short-circuits when the feature is off (processor.go:259). Skipping the
	// informer when the feature is disabled avoids a cluster-scoped list+watch
	// on core/v1/Services that would stream every Service create/update/delete
	// to every collector pod for no benefit (Copilot review on PR #55).
	// Phase 2 (EndpointSlice -> PodIP, ISI-875) plugs into the same
	// combinedLookup behind the same flag plus the PodIP sub-flag.
	//
	// Both informers receive the same kubernetes clientset so they share
	// one HTTP connection pool to the API server (Copilot review on
	// PR #66) rather than opening one per informer.
	//
	// Gate decoupling (ISI-6178 / design D5): the shared Service informer now
	// serves two features — the ClusterIP reverse-lookup index
	// (backendref_fallback) and the use-waypoint label cache (GAMMA waypoint
	// attribution). Start it when EITHER is enabled, and build only the indexes
	// the enabled features need so no feature pays for the other's index.
	var ipIndex *serviceIPIndex
	var wpIndex *waypointIndex
	var podIndex *podIPIndex
	buildIP := cfg.BackendRefFallback.Enabled
	buildWaypoint := cfg.Mesh.WaypointEnabled()
	if buildIP || buildWaypoint {
		k8sClient, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("build kubernetes clientset: %w", err)
		}

		var svcInformers []cache.SharedIndexInformer
		svcInformers, ipIndex, wpIndex, err = startServiceInformer(ctx, logger, k8sClient, cfg, buildIP, buildWaypoint)
		if err != nil {
			return nil, nil, fmt.Errorf("start service informer: %w", err)
		}
		informers = append(informers, svcInformers...)

		// EndpointSlice informer (ISI-875). Defaults to on whenever
		// BackendRefFallback.Enabled is true; opt out via
		// `backendref_fallback.pod_ip: false` on scale-sensitive clusters.
		// Scoped to backendref_fallback only — waypoint attribution never needs
		// PodIP resolution.
		if cfg.BackendRefFallback.Enabled && cfg.BackendRefFallback.PodIPEnabled() {
			var esInformers []cache.SharedIndexInformer
			esInformers, podIndex, err = startEndpointSliceInformer(ctx, logger, k8sClient, cfg)
			if err != nil {
				return nil, nil, fmt.Errorf("start endpointslice informer: %w", err)
			}
			informers = append(informers, esInformers...)
		}
	}

	syncCtx, cancel := context.WithTimeout(ctx, defaultSyncTimeout(cfg.InformerSyncTimeout))
	defer cancel()
	for _, inf := range informers {
		if !cache.WaitForCacheSync(syncCtx.Done(), inf.HasSynced) {
			return nil, nil, fmt.Errorf("gatewayapiprocessor: informer cache sync timed out after %s", defaultSyncTimeout(cfg.InformerSyncTimeout))
		}
	}

	stop := func(_ context.Context) error {
		// factories stop when ctx.Done() fires; nothing to do here beyond that.
		return nil
	}
	return &combinedLookup{routes: index, ips: ipIndex, pods: podIndex, waypoints: wpIndex}, stop, nil
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

func registerHTTPRouteHandlers(inf cache.SharedIndexInformer, idx *routeIndex, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, logger *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			hr, ok := obj.(*gwv1.HTTPRoute)
			if !ok {
				return
			}
			ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg)
			idx.upsertHTTPRoute(ra, backendRefsFromHTTPRoute(hr))
		},
		UpdateFunc: func(_, newObj any) {
			hr, ok := newObj.(*gwv1.HTTPRoute)
			if !ok {
				return
			}
			ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg)
			idx.upsertHTTPRoute(ra, backendRefsFromHTTPRoute(hr))
		},
		DeleteFunc: func(obj any) {
			hr, ok := obj.(*gwv1.HTTPRoute)
			if !ok {
				// tombstone path
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if hr2, ok2 := t.Obj.(*gwv1.HTTPRoute); ok2 {
						idx.deleteHTTPRoute(hr2.Namespace, hr2.Name)
						return
					}
				}
				logger.Debug("HTTPRoute delete: unexpected tombstone type")
				return
			}
			idx.deleteHTTPRoute(hr.Namespace, hr.Name)
		},
	})
}

func registerGRPCRouteHandlers(inf cache.SharedIndexInformer, idx *routeIndex, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, logger *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			gr, ok := obj.(*gwv1.GRPCRoute)
			if !ok {
				return
			}
			idx.upsertGRPCRoute(grpcRouteToAttrs(gr, gwStore, gcStore, cfg), backendRefsFromGRPCRoute(gr))
		},
		UpdateFunc: func(_, newObj any) {
			gr, ok := newObj.(*gwv1.GRPCRoute)
			if !ok {
				return
			}
			idx.upsertGRPCRoute(grpcRouteToAttrs(gr, gwStore, gcStore, cfg), backendRefsFromGRPCRoute(gr))
		},
		DeleteFunc: func(obj any) {
			gr, ok := obj.(*gwv1.GRPCRoute)
			if !ok {
				if t, isT := obj.(cache.DeletedFinalStateUnknown); isT {
					if gr2, ok2 := t.Obj.(*gwv1.GRPCRoute); ok2 {
						idx.deleteGRPCRoute(gr2.Namespace, gr2.Name)
						return
					}
				}
				logger.Debug("GRPCRoute delete: unexpected tombstone type")
				return
			}
			idx.deleteGRPCRoute(gr.Namespace, gr.Name)
		},
	})
}

func registerGatewayHandlers(inf cache.SharedIndexInformer, store *gatewayStore, _ *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { store.upsert(obj.(*gwv1.Gateway)) },
		UpdateFunc: func(_, n any) { store.upsert(n.(*gwv1.Gateway)) },
		DeleteFunc: func(obj any) {
			if gw, ok := obj.(*gwv1.Gateway); ok {
				store.delete(gw.Namespace, gw.Name)
			}
		},
	})
}

func registerGatewayClassHandlers(inf cache.SharedIndexInformer, store *gatewayClassStore, _ *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { store.upsert(obj.(*gwv1.GatewayClass)) },
		UpdateFunc: func(_, n any) { store.upsert(n.(*gwv1.GatewayClass)) },
		DeleteFunc: func(obj any) {
			if gc, ok := obj.(*gwv1.GatewayClass); ok {
				store.delete(gc.Name)
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
		RouteMode: RouteModeIngress,
	}
	if len(hr.Spec.ParentRefs) > 0 {
		applyParentRef(&ra, hr.Spec.ParentRefs[0], hr.Namespace, gwStore, gcStore, cfg)
	}
	ra.RuleNames = httpRuleNames(hr)
	if cfg.EmitStatusConds {
		ra.Accepted, ra.ResolvedRefs = statusFlags(hr.Status.Parents)
	}
	return ra
}

func grpcRouteToAttrs(gr *gwv1.GRPCRoute, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config) RouteAttributes {
	ra := RouteAttributes{
		Kind:      RouteKindGRPCRoute,
		Name:      gr.Name,
		Namespace: gr.Namespace,
		UID:       string(gr.UID),
		RouteMode: RouteModeIngress,
	}
	if len(gr.Spec.ParentRefs) > 0 {
		applyParentRef(&ra, gr.Spec.ParentRefs[0], gr.Namespace, gwStore, gcStore, cfg)
	}
	ra.RuleNames = grpcRuleNames(gr)
	if cfg.EmitStatusConds {
		ra.Accepted, ra.ResolvedRefs = statusFlags(gr.Status.Parents)
	}
	return ra
}

// applyParentRef resolves a route's first parentRef into ra. It is shared by
// httpRouteToAttrs and grpcRouteToAttrs so HTTP and GRPC enrichment stay
// symmetric (design §4.6). It fills, in GAMMA-aware order:
//
//   - ParentRef string + ParentKind (always; default "Gateway" per spec)
//   - Service-parent (GAMMA mesh): RouteMode=mesh, parent Service name/ns/port
//     (§4.1), and producer/consumer role (§4.4)
//   - Gateway-parent: Gateway/GatewayClass attribution, plus class-based
//     waypoint detection (§4.2/§4.3) — a Gateway whose gatewayClassName is a
//     configured waypoint class flips the route to mesh and stamps the
//     waypoint implementation + identity.
func applyParentRef(ra *RouteAttributes, pr gwv1.ParentReference, routeNS string, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config) {
	ra.ParentRef = formatParentRef(pr, routeNS)
	ra.ParentKind = parentKindOf(pr)

	ns := routeNS
	if pr.Namespace != nil && *pr.Namespace != "" {
		ns = string(*pr.Namespace)
	}

	if isServiceParent(pr) {
		// GAMMA mesh-mode route: the parent IS a backend Service. No
		// Gateway/GatewayClass attribution; stamp parent Service identity.
		ra.RouteMode = RouteModeMesh
		ra.ParentServiceName = string(pr.Name)
		ra.ParentServiceNamespace = ns
		if pr.Port != nil {
			ra.ParentServicePort = *pr.Port
		}
		classifyRouteRole(ra, routeNS, cfg)
		return
	}

	gw, ok := gwStore.get(ns, string(pr.Name))
	if !ok {
		return
	}
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

	// Class-based waypoint detection (§4.2/§4.3): a Gateway whose class is a
	// configured waypoint class is a mesh data-plane waypoint, not a
	// north-south ingress. Flip the route mode and stamp the waypoint identity
	// (the waypoint Gateway itself).
	if impl := cfg.Mesh.waypointImplForClass(ra.GatewayClassName); impl != "" {
		ra.RouteMode = RouteModeMesh
		ra.WaypointImplementation = impl
		ra.WaypointName = gw.Name
		ra.WaypointNamespace = gw.Namespace
	}
}

// classifyRouteRole stamps producer/consumer role on a Service-parent mesh
// route (§4.4): a route sharing its parent Service's namespace is a producer
// route (owned by the service team); any other namespace is a consumer route
// (client-team traffic-policy override). No-op when disabled or when the parent
// Service namespace is unknown.
func classifyRouteRole(ra *RouteAttributes, routeNS string, cfg *Config) {
	if !cfg.Mesh.ClassifyRouteRoleEnabled() || ra.ParentServiceNamespace == "" {
		return
	}
	if routeNS == ra.ParentServiceNamespace {
		ra.RouteRole = RouteRoleProducer
	} else {
		ra.RouteRole = RouteRoleConsumer
	}
}

// parentKindOf returns the parentRef Kind, defaulting to "Gateway" per the
// Gateway API spec when Kind is nil/empty.
func parentKindOf(pr gwv1.ParentReference) string {
	if pr.Kind != nil && *pr.Kind != "" {
		return string(*pr.Kind)
	}
	return "Gateway"
}

// httpRuleNames projects HTTPRouteRule.Name by rule index (§4.5). Returns nil
// when no rule carries a name so unnamed-rule routes keep a nil slice (no
// allocation, and stampRouteAttrs stamps nothing).
func httpRuleNames(hr *gwv1.HTTPRoute) []string {
	if len(hr.Spec.Rules) == 0 {
		return nil
	}
	names := make([]string, len(hr.Spec.Rules))
	any := false
	for i, rule := range hr.Spec.Rules {
		if rule.Name != nil && *rule.Name != "" {
			names[i] = string(*rule.Name)
			any = true
		}
	}
	if !any {
		return nil
	}
	return names
}

// grpcRuleNames mirrors httpRuleNames for GRPCRoute (§4.5/§4.6).
func grpcRuleNames(gr *gwv1.GRPCRoute) []string {
	if len(gr.Spec.Rules) == 0 {
		return nil
	}
	names := make([]string, len(gr.Spec.Rules))
	any := false
	for i, rule := range gr.Spec.Rules {
		if rule.Name != nil && *rule.Name != "" {
			names[i] = string(*rule.Name)
			any = true
		}
	}
	if !any {
		return nil
	}
	return names
}

// isServiceParent reports whether a ParentReference targets a core-group
// Service (GAMMA east-west routing) rather than a Gateway. Empty group is
// the core API group; explicit "" or unset means core. A nil Kind defaults
// to "Gateway" per the Gateway API CRDs.
func isServiceParent(pr gwv1.ParentReference) bool {
	if pr.Group != nil && *pr.Group != "" {
		return false
	}
	if pr.Kind == nil {
		return false
	}
	return *pr.Kind == "Service"
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
	// Group resolution mirrors isServiceParent so the two are consistent:
	//  - Kind=Service: ALWAYS core group (""), since Service is a core
	//    Kubernetes resource. Honors both nil-Group and explicit-empty-Group
	//    forms of GAMMA parents — both classify as mesh-mode upstream and
	//    must format the same way here.
	//  - Otherwise: nil Group defaults to "gateway.networking.k8s.io" per
	//    Gateway API; explicit value (including "") wins.
	kind := "Gateway"
	if pr.Kind != nil && *pr.Kind != "" {
		kind = string(*pr.Kind)
	}
	var group string
	switch {
	case kind == "Service":
		group = ""
	case pr.Group != nil:
		group = string(*pr.Group)
	default:
		group = "gateway.networking.k8s.io"
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
