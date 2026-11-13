#!/usr/bin/env bash
# Usage: scripts/release-notes.sh <tag>
# Prints the markdown body of the draft release for <tag>: a placeholder for the changes, the three pinned image refs
# of `release-pins.sh show`, and how to verify them. The pinned version must be <tag>.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: release-notes.sh <tag>" >&2
  exit 2
fi
tag=$1

fail() { echo "release-notes: FAIL: $*" >&2; exit 1; }

# shellcheck source=scripts/release-lib.sh
. "$(dirname "$0")/release-lib.sh"

[[ $tag =~ $RELEASE_TAG_RE ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $tag"

cd "$(dirname "$0")/.."
pins=scripts/release-pins.sh

version=$("$pins" version)
[ "$version" = "$tag" ] || fail "the pins are at $version, not $tag"
release_pins_read "$pins" || fail "cannot read the pinned images"

cat <<TEXT
Changes: (to be written before publishing)

## Images

TEXT
for ref in "${PIN_REFS[@]}"; do
  # shellcheck disable=SC2016
  printf -- '- `%s`\n' "$ref"
done

cat <<TEXT

## Deploy

Deploy from this tag: \`git checkout $tag\` (verify it with \`git tag -v $tag\`). The Compose files and
\`deploy/setup.sh\` of the tag pin the images above by digest.

## Verify the images

By attestation, which accepts only the release workflow of this repository, run from this tag on a GitHub-hosted
runner, for each image:

TEXT
for ref in "${PIN_REFS[@]}"; do
  printf -- '    gh attestation verify oci://%s -R SatBooth/SatBooth \\\n' "${ref/:$tag@/@}"
  printf -- '      --signer-workflow SatBooth/SatBooth/.github/workflows/release.yml \\\n'
  printf -- '      --source-ref refs/tags/%s --deny-self-hosted-runners\n' "$tag"
done

cat <<'TEXT'

By rebuild, which needs Docker with buildx and `jq`. The digests it prints must equal the pinned ones.
It writes large files, so the output directory is outside the checkout:

    out=$(mktemp -d)
    scripts/release-build.sh "$out"
    scripts/release-pins.sh check "$out/digests.txt"
TEXT
