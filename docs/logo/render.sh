#!/usr/bin/env bash
# Renders the logo PNGs from the committed SVGs with resvg (a local tool, not a dependency).
#   docs/logo/render.sh           writes the PNGs next to the SVGs
#   docs/logo/render.sh --check   renders into a temporary directory and requires every committed PNG to be
#                                 byte-identical
# resvg must be exactly version 0.49.0, so the same SVG always gives the same bytes.
set -euo pipefail

want=0.49.0
dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

mode="write"
case "${1:-}" in
  "") ;;
  --check) mode="check" ;;
  *) echo "usage: render.sh [--check]" >&2; exit 2 ;;
esac
if [ "$#" -gt 1 ]; then
  echo "usage: render.sh [--check]" >&2
  exit 2
fi

if ! command -v resvg >/dev/null 2>&1; then
  echo "render: resvg is not installed (need $want)" >&2
  exit 1
fi
have=$(resvg --version | awk '{print $NF}')
if [ "$have" != "$want" ]; then
  echo "render: resvg is $have, need exactly $want" >&2
  exit 1
fi

out=$dir
if [ "$mode" = check ]; then
  out=$(mktemp -d)
  trap 'rm -rf "$out"' EXIT
fi

# name, resvg size flag, size
jobs=(
  "satbooth-mark -w 512"
  "satbooth-mark -w 1024"
  "satbooth-mark-dark -w 512"
  "satbooth-logo -h 480"
  "satbooth-logo-dark -h 480"
)
files=()
for j in "${jobs[@]}"; do
  read -r name flag size <<<"$j"
  png="$name-$size.png"
  resvg "$flag" "$size" "$dir/$name.svg" "$out/$png"
  files+=("$png")
done

if [ "$mode" = check ]; then
  bad=0
  for png in "${files[@]}"; do
    if ! cmp -s "$out/$png" "$dir/$png"; then
      echo "render: $png differs from the committed file" >&2
      bad=1
    fi
  done
  [ "$bad" -eq 0 ] || exit 1
  echo "render: ${#files[@]} PNGs are byte-identical to a fresh render"
else
  echo "render: wrote ${#files[@]} PNGs"
fi
