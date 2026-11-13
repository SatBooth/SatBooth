#!/usr/bin/env bash
# The hub's bitcoind whitelist cannot drift: qualify runs the regtest file's line, never the hub bundle's. Each file is
# read as Bitcoin Core 32.1 reads it (src/common/config.cpp GetConfigOptions: "#" starts a comment, key and value are
# trimmed, a "[name]" header prefixes the keys after it with "name."; src/common/args.cpp InterpretKey: the part before
# the first "." is the network, a leading "no" negates; doc/bitcoin-conf.md "Network specific options"). Required of
# dev/regtest/bitcoind/bitcoin.conf and deploy/hub/config-example/bitcoin.conf:
# - the active network is set once, before any section (chain=<name>, or regtest=1, testnet=1, testnet4=1 or
#   signet=1), else main (args.cpp GetChainTypeString);
# - every rpcwhitelist and rpcwhitelistdefault line is before any section or under the active network's header:
#   Core reads only those two places for this network (common/settings.cpp GetSettingsList) and ignores the rest, so a
#   line elsewhere is a line that does nothing;
# - none is written with a network prefix (main.rpcwhitelist=...) or negated (norpcwhitelist=...): a prefixed copy is
#   one more rule for the same user, which Core intersects with the first (src/httprpc.cpp:315-332), and a negation
#   clears every list;
# - exactly one rpcwhitelist rule names the user hub, the same list in both files, and that list is the one
#   docs/dependencies.md §8 states (its one indented line starting with getblockchaininfo);
# - exactly one rpcwhitelistdefault line, =0. The hub's own list is checked whatever the default (httprpc.cpp:107-118),
#   but with the default on (=1, or the line gone: it defaults to on once any rpcwhitelist is set, httprpc.cpp:314) every
#   user without a list is refused every call: the regtest helpers' user, and bitcoin-cli's cookie in the bundle;
# - no includeconf line, which could add rules this check does not read.
# Usage: scripts/hub-whitelist-check.sh [repository root]; called by scripts/check.sh.
set -euo pipefail

root=${1:-"$(dirname "$0")/.."}
fail() { echo "hub-whitelist-check.sh: FAIL: $*" >&2; exit 1; }

# parse <file>: one line per option, its fields separated by the unit separator (0x1f, not white space, so that an
# empty field is kept by read): line number, the key as written, the network ("" before any
# section), 1 if negated, the option name, the value. Comments, blank lines and section headers produce nothing.
parse() {
  awk -v OFS=$'\037' '
    function trim(s) { sub(/^[ \t\r\n]+/, "", s); sub(/[ \t\r\n]+$/, "", s); return s }
    {
      s = $0
      i = index(s, "#"); if (i > 0) s = substr(s, 1, i - 1)
      s = trim(s)
      if (s == "") next
      if (s ~ /^\[.*\]$/) { prefix = substr(s, 2, length(s) - 2) "."; next }
      i = index(s, "=")
      if (i == 0) { print FNR, s, "", 0, "!unparsed", ""; next }
      key = trim(substr(s, 1, i - 1)); val = trim(substr(s, i + 1))
      full = prefix key; net = ""; name = full
      j = index(full, "."); if (j > 0) { net = substr(full, 1, j - 1); name = substr(full, j + 1) }
      neg = 0; if (substr(name, 1, 2) == "no") { neg = 1; name = substr(name, 3) }
      print FNR, key, net, neg, name, val
    }' "$1"
}

hub_lists=()
for f in "$root/dev/regtest/bitcoind/bitcoin.conf" "$root/deploy/hub/config-example/bitcoin.conf"; do
  rel=${f#"$root"/}
  opts=$(parse "$f")

  # The active network.
  active="" selectors=0
  while IFS=$'\037' read -r ln key net neg name val; do
    [ "$name" = "!unparsed" ] && fail "$rel:$ln: a line Bitcoin Core would refuse: $key"
    case "$name" in
      chain | regtest | testnet | testnet4 | signet) ;;
      *) continue ;;
    esac
    [ -z "$net" ] || fail "$rel:$ln: $key is set inside a section or with a prefix"
    [ "$neg" = 0 ] || fail "$rel:$ln: $key is negated"
    case "$name=$val" in
      chain=main | chain=test | chain=testnet4 | chain=signet | chain=regtest) active=${val} ;;
      regtest=1) active=regtest ;;
      testnet=1) active=test ;;
      testnet4=1) active=testnet4 ;;
      signet=1) active=signet ;;
      *) fail "$rel:$ln: $key=$val is not a network this check knows" ;;
    esac
    selectors=$((selectors + 1))
  done <<< "$opts"
  [ "$selectors" -le 1 ] || fail "$rel: $selectors network settings, want at most 1"
  active=${active:-main}

  hub=() defaults=()
  while IFS=$'\037' read -r ln key net neg name val; do
    case "$name" in
      rpcwhitelist | rpcwhitelistdefault) ;;
      includeconf) fail "$rel:$ln: includeconf could add rules this check does not read" ;;
      *) continue ;;
    esac
    [ "${key#*.}" = "$key" ] || fail "$rel:$ln: $key is written with a network prefix"
    [ "$neg" = 0 ] || fail "$rel:$ln: $key is negated"
    [ -z "$net" ] || [ "$net" = "$active" ] \
      || fail "$rel:$ln: $key is under [$net], which bitcoind ignores on $active"
    if [ "$name" = rpcwhitelistdefault ]; then
      defaults+=("$val")
    elif [ "${val%%:*}" = hub ]; then
      hub+=("$val")
    fi
  done <<< "$opts"
  [ "${#hub[@]}" -eq 1 ] || fail "$rel: ${#hub[@]} rpcwhitelist rules for the user hub, want exactly 1"
  [ "${#defaults[@]}" -eq 1 ] && [ "${defaults[0]}" = 0 ] \
    || fail "$rel: rpcwhitelistdefault must be set once, to 0 (found: ${defaults[*]:-none})"
  hub_lists+=("${hub[0]#hub:}")
done
[ "${hub_lists[0]}" = "${hub_lists[1]}" ] || fail "rpcwhitelist=hub: differs between dev/regtest and deploy/hub"

doc=$root/docs/dependencies.md
n=$(grep -c '^    getblockchaininfo,' "$doc" || true)
[ "$n" -eq 1 ] || fail "docs/dependencies.md: $n indented method lists, want exactly 1 (§8)"
listed=$(grep '^    getblockchaininfo,' "$doc")
[ "${listed#    }" = "${hub_lists[0]}" ] || fail "rpcwhitelist=hub: is not the method list of docs/dependencies.md §8"
echo "hub-whitelist-check.sh: OK"
