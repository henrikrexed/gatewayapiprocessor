# Makefile — gatewayapiprocessor demo repo.
#
# SCOPE NOTE (2026-04-21, ISI-671): `make demo` is no longer an all-in-one
# automation target. Henrik provisions his homelab cluster by hand; the
# authoritative runbook is `demo-steps` §C on ISI-671. This Makefile keeps
# convenience shortcuts for the processor Go module + the live demo beats,
# plus a `make steps` target that prints the install order so the script is
# always at the operator's fingertips.

.PHONY: steps test lint fmt ocb-install build-collector push-collector \
        dynatrace-secret break fix clean

# Versions — authoritative pins live in VERSIONS.md.
GO              ?= go
OCB_VERSION     ?= 0.150.0
COLLECTOR_TAG   ?= 2026-04-21
IMAGE_REGISTRY  ?= ghcr.io/henrikrexed
IMAGE_NAME      ?= otelcol-gatewayapi
IMAGE           := $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(COLLECTOR_TAG)
PLATFORMS       ?= linux/amd64,linux/arm64

# ------------------ default help ------------------
.DEFAULT_GOAL := steps

steps:
	@echo ""
	@echo "# Demo install order — see ISI-671 'demo-steps' §C for verbatim commands."
	@echo "# Homelab cluster is provisioned by Henrik; these commands run from the"
	@echo "# same checkout against that kubeconfig. Do not run on stage."
	@echo ""
	@echo "  1. Gateway API CRDs + OTel Operator"
	@echo "     kubectl apply -f deploy/00-operators/gateway-api-crds.yaml"
	@echo "     kubectl apply -f deploy/00-operators/otel-operator.yaml"
	@echo ""
	@echo "  2. Istio ambient (ztunnel first, before anything touches cgroups)"
	@echo "     istioctl install --set profile=ambient -y"
	@echo ""
	@echo "  3. Waypoint for the otel-demo namespace"
	@echo "     kubectl apply -f deploy/10-mesh/waypoint.yaml"
	@echo ""
	@echo "  4. Kgateway (helm) + ingress Gateway"
	@echo "     helm upgrade --install kgateway oci://cr.kgateway.dev/kgateway-dev/charts/kgateway \\"
	@echo "       --version v2.1.0 --namespace kgateway-system --create-namespace"
	@echo "     kubectl apply -f deploy/10-mesh/kgateway.yaml"
	@echo ""
	@echo "  5. OBI DaemonSet (kernel >= 5.15 required)"
	@echo "     kubectl apply -f deploy/20-obi/obi-daemonset.yaml"
	@echo ""
	@echo "  6. OTel Demo v2.2.0 + HTTPRoute/GRPCRoute"
	@echo "     helm upgrade --install otel-demo open-telemetry/opentelemetry-demo \\"
	@echo "       --version 0.38.0 --namespace otel-demo --create-namespace \\"
	@echo "       --values deploy/30-demo/helm/values.yaml"
	@echo "     kubectl apply -f deploy/30-demo/"
	@echo ""
	@echo "  7. GAMMA mesh-bound routes + mesh policies"
	@echo "     kubectl apply -f deploy/10-mesh/gamma-routes.yaml"
	@echo "     kubectl apply -f deploy/10-mesh/policies/"
	@echo ""
	@echo "  8. Custom collector (gatewayapiprocessor) — Dynatrace exporter"
	@echo "     make dynatrace-secret    # loads DT_TENANT_URL + DT_API_TOKEN into a Secret"
	@echo "     kubectl apply -f deploy/40-collector/rbac.yaml"
	@echo "     kubectl apply -f deploy/40-collector/collector.yaml"
	@echo ""
	@echo "  9. Hero demo beats (on stage)"
	@echo "     make break    # flip HTTPRoute backendRef to a non-existent Service"
	@echo "     make fix      # revert"
	@echo ""
	@echo "Full walkthrough + preflight + fallback: ISI-671 'demo-steps'."
	@echo ""

# ------------------ processor module convenience ------------------
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

# ------------------ Dynatrace Secret ------------------
# Creates the dynatrace-otlp Secret from host env vars. Stays silent + exits 0
# if the env is not set — the exporter is configured optional so the collector
# still starts without it (traces sit in the debug tap until the token lands).
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

# ------------------ teardown ------------------
# Full teardown lives in demo-steps §H (ISI-671). This target wipes only the
# in-cluster CRs; it does not touch the homelab cluster, CRDs, or helm charts.
clean:
	-kubectl delete -f deploy/40-collector/collector.yaml --ignore-not-found
	-kubectl delete -f deploy/40-collector/rbac.yaml --ignore-not-found
	-kubectl delete -f deploy/30-demo/ --ignore-not-found
	-kubectl delete -f deploy/10-mesh/policies/ --ignore-not-found
	-kubectl delete -f deploy/10-mesh/gamma-routes.yaml --ignore-not-found
