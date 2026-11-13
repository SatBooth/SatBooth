#!/usr/bin/env bash
# Tests release_tag_digest of scripts/release-lib.sh (how release-push.sh reads a registry tag) against a throwaway
# registry on 127.0.0.1: a tag that is absent, one that holds a manifest, a second tag that holds another, a repository
# that does not exist, and a registry that cannot be reached. The registry runs in Docker: it needs the pinned
# registry image already local (it pulls nothing, unless RELEASE_PUSH_TEST_PULL=1); otherwise it says so and skips.
# Usage: scripts/release-push_test.sh
set -euo pipefail

cd "$(dirname "$0")/.."

fail() { echo "release-push_test: FAIL: $*" >&2; exit 1; }

REGISTRY_IMAGE=registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373

if ! docker info > /dev/null 2>&1; then echo "release-push_test: skipped, Docker is not reachable"; exit 0; fi
if ! docker image inspect "$REGISTRY_IMAGE" > /dev/null 2>&1; then
  if [ "${RELEASE_PUSH_TEST_PULL:-}" = 1 ]; then
    docker pull --quiet "$REGISTRY_IMAGE" > /dev/null
  else
    echo "release-push_test: skipped, $REGISTRY_IMAGE is not local (RELEASE_PUSH_TEST_PULL=1 pulls it)"
    exit 0
  fi
fi

# shellcheck source=scripts/release-lib.sh
. scripts/release-lib.sh

tmp=$(mktemp -d)
name=satbooth-release-push-test-$$
trap 'docker rm -f "$name" > /dev/null 2>&1 || true; rm -rf "$tmp"' EXIT

docker run -d --rm --name "$name" -p 127.0.0.1::5000 "$REGISTRY_IMAGE" > /dev/null
port=$(docker port "$name" 5000/tcp | head -n 1 | sed 's/.*://')
[[ $port =~ ^[0-9]+$ ]] || fail "no port for the registry: $port"
host=127.0.0.1:$port
for _ in $(seq 1 50); do
  curl -fs "http://$host/v2/" > /dev/null 2>&1 && break
  sleep 0.2
done
curl -fs "http://$host/v2/" > /dev/null || fail "the registry did not start"

# Two small OCI images, put into the registry through its HTTP API: an empty config and no layers, the second with an
# annotation so that the manifests differ.
config='{}'
config_digest=sha256:$(printf '%s' "$config" | shasum -a 256 | cut -d' ' -f1)
upload=$(curl -fsi -X POST "http://$host/v2/test/blobs/uploads/" | tr -d '\r' | awk -F': ' 'tolower($1) == "location" { print $2 }')
[ -n "$upload" ] || fail "no upload location"
case $upload in /*) upload=http://$host$upload ;; esac
curl -fs -X PUT -o /dev/null --data-binary "$config" -H 'Content-Type: application/octet-stream' \
  "$upload&digest=$config_digest" || fail "cannot upload the config blob"
put_manifest() { # <tag> <annotation text>
  printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%s},"layers":[],"annotations":{"test":"%s"}}' \
    "$config_digest" "${#config}" "$2" > "$tmp/manifest-$1.json"
  curl -fs -X PUT -o /dev/null -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
    --data-binary "@$tmp/manifest-$1.json" "http://$host/v2/test/manifests/$1" || fail "cannot put the manifest $1"
}
put_manifest one first
put_manifest two second

# served <tag>: the digest the registry itself reports for the top-level manifest, by a plain HTTP request.
served() {
  curl -fsI -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "http://$host/v2/test/manifests/$1" | tr -d '\r' | awk -F': ' 'tolower($1) == "docker-content-digest" { print $2 }'
}

fail() { echo "release-push_test: FAIL: $*" >&2; exit 1; }
# release_tag_digest calls fail, which must exit; run it in a subshell so a refusal is a status.
digest_of_tag() { (release_tag_digest "$1" "$2" "$tmp") 2> "$tmp/stderr"; }

one=$(served one)
two=$(served two)
[[ $one =~ ^sha256:[0-9a-f]{64}$ ]] || fail "no digest for the first tag: $one"
[ "$one" != "$two" ] || fail "the two tags hold one manifest"

got=$(digest_of_tag "$host/test" one) || fail "present tag: $(cat "$tmp/stderr")"
[ "$got" = "$one" ] || fail "present tag: got $got, the registry serves $one"
got=$(digest_of_tag "$host/test" two) || fail "other tag: $(cat "$tmp/stderr")"
[ "$got" = "$two" ] || fail "other tag: got $got, the registry serves $two"

got=$(digest_of_tag "$host/test" absent) || fail "absent tag: $(cat "$tmp/stderr")"
[ -z "$got" ] || fail "absent tag: got $got"

got=$(digest_of_tag "$host/never-pushed" one) || fail "absent repository: $(cat "$tmp/stderr")"
[ -z "$got" ] || fail "absent repository: got $got"

# A registry that cannot be reached is a failure, not an absent tag.
if got=$(digest_of_tag "127.0.0.1:1/test" one); then fail "unreachable registry: accepted, got $got"; fi
grep -q 'cannot read the tag' "$tmp/stderr" || fail "unreachable registry: $(cat "$tmp/stderr")"

echo "release-push_test: ok"
