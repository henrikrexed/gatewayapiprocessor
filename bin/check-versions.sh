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
echo "[1/5] deploy/ + README.md + docs/ — all otelcol-gatewayapi image references match VERSIONS.md tag"
# The root README.md and docs/ copy-paste the pinned image too; a drifted tag
# there misleads demo operators even though deploy/ is clean. Scan all three.
# (docs/ carries no image ref today, but keeping it in scope means a future doc
# that pastes the image is covered; the version *prose* in docs/ is validated
# separately in check [5/5].)
scan_paths=(deploy/ README.md)
[[ -d docs ]] && scan_paths+=(docs/)
# Fail closed on a scan path that doesn't resolve: otherwise `2>/dev/null` +
# `|| true` below would flatten "unreadable path" into the same empty-output
# "ok" as "no drift", silently reporting a path we never read as clean.
for p in "${scan_paths[@]}"; do
  [[ -e "$p" ]] || err "check [1/5]: scan path '$p' does not exist — cannot validate it"
done
# No `grep -h`: keep the file:line prefix so a mismatch names the drifting file
# (-o still trims the match to the ref; the "<path>:<line>:" prefix survives).
mismatch=$(grep -rnoE 'ghcr\.io/henrikrexed/otelcol-gatewayapi:[A-Za-z0-9._+-]+' "${scan_paths[@]}" 2>/dev/null \
  | grep -v ":${COLLECTOR_TAG}\$" || true)
if [[ -n "$mismatch" ]]; then
  err "otelcol-gatewayapi image tag != $COLLECTOR_TAG (file:line:ref):"
  echo "$mismatch" | sed 's/^/    /'
else
  ok "deploy/ + README.md + docs/ image tags == $COLLECTOR_TAG"
fi

# ----------------------------------------------------------------------------
# 2. Makefile COLLECTOR_TAG must match VERSIONS.md tag.
# ----------------------------------------------------------------------------
echo "[2/5] Makefile — COLLECTOR_TAG matches VERSIONS.md"
mk_tag=$(awk -F'[?]=' '/^COLLECTOR_TAG[[:space:]]*\?=/ {gsub(/[[:space:]]/,"",$2); print $2; exit}' Makefile)
if [[ "$mk_tag" != "$COLLECTOR_TAG" ]]; then
  err "Makefile COLLECTOR_TAG=$mk_tag != VERSIONS.md $COLLECTOR_TAG"
else
  ok "Makefile COLLECTOR_TAG=$mk_tag"
fi

# ----------------------------------------------------------------------------
# 3. Makefile OCB_VERSION + builder-config.yaml dist.version must align with
#    VERSIONS.md OCB version.
# ----------------------------------------------------------------------------
echo "[3/5] OCB version pins"
mk_ocb=$(awk -F'[?]=' '/^OCB_VERSION[[:space:]]*\?=/ {gsub(/[[:space:]]/,"",$2); print $2; exit}' Makefile)
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

