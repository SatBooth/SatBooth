#!/bin/sh
# SatBooth hub host: what the hub's network namespace may send out. Run as root: sh <this file>
# The namespace (hub, relay, UI proxy) has the address 172.30.78.10. It may reach the bundle's own two networks, answer
# connections made to it, and open TCP connections to the Lightning peers in the chain SATBOOTH_HUB_PEERS, and nothing else:
# everything else it sends is logged and dropped. bitcoind is another container and is not bound. DNS is not covered here:
# Docker's resolver sends its queries from the host, not from the namespace; the bundle's netns service sets dns: for that.
# What leaves with an address on the electrum network (172.30.78.128/25) for anywhere outside it is dropped too.
# Add a peer BEFORE connecting to it or opening a channel with it, and remove it when it is gone:
#   iptables-legacy -w -A SATBOOTH_HUB_PEERS -d 203.0.113.7 -p tcp --dport 9735 -j ACCEPT
#   iptables-legacy -w -D SATBOOTH_HUB_PEERS -d 203.0.113.7 -p tcp --dport 9735 -j ACCEPT
# List them: iptables-legacy -w -S SATBOOTH_HUB_PEERS
set -eu
IPT=${IPT:-iptables-legacy}
"$IPT" -w -N SATBOOTH_HUB_PEERS 2>/dev/null || true
"$IPT" -w -C SATBOOTH_HUB_PEERS -d 203.0.113.5 -p tcp --dport 10009 -j ACCEPT 2>/dev/null || "$IPT" -w -A SATBOOTH_HUB_PEERS -d 203.0.113.5 -p tcp --dport 10009 -j ACCEPT
"${IPT}-restore" --noflush -w <<'RULES'
*filter
:SATBOOTH_HUB_EGRESS - [0:0]
-F SATBOOTH_HUB_EGRESS
-A SATBOOTH_HUB_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
-A SATBOOTH_HUB_EGRESS -d 172.30.78.0/25 -j RETURN
-A SATBOOTH_HUB_EGRESS -d 172.30.78.128/25 -j RETURN
-A SATBOOTH_HUB_EGRESS -p tcp -j SATBOOTH_HUB_PEERS
-A SATBOOTH_HUB_EGRESS -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "satbooth-hub-egress: "
-A SATBOOTH_HUB_EGRESS -j DROP
:SATBOOTH_HUB_ELECTRUM - [0:0]
-F SATBOOTH_HUB_ELECTRUM
-A SATBOOTH_HUB_ELECTRUM -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
-A SATBOOTH_HUB_ELECTRUM -d 172.30.78.128/25 -j RETURN
-A SATBOOTH_HUB_ELECTRUM -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "satbooth-hub-egress: "
-A SATBOOTH_HUB_ELECTRUM -j DROP
COMMIT
RULES
"$IPT" -w -C DOCKER-USER -s 172.30.78.10 -j SATBOOTH_HUB_EGRESS 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s 172.30.78.10 -j SATBOOTH_HUB_EGRESS
"$IPT" -w -C DOCKER-USER -s 172.30.78.128/25 -j SATBOOTH_HUB_ELECTRUM 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s 172.30.78.128/25 -j SATBOOTH_HUB_ELECTRUM
