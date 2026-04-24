# Pre-recorded fallback — hero demo

This is the **backup video** that plays if the live `kubectl apply` beat
fails on stage (per [ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)).

**Re-record trigger:** ANY bump in `VERSIONS.md` (see [ISI-670](https://paperclip.isitobservable.com/ISI/issues/ISI-670)).

## Tooling

- `asciinema` for the terminal capture (crisp, small, replayable).
- `obs-studio` for the Grafana browser capture (same 1080p profile the
  talk uses for slides).
- Final asset: side-by-side `.mp4` + `.cast` committed under `assets/`
  and linked from README.

## Script (2m45s target)

```
[00:00] TERM: kubectl get httproute api -n demo -o yaml | grep backendRef
        → shows backendRef.name: cartservice (working state).
[00:10] GRAFANA: dashboard "HTTPRoute misconfig — before/after" in steady state.
[00:20] TERM: make break
        → kubectl apply -f deploy/break-backendref.yaml
        → "httproute.gateway.networking.k8s.io/api configured"
[00:25] GRAFANA: 5xx panel spikes; table lights up with
        k8s.httproute.resolved_refs=false, k8s.httproute.name=api.
[01:10] TERM: kubectl get httproute api -n demo -o yaml \
                | yq '.status.parents[0].conditions[] | select(.type=="ResolvedRefs")'
        → shows status=False, reason=BackendNotFound.
[01:30] GRAFANA: click trace row → Tempo TraceView with
        k8s.httproute.name=api, k8s.httproute.resolved_refs=false,
        k8s.gatewayclass.controller_name=kgateway.dev/kgateway-controller.
[02:10] TERM: make fix
        → kubectl apply -f deploy/fix-backendref.yaml
[02:20] GRAFANA: 5xx panel drops back to baseline.
[02:40] Closing slide: reversion on-screen, no referenceGrant patterns.
```

## Re-record checklist

After any VERSIONS.md bump:

1. `make clean && make demo` on the locked pins.
2. Re-run the script above. Capture with both tools in parallel.
3. Commit the new `assets/fallback-<date>.mp4` + `.cast`.
4. Update README reference + the talk deck slide.
5. Comment on [ISI-671](https://paperclip.isitobservable.com/ISI/issues/ISI-671)
   with the new asset path.
