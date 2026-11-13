#!/usr/bin/env bash
# Usage: scripts/image-run-test.sh <satbooth-image> <relay-image> <tools-image>
# Runs each release image the way it ships (read-only root, no capabilities, numeric user) with a missing
# configuration file, and checks the refusal and the image's user and environment.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: image-run-test.sh <satbooth-image> <relay-image> <tools-image>" >&2
  exit 2
fi
sb=$1
relay=$2
tools=$3

fail() { echo "image-run-test: FAIL: $*" >&2; exit 1; }

run() {
  docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 "$@"
}

default_path="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
for image in "$sb" "$relay" "$tools"; do
  user=$(docker inspect --format '{{.Config.User}}' "$image")
  [ "$user" = "65532:65532" ] || fail "$image: User is $user"
  env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$image" | sed '/^$/d')
  [ -z "$env" ] || [ "$env" = "$default_path" ] || fail "$image: Env is not empty or Docker's default PATH"
done

# No image may panic at start-up inside the hardened container.
no_panic() {
  case "$2" in
    *panic:* | *goroutine\ *) fail "$1 printed a panic or goroutine trace" ;;
  esac
}

rc=0
out=$(run "$sb" -config /nope 2>&1) || rc=$?
[ "$rc" -eq 2 ] || fail "satbooth exited $rc, want 2"
no_panic satbooth "$out"
[ "$(printf '%s\n' "$out" | grep -c .)" -eq 1 ] || fail "satbooth printed more than one line"
case "$out" in
  '{'*'"msg":"configuration refused"'*) ;;
  *) fail "satbooth printed no configuration refused line" ;;
esac
echo "image-run-test: satbooth exit 2, configuration refused"

# satbooth-relay prints the error from relay.LoadConfig, which starts with "config: ", and exits 2.
rc=0
out=$(run "$relay" -config /nope 2>&1) || rc=$?
[ "$rc" -eq 2 ] || fail "satbooth-relay exited $rc, want 2"
no_panic satbooth-relay "$out"
[ "$(printf '%s\n' "$out" | grep -c .)" -eq 1 ] || fail "satbooth-relay printed more than one line"
case "$out" in
  'config: '*) ;;
  *) fail "satbooth-relay printed no config error line" ;;
esac
echo "image-run-test: satbooth-relay exit 2, config error printed"
# satbooth-setup without arguments is a usage error (2), with no network either: the tools image runs the way
# deploy/setup.sh runs it. It has no entrypoint, so the command is named.
rc=0
out=$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --user 65532:65532 "$tools" /satbooth-setup 2>&1) || rc=$?
[ "$rc" -eq 2 ] || fail "satbooth-setup exited $rc, want 2"
no_panic satbooth-setup "$out"
case "$out" in
  *usage:*) ;;
  *) fail "satbooth-setup printed no usage" ;;
esac
echo "image-run-test: satbooth-setup exit 2, usage printed"
echo "image-run-test: ok"
