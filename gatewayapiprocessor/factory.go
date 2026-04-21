package gatewayapiprocessor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
)

// typeStr is the processor's config type key.
// Matches the YAML key `processors.gatewayapi` used in processor-spec §2.2 and §2.3.
const typeStr = "gatewayapi"

// Type is the component.Type for this processor.
var Type = component.MustNewType(typeStr)

// NewFactory builds a processor.Factory for gatewayapiprocessor.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		Type,
		createDefaultConfig,
		processor.WithTraces(createTracesProcessor, component.StabilityLevelDevelopment),
		processor.WithLogs(createLogsProcessor, component.StabilityLevelDevelopment),
		processor.WithMetrics(createMetricsProcessor, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		AuthType: AuthTypeServiceAccount,
		Watch: WatchConfig{
			Namespaces:   nil,
			ResyncPeriod: 5 * time.Minute,
		},
		Parsers: []ParserConfig{
			{
				Name: "envoy",
				Controllers: []string{
					`^gateway\.envoyproxy\.io/gatewayclass-controller$`,
					`^kgateway\.dev/gatewayclass-controller$`,
					`^istio\.io/gateway-controller$`,
				},
				SourceAttribute: "route_name",
				FormatRegex:     `^httproute/(?P<ns>[^/]+)/(?P<name>[^/]+)(?:/rule/(?P<rule>\d+))?(?:/match/(?P<match>\d+))?`,
			},
			{
				Name: "linkerd",
				Controllers: []string{
					`^linkerd\.io/gateway-controller$`,
				},
				LinkerdLabels: LinkerdLabelsConfig{
					RouteName:      "route_name",
					RouteKind:      "route_kind",
					RouteNamespace: "route_namespace",
					ParentName:     "parent_name",
				},
			},
			{
				Name:                 "passthrough",
				SourceAttribute:      "route_name",
				PassthroughAttribute: "k8s.gatewayapi.raw_route_name",
			},
		},
		Enrich: EnrichConfig{
			Traces:  true,
			Logs:    true,
			Metrics: true,
			ExcludeFromMetricAttributes: []string{
				"k8s.httproute.uid",
				"k8s.gateway.uid",
				"k8s.gatewayapi.raw_route_name",
			},
		},
		EmitStatusConds: true,
		BackendRefFallba: BackendRefFallback{
			Enabled:         true,
			SourceAttribute: "server.address",
		},
		InformerSyncTimeout: 30 * time.Second,
	}
}

func createTracesProcessor(
	_ context.Context,
	set processor.Settings,
	cfg component.Config,
	next consumer.Traces,
) (processor.Traces, error) {
	p, err := newProcessor(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	p.tracesNext = next
	attachDefaultStartHook(p)
	return p, nil
}

func createLogsProcessor(
	_ context.Context,
	set processor.Settings,
	cfg component.Config,
	next consumer.Logs,
) (processor.Logs, error) {
	p, err := newProcessor(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	p.logsNext = next
	attachDefaultStartHook(p)
	return p, nil
}

func createMetricsProcessor(
	_ context.Context,
	set processor.Settings,
	cfg component.Config,
	next consumer.Metrics,
) (processor.Metrics, error) {
	p, err := newProcessor(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	p.metricsNext = next
	attachDefaultStartHook(p)
	return p, nil
}

// attachDefaultStartHook wires the informer-backed RouteLookup into the
// processor unless auth_type=none. Tests replace the hook before Start()
// to inject a static lookup or a never-syncing stub.
func attachDefaultStartHook(p *gatewayAPIProcessor) {
	if p.cfg.AuthType == AuthTypeNone {
		return
	}
	p.startHook = func(ctx context.Context) (RouteLookup, func(context.Context) error, error) {
		return newInformers(ctx, p.logger, p.cfg)
	}
}
