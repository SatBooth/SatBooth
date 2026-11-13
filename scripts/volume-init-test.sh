#!/usr/bin/env bash
# Each bundle's volume-init, run twice as Compose runs it on every up, on host directories laid out like an operator's
# (owned by this user, mode 0700, with 0600 dummy files): both runs must exit 0 and leave every directory and file to the
# user that reads it. CI runs it on Linux, where a bind mount keeps real owners; Docker Desktop masks them, so a pass
# there proves less. Usage: scripts/volume-init-test.sh
set -euo pipefail

cd "$(dirname "$0")/.."

root=$(mktemp -d)
export SATBOOTH_ETC="$root/etc" SATBOOTH_BACKUPS="$root/backups"
export HUB_CONFIG_DIR="$root/hub-config" MONERO_WALLET_DIR="$root/hub-wallet"
pub() { docker compose -p satbooth-volume-init-test -f deploy/public/compose.yml --env-file deploy/public/example.env "$@"; }
hub() { docker compose -p satbooth-volume-init-test-hub -f deploy/hub/compose.yml --env-file deploy/hub/example.env "$@"; }

cleanup() {
  # The files now belong to other users: a container with DAC_OVERRIDE empties the directories.
  pub run --rm --no-deps -T --cap-add DAC_OVERRIDE --entrypoint find volume-init /etc/satbooth /backups -mindepth 1 \
    -delete > /dev/null 2>&1 || true
  hub run --rm --no-deps -T --cap-add DAC_OVERRIDE --entrypoint find volume-init /config /wallet -mindepth 1 \
    -delete > /dev/null 2>&1 || true
  pub down -v > /dev/null 2>&1 || true
  hub down -v > /dev/null 2>&1 || true
  rmdir "$SATBOOTH_ETC" "$SATBOOTH_BACKUPS" "$HUB_CONFIG_DIR" "$MONERO_WALLET_DIR" "$root" 2> /dev/null \
    || echo "volume-init-test: could not remove $root" >&2
}
trap cleanup EXIT

# twice <name> <compose function>: two runs of that bundle's volume-init, each must exit 0.
twice() {
  local i
  for i in 1 2; do
    "$2" run --rm --no-deps -T volume-init || { echo "volume-init-test: FAIL: $1 run $i of volume-init failed" >&2; exit 1; }
    echo "volume-init-test: $1 run $i exited 0"
  done
}

# expect <name> <compose function> <want> <paths…>: owner and mode of each path, as volume-init sees it.
expect() {
  local name=$1 dc=$2 want=$3 got
  shift 3
  got=$("$dc" run --rm --no-deps -T --entrypoint stat volume-init -c '%u:%g %a %n' "$@" | tr '\n' ';')
  [ "$got" = "$want" ] || { echo "volume-init-test: FAIL: $name owners and modes after two runs: $got" >&2; exit 1; }
  echo "volume-init-test: $name OK ($got)"
}

mkdir -m 0700 "$SATBOOTH_ETC" "$SATBOOTH_BACKUPS" "$HUB_CONFIG_DIR" "$MONERO_WALLET_DIR" "$HUB_CONFIG_DIR/wallet-rpc"
(
  umask 077
  echo '{}' > "$SATBOOTH_ETC/satbooth.json"
  for f in hub.env bitcoin.conf relay.json fulcrum.conf monerod.conf wallet-rpc/rpc-login wallet-rpc/wallet.pass; do
    echo dummy > "$HUB_CONFIG_DIR/$f"
  done
  echo dummy > "$MONERO_WALLET_DIR/satbooth-view"
)

twice public pub
expect public pub "65532:65532 700 /etc/satbooth;65532:65532 600 /etc/satbooth/satbooth.json;65532:65532 700 /backups;\
65532:65532 700 /data;" /etc/satbooth /etc/satbooth/satbooth.json /backups /data

twice hub hub
expect hub hub "10003:10003 700 /vol/hub;10001:10001 700 /vol/bitcoind;10004:10004 700 /vol/fulcrum;\
10002:10002 700 /vol/monerod;10003:10003 600 /config/hub.env;10001:10001 600 /config/bitcoin.conf;\
65532:65532 600 /config/relay.json;10004:10004 600 /config/fulcrum.conf;10002:10002 600 /config/monerod.conf;\
10002:10002 700 /config/wallet-rpc;10002:10002 600 /config/wallet-rpc/rpc-login;\
10002:10002 600 /config/wallet-rpc/wallet.pass;10002:10002 700 /wallet;10002:10002 600 /wallet/satbooth-view;" \
  /vol/hub /vol/bitcoind /vol/fulcrum /vol/monerod /config/hub.env /config/bitcoin.conf /config/relay.json \
  /config/fulcrum.conf /config/monerod.conf /config/wallet-rpc /config/wallet-rpc/rpc-login \
  /config/wallet-rpc/wallet.pass /wallet /wallet/satbooth-view
