#!/bin/sh
# Entrypoint of wallet-rpc in the hub bundle (deploy/hub/compose.yml): SatBooth's view-only wallet in monero-wallet-rpc,
# behind an HTTP Digest login. The operator makes the wallet (deploy/README.md) and bind-mounts:
#   /wallet/satbooth-view (and its .keys file)  the view-only wallet, read-write (wallet-rpc stores it);
#   /etc/wallet-rpc/rpc-login                   one line "<user>:<password>", mode 0600, owned 10002 (the user is
#                                               SatBooth's monero.rpc_user);
#   /etc/wallet-rpc/wallet.pass                 the wallet's password, mode 0600, owned 10002.
# The login reaches wallet-rpc through RPC_LOGIN in its environment, which it reads when --rpc-login is absent
# (src/rpc/rpc_args.cpp:226-228 at v0.18.5.4): never argv, never a compose environment: value. It is read with the
# shell's own read, so no other process sees it either. Nothing read from either file is ever printed.
set -eu

login=/etc/wallet-rpc/rpc-login
pass=/etc/wallet-rpc/wallet.pass
wallet=/wallet/satbooth-view

for f in "$login" "$pass"; do
  if [ ! -f "$f" ] || [ ! -r "$f" ]; then
    echo "wallet-rpc: $f is missing or not readable by uid 10002" >&2
    exit 1
  fi
  case "$(stat -c %a "$f")" in
    400 | 600) ;;
    *) echo "wallet-rpc: $f must have mode 0600 (no group or other bits)" >&2; exit 1 ;;
  esac
done
if [ ! -f "$wallet" ] || [ ! -f "$wallet.keys" ]; then
  echo "wallet-rpc: no wallet at $wallet; make the view-only wallet first (deploy/README.md)" >&2
  exit 1
fi

IFS= read -r RPC_LOGIN < "$login" || true
case "$RPC_LOGIN" in
  ?*:?*) ;;
  *) echo "wallet-rpc: $login must hold one line <user>:<password>" >&2; exit 1 ;;
esac
export RPC_LOGIN

# --trusted-daemon: monerod is this bundle's own node, reachable only from wallet-rpc on the monero network, the case
# wallet-rpc's flag is for. The listener binds every address of the container; the bundle publishes it on the private
# address only, and the login guards it. The log goes to the tmpfs at /tmp (the root is read-only).
exec monero-wallet-rpc --wallet-file "$wallet" --password-file "$pass" --daemon-address monerod:18081 --trusted-daemon \
  --rpc-bind-ip 0.0.0.0 --rpc-bind-port 18083 --confirm-external-bind --non-interactive \
  --log-file /tmp/monero-wallet-rpc.log --max-log-file-size 10000000 --max-log-files 2
