# shellcheck shell=bash disable=SC2034
# Sourced by the release scripts. Every input that decides a release image digest: scripts/release-build.sh and
# scripts/release-push.sh build with the same builder, the same flags and the same targets, so the digest that
# release-build.sh writes is the digest that release-push.sh pushes. Also the one definition of a release version and
# tag, and the reader of the pinned refs. Not run directly.

# A release version: v<major>.<minor>.<patch>, with an optional -rc.<n>. A release tag is exactly a version.
RELEASE_VERSION_RE='v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?'
RELEASE_TAG_RE="^${RELEASE_VERSION_RE}\$"

# Docker's BuildKit, pinned by digest: layer compression and the exporter decide the image digest.
BUILDKIT_IMAGE=moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c

# The builder is named after the first 12 hex digits of the BuildKit digest, so a new pin makes a new builder.
_release_buildkit_digest=${BUILDKIT_IMAGE##*sha256:}
RELEASE_BUILDER=satbooth-release-${_release_buildkit_digest:0:12}

RELEASE_PLATFORMS=linux/amd64,linux/arm64
RELEASE_TARGETS=(satbooth satbooth-relay satbooth-tools)

# The exporter attributes of every release build, appended to every --output (type=oci here, type=image with push in
# release-push.sh): OCI media types, gzip layers at one fixed level applied to every layer, and file times rewritten to
# SOURCE_DATE_EPOCH. Each is set explicitly so no exporter default decides a digest.
RELEASE_EXPORT_ATTRS=oci-mediatypes=true,compression=gzip,compression-level=6,force-compression=true,rewrite-timestamp=true

# Flags of every release build. SOURCE_DATE_EPOCH=0 sets every file time. Provenance and SBOM attestations are left
# out: they hold build times and host data.
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

# release_pins_read <release-pins.sh>: reads `release-pins.sh show` into the parallel arrays PIN_NAMES, PIN_REFS (the
# whole pinned ref), PIN_REPOS (the ref up to its tag) and PIN_DIGESTS (the sha256:... of the ref). Fails unless it is
# three images.
release_pins_read() {
  local shown name ref
  PIN_NAMES=() PIN_REFS=() PIN_REPOS=() PIN_DIGESTS=()
  shown=$("$1" show) || return 1
  while read -r name ref || [ -n "$name" ]; do
    PIN_NAMES+=("$name")
    PIN_REFS+=("$ref")
    PIN_REPOS+=("${ref%%:*}")
    PIN_DIGESTS+=("${ref##*@}")
  done <<< "$shown"
  [ "${#PIN_NAMES[@]}" -eq 3 ] || { echo "expected three pinned images, have ${#PIN_NAMES[@]}" >&2; return 1; }
}

# release_sha256_of <file>: sha256:<hex> of the file's bytes.
release_sha256_of() {
  local sum
  if command -v sha256sum > /dev/null; then sum=$(sha256sum "$1"); else sum=$(shasum -a 256 "$1"); fi
  printf 'sha256:%s' "${sum%% *}"
}

# release_tag_absent: reads an error text on standard input; succeeds iff it is a registry's answer for a missing tag. A
# line ending in ": not found" is the resolver's own; "executable file not found" and "command not found" are not.
release_tag_absent() {
  local text
  text=$(cat)
  if grep -qiE 'executable file not found|command not found' <<< "$text"; then return 1; fi
  grep -qiE ': not found$|manifest unknown|name unknown' <<< "$text"
}

# release_tag_digest <repo> <tag> <scratch dir>: prints the digest of the top-level manifest <repo>:<tag> serves (the
# digest of the exact bytes `imagetools inspect --raw` returns), or nothing when the registry has no such tag. The
# caller defines `fail <message>`, which it must make exit.
# The tag is taken as absent only when the command fails and its stderr has a line that ends in ": not found" or holds
# "manifest unknown" or "name unknown", whatever the case (release_tag_absent): that is how a registry answers a tag that
# is not there (HTTP 404, MANIFEST_UNKNOWN), and the exact wording belongs to the buildx of the runner, which is not
# pinned. The command fetches only this one manifest, so such a message is about this ref. Any other failure (a network
# error, a refusal, a server error, "executable file not found", "command not found") and an empty manifest stop the
# caller.
release_tag_digest() {
  local ref=$1:$2 raw=$3/raw err=$3/err
  if docker buildx imagetools inspect --raw "$ref" > "$raw" 2> "$err"; then
    [ -s "$raw" ] || fail "$ref: the registry served an empty manifest"
    release_sha256_of "$raw"
  elif release_tag_absent < "$err"; then
    return 0
  else
    fail "$ref: cannot read the tag: $(head -c 300 "$err")"
  fi
}
