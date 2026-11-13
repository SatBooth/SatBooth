#!/usr/bin/env bash
# Tests scripts/release-notes.sh against the pins of this repository: the three refs of `release-pins.sh show` appear
# in the text, the placeholder and the verification steps are there, a wrong or malformed tag is refused, and the text
# is exactly the expected one. Usage: scripts/release-notes_test.sh
# shellcheck disable=SC2016
set -euo pipefail

cd "$(dirname "$0")/.."

fail() { echo "release-notes_test: FAIL: $*" >&2; exit 1; }

tag=$(scripts/release-pins.sh version)
notes=$(scripts/release-notes.sh "$tag")

shown=$(scripts/release-pins.sh show)
n=0
while read -r name ref || [ -n "$name" ]; do
  n=$((n + 1))
  grep -qF -- "\`$ref\`" <<< "$notes" || fail "$name: the ref is not listed: $ref"
  grep -qF -- "gh attestation verify oci://${ref/:$tag@/@} -R SatBooth/SatBooth" <<< "$notes" ||
    fail "$name: no attestation command"
done <<< "$shown"
[ "$n" -eq 3 ] || fail "show printed $n refs, want 3"

for want in 'Changes: (to be written before publishing)' "git tag -v $tag" \
  "git switch --detach \"refs/tags/$tag^{commit}\"" "git rev-parse HEAD" \
  'out=$(mktemp -d)' 'scripts/release-build.sh "$out"' \
  'scripts/release-pins.sh check "$out/digests.txt"'; do
  grep -qF -- "$want" <<< "$notes" || fail "the text lacks: $want"
done

# The whole text is fixed: besides the tag and the three refs, nothing in it varies, so it names no person.
refs=()
while read -r name ref || [ -n "$name" ]; do refs+=("$ref"); done <<< "$shown"
expected=$(
  printf 'Changes: (to be written before publishing)\n\n## Images\n\n'
  for ref in "${refs[@]}"; do printf -- '- `%s`\n' "$ref"; done
  printf '\n## Deploy\n\nDeploy from this tag, in a clean checkout. Verify the tag with `git tag -v %s`, then select its commit with\n' "$tag"
  printf '`git switch --detach "refs/tags/%s^{commit}"`, and check that `git rev-parse HEAD` equals\n' "$tag"
  printf '`git rev-parse "refs/tags/%s^{commit}"`. The Compose files and `deploy/setup.sh` of the tag pin the images\n' "$tag"
  printf 'above by digest.\n\n## Verify the images\n\n'
  printf 'By attestation, which accepts only the release workflow of this repository, run from this tag on a GitHub-hosted\nrunner, for each image:\n\n'
  for ref in "${refs[@]}"; do
    printf '    gh attestation verify oci://%s -R SatBooth/SatBooth \\\n' "${ref/:$tag@/@}"
    printf '      --signer-workflow SatBooth/SatBooth/.github/workflows/release.yml \\\n'
    printf '      --source-ref refs/tags/%s --deny-self-hosted-runners\n' "$tag"
  done
  printf '\nBy rebuild, which needs Docker with buildx and `jq`. The digests it prints must equal the pinned ones.\n'
  printf 'It writes large files, so the output directory is outside the checkout:\n\n'
  printf '    out=$(mktemp -d)\n    scripts/release-build.sh "$out"\n'
  printf '    scripts/release-pins.sh check "$out/digests.txt"\n'
)
[ "$notes" = "$expected" ] || fail "the text differs from the expected one"

# A tag other than the pinned version, and malformed tags, are refused.
for bad in v9.9.9 "$tag-x" 'v1.2' '' 'main' 'v1.2.3;id'; do
  if scripts/release-notes.sh "$bad" > /dev/null 2>&1; then fail "the tag '$bad' was accepted"; fi
done

echo "release-notes_test: ok"
