#!/usr/bin/env bash
# Tests scripts/release-pins.sh on copies of the three pin sites in a temporary directory (RELEASE_PINS_ROOT). Every
# input is fixed. Usage: scripts/release-pins_test.sh
set -euo pipefail

cd "$(dirname "$0")/.."
pins=$PWD/scripts/release-pins.sh
repo=$PWD

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "release-pins_test: FAIL: $*" >&2; exit 1; }

ns=ghcr.io/satbooth
hex() { # <char> <count>: that many copies of one hex character
  local out='' i
  for ((i = 0; i < $2; i++)); do out+=$1; done
  printf '%s' "$out"
}
d_sat=$(hex 1 64)
d_rel=$(hex 2 64)
d_tools=$(hex 3 64)

# fresh <dir>: three sites at v0.1.0-rc.1 with fixed digests, laid out as in the repository.
fresh() {
  mkdir -p "$1/deploy/public" "$1/deploy/hub"
  printf 'services:\n  satbooth:\n    image: %s/satbooth:v0.1.0-rc.1@sha256:%s\n' "$ns" "$d_sat" > "$1/deploy/public/compose.yml"
  printf 'services:\n  relay:\n    image: %s/satbooth-relay:v0.1.0-rc.1@sha256:%s\n' "$ns" "$d_rel" > "$1/deploy/hub/compose.yml"
  printf '#!/usr/bin/env bash\ntools_image=%s/satbooth-tools:v0.1.0-rc.1@sha256:%s\n' "$ns" "$d_tools" > "$1/deploy/setup.sh"
}
run() { RELEASE_PINS_ROOT=$1 "$pins" "${@:2}"; }

# refuses <name> <dir>: check-format must exit non-zero.
refuses() {
  if run "$2" check-format > "$tmp/out" 2>&1; then
    fail "$1: check-format accepted it"
  fi
}

# edit <file> <sed expression>: portable in-place edit.
edit() { sed "$2" "$1" > "$1.new" && mv "$1.new" "$1"; }

# The unchanged copy is accepted, and show and version report it.
d=$tmp/ok
fresh "$d"
run "$d" check-format || fail "the base case was refused"
[ "$(run "$d" version)" = v0.1.0-rc.1 ] || fail "version: $(run "$d" version)"
want=$(printf 'satbooth %s/satbooth:v0.1.0-rc.1@sha256:%s\nsatbooth-relay %s/satbooth-relay:v0.1.0-rc.1@sha256:%s\nsatbooth-tools %s/satbooth-tools:v0.1.0-rc.1@sha256:%s' \
  "$ns" "$d_sat" "$ns" "$d_rel" "$ns" "$d_tools")
[ "$(run "$d" show)" = "$want" ] || fail "show: $(run "$d" show)"

# A final release version is accepted.
d=$tmp/final
fresh "$d"
for f in deploy/public/compose.yml deploy/hub/compose.yml deploy/setup.sh; do edit "$d/$f" 's/v0\.1\.0-rc\.1/v1.2.3/'; done
run "$d" check-format || fail "v1.2.3 was refused"

# One site at another version.
d=$tmp/version
fresh "$d"
edit "$d/deploy/hub/compose.yml" 's/v0\.1\.0-rc\.1/v0.1.0-rc.2/'
refuses "one site at another version" "$d"
grep -q 'version' "$tmp/out" || fail "the version mismatch is not named: $(cat "$tmp/out")"

# An uppercase hex digest.
d=$tmp/upper
fresh "$d"
edit "$d/deploy/public/compose.yml" 's/sha256:1/sha256:A/'
refuses "an uppercase digest" "$d"

# A 63-hex digest, and a 65-hex digest.
d=$tmp/short
fresh "$d"
edit "$d/deploy/setup.sh" "s/@sha256:3/@sha256:/"
refuses "a 63-hex digest" "$d"
d=$tmp/long
fresh "$d"
edit "$d/deploy/setup.sh" "s/@sha256:3/@sha256:33/"
refuses "a 65-hex digest" "$d"

# -rc. with no number.
d=$tmp/rc
fresh "$d"
for f in deploy/public/compose.yml deploy/hub/compose.yml deploy/setup.sh; do edit "$d/$f" 's/-rc\.1/-rc./'; done
refuses "-rc. with no number" "$d"

