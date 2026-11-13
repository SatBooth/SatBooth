#!/usr/bin/env bash
# Usage: scripts/release-push.sh <outdir> <tag>
# Pushes the three release images to the repositories of the refs in `release-pins.sh show` (the only list of images
# and of their registry), after `docker login`. <outdir> is the directory release-build.sh wrote (digests.txt). For
# each image:
#   a. rebuilds it with the builder, flags and exporter attributes of scripts/release-lib.sh, pushing by digest, and
#      requires the pushed digest to equal the pinned one;
#   b. reads <tag> from the registry (the top-level manifest only, hashed as served): a tag that holds another digest
#      is a failure, one that holds the pinned digest needs no change, one the registry reports as not found is
#      created; any other answer or error is a failure;
#   c. points <tag> at the pinned digest;
#   d. reads <tag> back and requires the pinned digest.
# Every image is built and its tag read (a, b) before any tag is written (c), so a clash leaves no tag half moved.
# Prints names and digests only.
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

# The pins must be the release at <tag> and equal what this checkout builds.
"$pins" check "$outdir/digests.txt" "$tag" > /dev/null || fail "the built digests are not the pinned release $tag"

release_ensure_builder
meta=$(mktemp -d)
trap 'rm -rf "$meta"' EXIT

# The images: name, repository (the pinned ref up to its tag) and pinned digest, from `release-pins.sh show`.
shown=$("$pins" show)
names=()
repos=()
digests=()
while read -r name ref; do
  repo=${ref%%:*}
  names+=("$name")
  repos+=("$repo")
  digests+=("${ref##*@}")
done <<< "$shown"
[ "${#names[@]}" -eq 3 ] || fail "expected three pinned images, have ${#names[@]}"

sha256_of() { # <file>: sha256:<hex> of the file's bytes
  local sum
  if command -v sha256sum > /dev/null; then sum=$(sha256sum "$1"); else sum=$(shasum -a 256 "$1"); fi
  printf 'sha256:%s' "${sum%% *}"
}

# tag_digest <repo>: the digest of the top-level manifest <repo>:<tag> serves (the digest of the exact bytes), or
# nothing when the registry reports this very ref as not found. Every other outcome stops the push.
tag_digest() {
  local ref=$1:$tag
  if docker buildx imagetools inspect --raw "$ref" > "$meta/raw" 2> "$meta/err"; then
    [ -s "$meta/raw" ] || fail "$ref: the registry served an empty manifest"
    sha256_of "$meta/raw"
  elif [ "$(cat "$meta/err")" = "ERROR: $ref: not found" ]; then
    return 0
  else
    fail "$ref: cannot read the tag: $(head -c 300 "$meta/err")"
  fi
}

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
