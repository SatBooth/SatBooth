#!/usr/bin/env bash
# Applies the printed egress rules (the golden files of setup/testdata/lightning-only, which use the default addresses)
# to a router in a privileged Linux container, with four network namespaces behind it: the hub's (172.30.78.10),
# bitcoind (172.30.78.20), the stub (172.30.78.254) and "the internet" (203.0.113.5), and checks which connections
# succeed, first with no rules, then with nftables, then with iptables-legacy. Needs Docker with privileged containers
# on Linux (Docker Desktop's VM works) and the network, to install the tools in the container.
# Usage: scripts/egress-rules-test.sh
set -euo pipefail

if [ "${1:-}" != inside ]; then
  cd "$(dirname "$0")/.."
  rules=$(mktemp -d)
  trap 'rm -rf "$rules"' EXIT
  cp setup/testdata/lightning-only/egress.nft setup/testdata/lightning-only/egress.sh "$rules/"
  exec docker run --rm --privileged -v "$rules:/rules:ro" -v "$PWD/scripts/egress-rules-test.sh:/test.sh:ro" \
    debian:13-slim bash /test.sh inside
fi

set +e
apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq iproute2 nftables iptables netcat-openbsd procps >/dev/null 2>&1 || { echo "apt failed"; exit 1; }
sysctl -qw net.ipv4.ip_forward=1
mk() { # name ns-addr router-addr route-dst
  ip netns add $1; ip link add r$1 type veth peer name v$1; ip link set v$1 netns $1
  ip addr add $3/32 dev r$1; ip link set r$1 up
  ip netns exec $1 ip link set lo up; ip netns exec $1 ip link set v$1 up
  ip netns exec $1 ip addr add $2/32 dev v$1
  ip netns exec $1 ip route add $3 dev v$1; ip netns exec $1 ip route add default via $3
  ip route add $2/32 dev r$1
  ip netns exec $1 sysctl -qw net.ipv4.conf.all.rp_filter=0
  sysctl -qw net.ipv4.conf.r$1.rp_filter=0 net.ipv4.conf.all.rp_filter=0
}
mk hub 172.30.78.10 172.30.78.1      # the hub's namespace (HUB_IP)
mk btc 172.30.78.20 172.30.78.2      # bitcoind, hub network
mk stub 172.30.78.254 172.30.78.129  # the stub, electrum network
mk net 203.0.113.5 203.0.113.1       # "the internet"
for ns in btc stub net; do for p in 443 9735 53 8332 8080 50001; do (ip netns exec $ns nc -l -k -p $p >/dev/null 2>&1 &) ; done; done
(ip netns exec hub nc -l -k -p 8080 >/dev/null 2>&1 &)
sleep 1
probe() { # label ns dst port expect
  if ip netns exec $2 nc -z -w 2 $3 $4 2>/dev/null; then got=connects; else got=blocked; fi
  [ $got = $5 ] && r=ok || r=WRONG
  printf '%-6s %-62s %-9s (want %s)\n' $r "$1" $got $5
  [ $r = ok ] || FAILED=1
}
FAILED=0
suite() {
  echo "--- $1"
  probe "hub -> internet 9735 (Lightning)" hub 203.0.113.5 9735 connects
  probe "hub -> internet 443" hub 203.0.113.5 443 blocked
  probe "hub -> internet tcp 53" hub 203.0.113.5 53 blocked
  probe "hub -> stub 172.30.78.254:443 (electrum network)" hub 172.30.78.254 443 connects
  probe "hub -> bitcoind 172.30.78.20:8332 (hub network)" hub 172.30.78.20 8332 connects
  probe "internet -> hub 8080 (published port, reply path)" net 172.30.78.10 8080 connects
  probe "bitcoind -> internet 443 (not bound)" btc 203.0.113.5 443 connects
  probe "bitcoind -> internet 8080 (not bound either)" btc 203.0.113.5 8080 connects
}
echo "=== no rules: the baseline connects everywhere (the WRONG lines below are expected)"; suite "baseline"
FAILED=0
nft -f /rules/egress.nft || { echo nft load failed; exit 1; }
suite "nftables"
nft list chain inet satbooth_hub egress | grep counter
nft -f /rules/egress.nft && echo "nft reload ok (idempotent)"; nft list tables
nft delete table inet satbooth_hub
echo "=== iptables-legacy"
update-alternatives --set iptables /usr/sbin/iptables-legacy >/dev/null 2>&1
iptables-legacy -N DOCKER-USER; iptables-legacy -A DOCKER-USER -j RETURN; iptables-legacy -A FORWARD -j DOCKER-USER
IPT=iptables-legacy sh /rules/egress.sh || { echo legacy load failed; exit 1; }
IPT=iptables-legacy sh /rules/egress.sh && echo "script rerun ok"
echo "jump count in DOCKER-USER: $(iptables-legacy -S DOCKER-USER | grep -c SATBOOTH_HUB_EGRESS) (want 1)"
suite "iptables-legacy"
iptables-legacy -L SATBOOTH_HUB_EGRESS -n -v | tail -3
exit $FAILED
