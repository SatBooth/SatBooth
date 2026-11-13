#!/usr/bin/env bash
# Tests which tools image deploy/setup.sh runs: the pinned release image (pulled only when absent locally), or, with
# SATBOOTH_BUILD=1, one built from this repository. Docker is a stub on the PATH that records its calls; no engine
# and no network are used.
# Usage: scripts/setup-image_test.sh
set -euo pipefail

cd "$(dirname "$0")/.."

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin" "$tmp/work"
cat > "$tmp/bin/docker" << 'STUB'
#!/bin/sh
echo "$*" >> "$STUB_LOG"
case "$1" in
  info) echo "name=seccomp " ;;
  image) [ -n "${STUB_IMAGE_PRESENT:-}" ] || exit 1 ;;
  pull) [ -z "${STUB_PULL_FAIL:-}" ] || { echo "stub: pull refused" >&2; exit 1; } ;;
esac
exit 0
STUB
chmod +x "$tmp/bin/docker"
export STUB_LOG=$tmp/log

fail() { echo "setup-image_test: FAIL: $*" >&2; exit 1; }

pinned=$(scripts/release-pins.sh show | awk '$1 == "satbooth-tools" { print $2 }')
[ -n "$pinned" ] || fail "no pinned tools image"

# run_setup <env assignment...>: runs the wrapper with the stub first on the PATH; sets rc, err and the call log.
run_setup() {
  : > "$STUB_LOG"
  rc=0
  env -u SATBOOTH_BUILD PATH="$tmp/bin:$PATH" "$@" bash deploy/setup.sh "$tmp/work" public init > /dev/null 2> "$tmp/err" || rc=$?
}
calls() { cut -d' ' -f1 "$STUB_LOG" | grep -vxE 'info|--version|image' | tr '\n' ' '; }

# Without SATBOOTH_BUILD (and with 0): a pull of the pinned ref, then a run of it. No build.
for v in unset 0; do
  if [ "$v" = unset ]; then run_setup A=1; else run_setup SATBOOTH_BUILD=0; fi
  [ "$rc" -eq 0 ] || fail "SATBOOTH_BUILD $v: exit $rc"
  [ "$(calls)" = 'pull run ' ] || fail "SATBOOTH_BUILD $v: calls: $(calls)"
  grep -qxF "pull $pinned" "$STUB_LOG" || fail "SATBOOTH_BUILD $v: did not pull the pinned ref"
  grep -q "^run .* $pinned /satbooth-setup " "$STUB_LOG" || fail "SATBOOTH_BUILD $v: did not run the pinned ref"
  ! grep -q -- '-source-build' "$STUB_LOG" || fail "SATBOOTH_BUILD $v: -source-build was passed"
done

# The pinned image already local: no pull, and the run uses it.
run_setup STUB_IMAGE_PRESENT=1
[ "$rc" -eq 0 ] || fail "image present: exit $rc"
[ "$(calls)" = 'run ' ] || fail "image present: calls: $(calls)"
grep -qxF "image inspect $pinned" "$STUB_LOG" || fail "image present: did not look for the pinned ref locally"
grep -q "^run .* $pinned /satbooth-setup " "$STUB_LOG" || fail "image present: did not run the pinned ref"
# Absent (the default of the stub): the pull happens, as above, and a failed pull still stops it.

# With SATBOOTH_BUILD=1: a build of the local image, then a run of it. No pull.
run_setup SATBOOTH_BUILD=1
[ "$rc" -eq 0 ] || fail "SATBOOTH_BUILD=1: exit $rc"
[ "$(calls)" = 'build run ' ] || fail "SATBOOTH_BUILD=1: calls: $(calls)"
grep -q "^run .* satbooth-tools:local /satbooth-setup " "$STUB_LOG" || fail "SATBOOTH_BUILD=1: did not run the local image"
grep -q "^run .* /satbooth-setup public init .* -source-build\$" "$STUB_LOG" || fail "SATBOOTH_BUILD=1: no -source-build flag"

# Another value is refused before Docker is used.
run_setup SATBOOTH_BUILD=yes
[ "$rc" -eq 2 ] || fail "SATBOOTH_BUILD=yes: exit $rc, want 2"
grep -q 'SATBOOTH_BUILD must be empty, 0 or 1' "$tmp/err" || fail "SATBOOTH_BUILD=yes: refusal text: $(cat "$tmp/err")"
[ ! -s "$STUB_LOG" ] || fail "SATBOOTH_BUILD=yes: docker was called"

# A failed pull names the ref and the source build, exits 1, and runs nothing.
run_setup STUB_PULL_FAIL=1
[ "$rc" -eq 1 ] || fail "failed pull: exit $rc, want 1"
grep -qF "could not pull $pinned" "$tmp/err" || fail "failed pull: the ref is not named: $(cat "$tmp/err")"
grep -qF 'SATBOOTH_BUILD=1' "$tmp/err" || fail "failed pull: no pointer to SATBOOTH_BUILD=1"
[ "$(calls)" = 'pull ' ] || fail "failed pull: calls: $(calls)"

echo "setup-image_test: ok"
