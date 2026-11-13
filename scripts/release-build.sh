#!/usr/bin/env bash
# Usage: scripts/release-build.sh <outdir> [<platform>]
# Builds the three release images (satbooth, satbooth-relay, satbooth-tools) for linux/amd64 and linux/arm64 with the
# pinned BuildKit of scripts/release-lib.sh, and writes to <outdir>:
#   <name>.oci.tar     the multi-platform image as an OCI archive
#   <name>.meta.json   the build's metadata file
#   digests.txt        one line per image, "<name> sha256:<index digest>", in the order above
# The same inputs give the same digests. With a <platform> (linux/amd64 or linux/arm64) it also builds each image for
# that platform alone into <name>-<arch>.oci.tar, requires its manifest digest to equal the one the multi-platform
# index lists for the platform, loads it into the local Docker as <name>:release-test, and requires the loaded image
# to be that image by its ID. RELEASE_NO_CACHE=1 builds without the cache. Prints names and digests only.
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: release-build.sh <outdir> [<platform>]" >&2
  exit 2
fi
platform=${2:-}
case "$platform" in
  '' | linux/amd64 | linux/arm64) ;;
  *) echo "release-build: platform must be linux/amd64 or linux/arm64: $platform" >&2; exit 2 ;;
esac
# <outdir> may be relative to the caller's directory: made absolute before the change to the repository root.
mkdir -p -- "$1"
outdir=$(cd -- "$1" && pwd)

fail() { echo "release-build: FAIL: $*" >&2; exit 1; }

cd "$(dirname "$0")/.."
# shellcheck source=scripts/release-lib.sh
. scripts/release-lib.sh

release_ensure_builder

# Prints the digest of the image or index at the top of an OCI archive's index.json.
top_digest() {
  tar -xOf "$1" index.json | jq -er '.manifests | if length == 1 then .[0].digest else error("not one top-level descriptor") end'
}

# Prints the content of a blob (sha256:<hex>) of an OCI archive.
blob() {
  tar -xOf "$1" "blobs/sha256/${2#sha256:}"
}

# Prints the digest of the manifest the index of a multi-platform archive lists for a platform (os/arch).
platform_manifest() {
  local os=${2%/*} arch=${2#*/}
  blob "$1" "$(top_digest "$1")" |
    jq -er --arg os "$os" --arg arch "$arch" \
      '[.manifests[] | select(.platform.os == $os and .platform.architecture == $arch)] |
       if length == 1 then .[0].digest else error("not one manifest for the platform") end'
}

# Loads <name>:release-test from a single-platform archive and requires it to be the image whose manifest digest is
# <manifest>. The archive is loaded as it is and the loaded image's ID must be that manifest or its config (the ID of
# the containerd image store is the manifest digest, that of the classic store the config digest). A Docker that
# refuses the archive or reports another ID gets the same platform built straight into its image store (cached, same
# builder), and the ID of that image must be the manifest or config digest too. Anything else fails: no partial
# comparison of the configuration stands in for the identity.
# Arguments: <name> <platform> <archive> <manifest>.
load_release_test() {
  local name=$1 plat=$2 archive=$3 manifest=$4 loaded config
  config=$(blob "$archive" "$manifest" | jq -er '.config.digest')
  if loaded=$(docker load -i "$archive" 2> /dev/null | sed -n 's/^Loaded image ID: //p') &&
    { [ "$loaded" = "$manifest" ] || [ "$loaded" = "$config" ]; }; then
    docker tag "$loaded" "$name:release-test"
    echo "release-build: $name:release-test is the loaded archive, ID $loaded"
    return 0
  fi
  echo "release-build: $name: loading the archive did not give the pinned image; building into the image store"
  docker buildx build "${RELEASE_BUILD_FLAGS[@]}" --progress=quiet --target "$name" --platform "$plat" \
    --output "type=docker,name=$name:release-test,$RELEASE_EXPORT_ATTRS" . > /dev/null
  loaded=$(docker image inspect --format '{{.Id}}' "$name:release-test")
  { [ "$loaded" = "$manifest" ] || [ "$loaded" = "$config" ]; } ||
    fail "$name:release-test has ID $loaded, not the index's $plat manifest $manifest or its config $config"
  echo "release-build: $name:release-test, built into the image store, has ID $loaded"
}

: > "$outdir/digests.txt"
for name in "${RELEASE_TARGETS[@]}"; do
  docker buildx build "${RELEASE_BUILD_FLAGS[@]}" --progress=quiet --target "$name" --platform "$RELEASE_PLATFORMS" \
    --output "type=oci,dest=$outdir/$name.oci.tar,$RELEASE_EXPORT_ATTRS" \
    --metadata-file "$outdir/$name.meta.json" . > /dev/null
  digest=$(jq -er '."containerimage.digest"' "$outdir/$name.meta.json")
  [ "$digest" = "$(top_digest "$outdir/$name.oci.tar")" ] || fail "$name: the archive's index is not the built digest"
  echo "$name $digest" >> "$outdir/digests.txt"
  echo "release-build: $name $digest"
done

[ -n "$platform" ] || exit 0

arch=${platform#*/}
for name in "${RELEASE_TARGETS[@]}"; do
  single=$outdir/$name-$arch.oci.tar
  docker buildx build "${RELEASE_BUILD_FLAGS[@]}" --progress=quiet --target "$name" --platform "$platform" \
    --output "type=oci,dest=$single,$RELEASE_EXPORT_ATTRS" . > /dev/null
  want=$(platform_manifest "$outdir/$name.oci.tar" "$platform")
  got=$(top_digest "$single")
  [ "$got" = "$want" ] || fail "$name $platform: single-platform manifest $got differs from the index's $want"
  echo "release-build: $name $platform manifest $got equals the index's"
  load_release_test "$name" "$platform" "$single" "$want"
done
