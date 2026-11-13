#!/usr/bin/env bash
# Offline gate: every check must pass. Run from anywhere.
set -euo pipefail

cd "$(dirname "$0")/.."

export GOFLAGS=-mod=vendor
export GOPROXY=off CGO_ENABLED=0

fail() { echo "check.sh: FAIL: $*" >&2; exit 1; }

echo "== gofmt"
unformatted=$(find . -name '*.go' -not -path './vendor/*' -not -path './.worktrees/*' -not -path './.tools/*' -print0 \
  | xargs -0 -r gofmt -l)
if [ -n "$unformatted" ]; then
  echo "$unformatted" >&2
  fail "gofmt: files above are not formatted"
fi

echo "== go vet"
go vet ./... || fail "go vet"

echo "== vendor consistency"
# This is the one step that may use the network: re-vendoring needs the original modules. Offline by
# default; CI sets GOPROXY_VENDOR=https://proxy.golang.org (the checksum database stays on).
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -d vendor ] || grep -q '^require' go.mod; then
  # A gate must not rewrite what it checks: keep copies and compare after vendoring.
  cp go.mod "$tmp/go.mod.orig"
  cp go.sum "$tmp/go.sum.orig"
  # Checksum verification stays on whatever the caller's environment says.
  env -u GONOSUMDB -u GONOSUMCHECK -u GOINSECURE -u GOPRIVATE -u GONOPROXY \
    GOSUMDB=sum.golang.org GOPROXY="${GOPROXY_VENDOR:-off}" GOFLAGS=-mod=mod \
    go mod vendor -o "$tmp/vendor" || fail "go mod vendor"
  diff -r vendor "$tmp/vendor" || fail "vendor/ does not match go.mod and go.sum"
  cmp go.mod "$tmp/go.mod.orig" || fail "go.mod changed while vendoring"
  cmp go.sum "$tmp/go.sum.orig" || fail "go.sum changed while vendoring"
fi

echo "== go mod verify"
# Needs the module cache, so it uses the same proxy setting as the vendor step (a cold CI cache fills here).
env -u GONOSUMDB -u GONOSUMCHECK -u GOINSECURE -u GOPRIVATE -u GONOPROXY \
  GOSUMDB=sum.golang.org GOPROXY="${GOPROXY_VENDOR:-off}" go mod verify || fail "go mod verify"

