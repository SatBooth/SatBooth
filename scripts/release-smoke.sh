#!/usr/bin/env bash
# Usage: scripts/release-smoke.sh <tag>
# Pulls the three published release images by the `tag@digest` refs of `release-pins.sh show` and runs
# scripts/image-run-test.sh on the pulled images, on this machine's own platform. It needs `docker login` when the
# packages are private. The pinned version must be <tag>, and <tag> in the registry must hold the pinned digest.
# Prints names and digests only.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: release-smoke.sh <tag>" >&2
  exit 2
fi
tag=$1

fail() { echo "release-smoke: FAIL: $*" >&2; exit 1; }

[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $tag"

cd "$(dirname "$0")/.."
pins=scripts/release-pins.sh

version=$("$pins" version)
[ "$version" = "$tag" ] || fail "the pins are at $version, not $tag"

shown=$("$pins" show)
refs=()
while read -r name ref; do
  digest=${ref##*@}
  have=$(docker buildx imagetools inspect "${ref%%@*}" --format '{{.Manifest.Digest}}')
  [ "$have" = "$digest" ] || fail "$name:$tag holds $have, pinned $digest"
  docker pull --quiet "$ref" > /dev/null
  echo "release-smoke: $name $digest pulled"
  refs+=("$ref")
done <<< "$shown"
[ "${#refs[@]}" -eq 3 ] || fail "expected three images, have ${#refs[@]}"

scripts/image-run-test.sh "${refs[@]}"
