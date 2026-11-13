#!/usr/bin/env bash
# Usage: scripts/release-push.sh <outdir> <tag>
# Pushes the three release images to the repositories of the refs in `release-pins.sh show` (the only list of images
# and of their registry), after `docker login`. <outdir> is the directory release-build.sh wrote (digests.txt). For
# each image:
#   a. rebuilds it with the builder, flags and exporter attributes of scripts/release-lib.sh, pushing by digest, and
#      requires the pushed digest to equal the pinned one;
#   b. reads <tag> from the registry (the top-level manifest only, hashed as served; release_tag_digest of
#      scripts/release-lib.sh): a tag that holds another digest is a failure, one that holds the pinned digest needs no
#      change, one the registry reports as not found is created; any other answer or error is a failure;
#   c. points <tag> at the pinned digest;
#   d. reads <tag> back and requires the pinned digest.
# Every image is built and its tag read (a, b) before any tag is written (c), so a clash leaves no tag half moved.
# Prints names and digests only.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: release-push.sh <outdir> <tag>" >&2
  exit 2
fi
tag=$2
# <outdir> may be relative to the caller's directory: resolved before the change to the repository root.
outdir=$(cd -- "$1" 2> /dev/null && pwd) || { echo "release-push: no such directory: $1" >&2; exit 2; }

fail() { echo "release-push: FAIL: $*" >&2; exit 1; }

# shellcheck source=scripts/release-lib.sh
. "$(dirname "$0")/release-lib.sh"

[[ $tag =~ $RELEASE_TAG_RE ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $tag"

cd "$(dirname "$0")/.."

pins=scripts/release-pins.sh

# The pins must be the release at <tag> and equal what this checkout builds.
"$pins" check "$outdir/digests.txt" "$tag" > /dev/null || fail "the built digests are not the pinned release $tag"

release_ensure_builder
meta=$(mktemp -d)
trap 'rm -rf "$meta"' EXIT

# The images: name, repository (the pinned ref up to its tag) and pinned digest, from `release-pins.sh show`.
release_pins_read "$pins" || fail "cannot read the pinned images"
names=("${PIN_NAMES[@]}")
repos=("${PIN_REPOS[@]}")
digests=("${PIN_DIGESTS[@]}")

tag_digest() { release_tag_digest "$1" "$tag" "$meta"; }

existing=()
for i in 0 1 2; do
  name=${names[$i]} repo=${repos[$i]} pin=${digests[$i]}
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

for i in 0 1 2; do
  name=${names[$i]} repo=${repos[$i]} pin=${digests[$i]}
  if [ "${existing[$i]}" = no ]; then
    created=$(docker buildx imagetools create -t "$repo:$tag" "$repo@$pin" 2>&1) || fail "$name:$tag: $created"
    echo "release-push: $name:$tag created"
  fi
  back=$(tag_digest "$repo")
  [ "$back" = "$pin" ] || fail "$name:$tag reads back as ${back:-nothing}, pinned $pin"
  echo "release-push: $name:$tag is $pin"
done
