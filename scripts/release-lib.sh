# shellcheck shell=bash disable=SC2034
# Sourced by scripts/release-build.sh and scripts/release-push.sh: every input that decides a release image digest.
# Both scripts build with the same builder, the same flags and the same targets, so the digest that
# release-build.sh writes is the digest that release-push.sh pushes. Not run directly.

# Docker's BuildKit, pinned by digest: layer compression and the exporter decide the image digest.
BUILDKIT_IMAGE=moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c

# The builder is named after the first 12 hex digits of the BuildKit digest, so a new pin makes a new builder.
_release_buildkit_digest=${BUILDKIT_IMAGE##*sha256:}
RELEASE_BUILDER=satbooth-release-${_release_buildkit_digest:0:12}

RELEASE_PLATFORMS=linux/amd64,linux/arm64
RELEASE_TARGETS=(satbooth satbooth-relay satbooth-tools)

# Flags of every release build. SOURCE_DATE_EPOCH=0 sets every file time; the callers' --output adds
# rewrite-timestamp=true. Provenance and SBOM attestations are left out: they hold build times and host data.
RELEASE_BUILD_FLAGS=(
  --builder "$RELEASE_BUILDER"
  --file deploy/image/Dockerfile
  --provenance=false
  --sbom=false
  --build-arg SOURCE_DATE_EPOCH=0
)
if [ "${RELEASE_NO_CACHE:-}" = 1 ]; then
  RELEASE_BUILD_FLAGS+=(--no-cache)
fi

# Creates the builder when it is missing and starts it.
release_ensure_builder() {
  if ! docker buildx inspect "$RELEASE_BUILDER" > /dev/null 2>&1; then
    docker buildx create --name "$RELEASE_BUILDER" --driver docker-container \
      --driver-opt "image=$BUILDKIT_IMAGE" > /dev/null
  fi
  docker buildx inspect --bootstrap "$RELEASE_BUILDER" > /dev/null
}
