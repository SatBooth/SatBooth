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

[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || fail "not a release tag like v1.2.3 or v1.2.3-rc.1: $tag"

cd "$(dirname "$0")/.."
pins=scripts/release-pins.sh

version=$("$pins" version)
[ "$version" = "$tag" ] || fail "the pins are at $version, not $tag"
shown=$("$pins" show)

cat <<TEXT
Changes: (to be written before publishing)

## Images

TEXT
while read -r _ ref; do
  # shellcheck disable=SC2016
  printf -- '- `%s`\n' "$ref"
done <<< "$shown"

cat <<TEXT

## Deploy

Deploy from this tag: \`git checkout $tag\` (verify it with \`git tag -v $tag\`). The Compose files and
\`deploy/setup.sh\` of the tag pin the images above by digest.

## Verify the images

By attestation, which accepts only the release workflow of this repository, run from this tag on a GitHub-hosted
runner, for each image:

TEXT
while read -r _ ref; do
  printf -- '    gh attestation verify oci://%s -R SatBooth/SatBooth \\\n' "${ref/:$tag@/@}"
  printf -- '      --signer-workflow SatBooth/SatBooth/.github/workflows/release.yml \\\n'
  printf -- '      --source-ref refs/tags/%s --deny-self-hosted-runners\n' "$tag"
done <<< "$shown"

cat <<'TEXT'

By rebuild, which needs Docker with buildx. The digests it prints must equal the pinned ones.
It writes large files, so the output directory is outside the checkout:

    out=$(mktemp -d)
    scripts/release-build.sh "$out"
    scripts/release-pins.sh check "$out/digests.txt"
TEXT
