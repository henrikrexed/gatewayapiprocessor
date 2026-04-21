.PHONY: demo clean test lint build-collector push-collector ocb-install fmt \
        backends-up backends-down dynatrace-secret break fix

# Versions — see VERSIONS.md
GO              ?= go
OCB_VERSION     ?= 0.124.0
COLLECTOR_TAG   ?= 2026-04-21
IMAGE_REGISTRY  ?= ghcr.io/henrikrexed
IMAGE_NAME      ?= otelcol-gatewayapi
IMAGE           := $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(COLLECTOR_TAG)
PLATFORMS       ?= linux/amd64,linux/arm64

ISTIO_VERSION   ?= 1.26.0
KGATEWAY_VERSION ?= v2.1.0
DEMO_CHART_VERSION ?= 0.38.0   # OTel Demo chart pinned to demo v2.2.0

# Kind cluster bring-up ordering per processor-spec §4.1 / obs-feasibility §D7:
# operator → ambient ztunnel → waypoint → Kgateway → OBI → OTel Demo → collector
demo: backends-up
	kind create cluster --config deploy/kind-cluster.yaml
	# 1. CRDs + operators
	kubectl apply -k deploy/00-operators/gateway-api-crds.yaml
	kubectl apply -k deploy/00-operators/otel-operator.yaml
	# 2. Ambient ztunnel (helm) + telemetry
	helm upgrade --install istio-base oci://gcr.io/istio-release/charts/base \
		--version $(ISTIO_VERSION) --namespace istio-system --create-namespace
	helm upgrade --install ztunnel oci://gcr.io/istio-release/charts/ztunnel \
		--version $(ISTIO_VERSION) --namespace istio-system --set profile=ambient
	kubectl apply -f deploy/00-operators/ambient-ztunnel.yaml
	# 3. Waypoint
	kubectl apply -f deploy/10-mesh/waypoint.yaml
	# 4. Kgateway
	helm upgrade --install kgateway \
		oci://cr.kgateway.dev/kgateway-dev/charts/kgateway \
		--version $(KGATEWAY_VERSION) \
		--namespace kgateway-system --create-namespace
	kubectl apply -f deploy/10-mesh/kgateway.yaml
	# 5. OBI DaemonSet
	kubectl apply -f deploy/20-obi/obi-daemonset.yaml
	# 6. OTel Demo + HTTPRoute/GRPCRoute
	helm upgrade --install otel-demo open-telemetry/opentelemetry-demo \
		--version $(DEMO_CHART_VERSION) --namespace demo --create-namespace \
		--values deploy/30-demo/otel-demo-values.yaml
	kubectl apply -f deploy/30-demo/otel-demo.yaml
	# 7. Custom collector
	kubectl apply -f deploy/40-collector/rbac.yaml
	$(MAKE) dynatrace-secret
	kubectl apply -f deploy/40-collector/collector.yaml
	@echo ""
	@echo "Waiting for HTTPRoute Accepted=True..."
	kubectl wait --for=condition=Accepted=True httproute/api --namespace demo --timeout=180s
	@echo ""
	@echo "Demo ready. Grafana: http://localhost:3000 (anonymous)."
	@echo "Hero beat: make break    → flip to missing backendRef"
	@echo "Revert:    make fix"

clean:
	-kind delete cluster --name gatewayapi-demo
	$(MAKE) backends-down

test:
	cd gatewayapiprocessor && $(GO) test ./...

lint:
	cd gatewayapiprocessor && golangci-lint run

fmt:
	cd gatewayapiprocessor && $(GO) fmt ./...

ocb-install:
	$(GO) install go.opentelemetry.io/collector/cmd/builder@v$(OCB_VERSION)

# Builds the OCB-produced collector binary locally. CI is authoritative for the image push.
build-collector: ocb-install
	builder --config=builder-config.yaml

# Multi-arch image build + push via buildx. Requires docker buildx + QEMU setup.
# Used by .github/workflows/build.yaml — documented here so it can be run manually.
push-collector:
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMAGE) \
		--tag $(IMAGE_REGISTRY)/$(IMAGE_NAME):latest \
		--push \
		--label org.opencontainers.image.source="https://github.com/henrikrexed/gatewayapiprocessor" \
		--label org.opencontainers.image.description="OTel Collector with gatewayapiprocessor (ObsSummit NA 2026 demo)" \
		--label org.opencontainers.image.licenses="Apache-2.0" \
		--label org.opencontainers.image.version="$(COLLECTOR_TAG)" \
		.

# ------------------ backends ------------------
backends-up:
	docker compose -f backends/grafana/docker-compose.yaml up -d

backends-down:
	-docker compose -f backends/grafana/docker-compose.yaml down -v

# Creates the dynatrace-otlp Secret from host env vars. Stays silent if the
# env is not set (exporter falls back to warning, traces still flow to Grafana).
dynatrace-secret:
	@if [ -n "$$DT_TENANT_URL" ] && [ -n "$$DT_API_TOKEN" ]; then \
		kubectl create namespace otel-system --dry-run=client -o yaml | kubectl apply -f - ; \
		kubectl create secret generic dynatrace-otlp \
			--namespace otel-system \
			--from-literal=endpoint="$$DT_TENANT_URL" \
			--from-literal=api-token="$$DT_API_TOKEN" \
			--dry-run=client -o yaml | kubectl apply -f - ; \
		echo "dynatrace-otlp Secret applied." ; \
	else \
		echo "DT_TENANT_URL / DT_API_TOKEN not set — skipping Dynatrace Secret (exporter stays inert)." ; \
	fi

# ------------------ hero demo beats ------------------
break:
	kubectl apply -f deploy/break-backendref.yaml

fix:
	kubectl apply -f deploy/fix-backendref.yaml
