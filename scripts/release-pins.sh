#!/usr/bin/env bash
# Usage: scripts/release-pins.sh show | check-format | version | check <digests.txt> [<tag>]
#        | set <version> <digests.txt>
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
# site_needle <name>: the fixed string that marks the site's line. Every line holding it counts: a pin line may not
# repeat, anywhere in the file, at any indentation, in a comment or not.
site_needle() {
  case $1 in
    satbooth-tools) echo 'tools_image=' ;;
    *) echo "$NAMESPACE/" ;;
  esac
}

# site_ref <name>: the ref held by the one line of the site; fails when there is not exactly one such line.
site_ref() {
  local file lines n
  file=$(site_file "$1")
  [ -f "$file" ] || fail "$1: no such file: $file"
  lines=$(grep -F -- "$(site_needle "$1")" "$file" || true)
  n=$(printf '%s' "$lines" | grep -c . || true)
  [ "$n" -eq 1 ] || fail "$1: $n pin lines in $file, want exactly one"
  printf '%s\n' "$lines" |
    sed -E 's/^[[:space:]]*image:[[:space:]]*//; s/^[[:space:]]*tools_image=//; s/[[:space:]]+$//'
}

# ref_version <ref>: the tag of a ref that has passed the format check.
ref_version() { local t=${1#*:}; printf '%s' "${t%%@*}"; }
ref_digest() { printf '%s' "${1##*@}"; }

# check_refs: every site is well formed and the three versions agree. Prints the shared version.
shared_version() {
  local name ref v prefix want=''
  for name in "${names[@]}"; do
    ref=$(site_ref "$name") || exit 1
    prefix=$NAMESPACE/$name:
    [ "${ref#"$prefix"}" != "$ref" ] && [[ ${ref#"$prefix"} =~ ^$version_re@$digest_re$ ]] \
      || fail "$name: not $NAMESPACE/$name:<version>@sha256:<64 lowercase hex>: $ref"
    v=$(ref_version "$ref")
    if [ -z "$want" ]; then want=$v; fi
    [ "$v" = "$want" ] || fail "version mismatch: $name is at $v, satbooth is at $want"
  done
  printf '%s\n' "$want"
}

# check_digests_file <digests.txt>: every line is "<one of the three names> sha256:<64 lowercase hex>", and every name
# is listed exactly once.
check_digests_file() {
  local line name digest extra seen=' ' n
  [ -f "$1" ] || fail "no such file: $1"
  n=0
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    read -r name digest extra <<< "$line"
    [ -z "$extra" ] && [[ $line == "$name $digest" ]] && [[ $digest =~ ^$digest_re$ ]] \
      || fail "$1 line $n: not \"<name> $digest_re\""
    case " ${names[*]} " in *" $name "*) ;; *) fail "$1 line $n: unknown name $name" ;; esac
    case $seen in *" $name "*) fail "$1 line $n: $name listed twice" ;; esac
    seen+="$name "
  done < "$1"
  for name in "${names[@]}"; do
    case $seen in *" $name "*) ;; *) fail "$1: no line for $name" ;; esac
  done
}

# digest_of <digests.txt> <name>: the digest listed for the name, in a file that check_digests_file accepted.
digest_of() { awk -v n="$2" '$1 == n { print $2 }' "$1"; }

cmd=${1:-}
case $cmd in
  show)
    [ "$#" -eq 1 ] || fail "usage: show"
    for name in "${names[@]}"; do
      ref=$(site_ref "$name") || exit 1
      printf '%s %s\n' "$name" "$ref"
    done
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
    [ "$#" -lt 3 ] || [ -n "$3" ] || fail "check: the tag is empty"
    v=$(shared_version) || exit 1
    check_digests_file "$2"
    status=0
    for name in "${names[@]}"; do
      ref=$(site_ref "$name") || exit 1
      pinned=$(ref_digest "$ref")
      built=$(digest_of "$2" "$name") || exit 1
      if [ "$pinned" = "$built" ]; then
        echo "$name match"
      else
        echo "$name pinned $pinned, built $built"
        status=1
      fi
    done
    if [ "$#" -eq 3 ] && [ "$v" != "$3" ]; then
      echo "version pinned $v, tag $3"
      status=1
    fi
    exit "$status"
    ;;
  set)
    [ "$#" -eq 3 ] || fail "usage: set <version> <digests.txt>"
    [[ $2 =~ ^$version_re$ ]] || fail "not a version like v1.2.3 or v1.2.3-rc.1: $2"
    # Everything is read and checked before the first file changes.
    check_digests_file "$3"
    refs=()
    for name in "${names[@]}"; do
      site_ref "$name" > /dev/null || exit 1
      d=$(digest_of "$3" "$name") || exit 1
      refs+=("$NAMESPACE/$name:$2@$d")
    done
    i=0
    for name in "${names[@]}"; do
      file=$(site_file "$name")
      new=$(mktemp "$file.XXXXXX") || exit 1
      case $name in
        satbooth-tools) repl="tools_image=${refs[$i]}" ;;
        *) repl="image: ${refs[$i]}" ;;
      esac
      awk -v needle="$(site_needle "$name")" -v repl="$repl" '
        index($0, needle) { match($0, /^[[:space:]]*/); print substr($0, 1, RLENGTH) repl; next }
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
