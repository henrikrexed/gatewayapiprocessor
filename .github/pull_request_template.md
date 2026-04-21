## Summary

<!-- What does this PR change and why? Link to the Paperclip issue or ISI-### ticket. -->

## Checklist

- [ ] Paperclip issue linked (e.g. `ISI-685`)
- [ ] Tests added or updated (`go test ./...` in `gatewayapiprocessor/`)
- [ ] `go vet ./...` and `golangci-lint run` pass locally
- [ ] Pinned versions reviewed against `VERSIONS.md` (no silent bumps)
- [ ] Docs updated if behavior changed (`docs/` or `README.md`)
- [ ] Demo path verified if this touches the hero demo (`deploy/`, `Makefile`)

## Version / pin impact

<!-- If this PR changes any pinned version (Go toolchain, OCB, OTel Collector,
     Gateway API, Istio, Kgateway, etc.), note it here and bump VERSIONS.md in
     the same PR. Version bumps require re-recording the pre-recorded fallback
     clip per ISI-671. -->

- [ ] No pin impact
- [ ] `VERSIONS.md` updated (single file per bump)
- [ ] Fallback re-record required (`ISI-671`)

## Test plan

<!-- How did you verify this? What should a reviewer run locally? -->
