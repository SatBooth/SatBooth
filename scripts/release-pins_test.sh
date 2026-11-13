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

# refuses <name> <dir> <fragment>: check-format must exit non-zero, and say why (stderr holds the fragment).
refuses() {
  if run "$2" check-format > "$tmp/out" 2>&1; then
    fail "$1: check-format accepted it"
  fi
  grep -qF -- "$3" "$tmp/out" || fail "$1: the refusal does not say \"$3\": $(cat "$tmp/out")"
}

# fails <name> <fragment> <command...>: the command exits non-zero and says why.
fails() {
  local name=$1 frag=$2
  shift 2
  if "$@" > "$tmp/out" 2>&1; then
    fail "$name: it was accepted"
  fi
  grep -qF -- "$frag" "$tmp/out" || fail "$name: the refusal does not say \"$frag\": $(cat "$tmp/out")"
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
refuses "one site at another version" "$d" "version mismatch: satbooth-relay is at v0.1.0-rc.2"

# An uppercase hex digest.
d=$tmp/upper
fresh "$d"
edit "$d/deploy/public/compose.yml" 's/sha256:1/sha256:A/'
refuses "an uppercase digest" "$d" "satbooth: not ghcr.io/satbooth/satbooth:"

# A 63-hex digest, and a 65-hex digest.
d=$tmp/short
fresh "$d"
edit "$d/deploy/setup.sh" "s/@sha256:3/@sha256:/"
refuses "a 63-hex digest" "$d" "satbooth-tools: not ghcr.io/satbooth/satbooth-tools:"
d=$tmp/long
fresh "$d"
edit "$d/deploy/setup.sh" "s/@sha256:3/@sha256:33/"
refuses "a 65-hex digest" "$d" "satbooth-tools: not ghcr.io/satbooth/satbooth-tools:"

# -rc. with no number.
d=$tmp/rc
fresh "$d"
for f in deploy/public/compose.yml deploy/hub/compose.yml deploy/setup.sh; do edit "$d/$f" 's/-rc\.1/-rc./'; done
refuses "-rc. with no number" "$d" "satbooth: not ghcr.io/satbooth/satbooth:"

# Two refs at one site.
d=$tmp/two
fresh "$d"
printf '  other:\n    image: %s/satbooth:v0.1.0-rc.1@sha256:%s\n' "$ns" "$d_sat" >> "$d/deploy/public/compose.yml"
refuses "two refs at one site" "$d" "satbooth: 2 pin lines"

# A site with no ref.
d=$tmp/none
fresh "$d"
printf 'services:\n  satbooth:\n    build: .\n' > "$d/deploy/public/compose.yml"
refuses "a site with no ref" "$d" "satbooth: 0 pin lines"

# A latest tag.
d=$tmp/latest
fresh "$d"
edit "$d/deploy/hub/compose.yml" 's/:v0\.1\.0-rc\.1@/:latest@/'
refuses "a latest tag" "$d" "satbooth-relay: not ghcr.io/satbooth/satbooth-relay:"

# A ref without a digest.
d=$tmp/nodigest
fresh "$d"
edit "$d/deploy/hub/compose.yml" 's/@sha256:[0-9a-f]*//'
refuses "a ref without a digest" "$d" "satbooth-relay: not ghcr.io/satbooth/satbooth-relay:"

# Another namespace.
d=$tmp/ns
fresh "$d"
edit "$d/deploy/setup.sh" 's#ghcr.io/satbooth/#ghcr.io/other/#'
refuses "another namespace" "$d" "satbooth-tools: not ghcr.io/satbooth/satbooth-tools:"

# The namespace is compared as text: a dot is not a wildcard.
d=$tmp/dot
fresh "$d"
edit "$d/deploy/setup.sh" 's#ghcr\.io/satbooth/#ghcrXio/satbooth/#'
refuses "a namespace that matches only as a pattern" "$d" "satbooth-tools: not ghcr.io/satbooth/satbooth-tools:"

# An indented second tools_image= line is counted, and so is a second mention of the namespace in a compose site.
d=$tmp/indented
fresh "$d"
printf 'if true; then\n  tools_image=satbooth-tools:local\nfi\n' >> "$d/deploy/setup.sh"
refuses "an indented second tools_image=" "$d" "satbooth-tools: 2 pin lines"
d=$tmp/comment
fresh "$d"
printf '# was %s/satbooth:v0.0.1@sha256:%s\n' "$ns" "$d_sat" >> "$d/deploy/public/compose.yml"
refuses "a second mention of the namespace in a comment" "$d" "satbooth: 2 pin lines"

# show fails when a site has no ref, and prints nothing for it.
d=$tmp/showfail
fresh "$d"
edit "$d/deploy/setup.sh" 's/^tools_image=.*//'
fails "show with no tools_image" "satbooth-tools: 0 pin lines" run "$d" show
if run "$d" show 2> /dev/null | grep -q '^satbooth-tools'; then fail "show printed a line for the site with no ref"; fi
fails "version with no tools_image" "satbooth-tools: 0 pin lines" run "$d" version

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
fails "set with the version v1" "not a version" run "$d" set v1 "$digests"
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\n' "$(hex a 64)" "$(hex b 64)" > "$tmp/two.txt"
fails "a digests file without satbooth-tools" "no line for satbooth-tools" run "$d" set v0.3.0 "$tmp/two.txt"
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\nsatbooth-tools sha256:%s\n' "$(hex A 64)" "$(hex b 64)" "$(hex c 64)" > "$tmp/upper.txt"
fails "an uppercase digest in the digests file" "line 1: not" run "$d" set v0.3.0 "$tmp/upper.txt"
{ cat "$digests"; printf 'satbooth sha256:%s\n' "$(hex a 64)"; } > "$tmp/dup.txt"
fails "a duplicate name in the digests file" "line 4: satbooth listed twice" run "$d" set v0.3.0 "$tmp/dup.txt"
{ cat "$digests"; printf 'other sha256:%s\n' "$(hex a 64)"; } > "$tmp/extra.txt"
fails "an extra name in the digests file" "line 4: unknown name other" run "$d" set v0.3.0 "$tmp/extra.txt"
{ cat "$digests"; printf 'garbage\n'; } > "$tmp/garbage.txt"
fails "a garbage line in the digests file" "line 4: not" run "$d" set v0.3.0 "$tmp/garbage.txt"
{ cat "$digests"; printf 'satbooth-tools sha256:%s trailing\n' "$(hex a 64)"; } > "$tmp/trail.txt"
fails "a trailing word in the digests file" "line 4: not" run "$d" set v0.3.0 "$tmp/trail.txt"
fails "check with an extra line" "line 4: unknown name other" run "$d" check "$tmp/extra.txt"
diff -r "$d" "$tmp/set-before" > /dev/null || fail "a refused set changed a site"

# check: all match, with and without the tag.
run "$d" check "$digests" > "$tmp/out" || fail "check refused matching digests"
[ "$(grep -c ' match$' "$tmp/out")" = 3 ] || fail "check output: $(cat "$tmp/out")"
run "$d" check "$digests" v0.2.0 > /dev/null || fail "check refused the matching tag"
fails "check with another tag" "version pinned v0.2.0, tag v0.2.1" run "$d" check "$digests" v0.2.1
fails "check with an empty tag" "the tag is empty" run "$d" check "$digests" ""

# check: one changed digest is reported by name, and the others still match.
printf 'satbooth sha256:%s\nsatbooth-relay sha256:%s\nsatbooth-tools sha256:%s\n' "$(hex a 64)" "$(hex d 64)" "$(hex c 64)" > "$tmp/changed.txt"
if run "$d" check "$tmp/changed.txt" > "$tmp/out" 2>&1; then fail "check accepted a changed digest"; fi
grep -qx "satbooth-relay pinned sha256:$(hex b 64), built sha256:$(hex d 64)" "$tmp/out" || fail "check output: $(cat "$tmp/out")"
grep -qx 'satbooth match' "$tmp/out" || fail "check did not report satbooth as a match: $(cat "$tmp/out")"
grep -qx 'satbooth-tools match' "$tmp/out" || fail "check did not report satbooth-tools as a match"

# The repository's own sites parse when RELEASE_PINS_ROOT is unset.
(cd "$repo" && "$pins" check-format) || fail "the repository's pins were refused"

echo "release-pins_test: ok"