# Two refs at one site.
d=$tmp/two
fresh "$d"
printf '  other:\n    image: %s/satbooth:v0.1.0-rc.1@sha256:%s\n' "$ns" "$d_sat" >> "$d/deploy/public/compose.yml"
refuses "two refs at one site" "$d"

# A site with no ref.
d=$tmp/none
fresh "$d"
printf 'services:\n  satbooth:\n    build: .\n' > "$d/deploy/public/compose.yml"
refuses "a site with no ref" "$d"

# A latest tag.
d=$tmp/latest
fresh "$d"
edit "$d/deploy/hub/compose.yml" 's/:v0\.1\.0-rc\.1@/:latest@/'
refuses "a latest tag" "$d"

# A ref without a digest.
d=$tmp/nodigest
fresh "$d"
edit "$d/deploy/hub/compose.yml" 's/@sha256:[0-9a-f]*//'
refuses "a ref without a digest" "$d"

# Another namespace.
d=$tmp/ns
fresh "$d"
edit "$d/deploy/setup.sh" 's#ghcr.io/satbooth/#ghcr.io/other/#'
refuses "another namespace" "$d"

# set writes the three sites, and check-format accepts the result.
d=$tmp/set
fresh "$d"
digests=$tmp/digests.txt
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\nsatbooth-tools sha256:%s\n' "$(hex a 64)" "$(hex b 64)" "$(hex c 64)" > "$digests"
run "$d" set v0.2.0 "$digests" || fail "set failed"
run "$d" check-format || fail "check-format refused the result of set"
[ "$(run "$d" version)" = v0.2.0 ] || fail "version after set"
grep -qx "    image: $ns/satbooth:v0.2.0@sha256:$(hex a 64)" "$d/deploy/public/compose.yml" || fail "set: public site"
grep -qx "    image: $ns/satbooth-relay:v0.2.0@sha256:$(hex b 64)" "$d/deploy/hub/compose.yml" || fail "set: hub site"
grep -qx "tools_image=$ns/satbooth-tools:v0.2.0@sha256:$(hex c 64)" "$d/deploy/setup.sh" || fail "set: tools site"
# set refuses a bad version and a bad digests file, and leaves the sites as they were.
cp -R "$d" "$tmp/set-before"
run "$d" set v1 "$digests" > /dev/null 2>&1 && fail "set accepted the version v1"
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\n' "$(hex a 64)" "$(hex b 64)" > "$tmp/two.txt"
run "$d" set v0.3.0 "$tmp/two.txt" > /dev/null 2>&1 && fail "set accepted a digests file without satbooth-tools"
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\nsatbooth-tools sha256:%s\n' "$(hex A 64)" "$(hex b 64)" "$(hex c 64)" > "$tmp/upper.txt"
run "$d" set v0.3.0 "$tmp/upper.txt" > /dev/null 2>&1 && fail "set accepted an uppercase digest"
diff -r "$d" "$tmp/set-before" > /dev/null || fail "a refused set changed a site"

# check: all match, with and without the tag.
run "$d" check "$digests" > "$tmp/out" || fail "check refused matching digests"
[ "$(grep -c ' match$' "$tmp/out")" = 3 ] || fail "check output: $(cat "$tmp/out")"
run "$d" check "$digests" v0.2.0 > /dev/null || fail "check refused the matching tag"
run "$d" check "$digests" v0.2.1 > /dev/null 2>&1 && fail "check accepted another tag"

# check: one changed digest is reported by name, and the others still match.
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\nsatbooth-tools sha256:%s\n' "$(hex a 64)" "$(hex d 64)" "$(hex c 64)" > "$tmp/changed.txt"
if run "$d" check "$tmp/changed.txt" > "$tmp/out" 2>&1; then fail "check accepted a changed digest"; fi
grep -qx "satbooth-relay pinned sha256:$(hex b 64), built sha256:$(hex d 64)" "$tmp/out" || fail "check output: $(cat "$tmp/out")"
grep -qx 'satbooth match' "$tmp/out" || fail "check did not report satbooth as a match: $(cat "$tmp/out")"
grep -qx 'satbooth-tools match' "$tmp/out" || fail "check did not report satbooth-tools as a match"

# The repository's own sites parse when RELEASE_PINS_ROOT is unset.
(cd "$repo" && "$pins" check-format) || fail "the repository's pins were refused"

echo "release-pins_test: ok"
