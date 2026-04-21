# syntax=docker/dockerfile:1.7
#
# Multi-arch runtime image for the custom OTel Collector with
# gatewayapiprocessor baked in. The binaries are cross-compiled natively by
# the `binaries` job in .github/workflows/release.yml (fast, no QEMU) and
# staged under dist/linux/<arch>[variant]/otelcol-gatewayapi. buildx then
# assembles a manifest list across all target platforms by COPY only —
# zero compilation inside Docker.
#
# Expected layout at build time:
#   dist/linux/amd64/otelcol-gatewayapi
#   dist/linux/arm64/otelcol-gatewayapi
#   dist/linux/armv7/otelcol-gatewayapi
#   dist/linux/386/otelcol-gatewayapi
#   dist/linux/ppc64le/otelcol-gatewayapi
#   dist/linux/s390x/otelcol-gatewayapi

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

ARG TARGETARCH
ARG TARGETVARIANT

COPY dist/linux/${TARGETARCH}${TARGETVARIANT}/otelcol-gatewayapi /otelcol-gatewayapi

USER nonroot:nonroot

# OTLP gRPC, OTLP HTTP, healthcheck, Prometheus scrape
EXPOSE 4317 4318 13133 8888

ENTRYPOINT ["/otelcol-gatewayapi"]
CMD ["--config=/etc/otelcol/config.yaml"]

LABEL org.opencontainers.image.source="https://github.com/henrikrexed/gatewayapiprocessor" \
      org.opencontainers.image.description="OTel Collector with gatewayapiprocessor (ObsSummit NA 2026 demo)" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="IsItObservable"
