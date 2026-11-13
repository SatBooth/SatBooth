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

# shellcheck source=scripts/release-lib.sh
. "$(dirname "$0")/release-lib.sh"

[[ $tag =~ $RELEASE_TAG_RE ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $tag"

cd "$(dirname "$0")/.."
pins=scripts/release-pins.sh

version=$("$pins" version)
[ "$version" = "$tag" ] || fail "the pins are at $version, not $tag"

release_pins_read "$pins" || fail "cannot read the pinned images"
for i in 0 1 2; do
  name=${PIN_NAMES[$i]} ref=${PIN_REFS[$i]} digest=${PIN_DIGESTS[$i]}
  have=$(docker buildx imagetools inspect "${ref%%@*}" --format '{{.Manifest.Digest}}')
  [ "$have" = "$digest" ] || fail "$name:$tag holds $have, pinned $digest"
  docker pull --quiet "$ref" > /dev/null
  echo "release-smoke: $name $digest pulled"
done

scripts/image-run-test.sh "${PIN_REFS[@]}"
