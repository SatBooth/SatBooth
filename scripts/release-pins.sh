#!/usr/bin/env bash
# Usage: scripts/release-pins.sh show | check-format | version | check <digests.txt> [<tag>] | check-tag <tag>
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
#   check-tag     exits 0 iff <tag> is a release tag (RELEASE_TAG_RE of scripts/release-lib.sh); reads no site
#   set           rewrites the three sites to <version> and the digests of <digests.txt>: every new content is written
#                 to a temporary file beside its site first, read back by the rules of check-format, and the sites are
#                 replaced only when all three are ready and well formed
# RELEASE_PINS_ROOT is the tree whose sites are read (default: this repository).
set -euo pipefail
# Bytes are compared as bytes: a letter outside a-z or a space outside space and tab never passes for another.
export LC_ALL=C

# The registry namespace of the release images.
NAMESPACE=ghcr.io/satbooth

names=(satbooth satbooth-relay satbooth-tools)
here=${BASH_SOURCE[0]%/*}
[ "$here" != "${BASH_SOURCE[0]}" ] || here=.
# shellcheck source=scripts/release-lib.sh
. "$here/release-lib.sh"
version_re=$RELEASE_VERSION_RE
digest_re='sha256:[0-9a-f]{64}'

fail() { echo "release-pins: $*" >&2; exit 1; }

root=${RELEASE_PINS_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}

# site_file <name>: the file that pins the image.
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

# has_nul <file>: succeeds iff the file holds a NUL byte, which bash would drop without a word when reading it.
has_nul() { local _; IFS= read -r -d '' _ < "$1"; }

# ref_in_file <file> <name>: sets REF to the ref held by the site's line in the file, that is by the line holding the needle (less
# its `image:` or `tools_image=` key and its trailing blanks); fails unless exactly one line holds it. The key may be
# preceded by, and the ref followed by, spaces and tabs (and a CR at the end) and no other byte of white space; a file
# with a NUL byte is refused.
ref_in_file() {
  local file=$1 needle line hit='' n=0 ref
  if [ "$2" = satbooth-tools ]; then needle='tools_image='; else needle="$NAMESPACE/"; fi
  ! has_nul "$file" || fail "$2: $file holds a NUL byte"
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in *"$needle"*) n=$((n + 1)); hit=$line ;; esac
  done < "$file"
  [ "$n" -eq 1 ] || fail "$2: $n pin lines in $file, want exactly one"
  ref=$hit
  if [[ $ref =~ ^[$' \t']*image:[$' \t']*(.*)$ ]]; then ref=${BASH_REMATCH[1]}; fi
  if [[ $ref =~ ^[$' \t']*tools_image=(.*)$ ]]; then ref=${BASH_REMATCH[1]}; fi
  while [[ $ref == *[$' \t\r'] ]]; do ref=${ref%?}; done
  REF=$ref
}

# site_ref <name>: sets REF to the ref held by the site.
site_ref() {
  local file
  file=$(site_file "$1")
  [ -f "$file" ] || fail "$1: no such file: $file"
  ref_in_file "$file" "$1"
}

# check_ref <name> <ref>: the ref is NAMESPACE/<name>:<version>@sha256:<64 lowercase hex>.
check_ref() {
  local prefix=$NAMESPACE/$1:
  [ "${2#"$prefix"}" != "$2" ] && [[ ${2#"$prefix"} =~ ^$version_re@$digest_re$ ]] \
    || fail "$1: not $NAMESPACE/$1:<version>@sha256:<64 lowercase hex>: $2"
}

# shared_version: every site is well formed and the three versions agree. Sets SHARED_VERSION to it and SITE_REFS to
# the three refs, in the order of names.
shared_version() {
  local name v t want=''
  SITE_REFS=()
  for name in "${names[@]}"; do
    site_ref "$name"
    check_ref "$name" "$REF"
    t=${REF#*:}
    v=${t%%@*}
    if [ -z "$want" ]; then want=$v; fi
    [ "$v" = "$want" ] || fail "version mismatch: $name is at $v, satbooth is at $want"
    SITE_REFS+=("$REF")
  done
  SHARED_VERSION=$want
}

# check_digests_file <digests.txt>: every line is "<one of the three names> sha256:<64 lowercase hex>", and every name
# is listed exactly once.
check_digests_file() {
  local line name digest extra seen=' ' n
  [ -f "$1" ] || fail "no such file: $1"
  ! has_nul "$1" || fail "$1 holds a NUL byte"
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

# digest_of <digests.txt> <name>: sets DIGEST to the digest listed for the name, in a file that check_digests_file
# accepted.
digest_of() {
  local name digest
  DIGEST=
  while read -r name digest || [ -n "$name" ]; do
    if [ "$name" = "$2" ]; then DIGEST=$digest; fi
  done < "$1"
}

cmd=${1:-}
case $cmd in
  show)
    [ "$#" -eq 1 ] || fail "usage: show"
    for name in "${names[@]}"; do
      site_ref "$name"
      printf '%s %s\n' "$name" "$REF"
    done
    ;;
  check-format)
    [ "$#" -eq 1 ] || fail "usage: check-format"
    shared_version
    ;;
  version)
    [ "$#" -eq 1 ] || fail "usage: version"
    shared_version
    printf '%s\n' "$SHARED_VERSION"
    ;;
  check)
    [ "$#" -ge 2 ] && [ "$#" -le 3 ] || fail "usage: check <digests.txt> [<tag>]"
    [ "$#" -lt 3 ] || [ -n "$3" ] || fail "check: the tag is empty"
    shared_version
    v=$SHARED_VERSION
    check_digests_file "$2"
    status=0
    i=0
    for name in "${names[@]}"; do
      pinned=${SITE_REFS[$i]##*@}
      i=$((i + 1))
      digest_of "$2" "$name"
      built=$DIGEST
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
  check-tag)
    [ "$#" -eq 2 ] || fail "usage: check-tag <tag>"
    [[ $2 =~ $RELEASE_TAG_RE ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $2"
    ;;
  set)
    [ "$#" -eq 3 ] || fail "usage: set <version> <digests.txt>"
    [[ $2 =~ ^$version_re$ ]] || fail "not a version like v1.2.3 or v1.2.3-rc.1: $2"
    # Everything is read and checked before the first file changes.
    check_digests_file "$3"
    refs=()
    for name in "${names[@]}"; do
      site_ref "$name"
      digest_of "$3" "$name"
      refs+=("$NAMESPACE/$name:$2@$DIGEST")
    done
    # Each site's new content goes to a temporary file in the site's directory, and a trap removes whatever is left
    # of them on any exit. Only when all three exist are they moved over the sites.
    tmps=()
    trap 'rm -f ${tmps[@]+"${tmps[@]}"}' EXIT
    files=()
    i=0
    for name in "${names[@]}"; do
      file=$(site_file "$name")
      # cp -p gives the temporary file the site's mode and owner; it never replaces a file that is there.
      new=$file.$$.$i.new
      if [ -e "$new" ] || [ -L "$new" ] || ! cp -p "$file" "$new" 2> /dev/null; then
        fail "$name: cannot write beside $file"
      fi
      tmps+=("$new")
      files+=("$file")
      case $name in
        satbooth-tools) repl="tools_image=${refs[$i]}"; needle='tools_image=' ;;
        *) repl="image: ${refs[$i]}"; needle="$NAMESPACE/" ;;
      esac
      # The line holding the needle is replaced whole, keeping the white space that leads it.
      while IFS= read -r line || [ -n "$line" ]; do
        case $line in
          *"$needle"*) [[ $line =~ ^([[:space:]]*) ]]; printf '%s%s\n' "${BASH_REMATCH[1]}" "$repl" ;;
          *) printf '%s\n' "$line" ;;
        esac
      done < "$file" > "$new"
      i=$((i + 1))
    done
    # What was staged is read back by the same rules as check-format: a pin that check-format would refuse is never
    # moved into place.
    for i in 0 1 2; do
      ref_in_file "${tmps[$i]}" "${names[$i]}"
      staged=$REF
      check_ref "${names[$i]}" "$staged"
      [ "$staged" = "${refs[$i]}" ] || fail "${names[$i]}: the staged pin is not the one to write"
    done
    for i in 0 1 2; do
      mv "${tmps[$i]}" "${files[$i]}"
    done
    ;;
  *)
    fail "usage: release-pins.sh show | check-format | version | check <digests.txt> [<tag>] | check-tag <tag>" \
      "| set <version> <digests.txt>"
    ;;
esac
