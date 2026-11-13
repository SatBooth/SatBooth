#!/usr/bin/env bash
# Usage: scripts/image-check.sh <image> <allowed-file>...
# Exports the image, lists every entry of every layer and fails on any entry that is not an allowed file or a
# parent directory of one. Only regular files and directories are allowed: a symlink, hard link, device, FIFO or
# whiteout fails. Each allowed path must be absolute. Prints entry names only, never file contents.
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: image-check.sh <image> <allowed-file>..." >&2
  exit 2
fi
image=$1
shift

for p in "$@"; do
  case "$p" in
    /*/ | / | *//*) echo "image-check: allowed path must be an absolute file path: $p" >&2; exit 2 ;;
    /*) ;;
    *) echo "image-check: allowed path must be absolute: $p" >&2; exit 2 ;;
  esac
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

docker image save "$image" -o "$tmp/image.tar"
mkdir "$tmp/x"
tar -xf "$tmp/image.tar" -C "$tmp/x"

# Every entry of every layer named by manifest.json, as "<type><TAB><absolute path>" without a trailing slash.
# The type is the first character of tar's mode string; a hard link is reported as h.
: > "$tmp/entries"
while IFS= read -r layer; do
  tar -tvf "$tmp/x/$layer" | awk '{t = substr($0, 1, 1); if ($0 ~ / link to /) t = "h"; print t}' > "$tmp/types"
  tar -tf "$tmp/x/$layer" | sed -e 's|^\./||' -e 's|/$||' -e 's|^|/|' > "$tmp/names"
  [ "$(wc -l < "$tmp/types")" -eq "$(wc -l < "$tmp/names")" ] || { echo "image-check: FAIL: cannot read a layer of $image" >&2; exit 1; }
  paste "$tmp/types" "$tmp/names" | grep -vE '	/\.?$' >> "$tmp/entries" || true
done < <(sed 's/.*"Layers"://' "$tmp/x/manifest.json" | grep -oE '(blobs/sha256/[0-9a-f]{64}|[0-9a-f]{64}/layer\.tar)')
[ -s "$tmp/entries" ] || { echo "image-check: FAIL: no layers found in $image" >&2; exit 1; }

# Allowed: the files, and every parent directory of them.
: > "$tmp/dirs"
for p in "$@"; do
  d=${p%/*}
  while [ -n "$d" ]; do
    printf '%s\n' "$d" >> "$tmp/dirs"
    d=${d%/*}
  done
done
printf '%s\n' "$@" > "$tmp/files"

sort -u "$tmp/entries" > "$tmp/entries.sorted"
echo "image-check: $image holds:"
sed 's/^\(.\)	/  \1 /' "$tmp/entries.sorted"

# Prints each entry that is not allowed with its type, and each allowed file that is absent.
bad=$(awk -F'\t' -v files="$tmp/files" -v dirs="$tmp/dirs" '
  BEGIN {
    while ((getline l < files) > 0) f[l] = 1
    while ((getline l < dirs) > 0) d[l] = 1
  }
  { seen[$2] = 1 }
  ($2 in f) && $1 == "-" { next }
  ($2 in d) && $1 == "d" { next }
  { print "  " $1 " " $2 " (not allowed)" }
  END { for (p in f) if (!(p in seen)) print "  - " p " (missing)" }
' "$tmp/entries.sorted" | sort)
if [ -n "$bad" ]; then
  echo "image-check: FAIL: $image:" >&2
  printf '%s\n' "$bad" >&2
  exit 1
fi
echo "image-check: ok: $image"
