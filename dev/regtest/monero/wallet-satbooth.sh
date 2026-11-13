#!/bin/sh
# Entrypoint of wallet-satbooth (dev/regtest only): SatBooth's view-only wallet in monero-wallet-rpc.
# TestMoneroSetup writes /state/monero/view.json (the merchant's address and view key, no spend key) and
# /state/monero/wallet.pass into the state volume; nothing of them is committed. The wallet is generated from the JSON
# only when /data/satbooth-view does not exist yet: a start after the kill phase reopens the (restored) wallet file.
# The new wallet's password is the JSON's "password" member (wallet2::make_from_json gives every generate call the JSON's
# field, src/wallet/wallet2.cpp at v0.18.5.4); wallet.pass holds the same text for wallet-rpc's --password-file.
# The Digest login comes from RPC_LOGIN in the environment (src/rpc/rpc_args.cpp:226-228 at v0.18.5.4), never argv.
set -eu

view=/state/monero/view.json
pass=/state/monero/wallet.pass
wallet=/data/satbooth-view
daemon=172.30.78.3:18081

echo "wallet-satbooth: waiting for $view"
until [ -r "$view" ] && [ -r "$pass" ]; do
  sleep 2
done

if [ ! -e "$wallet" ]; then
  echo "wallet-satbooth: generating the view-only wallet from $view"
  monero-wallet-cli --generate-from-json "$view" --password-file "$pass" --daemon-address "$daemon" \
    --trusted-daemon --allow-mismatched-daemon-version --log-file /tmp/monero-wallet-cli.log --command exit
fi

exec monero-wallet-rpc --wallet-file "$wallet" --password-file "$pass" --daemon-address "$daemon" --trusted-daemon \
  --rpc-bind-ip 172.30.78.5 --rpc-bind-port 18083 --confirm-external-bind --allow-mismatched-daemon-version \
  --non-interactive --log-file /tmp/monero-wallet-rpc.log
