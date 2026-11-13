#!/usr/bin/env bash
# Usage: scripts/fuzz.sh [seconds]  (default 30). Runs every Fuzz* target, one go test invocation each.
set -euo pipefail

cd "$(dirname "$0")/.."

secs=${1:-30}
case "$secs" in
  ''|*[!0-9]*) echo "usage: scripts/fuzz.sh [seconds]" >&2; exit 2 ;;
esac

export GOFLAGS=-mod=vendor GOPROXY=off CGO_ENABLED=0

# "path/to/file_test.go:LINE:func FuzzName(f *testing.F)" -> package dir and target name
found=0
failed=()
while IFS= read -r line; do
  file=${line%%:*}
  dir=$(dirname "$file")
  name=${line#*func }
  name=${name%%(*}
  found=$((found + 1))
  echo "== fuzz $dir $name (${secs}s)"
  if ! go test "./$dir/" -run=NONE -fuzz="^${name}\$" -fuzztime="${secs}s"; then
    failed+=("$dir $name")
  fi
done < <(grep -rn --include='*_test.go' -E '^func Fuzz[A-Za-z0-9_]*\([A-Za-z0-9_]+ \*testing\.F\)' \
  --exclude-dir=vendor --exclude-dir=.worktrees --exclude-dir=.tools . | sed 's|^\./||' | sort)

[ "$found" -gt 0 ] || { echo "fuzz.sh: no fuzz targets found" >&2; exit 1; }
if [ "${#failed[@]}" -gt 0 ]; then
  echo "fuzz.sh: FAIL: ${#failed[@]} of $found targets failed:" >&2
  printf '  %s\n' "${failed[@]}" >&2
  exit 1
fi
echo "fuzz.sh: OK ($found targets)"
