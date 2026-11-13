#!/usr/bin/env bash
# Tests `release-pins.sh check-bundles` on copies of the deploy bundles: the real ones pass, and an explicit image under
# `satbooth`, an override of `relay`, a service of the SatBooth namespace that is not one of the three, and a service on
# an unpinned image are each refused. Needs `docker compose` (the client only); without it a local run skips and CI
# fails. Usage: scripts/release-bundles_test.sh
set -euo pipefail

cd "$(dirname "$0")/.."
pins=$PWD/scripts/release-pins.sh
repo=$PWD

fail() { echo "release-bundles_test: FAIL: $*" >&2; exit 1; }

if ! docker compose version > /dev/null 2>&1; then
  [ -z "${CI-}" ] || fail "docker compose is not available, and CI must run this test"
  echo "release-bundles_test: docker compose not found: skipping (CI runs it)"
  exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fresh <dir>: the two bundles and setup.sh of this repository, copied.
fresh() {
  mkdir -p "$1/deploy"
  cp -R "$repo/deploy/public" "$repo/deploy/hub" "$1/deploy/"
  cp "$repo/deploy/setup.sh" "$1/deploy/setup.sh"
}
run() { RELEASE_PINS_ROOT=$1 "$pins" check-bundles; }

# refused <name> <dir> <fragment>: the check exits non-zero and says why.
refused() {
  if run "$2" > "$tmp/out" 2>&1; then fail "$1: it was accepted"; fi
  grep -qF -- "$3" "$tmp/out" || fail "$1: the refusal does not say \"$3\": $(cat "$tmp/out")"
  echo "refused: $1"
}

# The repository's own bundles pass, and so does an unchanged copy.
"$pins" check-bundles || fail "the repository's bundles were refused"
d=$tmp/ok
fresh "$d"
run "$d" || fail "an unchanged copy was refused"

# An explicit image under `satbooth` overrides the inherited one; the pin text is still found once.
d=$tmp/override
fresh "$d"
awk '{ print }
  /^  satbooth:$/ { in_svc = 1 }
  in_svc && /^    <<: \*satbooth$/ { print "    image: ghcr.io/example/checkout:latest"; in_svc = 0 }' \
  "$d/deploy/public/compose.yml" > "$d/c.yml" && mv "$d/c.yml" "$d/deploy/public/compose.yml"
grep -q 'ghcr.io/example/checkout' "$d/deploy/public/compose.yml" || fail "the override was not written"
RELEASE_PINS_ROOT=$d "$pins" check-format || fail "the override changed what check-format reads"
refused "an explicit image under satbooth" "$d" "satbooth runs ghcr.io/example/checkout:latest"

# An override of `relay`: the pinned ref stays once, on a service renamed `relay-old`, and `relay` runs another image.
d=$tmp/relay
fresh "$d"
awk '/^  relay:$/ { print "  relay:"; print "    image: ghcr.io/example/relay:latest"; print "  relay-old:"; next }
  { print }' \
  "$d/deploy/hub/compose.yml" > "$d/c.yml" && mv "$d/c.yml" "$d/deploy/hub/compose.yml"
RELEASE_PINS_ROOT=$d "$pins" check-format || fail "the relay override changed what check-format reads"
refused "an override of relay" "$d" "relay runs ghcr.io/example/relay:latest"

# A service of the SatBooth namespace that is not one of the three. The namespace is written with a YAML escape, so
# the text search for the pin does not see it.
d=$tmp/extra
fresh "$d"
zeros=$(printf '%064d' 0)
awk -v zeros="$zeros" '{ print }
  /^services:$/ { print "  extra:"; print "    image: \"ghcr.io/\\x73atbooth/other:v0.1.0-rc.1@sha256:" zeros "\"" }' \
  "$d/deploy/public/compose.yml" > "$d/c.yml" && mv "$d/c.yml" "$d/deploy/public/compose.yml"
RELEASE_PINS_ROOT=$d "$pins" check-format || fail "the extra service changed what check-format reads"
refused "a SatBooth namespace image on another service" "$d" "extra runs ghcr.io/satbooth/other:"

# A service on an image that is neither pinned by digest nor local.
d=$tmp/unpinned
fresh "$d"
awk '{ print } /^services:$/ { print "  extra:"; print "    image: alpine:3" }' \
  "$d/deploy/hub/compose.yml" > "$d/c.yml" && mv "$d/c.yml" "$d/deploy/hub/compose.yml"
refused "an unpinned image" "$d" "extra runs alpine:3"

echo "release-bundles_test: ok"
