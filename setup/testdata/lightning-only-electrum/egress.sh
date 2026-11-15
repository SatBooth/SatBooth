#!/bin/sh
# SatBooth hub host: what the hub's network namespace may send out. Run as root: sh <this file>
# The namespace (hub, relay, UI proxy) has the address 172.30.78.10. It may reach the bundle's own two networks, answer
# connections made to it, and open TCP connections to Lightning peers on port(s) 9735. Everything else, outside DNS
# included, is logged and dropped. bitcoind is another container and is not bound.
set -eu
IPT=${IPT:-iptables-legacy}
"$IPT" -w -N SATBOOTH_HUB_EGRESS 2>/dev/null || "$IPT" -w -F SATBOOTH_HUB_EGRESS
"$IPT" -w -A SATBOOTH_HUB_EGRESS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -d 172.30.78.0/25 -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -d 172.30.78.128/25 -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -p tcp -m multiport --dports 9735 -j RETURN
"$IPT" -w -A SATBOOTH_HUB_EGRESS -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "satbooth-hub-egress: "
"$IPT" -w -A SATBOOTH_HUB_EGRESS -j DROP
"$IPT" -w -C DOCKER-USER -s 172.30.78.10 -j SATBOOTH_HUB_EGRESS 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s 172.30.78.10 -j SATBOOTH_HUB_EGRESS
