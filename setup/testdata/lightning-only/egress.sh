#!/bin/sh
# SatBooth hub host: what the hub's network namespace may send out. Run as root: sh <this file>
# The namespace (hub, relay, UI proxy) has the address 172.30.78.10. It may reach the bundle's own two networks, answer
# connections made to it, and open TCP connections to the Lightning peers in the chain SATBOOTH_HUB_PEERS, and nothing else:
# everything else, outside DNS included, is logged and dropped. bitcoind is another container and is not bound.
# Add a peer BEFORE connecting to it or opening a channel with it, and remove it when it is gone:
#   iptables-legacy -w -A SATBOOTH_HUB_PEERS -d 203.0.113.7 -p tcp --dport 9735 -j ACCEPT
#   iptables-legacy -w -D SATBOOTH_HUB_PEERS -d 203.0.113.7 -p tcp --dport 9735 -j ACCEPT
# List them: iptables-legacy -w -S SATBOOTH_HUB_PEERS
set -eu
IPT=${IPT:-iptables-legacy}
"$IPT" -w -N SATBOOTH_HUB_PEERS 2>/dev/null || true
"$IPT" -w -N SATBOOTH_HUB_EGRESS 2>/dev/null || "$IPT" -w -F SATBOOTH_HUB_EGRESS
"$IPT" -w -A SATBOOTH_HUB_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -d 172.30.78.0/25 -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -d 172.30.78.128/25 -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -p tcp -j SATBOOTH_HUB_PEERS
"$IPT" -w -A SATBOOTH_HUB_EGRESS -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "satbooth-hub-egress: "
"$IPT" -w -A SATBOOTH_HUB_EGRESS -j DROP
"$IPT" -w -C DOCKER-USER -s 172.30.78.10 -j SATBOOTH_HUB_EGRESS 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s 172.30.78.10 -j SATBOOTH_HUB_EGRESS
