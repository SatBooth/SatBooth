#!/usr/bin/env bash
# Usage: scripts/release-pins.sh show | check-format | version | check <digests.txt> [<tag>] | set <version> <digests.txt>
# The release images are pinned by tag and digest at three sites: the service `satbooth` of deploy/public/compose.yml,
# the service `relay` of deploy/hub/compose.yml, and the tools_image= line of deploy/setup.sh. This script reads and
# rewrites those three lines, and nothing else.
#   show          prints "<name> <ref>" for each site, in the order of scripts/release-lib.sh (satbooth,
#                 satbooth-relay, satbooth-tools)
#   check-format  exits 0 iff each site holds exactly one ref of the form
#                 <namespace>/<name>:v<major>.<minor>.<patch>[-rc.<n>]@sha256:<64 lowercase hex>, and all three carry
#                 the same version
#   version       prints that version
#   check         exits 0 iff every pinned digest equals the digest in <digests.txt> (the file release-build.sh
#                 writes) and, with a <tag>, the version equals it; prints each name with "match" or
#                 "pinned X, built Y"
#   set           rewrites the three sites to <version> and the digests of <digests.txt>
# RELEASE_PINS_ROOT is the tree whose sites are read (default: this repository).
set -euo pipefail

# The registry namespace of the release images.
NAMESPACE=ghcr.io/satbooth

names=(satbooth satbooth-relay satbooth-tools)
version_re='v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?'
digest_re='sha256:[0-9a-f]{64}'

fail() { echo "release-pins: $*" >&2; exit 1; }

root=${RELEASE_PINS_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}

# site_file <name> and site_pattern <name>: the file that pins the image, and the ERE of its one line.
site_file() {
  case $1 in
    satbooth) echo "$root/deploy/public/compose.yml" ;;
    satbooth-relay) echo "$root/deploy/hub/compose.yml" ;;
    satbooth-tools) echo "$root/deploy/setup.sh" ;;
  esac
}
site_pattern() {
  case $1 in
    satbooth-tools) echo '^tools_image=' ;;
    *) echo "^[[:space:]]*image:[[:space:]]*([^[:space:]]*/)?$1[:@]" ;;
  esac
}

# site_ref <name>: the ref held by the one line of the site; fails when there is not exactly one such line.
site_ref() {
  local file lines n
  file=$(site_file "$1")
  [ -f "$file" ] || fail "$1: no such file: $file"
  lines=$(grep -E "$(site_pattern "$1")" "$file" || true)
  n=$(printf '%s' "$lines" | grep -c . || true)
  [ "$n" -eq 1 ] || fail "$1: $n pin lines in $file, want exactly one"
  printf '%s\n' "$lines" | sed -E 's/^[[:space:]]*image:[[:space:]]*//; s/^tools_image=//; s/[[:space:]]+$//'
}

# ref_version <ref>: the tag of a ref that has passed the format check.
ref_version() { local t=${1#*:}; printf '%s' "${t%%@*}"; }
ref_digest() { printf '%s' "${1##*@}"; }

# check_refs: every site is well formed and the three versions agree. Prints the shared version.
shared_version() {
  local name ref v want=''
  for name in "${names[@]}"; do
    ref=$(site_ref "$name")
    [[ $ref =~ ^$NAMESPACE/$name:$version_re@$digest_re$ ]] \
      || fail "$name: not $NAMESPACE/$name:<version>@sha256:<64 lowercase hex>: $ref"
    v=$(ref_version "$ref")
    if [ -z "$want" ]; then want=$v; fi
    [ "$v" = "$want" ] || fail "version mismatch: $name is at $v, satbooth is at $want"
  done
  printf '%s\n' "$want"
}

# digest_of <digests.txt> <name>: the digest listed for the name; fails unless exactly one well-formed line.
digest_of() {
  local d
  d=$(awk -v n="$2" '$1 == n { print $2 }' "$1")
  [ "$(printf '%s' "$d" | grep -c .)" -eq 1 ] || fail "$1: want exactly one line for $2"
  [[ $d =~ ^$digest_re$ ]] || fail "$1: the digest of $2 is not $digest_re"
  printf '%s\n' "$d"
}

cmd=${1:-}
case $cmd in
  show)
    [ "$#" -eq 1 ] || fail "usage: show"
    for name in "${names[@]}"; do printf '%s %s\n' "$name" "$(site_ref "$name")"; done
    ;;
  check-format)
    [ "$#" -eq 1 ] || fail "usage: check-format"
    shared_version > /dev/null
    ;;
  version)
    [ "$#" -eq 1 ] || fail "usage: version"
    shared_version
    ;;
  check)
    [ "$#" -ge 2 ] && [ "$#" -le 3 ] || fail "usage: check <digests.txt> [<tag>]"
    v=$(shared_version)
    status=0
    for name in "${names[@]}"; do
      pinned=$(ref_digest "$(site_ref "$name")")
      built=$(digest_of "$2" "$name")
      if [ "$pinned" = "$built" ]; then
        echo "$name match"
      else
        echo "$name pinned $pinned, built $built"
        status=1
      fi
    done
    if [ -n "${3:-}" ] && [ "$v" != "$3" ]; then
      echo "version pinned $v, tag $3"
      status=1
    fi
    exit "$status"
    ;;
  set)
    [ "$#" -eq 3 ] || fail "usage: set <version> <digests.txt>"
    [[ $2 =~ ^$version_re$ ]] || fail "not a version like v1.2.3 or v1.2.3-rc.1: $2"
    # Everything is read and checked before the first file changes.
    refs=()
    for name in "${names[@]}"; do
      site_ref "$name" > /dev/null
      refs+=("$NAMESPACE/$name:$2@$(digest_of "$3" "$name")")
    done
    i=0
    for name in "${names[@]}"; do
      file=$(site_file "$name")
      new=$(mktemp "$file.XXXXXX")
      case $name in
        satbooth-tools) repl="tools_image=${refs[$i]}" ;;
        *) repl="image: ${refs[$i]}" ;;
      esac
      awk -v pat="$(site_pattern "$name")" -v repl="$repl" '
        $0 ~ pat { match($0, /^[[:space:]]*/); print substr($0, 1, RLENGTH) repl; next }
        { print }' "$file" > "$new"
      chmod --reference="$file" "$new" 2> /dev/null || chmod "$(stat -f '%Lp' "$file" 2> /dev/null || echo 644)" "$new"
      mv "$new" "$file"
      i=$((i + 1))
    done
    ;;
  *)
    fail "usage: release-pins.sh show | check-format | version | check <digests.txt> [<tag>] | set <version> <digests.txt>"
    ;;
esac