echo "== dependency allowlist"
# Every shipped target is checked, tests included, so a platform-specific or test-only import cannot
# bypass the list. golang.org/x/sys/cpu is deliberately unlisted: no shipped target builds it.
module=$(go list -m)
bad=0
targets=(linux/amd64 linux/arm64 darwin/arm64)
for target in "${targets[@]}"; do
  goos=${target%/*}
  goarch=${target#*/}
  deps=$(GOOS=$goos GOARCH=$goarch go list -deps -test -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./...) \
    || fail "go list -deps for $target"
  while IFS= read -r pkg; do
    [ -z "$pkg" ] && continue
    # Test variants print as "pkg [pkg.test]"; strip the suffix.
    pkg=${pkg%% \[*}
    case "$pkg" in
      "$module"|"$module"/*) continue ;;
    esac
    if ! grep -qxF -- "$pkg" scripts/deps-allowlist.txt; then
      echo "not on scripts/deps-allowlist.txt ($target): $pkg" >&2
      bad=1
    fi
  done <<< "$deps"
done
[ "$bad" -eq 0 ] || fail "dependency allowlist"

echo "== go build"
go build -trimpath ./... || fail "go build"

echo "== go test"
go test ./... || fail "go test"

echo "== release pins"
scripts/release-pins.sh check-format || fail "release pins: the three pin sites do not hold well-formed refs of one version"
scripts/release-pins_test.sh || fail "release pins: scripts/release-pins_test.sh"
scripts/release-pins_prop_test.sh || fail "release pins: scripts/release-pins_prop_test.sh"
scripts/release-notes_test.sh || fail "release notes: scripts/release-notes_test.sh"
scripts/setup-image_test.sh || fail "setup wrapper: scripts/setup-image_test.sh"
# Reads tags from a throwaway registry in Docker; skips without Docker, and without the registry image unless CI.
RELEASE_PUSH_TEST_PULL=${CI:+1} scripts/release-push_test.sh || fail "release push: scripts/release-push_test.sh"

echo "== compose bundles"
# Each per-host bundle must parse with its example.env, and must refuse an empty published address (Compose would read
# an empty host as every interface). The variables of example.env are unset first: the shell's environment would
# override the env file. Every profile is on, so optional services are checked too. Needs only the Compose client, no
# daemon. CI has it; a machine without it skips.
if docker compose version > /dev/null 2>&1; then
  for host in hub public; do
    dir=deploy/$host
    vars=()
    while IFS= read -r name; do
      vars+=(-u "$name")
    done < <(grep -oE '^[A-Z0-9_]+' "$dir/example.env")
    env "${vars[@]}" docker compose --profile "*" -f "$dir/compose.yml" --env-file "$dir/example.env" config -q \
      || fail "compose: $dir/compose.yml does not parse with its example.env"
    # Every variable is required, so no single use of an address can fall back to empty: the only interpolation allowed
    # is ${NAME:?…}. Bare $NAME, ${NAME}, ${NAME-…}, ${NAME:-…}, ${NAME+…} and ${NAME?…} (which allows an empty value)
    # are refused; $$ is Compose's literal dollar. Every published port then has a host address once rendered, and
    # never a wildcard one.
    if grep -noE '\$+\{?[A-Za-z_][A-Za-z0-9_]*.?.?' "$dir/compose.yml" \
      | grep -vE ':\$\$[A-Za-z_{]|:\$\{[A-Za-z_][A-Za-z0-9_]*:\?$'; then
      fail "compose: $dir/compose.yml uses a variable without the required form \${VAR:?message}"
    fi
    rendered=$(env "${vars[@]}" docker compose --profile "*" -f "$dir/compose.yml" --env-file "$dir/example.env" config)
    if grep -nE '^ +host_ip: (0\.0\.0\.0|::|\[::\])$' <<< "$rendered"; then
      fail "compose: $dir/compose.yml publishes a port on a wildcard address"
    fi
    published=$(grep -cE '^ +published: ' <<< "$rendered" || true)
    bound=$(grep -cE '^ +host_ip: [0-9A-Fa-f:]' <<< "$rendered" || true)
    [ "$published" -gt 0 ] && [ "$published" -eq "$bound" ] \
      || fail "compose: $dir/compose.yml publishes $published ports, $bound of them on a host address"
    case "$host" in
      hub) addrs=(HUB_PRIVATE_ADDR HUB_ADMIN_ADDR) ;;
      public) addrs=(PUBLIC_ADDR) ;;
    esac
    for addr in "${addrs[@]}"; do
      sed "s/^$addr=.*/$addr=/" "$dir/example.env" > "$tmp/$host-empty.env"
      if env "${vars[@]}" docker compose --profile "*" -f "$dir/compose.yml" --env-file "$tmp/$host-empty.env" config -q \
        > /dev/null 2> "$tmp/$host-empty.err"; then
        fail "compose: $dir/compose.yml accepts an empty $addr"
      fi
      grep -q "required variable $addr is missing a value" "$tmp/$host-empty.err" \
        || fail "compose: $dir/compose.yml refused an empty $addr for another reason"
    done
    # The bundle alone runs SatBooth's images only by their pinned refs; with compose.build.yml no service, the one-off
    # ones included, names a release image from the registry.
    base_images=$(env "${vars[@]}" docker compose --profile "*" -f "$dir/compose.yml" --env-file "$dir/example.env" \
      config --images) || fail "compose: $dir/compose.yml: config --images"
    if grep -E '^ghcr\.io/satbooth/' <<< "$base_images" | grep -vE '@sha256:[0-9a-f]{64}$'; then
      fail "compose: $dir/compose.yml names a SatBooth image without its digest"
    fi
    grep -q '^ghcr\.io/satbooth/' <<< "$base_images" || fail "compose: $dir/compose.yml pins no SatBooth image"
    build_images=$(env "${vars[@]}" docker compose --profile "*" -f "$dir/compose.yml" -f "$dir/compose.build.yml" \
      --env-file "$dir/example.env" config --images) \
      || fail "compose: $dir/compose.yml with $dir/compose.build.yml does not parse with its example.env"
    if grep '^ghcr\.io/satbooth/' <<< "$build_images"; then
      fail "compose: $dir/compose.build.yml leaves a service on a release image from the registry"
    fi
  done
elif [ -n "${CI-}" ]; then
  fail "compose: docker compose is not available, and CI must check the bundles"
else
  echo "check.sh: docker compose not found: skipping the bundle check (CI runs it)"
fi

echo "== hub bitcoind whitelist"
# The bundle's rpcwhitelist=hub: line is never run by qualify: it must equal the regtest one and the documented list.
scripts/hub-whitelist-check.sh || fail "hub bitcoind whitelist"

echo "== capabilities"
# Offline, but needs the pinned capslock from scripts/tools.sh (run it once).
scripts/capslock.sh || fail "capslock"

echo "check.sh: OK"
