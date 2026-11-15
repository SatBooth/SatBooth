#!/usr/bin/env bash
# Applies the printed egress rules (the golden files egress-several of setup/testdata/lightning-only: the default
# addresses and the peers 203.0.113.5:10009, 198.51.100.7:9735 and 198.51.100.8:9736; the hub's namespace has a
# second address, 172.30.78.130, on the electrum network)
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
  cp setup/testdata/lightning-only/egress-several.nft setup/testdata/lightning-only/egress-several.sh "$rules/"
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
# The hub's namespace also holds an address of the electrum network (ELECTRUM_SUBNET 172.30.78.128/25).
ip netns exec hub ip addr add 172.30.78.130/32 dev vhub
ip route add 172.30.78.130/32 dev rhub
for ns in btc stub net; do for p in 443 9735 10009 53 8332 8080 50001; do (ip netns exec $ns nc -l -k -p $p >/dev/null 2>&1 &) ; done; done
(ip netns exec hub nc -l -k -p 8080 >/dev/null 2>&1 &)
# Listeners on the router itself: its addresses on the hub's and the electrum network's side are "the host".
(nc -l -k -p 4446 >/dev/null 2>&1 &)
sleep 1
probe() { # label ns dst port expect [source address]; ns "-" is the router itself (the host)
  if [ "$2" = - ]; then run=; else run="ip netns exec $2"; fi
  if $run nc ${6:+-s $6} -z -w 2 $3 $4 2>/dev/null; then got=connects; else got=blocked; fi
  [ $got = $5 ] && r=ok || r=WRONG
  printf '%-6s %-62s %-9s (want %s)\n' $r "$1" $got $5
  [ $r = ok ] || FAILED=1
}
FAILED=0
suite() {
  echo "--- $1"
  probe "hub -> listed peer 203.0.113.5:10009" hub 203.0.113.5 10009 connects
  probe "hub -> 203.0.113.5:9735 (same address, port not listed)" hub 203.0.113.5 9735 blocked
  probe "hub -> internet 443" hub 203.0.113.5 443 blocked
  probe "hub -> internet tcp 53" hub 203.0.113.5 53 blocked
  probe "hub -> stub 172.30.78.254:443 (electrum network)" hub 172.30.78.254 443 connects
  probe "hub -> bitcoind 172.30.78.20:8332 (hub network)" hub 172.30.78.20 8332 connects
  probe "internet -> hub 8080 (published port, reply path)" net 172.30.78.10 8080 connects
  probe "hub, electrum-network source -> internet 443" hub 203.0.113.5 443 blocked 172.30.78.130
  probe "hub, electrum-network source -> listed peer 203.0.113.5:10009 (not for this source)" hub 203.0.113.5 10009 blocked 172.30.78.130
  probe "hub, electrum-network source -> stub 172.30.78.254:443" hub 172.30.78.254 443 connects 172.30.78.130
  probe "hub -> the host itself 172.30.78.1:4446 (input path, new connection)" hub 172.30.78.1 4446 blocked
  probe "hub, electrum-network source -> the host itself 172.30.78.1:4446" hub 172.30.78.1 4446 blocked 172.30.78.130
  probe "host -> hub 172.30.78.10:8080 (connection made to the namespace, replies pass the input path)" - 172.30.78.10 8080 connects
  probe "bitcoind -> the host itself 172.30.78.1:4446 (not bound)" btc 172.30.78.1 4446 connects
  probe "bitcoind -> internet 443 (not bound)" btc 203.0.113.5 443 connects
  probe "bitcoind -> internet 8080 (not bound either)" btc 203.0.113.5 8080 connects
}
echo "=== no rules: the baseline connects everywhere (the WRONG lines below are expected: 9735, 443 and 53 are not blocked)"; suite "baseline"
FAILED=0
nft -f /rules/egress-several.nft || { echo nft load failed; exit 1; }
suite "nftables"
nft list chain inet satbooth_hub egress | grep counter
echo "--- nftables: add and remove a peer at run time"
nft add element inet satbooth_hub satbooth_hub_peers { 203.0.113.5 . 9735 }
probe "after add element: hub -> 203.0.113.5:9735" hub 203.0.113.5 9735 connects
nft -f /rules/egress-several.nft
probe "after reloading the file: the added peer is kept" hub 203.0.113.5 9735 connects
nft delete element inet satbooth_hub satbooth_hub_peers { 203.0.113.5 . 9735 }
probe "after delete element: hub -> 203.0.113.5:9735" hub 203.0.113.5 9735 blocked
nft add element inet satbooth_hub satbooth_hub_peers { 203.0.113.5 . 9735 }
nft list table inet satbooth_hub > /saved.nft
nft delete table inet satbooth_hub
nft -f /saved.nft
probe "after saving the listing, deleting the table and loading the listing" hub 203.0.113.5 9735 connects
probe "  ... and an unlisted port stays blocked" hub 203.0.113.5 443 blocked
nft delete table inet satbooth_hub; nft -f /rules/egress-several.nft
nft -f /rules/egress-several.nft && echo "nft reload ok (idempotent)"; nft list tables
nft delete table inet satbooth_hub
echo "=== iptables-legacy"
update-alternatives --set iptables /usr/sbin/iptables-legacy >/dev/null 2>&1
iptables-legacy -N DOCKER-USER; iptables-legacy -A DOCKER-USER -j RETURN; iptables-legacy -A FORWARD -j DOCKER-USER
IPT=iptables-legacy sh /rules/egress-several.sh || { echo legacy load failed; exit 1; }
IPT=iptables-legacy sh /rules/egress-several.sh && echo "script rerun ok"
echo "jump count in DOCKER-USER: $(iptables-legacy -S DOCKER-USER | grep -c SATBOOTH_HUB_EGRESS) (want 1), electrum: $(iptables-legacy -S DOCKER-USER | grep -c SATBOOTH_HUB_ELECTRUM) (want 1)"
suite "iptables-legacy"
echo "--- iptables-legacy: add and remove a peer at run time"
iptables-legacy -w -A SATBOOTH_HUB_PEERS -d 203.0.113.5 -p tcp --dport 9735 -j ACCEPT
probe "after -A: hub -> 203.0.113.5:9735" hub 203.0.113.5 9735 connects
IPT=iptables-legacy sh /rules/egress-several.sh
probe "after rerunning the script: the added peer is kept" hub 203.0.113.5 9735 connects
iptables-legacy -w -D SATBOOTH_HUB_PEERS -d 203.0.113.5 -p tcp --dport 9735 -j ACCEPT
probe "after -D: hub -> 203.0.113.5:9735" hub 203.0.113.5 9735 blocked
echo "peer rules after the rerun: $(iptables-legacy -S SATBOOTH_HUB_PEERS | grep -c -- '-A') (want 3)"
iptables-legacy -L SATBOOTH_HUB_EGRESS -n -v | tail -3
echo "--- iptables-legacy: rerunning the script never opens the chain (UDP datagrams to the internet, sent without a pause while the script runs 40 times, must not arrive)"
(ip netns exec net nc -u -l -k -p 4444 > /leak 2>&1 &)
sleep 1
: > /leak
(ip netns exec hub bash -c 'for i in $(seq 1 6000); do echo x > /dev/udp/203.0.113.5/4444; done' 2>/dev/null) &
sender=$!
for i in $(seq 1 40); do IPT=iptables-legacy sh /rules/egress-several.sh; done
wait $sender
leaked=$(wc -c < /leak)
[ "$leaked" -eq 0 ] && echo "ok     no datagram arrived during the reruns" || { echo "WRONG  $leaked bytes arrived during the reruns"; FAILED=1; }
exit $FAILED
