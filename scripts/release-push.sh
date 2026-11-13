#!/usr/bin/env bash
# Usage: scripts/release-push.sh <outdir> <tag>
# Pushes the three release images to the registry named by scripts/release-pins.sh (ghcr.io/satbooth), after
# `docker login`. <outdir> is the directory release-build.sh wrote (digests.txt). The images and their pinned digests
# are read from `release-pins.sh show`. For each image:
#   a. rebuilds it with the builder, flags and exporter attributes of scripts/release-lib.sh, pushing by digest, and
#      requires the pushed digest to equal the pinned one;
#   b. reads <tag> from the registry: a tag that holds another digest is a failure, one that holds the pinned digest
#      needs no change;
#   c. points <tag> at the pinned digest;
#   d. reads <tag> back and requires the pinned digest.
# Every image is built and its tag read (a, b) before any tag is written (c), so a clash leaves no tag half moved.
# Prints names and digests only.
#
# RELEASE_PUSH_TEST_NAMESPACE replaces the registry namespace in the tests of this script. It is honoured only for a
# loopback registry ("localhost:<port>/<path>" or "127.0.0.1:<port>/<path>"); any other value is refused.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: release-push.sh <outdir> <tag>" >&2
  exit 2
fi
outdir=$1
tag=$2

fail() { echo "release-push: FAIL: $*" >&2; exit 1; }

[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $tag"

cd "$(dirname "$0")/.."
# shellcheck source=scripts/release-lib.sh
. scripts/release-lib.sh

pins=scripts/release-pins.sh
production_ns=ghcr.io/satbooth
ns=$production_ns
if [ -n "${RELEASE_PUSH_TEST_NAMESPACE:-}" ]; then
  [[ $RELEASE_PUSH_TEST_NAMESPACE =~ ^(localhost|127\.0\.0\.1):[0-9]+/[a-z0-9._/-]+$ ]] ||
    fail "RELEASE_PUSH_TEST_NAMESPACE must be a loopback registry namespace: $RELEASE_PUSH_TEST_NAMESPACE"
  ns=$RELEASE_PUSH_TEST_NAMESPACE
fi

# The pins must be the release at <tag> and equal what this checkout builds.
"$pins" check "$outdir/digests.txt" "$tag" > /dev/null || fail "the built digests are not the pinned release $tag"

release_ensure_builder
meta=$(mktemp -d)
trap 'rm -rf "$meta"' EXIT

shown=$("$pins" show)
pin_of() { # <name>: the pinned digest
  local ref
  ref=$(awk -v n="$1" '$1 == n { print $2 }' <<< "$shown")
  [ -n "$ref" ] || fail "$1: no pin"
  printf '%s' "${ref##*@}"
}

# tag_digest <repo>: the digest <repo>:<tag> has in the registry, or nothing when the tag does not exist. Any other
# failure of the registry stops the push.
tag_digest() {
  local out
  if out=$(docker buildx imagetools inspect "$1:$tag" --format '{{.Manifest.Digest}}' 2>&1); then
    printf '%s' "$out"
  elif [[ $out == *"not found"* ]]; then
    return 0
  else
    fail "$1:$tag: cannot read the tag: $out"
  fi
}

existing=()
for name in "${RELEASE_TARGETS[@]}"; do
  pin=$(pin_of "$name")
  repo=$ns/$name
  docker buildx build "${RELEASE_BUILD_FLAGS[@]}" --progress=quiet --target "$name" --platform "$RELEASE_PLATFORMS" \
    --output "type=image,name=$repo,push-by-digest=true,name-canonical=true,push=true,$RELEASE_EXPORT_ATTRS" \
    --metadata-file "$meta/$name.json" . > /dev/null
  pushed=$(jq -er '."containerimage.digest"' "$meta/$name.json")
  [ "$pushed" = "$pin" ] || fail "$name: pushed $pushed, pinned $pin"
  echo "release-push: $name pushed $pushed"
  have=$(tag_digest "$repo")
  if [ -z "$have" ]; then
    existing+=(no)
  elif [ "$have" = "$pin" ]; then
    echo "release-push: $name:$tag already points at $pin"
    existing+=(yes)
  else
    fail "$name:$tag already points at $have, not the pinned $pin"
  fi
done

i=0
for name in "${RELEASE_TARGETS[@]}"; do
  pin=$(pin_of "$name")
  repo=$ns/$name
  if [ "${existing[$i]}" = no ]; then
    created=$(docker buildx imagetools create -t "$repo:$tag" "$repo@$pin" 2>&1) || fail "$name:$tag: $created"
    echo "release-push: $name:$tag created"
  fi
  back=$(tag_digest "$repo")
  [ "$back" = "$pin" ] || fail "$name:$tag reads back as ${back:-nothing}, pinned $pin"
  echo "release-push: $name:$tag is $pin"
  i=$((i + 1))
done
