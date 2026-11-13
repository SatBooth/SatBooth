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

# add_service <name> <dir> <host> <fragment> <line>...: the lines, as a service `extra`, at the top of the bundle's
# services; the bundle is refused and the refusal says <fragment>. Check-format must still read the pins.
add_service() {
  local name=$1 d=$tmp/$1 host=$3 fragment=$4
  fresh "$d"
  printf '  extra:\n' > "$d/block"
  shift 4
  printf '    %s\n' "$@" >> "$d/block"
  awk 'FNR == NR { block = block $0 "\n"; next } { print } /^services:$/ { printf "%s", block }' \
    "$d/block" "$d/deploy/$host/compose.yml" > "$d/c.yml" && mv "$d/c.yml" "$d/deploy/$host/compose.yml"
  RELEASE_PINS_ROOT=$d "$pins" check-format || fail "$name: the extra service changed what check-format reads"
  refused "$name" "$d" "$fragment"
}
zeros=$(printf '%064d' 0)

add_service "an unpinned image" x hub "extra runs alpine:3" "image: alpine:3"
add_service "a remote image tagged local" x2 hub "extra runs docker.io/evil/x:local" \
  "image: docker.io/evil/x:local" "build: {context: .}"
add_service "a remote image tagged local, always pulled" x3 hub "extra runs evil.example/x:local" \
  "image: evil.example/x:local" "build: {context: .}" "pull_policy: always"
add_service "a local image without a build" x4 hub "extra runs nobuild:local" "image: nobuild:local"
add_service "a local build with pull_policy always" x5 hub "extra runs built:local" \
  "image: built:local" "build: {context: .}" "pull_policy: always"
add_service "the namespace in capitals" x6 hub "extra runs GHCR.IO/SatBooth/other@sha256:" \
  "image: GHCR.IO/SatBooth/other@sha256:$zeros"
add_service "the namespace behind a port" x7 hub "extra runs ghcr.io:443/satbooth/other@sha256:" \
  "image: ghcr.io:443/satbooth/other@sha256:$zeros"
add_service "an image named by a variable" x8 hub "extra names its image with a variable" \
  "image: \${EXTRA_IMAGE:-alpine@sha256:$zeros}"

# A local build with no registry, and a digest-pinned image elsewhere, are accepted.
d=$tmp/good
fresh "$d"
printf '  extra:\n    image: built:local\n    build: {context: .}\n' > "$d/block"
printf '  extra2:\n    image: alpine@sha256:%s\n' "$zeros" >> "$d/block"
awk 'FNR == NR { block = block $0 "\n"; next } { print } /^services:$/ { printf "%s", block }' \
  "$d/block" "$d/deploy/hub/compose.yml" > "$d/c.yml" && mv "$d/c.yml" "$d/deploy/hub/compose.yml"
run "$d" || fail "a local build and a digest-pinned image were refused"

echo "release-bundles_test: ok"
