#!/usr/bin/env bash
#
# check-versions.sh — verify version pins under deploy/, builder-config.yaml,
# and Makefile match the authoritative values in VERSIONS.md.
#
# Per ISI-671 demo-risk §6: this is the "versions-check" CI gate. Fails the PR
# when a version string drifts so the homelab demo never sees pin mismatch
# between what the slide says and what the cluster runs.
#
# Authoritative source: VERSIONS.md (root). All other files mirror it.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

VERSIONS_FILE="VERSIONS.md"
[[ -f "$VERSIONS_FILE" ]] || { echo "::error::$VERSIONS_FILE not found"; exit 1; }

fail=0
err() { echo "::error::$1"; fail=1; }
ok()  { echo "  ok: $1"; }

# ----------------------------------------------------------------------------
# Pull canonical values out of VERSIONS.md.
# ----------------------------------------------------------------------------

# The custom-collector image tag is a single string in VERSIONS.md under the
# "Custom collector:" bullet, formatted as
#   - Custom collector: `ghcr.io/henrikrexed/otelcol-gatewayapi:<TAG>`
COLLECTOR_IMAGE=$(grep -oE 'ghcr\.io/henrikrexed/otelcol-gatewayapi:[A-Za-z0-9._+-]+' "$VERSIONS_FILE" | head -1)
[[ -n "$COLLECTOR_IMAGE" ]] || err "could not extract collector image from $VERSIONS_FILE"
COLLECTOR_TAG="${COLLECTOR_IMAGE##*:}"

# OCB / OTel Collector version (the row "OTel Collector (OCB build) | vX.Y.Z").
OCB_VERSION=$(awk -F'|' '/OTel Collector \(OCB build\)/ {gsub(/[ v]/,"",$3); print $3; exit}' "$VERSIONS_FILE")
[[ -n "$OCB_VERSION" ]] || err "could not extract OCB version from $VERSIONS_FILE"

# Go version pin.
GO_VERSION=$(awk -F'|' '/^\| Go +\|/ {gsub(/[ ]/,"",$3); print $3; exit}' "$VERSIONS_FILE")
[[ -n "$GO_VERSION" ]] || err "could not extract Go version from $VERSIONS_FILE"

echo "Authoritative values from VERSIONS.md:"
echo "  collector_image = $COLLECTOR_IMAGE"
echo "  collector_tag   = $COLLECTOR_TAG"
echo "  ocb_version     = $OCB_VERSION"
echo "  go_version      = $GO_VERSION"
echo

# ----------------------------------------------------------------------------
# 1. Every image reference under deploy/ must use the pinned tag.
# ----------------------------------------------------------------------------
echo "[1/4] deploy/ — all otelcol-gatewayapi image references match VERSIONS.md tag"
mismatch=$(grep -RhnoE 'ghcr\.io/henrikrexed/otelcol-gatewayapi:[A-Za-z0-9._+-]+' deploy/ 2>/dev/null \
  | grep -v ":${COLLECTOR_TAG}\$" || true)
if [[ -n "$mismatch" ]]; then
  err "deploy/ contains otelcol-gatewayapi image with tag != $COLLECTOR_TAG:"
  echo "$mismatch" | sed 's/^/    /'
else
  ok "deploy/ image tags == $COLLECTOR_TAG"
fi

# ----------------------------------------------------------------------------
# 2. Makefile COLLECTOR_TAG must match VERSIONS.md tag.
# ----------------------------------------------------------------------------
echo "[2/4] Makefile — COLLECTOR_TAG matches VERSIONS.md"
mk_tag=$(awk -F'?=' '/^COLLECTOR_TAG[[:space:]]*\?=/ {gsub(/[[:space:]]/,"",$2); print $2; exit}' Makefile)
if [[ "$mk_tag" != "$COLLECTOR_TAG" ]]; then
  err "Makefile COLLECTOR_TAG=$mk_tag != VERSIONS.md $COLLECTOR_TAG"
else
  ok "Makefile COLLECTOR_TAG=$mk_tag"
fi

# ----------------------------------------------------------------------------
# 3. Makefile OCB_VERSION + builder-config.yaml dist.version must align with
#    VERSIONS.md OCB version.
# ----------------------------------------------------------------------------
echo "[3/4] OCB version pins"
mk_ocb=$(awk -F'?=' '/^OCB_VERSION[[:space:]]*\?=/ {gsub(/[[:space:]]/,"",$2); print $2; exit}' Makefile)
if [[ "$mk_ocb" != "$OCB_VERSION" ]]; then
  err "Makefile OCB_VERSION=$mk_ocb != VERSIONS.md $OCB_VERSION"
else
  ok "Makefile OCB_VERSION=$mk_ocb"
fi

# builder-config.yaml dist.version is "<ocb-version>-isi.<n>" — the prefix
# before -isi must match VERSIONS.md.
bc_version=$(awk '/^dist:/{f=1; next} f && /^[^[:space:]]/{exit} f && /^[[:space:]]+version:/ {gsub(/[[:space:]]/,""); split($0,a,":"); print a[2]; exit}' builder-config.yaml)
bc_ocb="${bc_version%%-*}"
if [[ "$bc_ocb" != "$OCB_VERSION" ]]; then
  err "builder-config.yaml dist.version=$bc_version (ocb=$bc_ocb) != VERSIONS.md $OCB_VERSION"
else
  ok "builder-config.yaml dist.version=$bc_version (ocb=$bc_ocb)"
fi

# ----------------------------------------------------------------------------
# 4. Workflow Go pins must match VERSIONS.md.
# ----------------------------------------------------------------------------
echo "[4/4] CI workflow Go pins"
wf_drift=$(grep -RhnE "go-version(-input)?:[[:space:]]*['\"]?[0-9]+\.[0-9]+(\.[0-9]+)?['\"]?" .github/workflows/ 2>/dev/null \
  | grep -vE "go-version(-input)?:[[:space:]]*['\"]?${GO_VERSION//./\\.}['\"]?" \
  | grep -vE "matrix\.go" || true)
if [[ -n "$wf_drift" ]]; then
  err "Go version drift in .github/workflows/ vs VERSIONS.md ($GO_VERSION):"
  echo "$wf_drift" | sed 's/^/    /'
else
  ok ".github/workflows/ Go pins == $GO_VERSION"
fi

# ----------------------------------------------------------------------------

echo
if [[ $fail -ne 0 ]]; then
  echo "::error::version drift detected — bump VERSIONS.md or fix the consumer file."
  exit 1
fi
echo "All version pins agree with VERSIONS.md."