# builder-config.yaml component gomod pins — these ~15 lines decide which
# collector code actually links. dist.version agreeing with VERSIONS.md is not
# enough: a component left behind at an old release-train version still builds
# green but links stale code. Collector-core (go.opentelemetry.io/collector/*)
# and contrib (opentelemetry-collector-contrib/*) modules on the unstable v0.x
# train move in lockstep with the collector version, so every v0.x pin must
# equal v$OCB_VERSION.
# Note: stable modules (v1+/v2+, e.g. k8sattributesprocessor v1.2.0) version
# independently and have no authoritative pin in VERSIONS.md, so they are NOT
# validated here. Upgrade path: record stable-module versions in VERSIONS.md and
# extend this check to assert them too.
#
# The awk below is hardened against three mutation-proven failure modes:
#   1. Strip a trailing `# comment` before taking the version, or a held-back
#      pin like `... v0.154.0  # ISI-999` would make the comment token the last
#      field, dodge the /^v0\./ guard, and let real drift pass green.
#   2. Anchor the match to a real list entry (`^ - gomod:`) so a commented-out
#      line (which strips to blank) can't be flagged as drift on a component
#      that isn't even linked.
#   3. A collector/contrib entry with no version at all is unpinned — OCB would
#      float it to latest at build time — so flag it explicitly.
gomod_drift=$(awk -v ocb="v${OCB_VERSION}" '
  {
    line = $0
    sub(/#.*/, "", line)              # drop trailing comment before field split
    sub(/[[:space:]]+$/, "", line)
  }
  line ~ /^[[:space:]]*-[[:space:]]*gomod:[[:space:]]/ &&
  line ~ /(go\.opentelemetry\.io\/collector\/|github\.com\/open-telemetry\/opentelemetry-collector-contrib\/)/ {
    n = split(line, f, /[[:space:]]+/)
    ver = f[n]
    if (ver !~ /^v[0-9]/)              { print NR": UNPINNED (no version): "$0; next }
    if (ver ~ /^v0\./ && ver != ocb)   { print NR": "$0 }
  }' builder-config.yaml)
if [[ -n "$gomod_drift" ]]; then
  err "builder-config.yaml collector/contrib gomod pin problem(s) (line: entry):"
  echo "$gomod_drift" | sed 's/^/    /'
else
  ok "builder-config.yaml collector/contrib v0.x gomod pins == v$OCB_VERSION (v1+/v2+ stable modules exempt)"
fi

# ----------------------------------------------------------------------------
# 4. Workflow Go pins must match VERSIONS.md.
# ----------------------------------------------------------------------------
echo "[4/5] CI workflow Go pins"
[[ -d .github/workflows ]] || err "check [4/5]: .github/workflows/ does not exist — cannot validate Go pins"
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
# 5. docs/requirements.md version *prose* must match VERSIONS.md.
#    docs/ is published to Pages via mkdocs, so a stale collector-runtime or
#    Go-toolchain row misleads readers even though it carries no image ref for
#    check [1/5] to catch. Validate the two rows the same way [4/5] handles
#    workflow Go pins.
# ----------------------------------------------------------------------------
echo "[5/5] docs/requirements.md version prose"
REQ_DOC="docs/requirements.md"
if [[ ! -f "$REQ_DOC" ]]; then
  ok "$REQ_DOC absent — nothing to validate"
else
  # Collector runtime row: "| Collector runtime | v0.163.0 (OCB dist ...". Pull
  # the first vX.Y.Z on the line and compare its minor to $OCB_VERSION.
  doc_ocb=$(awk -F'|' '/Collector runtime/ {
    if (match($0, /v[0-9]+\.[0-9]+\.[0-9]+/)) { print substr($0, RSTART+1, RLENGTH-1); exit }
  }' "$REQ_DOC")
  if [[ -z "$doc_ocb" ]]; then
    err "$REQ_DOC: could not find a 'Collector runtime' version row"
  elif [[ "$doc_ocb" != "$OCB_VERSION" ]]; then
    err "$REQ_DOC Collector runtime=v$doc_ocb != VERSIONS.md v$OCB_VERSION"
  else
    ok "$REQ_DOC Collector runtime == v$OCB_VERSION"
  fi

  # Go toolchain row: "| Go toolchain | Go 1.26 (required ...". Compare X.Y.
  doc_go=$(awk -F'|' '/Go toolchain/ {
    if (match($0, /Go[[:space:]]+[0-9]+\.[0-9]+/)) {
      s = substr($0, RSTART, RLENGTH); gsub(/Go[[:space:]]+/, "", s); print s; exit
    }
  }' "$REQ_DOC")
  if [[ -z "$doc_go" ]]; then
    err "$REQ_DOC: could not find a 'Go toolchain' version row"
  elif [[ "$doc_go" != "$GO_VERSION" ]]; then
    err "$REQ_DOC Go toolchain=$doc_go != VERSIONS.md $GO_VERSION"
  else
    ok "$REQ_DOC Go toolchain == $GO_VERSION"
  fi
fi

# ----------------------------------------------------------------------------

echo
if [[ $fail -ne 0 ]]; then
  echo "::error::version drift detected — bump VERSIONS.md or fix the consumer file."
  exit 1
fi
echo "All version pins agree with VERSIONS.md."
