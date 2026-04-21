# syntax=docker/dockerfile:1.7
#
# Multi-stage, multi-arch image for the custom OTel Collector with
# gatewayapiprocessor baked in. Built via OCB against builder-config.yaml.
#
# Build context is the repo root. buildx handles TARGETOS/TARGETARCH.

ARG GO_VERSION=1.25
ARG OCB_VERSION=0.150.0

# -----------------------------------------------------------------------------
# Stage 1 — OCB builder
# -----------------------------------------------------------------------------
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-bookworm AS build

ARG OCB_VERSION
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src

# Step 1 — install OCB for the BUILD platform (not the target). If GOOS/GOARCH
# were set here, `go install` would drop the binary into $GOPATH/bin/<goos>_<goarch>/
# and PATH would not find it on cross builds (ISI-703).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install go.opentelemetry.io/collector/cmd/builder@v${OCB_VERSION}

# Step 2 — now switch to target platform env so OCB cross-compiles the collector.
# TARGETVARIANT is "v7" for linux/arm/v7; map it into GOARM.
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    GOFLAGS=-trimpath

# Copy only what OCB needs. The replace block in builder-config.yaml points
# at the local module, so the subdir must be present.
COPY builder-config.yaml ./builder-config.yaml
COPY gatewayapiprocessor ./gatewayapiprocessor

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "${TARGETARCH}" = "arm" ] && [ -n "${TARGETVARIANT}" ]; then \
      export GOARM="${TARGETVARIANT#v}"; \
    fi \
 && builder --config=builder-config.yaml \
 && ls -lh _build/ \
 && mkdir -p /out \
 && cp _build/otelcol-gatewayapi /out/otelcol-gatewayapi

# -----------------------------------------------------------------------------
# Stage 2 — distroless runtime
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

ARG TARGETOS
ARG TARGETARCH

COPY --from=build /out/otelcol-gatewayapi /otelcol-gatewayapi

USER nonroot:nonroot

# OTLP gRPC, OTLP HTTP, healthcheck, Prometheus scrape
EXPOSE 4317 4318 13133 8888

ENTRYPOINT ["/otelcol-gatewayapi"]
CMD ["--config=/etc/otelcol/config.yaml"]

LABEL org.opencontainers.image.source="https://github.com/henrikrexed/gatewayapiprocessor" \
      org.opencontainers.image.description="OTel Collector with gatewayapiprocessor (ObsSummit NA 2026 demo)" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="IsItObservable"
