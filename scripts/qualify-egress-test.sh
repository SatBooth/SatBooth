#!/usr/bin/env bash
# Tests of the pure parsers of scripts/qualify-egress.sh on canned recorder output (no Docker, no root).
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=scripts/qualify-egress.sh
. scripts/qualify-egress.sh

tmpd=$(mktemp -d)
trap 'rm -rf "${tmpd:?}"' EXIT
fail() { echo "qualify-egress-test: FAIL: $*" >&2; exit 1; }
want() { [ "$2" = "$3" ] || fail "$1: got '$2', want '$3'"; }

# tcpdump -nn -e lines: TCP with a port, UDP, ICMP, ARP (ignored), IPv6 (ignored).
got=$(egress_flows << 'EOF2'
10:00:00.000001 02:42:ac:1e:4f:0a > 02:42:11:22:33:44, ethertype IPv4 (0x0800), length 74: 172.30.79.10.41234 > 203.0.113.9.4443: Flags [S], seq 1, win 64240, length 0
10:00:00.000002 02:42:ac:1e:4f:0a > 02:42:11:22:33:44, ethertype IPv4 (0x0800), length 47: 172.30.79.10.5555 > 203.0.113.9.4444: UDP, length 5
10:00:00.000003 02:42:ac:1e:4f:0a > 02:42:11:22:33:44, ethertype IPv4 (0x0800), length 98: 172.30.79.10 > 1.1.1.1: ICMP echo request, id 1, seq 1, length 64
10:00:00.000004 02:42:ac:1e:4f:0a > ff:ff:ff:ff:ff:ff, ethertype ARP (0x0806), length 42: Request who-has 172.30.79.1 tell 172.30.79.10, length 28
10:00:00.000005 02:42:ac:1e:4f:0a > 33:33:00:00:00:16, ethertype IPv6 (0x86dd), length 90: fe80::1 > ff02::16: ICMP6, length 36
EOF2
)
want flows "$got" "02:42:ac:1e:4f:0a tcp 172.30.79.10 41234 203.0.113.9 4443
02:42:ac:1e:4f:0a udp 172.30.79.10 5555 203.0.113.9 4444
02:42:ac:1e:4f:0a icmp 172.30.79.10 - 1.1.1.1 -"

# conntrack -E lines: tcp, udp, icmp; the original direction is the first src/dst.
got=$(egress_conntrack << 'EOF2'
[1700000000.123456]	    [NEW] ipv4     2 tcp      6 120 SYN_SENT src=172.30.79.10 dst=203.0.113.9 sport=40000 dport=4440 [UNREPLIED] src=203.0.113.9 dst=172.30.79.10 sport=4440 dport=40000
[1700000001.000000]	    [NEW] ipv4     2 udp      17 30 src=172.30.79.10 dst=10.99.0.2 sport=5000 dport=53 [UNREPLIED] src=10.99.0.2 dst=172.30.79.10 sport=53 dport=5000
[1700000002.000000]	    [NEW] ipv4     2 icmp     1 30 src=172.30.79.10 dst=1.1.1.1 type=8 code=0 id=7 [UNREPLIED] src=1.1.1.1 dst=172.30.79.10 type=0 code=0 id=7
EOF2
)
want conntrack "$got" "tcp 172.30.79.10 40000 203.0.113.9 4440
udp 172.30.79.10 5000 10.99.0.2 53
icmp 172.30.79.10 - 1.1.1.1 -"

# Network membership at its edges, and the outside test.
egress_in_net 172.30.79.0 172.30.79.0/25 || fail "first address"
egress_in_net 172.30.79.127 172.30.79.0/25 || fail "last address"
! egress_in_net 172.30.79.128 172.30.79.0/25 || fail "one past the end"
! egress_in_net 172.30.78.255 172.30.79.0/25 || fail "one before the start"
egress_in_net 10.1.2.3 0.0.0.0/0 || fail "everything"
egress_in_net 172.30.79.5 172.30.79.5/32 || fail "a /32"
egress_outside 203.0.113.9 172.30.79.0/25 172.30.79.128/25 172.30.78.0/24 || fail "outside"
! egress_outside 172.30.79.200 172.30.79.0/25 172.30.79.128/25 || fail "inside the second"

# Seeded property: membership agrees with integer arithmetic for random addresses and prefix lengths.
RANDOM=8
for _ in $(seq 1 300); do
  a=$((RANDOM % 256)).$((RANDOM % 256)).$((RANDOM % 256)).$((RANDOM % 256))
  bits=$((RANDOM % 33))
  IFS=. read -r o1 o2 o3 o4 <<< "$a"
  ai=$(((o1 << 24) | (o2 << 16) | (o3 << 8) | o4))
  size=$((1 << (32 - bits)))
  base=$((ai / size * size))
  b1=$(((base >> 24) & 255)).$(((base >> 16) & 255)).$(((base >> 8) & 255)).$((base & 255))
  egress_in_net "$a" "$b1/$bits" || fail "$a is in $b1/$bits"
  if [ "$bits" -gt 0 ]; then
    other=$(((base + size) & 0xffffffff))
    c=$(((other >> 24) & 255)).$(((other >> 16) & 255)).$(((other >> 8) & 255)).$((other & 255))
    ! egress_in_net "$c" "$b1/$bits" || fail "$c is not in $b1/$bits"
  fi
done

# DNS names from tcpdump text: A and AAAA queries, once each.
got=$(egress_dns_names << 'EOF2'
10:00:00.1 IP 172.30.79.10.40000 > 127.0.0.11.53: 1234+ A? getalby.com. (29)
10:00:00.1 IP 172.30.79.10.40000 > 127.0.0.11.53: 1235+ AAAA? getalby.com. (29)
10:00:00.2 IP 127.0.0.11.53 > 172.30.79.10.40000: 1234 1/0/0 A 10.0.0.1 (45)
10:00:00.3 IP 172.30.79.10.40001 > 127.0.0.11.53: 1236+ A? sbdns-1.example.net. (38)
EOF2
)
want dns "$got" "getalby.com
sbdns-1.example.net"
# Question names from a capture of queries to a resolver on a port tcpdump does not decode (a synthetic pcap).
python3 -I - "$tmpd/q.pcap" << 'PY'
import struct, sys
def q(name):
    d = struct.pack('>HHHHHH', 1, 0x0100, 1, 0, 0, 0)
    for l in name.split('.'):
        d += bytes([len(l)]) + l.encode()
    d += b'\0' + struct.pack('>HH', 1, 1)
    udp = struct.pack('>HHHH', 50000, 43853, 8 + len(d), 0) + d
    ip = struct.pack('>BBHHHBBH4s4s', 0x45, 0, 20 + len(udp), 0, 0, 64, 17, 0, bytes([127, 0, 0, 1]), bytes([127, 0, 0, 11]))
    return b'\0' * 12 + b'\x08\x00' + ip + udp
out = struct.pack('<IHHiIII', 0xa1b2c3d4, 2, 4, 0, 0, 65535, 1)
for n in ['getalby.com', 'Sb-1.example.net', 'getalby.com']:
    p = q(n)
    out += struct.pack('<IIII', 0, 0, len(p), len(p)) + p
open(sys.argv[1], 'wb').write(out)
PY
want pcap_names "$(egress_pcap_names "$tmpd/q.pcap")" "getalby.com
sb-1.example.net"
want pcap_names_empty "$(egress_pcap_names "$tmpd/none.pcap")" ""
echo "qualify-egress-test: ok"
