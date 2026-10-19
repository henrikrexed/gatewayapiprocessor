package gatewayapiprocessor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreinformers "k8s.io/client-go/informers"
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
func newInformers(ctx context.Context, logger *zap.Logger, cfg *Config, inferCounter metric.Int64Counter) (RouteLookup, func(context.Context) error, error) {
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

	// Pass 1: create every Gateway-API informer and register the Gateway /
	// GatewayClass handlers (they populate the stores the route projection
	// reads). Route handlers are deferred to pass 2 because the nil-Kind
	// inference (ISI-6188 / D4) threads a parentInferrer into them, and that
	// inferrer needs both the Gateway informers' HasSynced signal (guardrail 1)
	// and the Service name index — neither of which exists until every informer
	// is created. f.Start is deferred to pass 3 so no projection runs — and so
	// no inference is attempted — before the inferrer is wired.
	var gwInfs []cache.SharedIndexInformer
	type routeInfPair struct{ hr, gr cache.SharedIndexInformer }
	var routeInfs []routeInfPair
	for _, f := range factories {
		hrInf := f.Gateway().V1().HTTPRoutes().Informer()
		grInf := f.Gateway().V1().GRPCRoutes().Informer()
		gwInf := f.Gateway().V1().Gateways().Informer()
		gcInf := f.Gateway().V1().GatewayClasses().Informer()

		registerGatewayHandlers(gwInf, gwStore, logger)
		registerGatewayClassHandlers(gcInf, gcStore, logger)

		gwInfs = append(gwInfs, gwInf)
		routeInfs = append(routeInfs, routeInfPair{hr: hrInf, gr: grInf})
		informers = append(informers, hrInf, grInf, gwInf, gcInf)
	}

	// Service informer (ISI-851 / ISI-6178 / ISI-6188). Gated independently per
	// feature; the shared informer builds only the indexes the enabled features
	// need (design D5 — one watch, N indexes):
	//   - buildIP:       ClusterIP reverse-lookup index (backendref_fallback).
	//                    Consulted from applyBackendRefFallback, which
	//                    short-circuits when the feature is off, so skipping the
	//                    informer avoids a cluster-scoped Service list+watch for
	//                    no benefit (Copilot review on PR #55).
	//   - buildWaypoint: use-waypoint label cache (GAMMA waypoint attribution).
	//   - buildName:     Service name index (nil-Kind parent-Service inference).
	//
	// The Service informer and the EndpointSlice informer (ISI-875) share one
	// kubernetes clientset / HTTP connection pool (Copilot review on PR #66).
	var ipIndex *serviceIPIndex
	var wpIndex *waypointIndex
	var nameIndex *serviceNameIndex
	var podIndex *podIPIndex
	var svcFactories []coreinformers.SharedInformerFactory
	buildIP := cfg.BackendRefFallback.Enabled
	buildWaypoint := cfg.Mesh.WaypointEnabled()
	buildName := cfg.Mesh.InferServiceParentEnabled()
	if buildIP || buildWaypoint || buildName {
		k8sClient, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("build kubernetes clientset: %w", err)
		}

		var svcInformers []cache.SharedIndexInformer
		svcInformers, svcFactories, ipIndex, wpIndex, nameIndex, err = startServiceInformer(logger, k8sClient, cfg, buildIP, buildWaypoint, buildName)
		if err != nil {
			return nil, nil, fmt.Errorf("start service informer: %w", err)
		}
		informers = append(informers, svcInformers...)

		// EndpointSlice informer (ISI-875). Defaults to on whenever
		// BackendRefFallback.Enabled is true; opt out via
		// `backendref_fallback.pod_ip: false` on scale-sensitive clusters.
		// Scoped to backendref_fallback only — waypoint attribution and nil-Kind
		// inference never need PodIP resolution.
		if cfg.BackendRefFallback.Enabled && cfg.BackendRefFallback.PodIPEnabled() {
			var esInformers []cache.SharedIndexInformer
			esInformers, podIndex, err = startEndpointSliceInformer(ctx, logger, k8sClient, cfg)
			if err != nil {
				return nil, nil, fmt.Errorf("start endpointslice informer: %w", err)
			}
			informers = append(informers, esInformers...)
		}
	}

	// Build the D4 inferrer now that both halves of its predicate exist: the
	// Gateway informers' aggregate HasSynced (guardrail 1) and the Service name
	// index. newParentInferrer returns nil when mesh.infer_service_parent is
	// off, and the projection path is nil-safe, so inference simply never fires.
	gatewaySynced := func() bool {
		for _, gi := range gwInfs {
			if !gi.HasSynced() {
				return false
			}
		}
		return true
	}
	inferrer := newParentInferrer(cfg, logger, inferCounter, gatewaySynced, nameIndex.Exists)

	// Pass 2: register the route handlers with the inferrer wired in. Still no
	// f.Start, so these handlers won't fire until pass 3.
	for _, ri := range routeInfs {
		registerHTTPRouteHandlers(ri.hr, index, gwStore, gcStore, cfg, inferrer, logger)
		registerGRPCRouteHandlers(ri.gr, index, gwStore, gcStore, cfg, inferrer, logger)
	}

	// Policy informers (ISI-804). Only fires when cfg.Watch.Policies is
	// configured; routes & enrichment continue to work unchanged for users
	// who haven't opted in. No inferrer dependency, so it manages its own start.
	policyInformers, err := startPolicyInformers(ctx, logger, restCfg, cfg, index)
	if err != nil {
		return nil, nil, fmt.Errorf("start policy informers: %w", err)
	}
	informers = append(informers, policyInformers...)

	// Pass 3: start every Gateway-API and Service factory. All handlers are now
	// registered and the inferrer is wired, so the guardrail-1 invariant holds:
	// a projection that runs during the initial list sees gatewaySynced()==false
	// and never infers.
	for _, f := range factories {
		f.Start(ctx.Done())
	}
	for _, f := range svcFactories {
		f.Start(ctx.Done())
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

func registerHTTPRouteHandlers(inf cache.SharedIndexInformer, idx *routeIndex, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, inferrer *parentInferrer, logger *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			hr, ok := obj.(*gwv1.HTTPRoute)
			if !ok {
				return
			}
			ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg, inferrer)
			idx.upsertHTTPRoute(ra, backendRefsFromHTTPRoute(hr))
		},
		UpdateFunc: func(_, newObj any) {
			hr, ok := newObj.(*gwv1.HTTPRoute)
			if !ok {
				return
			}
			ra := httpRouteToAttrs(hr, gwStore, gcStore, cfg, inferrer)
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

func registerGRPCRouteHandlers(inf cache.SharedIndexInformer, idx *routeIndex, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, inferrer *parentInferrer, logger *zap.Logger) {
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			gr, ok := obj.(*gwv1.GRPCRoute)
			if !ok {
				return
			}
			idx.upsertGRPCRoute(grpcRouteToAttrs(gr, gwStore, gcStore, cfg, inferrer), backendRefsFromGRPCRoute(gr))
		},
		UpdateFunc: func(_, newObj any) {
			gr, ok := newObj.(*gwv1.GRPCRoute)
			if !ok {
				return
			}
			idx.upsertGRPCRoute(grpcRouteToAttrs(gr, gwStore, gcStore, cfg, inferrer), backendRefsFromGRPCRoute(gr))
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

// httpRouteToAttrs projects an HTTPRoute to RouteAttributes. inferrer carries
// the nil-Kind parent-Service inference dependencies (ISI-6188 / D4) and may be
// nil (inference off / no-Kubernetes mode / tests that don't exercise D4).
func httpRouteToAttrs(hr *gwv1.HTTPRoute, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, inferrer *parentInferrer) RouteAttributes {
	ra := RouteAttributes{
		Kind:      RouteKindHTTPRoute,
		Name:      hr.Name,
		Namespace: hr.Namespace,
		UID:       string(hr.UID),
		RouteMode: RouteModeIngress,
	}
	if len(hr.Spec.ParentRefs) > 0 {
		applyParentRef(&ra, hr.Spec.ParentRefs[0], hr.Namespace, gwStore, gcStore, cfg, inferrer)
	}
	ra.RuleNames = httpRuleNames(hr)
	if cfg.EmitStatusConds {
		ra.Accepted, ra.ResolvedRefs = statusFlags(hr.Status.Parents)
	}
	return ra
}

// grpcRouteToAttrs mirrors httpRouteToAttrs for GRPCRoute (§4.6). inferrer may
// be nil; see httpRouteToAttrs.
func grpcRouteToAttrs(gr *gwv1.GRPCRoute, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, inferrer *parentInferrer) RouteAttributes {
	ra := RouteAttributes{
		Kind:      RouteKindGRPCRoute,
		Name:      gr.Name,
		Namespace: gr.Namespace,
		UID:       string(gr.UID),
		RouteMode: RouteModeIngress,
	}
	if len(gr.Spec.ParentRefs) > 0 {
		applyParentRef(&ra, gr.Spec.ParentRefs[0], gr.Namespace, gwStore, gcStore, cfg, inferrer)
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
//   - nil-Kind inference (ISI-6188 / D4): a Gateway-less, nil-Kind core-group
//     parentRef whose ns/name names a real Service (post Gateway-sync) is
//     reclassified as a mesh Service parent — see inferrer.maybeInferServiceParent.
//
// inferrer may be nil (inference off), in which case the nil-Kind fallback is
// skipped and the function behaves exactly as before.
func applyParentRef(ra *RouteAttributes, pr gwv1.ParentReference, routeNS string, gwStore *gatewayStore, gcStore *gatewayClassStore, cfg *Config, inferrer *parentInferrer) {
	ra.ParentRef = formatParentRef(pr, routeNS)
	ra.ParentKind = parentKindOf(pr)

	ns := routeNS
	if pr.Namespace != nil && *pr.Namespace != "" {
		ns = string(*pr.Namespace)
	}

	if isServiceParent(pr) {
		// GAMMA mesh-mode route: the parent IS a backend Service (explicit
		// Kind=Service). No Gateway/GatewayClass attribution.
		applyServiceParent(ra, pr, ns, routeNS, cfg)
		return
	}

	gw, ok := gwStore.get(ns, string(pr.Name))
	if !ok {
		// No Gateway with this ns/name. D4 (ISI-6188): if this is a nil-Kind
		// core-group parentRef whose ns/name names a real Service — and the
		// Gateway informer has synced — reclassify as a mesh Service parent.
		// ParentKind flips to "Service" so it matches the explicit-Service path
		// and stays consistent with route-mode=mesh. No inference marker is
		// stamped (guardrail 2); the only external signal is the self-telemetry
		// counter + debug-log emitted by maybeInferServiceParent.
		if inferrer.maybeInferServiceParent(pr, ns, ra.Kind) {
			ra.ParentKind = "Service"
			applyServiceParent(ra, pr, ns, routeNS, cfg)
		}
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

// applyServiceParent stamps the GAMMA mesh-mode attributes for a Service
// parent (§4.1/§4.4): route mode, parent Service name/namespace/port, and
// producer/consumer role. Shared by the explicit-Kind=Service path and the D4
// nil-Kind inference path so both produce identical mesh attributes. parentNS
// is the resolved parent-Service namespace (routeNS unless pr.Namespace is set);
// routeNS is the route's own namespace, used for role classification.
func applyServiceParent(ra *RouteAttributes, pr gwv1.ParentReference, parentNS, routeNS string, cfg *Config) {
	ra.RouteMode = RouteModeMesh
	ra.ParentServiceName = string(pr.Name)
	ra.ParentServiceNamespace = parentNS
	if pr.Port != nil {
		ra.ParentServicePort = *pr.Port
	}
	classifyRouteRole(ra, routeNS, cfg)
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
