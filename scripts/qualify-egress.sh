#!/usr/bin/env bash
# The egress group of scripts/qualify.sh (sourced by it: it uses its dc, hdc, log and variables). It proves, on a Linux
# host with root, that the hub bundle makes no outside connection except to a listed Lightning peer, and that its web UI
# loads nothing from outside: the printed egress rules are applied for real, every packet of the hub's network is
# recorded from the host's side, the existing Lightning phases run with the rules on, and the logged-in hub pages are
# loaded through the UI proxy in headless Chrome. Without root it prints "not run" and stops: nothing is claimed.
#
# Needs: Linux, Docker, root by sudo without a password (or QUALIFY_ROOT: a command prefix that runs its arguments as
# root in the host's network namespace, for a rehearsal on Docker Desktop: see qualify/REPORT.md), and the tools
# tcpdump, conntrack, nft or iptables, nsenter, ip (installed with apt on the runner).
# The pure parsers at the top are tested by scripts/qualify-egress-test.sh.

# --- parsers (pure: text in, text out) ---------------------------------------------------------------------------

# egress_flows: `tcpdump -nn -e -r <pcap>` on stdin -> one line per IPv4 packet: "<src mac> <proto> <src> <sport> <dst> <dport>"
# (a port is - for a protocol without one).
egress_flows() {
  awk '
    function split_addr(a, out,   n, parts) {
      n = split(a, parts, ".")
      if (n == 5) { out[1] = parts[1] "." parts[2] "." parts[3] "." parts[4]; out[2] = parts[5] }
      else if (n == 4) { out[1] = a; out[2] = "-" }
      else { out[1] = ""; out[2] = "-" }
    }
    / ethertype IPv4 / {
      mac = $2
      i = index($0, "length ")
      rest = substr($0, i)
      sub(/^length [0-9]+: /, "", rest)
      n = split(rest, f, " ")
      if (n < 3 || f[2] != ">") next
      dstf = f[3]; sub(/:$/, "", dstf)
      split_addr(f[1], s); split_addr(dstf, d)
      if (s[1] == "" || d[1] == "") next
      proto = "other"
      if (rest ~ /Flags \[/) proto = "tcp"
      else if (rest ~ / UDP,/) proto = "udp"
      else if (rest ~ / ICMP /) proto = "icmp"
      print mac, proto, s[1], s[2], d[1], d[2]
    }'
}

# egress_conntrack: `conntrack -E` or `-L` lines on stdin -> "<proto> <src> <sport> <dst> <dport>" of the original direction.
egress_conntrack() {
  awk '
    {
      proto = ""; src = ""; dst = ""; sp = "-"; dp = "-"
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^(tcp|udp|icmp)$/ && proto == "") proto = $i
        if ($i ~ /^src=/ && src == "") src = substr($i, 5)
        else if ($i ~ /^dst=/ && dst == "") dst = substr($i, 5)
        else if ($i ~ /^sport=/ && sp == "-") sp = substr($i, 7)
        else if ($i ~ /^dport=/ && dp == "-") dp = substr($i, 7)
      }
      if (proto != "" && src != "" && dst != "") print proto, src, sp, dst, dp
    }'
}

# egress_in_net <ip> <cidr>: the address is inside the IPv4 network.
egress_in_net() {
  awk -v ip="$1" -v net="$2" '
    function toint(a,   p) { split(a, p, "."); return ((p[1] * 256 + p[2]) * 256 + p[3]) * 256 + p[4] }
    BEGIN {
      split(net, c, "/"); bits = c[2] + 0
      size = 2 ^ (32 - bits)
      base = toint(c[1]); base = base - (base % size)
      v = toint(ip)
      exit !(v >= base && v < base + size)
    }'
}

# egress_outside <ip> <cidr>...: the address is in none of the networks.
egress_outside() {
  local ip=$1 n
  shift
  for n in "$@"; do
    if egress_in_net "$ip" "$n"; then return 1; fi
  done
  return 0
}

# egress_dns_names: text of `tcpdump -nn` on stdin -> the names asked (A, AAAA, ... queries), one a line.
egress_dns_names() {
  awk '{ for (i = 1; i < NF; i++) if ($i ~ /^(A|AAAA|HTTPS|PTR|TXT|MX|SRV|NS|CNAME)\?$/) { n = $(i + 1); sub(/\.$/, "", n); print n; break } }' | sort -u
}

# egress_pcap_names <pcap>: the question names of the DNS queries (UDP, IPv4, Ethernet) in the capture, one a line, unique.
egress_pcap_names() {
  python3 -I - "$1" << 'PY'
import struct, sys
names = set()
try:
    data = open(sys.argv[1], 'rb').read()
except OSError:
    data = b''
if len(data) >= 24:
    magic = struct.unpack('<I', data[:4])[0]
    end = '<' if magic in (0xa1b2c3d4, 0xa1b23c4d) else '>'
    pos = 24
    while pos + 16 <= len(data):
        incl = struct.unpack(end + 'I', data[pos + 8:pos + 12])[0]
        pkt = data[pos + 16:pos + 16 + incl]
        pos += 16 + incl
        if len(pkt) < 14 + 20 + 8 + 12 or pkt[12:14] != b'\x08\x00':
            continue
        ihl = (pkt[14] & 15) * 4
        if pkt[14 + 9] != 17:
            continue
        dns = pkt[14 + ihl + 8:]
        if len(dns) < 12 or dns[2] & 0x80 or struct.unpack('>H', dns[4:6])[0] < 1:
            continue
        i, labels = 12, []
        while i < len(dns) and dns[i] != 0 and dns[i] < 64:
            labels.append(dns[i + 1:i + 1 + dns[i]].decode('ascii', 'replace'))
            i += 1 + dns[i]
        if labels:
            names.add('.'.join(labels).lower())
print('\n'.join(sorted(names)))
PY
}

# --- root and the host --------------------------------------------------------------------------------------------

egress_rr() { # <command...>: as root, in the host's network namespace
  if [ -n "${QUALIFY_ROOT:-}" ]; then
    # shellcheck disable=SC2086
    $QUALIFY_ROOT "$@"
  else
    sudo -n "$@"
  fi
}

# egress_bg <name> <stdout file> <command...>: a root command in the background; its pid is in $egress_ev/<name>.pid.
egress_bg() {
  local name=${1:?} out=${2:?} i
  shift 2
  rm -f "${egress_ev:?}/${name:?}.pid"
  egress_rr sh -c 'echo $$ > "$0"; exec "$@"' "$egress_ev/$name.pid" "$@" > "$out" 2>&1 &
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    [ -s "$egress_ev/$name.pid" ] && return 0
    sleep 0.5
  done
  echo "qualify-egress: $name did not start" >&2
  return 1
}

egress_stop() { # <name>
  local pid
  [ -s "${egress_ev:?}/${1:?}.pid" ] || return 0
  pid=$(cat "$egress_ev/$1.pid")
  egress_rr kill -INT "$pid" 2> /dev/null || true
  rm -f "${egress_ev:?}/${1:?}.pid"
}

egress_tools_image=satbooth-egress-tools
# The container image for the peer's namespace, the relay into the merchant's namespace and the probes that join the
# hub's namespace (nc, socat, getent). Built before any rule is applied: the probes themselves are bound by them.
egress_build_tools() {
  docker build -q -t "$egress_tools_image" - << 'DOCKERFILE' > /dev/null
FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends iproute2 netcat-openbsd socat tcpdump conntrack nftables iptables util-linux procps ca-certificates && apt-get clean
DOCKERFILE
}

# egress_probe <kind> <args>: a probe from inside the hub's namespace (a throwaway container that joins it), as the hub's
# source address. tcp <ip> <port> [<source>]: connects within 3 s; udp <ip> <port>: sends one datagram; dns <name>:
# resolves through the namespace's resolver. Prints connects, sent, blocked or resolved/unresolved.
egress_probe() {
  local kind=$1
  shift
  local nid
  nid=$(hdc ps -q netns)
  case "$kind" in
    tcp)
      if docker run --rm --network "container:$nid" "$egress_tools_image" nc ${3:+-s "$3"} -z -w 3 "$1" "$2" > /dev/null 2>&1; then echo connects; else echo blocked; fi
      ;;
    udp)
      docker run --rm --network "container:$nid" "$egress_tools_image" sh -c "echo x | nc -u -w1 $1 $2" > /dev/null 2>&1 || true
      echo sent
      ;;
    dns)
      # The answer and the seconds it took: a refused name must fail at once, not after a resolver's timeouts.
      local began=$SECONDS res=unresolved
      if docker run --rm --network "container:$nid" "$egress_tools_image" timeout 12 getent ahosts "$1" > /dev/null 2>&1; then res=resolved; fi
      echo "$res $((SECONDS - began))"
      ;;
    dnsctl)
      # The control: a container on the hub's network WITHOUT the namespace's dns: setting, which resolves through the
      # host's resolver, the path the recorder must be able to see.
      docker run --rm --network "${hub_project}_hub" "$egress_tools_image" timeout 12 getent ahosts "$1" > /dev/null 2>&1 || true
      echo done
      ;;
  esac
}

# How many packets the printed rules dropped so far (nftables: the counters of the drop rules; iptables: the DROP rules
# of the two chains).
egress_drops() {
  local out
  if [ "$egress_fw" = nft ]; then
    out=$(egress_rr nft list chain inet satbooth_hub egress 2> /dev/null || true)
    awk '/counter packets/ { for (i = 1; i <= NF; i++) if ($i == "packets") s += $(i + 1) } END { print s + 0 }' <<< "$out"
  else
    out=$({ egress_rr "$egress_ipt" -w -L SATBOOTH_HUB_EGRESS -n -v -x; egress_rr "$egress_ipt" -w -L SATBOOTH_HUB_ELECTRUM -n -v -x; } 2> /dev/null || true)
    awk '$3 == "DROP" { s += $1 } END { print s + 0 }' <<< "$out"
  fi
}

egress_note() { printf '%s\n' "$*" | tee -a "$egress_ev/summary.txt"; }
egress_findings=0
egress_finding() { egress_findings=$((egress_findings + 1)); egress_note "FINDING: $*"; }
egress_ok() { egress_note "ok: $*"; }
egress_bad() { egress_findings=$((egress_findings + 1)); egress_note "FAIL: $*"; }

egress_cleanup() {
  [ -n "${egress_ev:-}" ] || return 0
  local n
  for n in hubnet peer dns-any dns-lo conntrack; do egress_stop "$n"; done
  if [ -n "${egress_fw:-}" ]; then
    if [ "$egress_fw" = nft ]; then
      egress_rr nft delete table inet satbooth_hub > /dev/null 2>&1 || true
    else
      egress_rr "$egress_ipt" -w -D DOCKER-USER -s "$egress_hub_ip" -j SATBOOTH_HUB_EGRESS > /dev/null 2>&1 || true
      egress_rr "$egress_ipt" -w -D DOCKER-USER -s "$egress_electrum_subnet" -j SATBOOTH_HUB_ELECTRUM > /dev/null 2>&1 || true
      for n in SATBOOTH_HUB_EGRESS SATBOOTH_HUB_ELECTRUM SATBOOTH_HUB_PEERS; do
        egress_rr "$egress_ipt" -w -F "$n" > /dev/null 2>&1 || true
        egress_rr "$egress_ipt" -w -X "$n" > /dev/null 2>&1 || true
      done
    fi
    egress_fw=""
  fi
  docker rm -f satbooth-egress-peer satbooth-egress-relay > /dev/null 2>&1 || true
  egress_rr ip link del vpeer-h > /dev/null 2>&1 || true
}

# --- the group ----------------------------------------------------------------------------------------------------

egress_ran=0
egress_group() {
  egress_findings=0
  egress_fw=""
  egress_ev="$PWD/$logdir/egress"
  if [ "$(uname -s)" != Linux ] && [ -z "${QUALIFY_ROOT:-}" ]; then
    log "egress: NOT RUN: this is $(uname -s), and the firewall, packet and DNS recording need Linux with root (the Linux runner runs it)"
    return 0
  fi
  if [ -z "${QUALIFY_ROOT:-}" ] && ! sudo -n true 2> /dev/null; then
    log "egress: NOT RUN: no root without a password (sudo -n)"
    return 0
  fi
  command -v jq > /dev/null || { echo "qualify.sh: jq is required for the egress group" >&2; return 1; }
  rm -rf "${egress_ev:?}"
  mkdir -p "$egress_ev/relay"
  chmod 0777 "$egress_ev/relay"
  : > "$egress_ev/summary.txt"
  log "egress: the hub bundle with the printed egress rules applied, recorded from the host"
  egress_build_tools

  # What this host is, before anything is changed.
  {
    echo "== docker version"
    docker version --format 'client {{.Client.Version}} server {{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}'
    echo "== uname"
    uname -a
    echo "== iptables"
    egress_rr iptables --version 2>&1 || true
    egress_rr iptables-legacy --version 2>&1 || true
    echo "== DOCKER-USER (iptables)"
    egress_rr iptables -w -S DOCKER-USER 2>&1 || true
    echo "== DOCKER-USER (iptables-legacy)"
    egress_rr iptables-legacy -w -S DOCKER-USER 2>&1 || true
    echo "== nft tables"
    egress_rr nft list tables 2>&1 || true
    echo "== bridge-nf-call-iptables, ip_forward"
    egress_rr sh -c 'cat /proc/sys/net/bridge/bridge-nf-call-iptables 2>&1; cat /proc/sys/net/ipv4/ip_forward' || true
    echo "== addresses and routes"
    egress_rr ip -br addr 2>&1 || true
    egress_rr ip route 2>&1 || true
  } > "$egress_ev/host.txt" 2>&1
  # Which firewall: the one Docker's own rules are in. iptables-legacy holds DOCKER-USER only on a legacy host; any other
  # (iptables-nft, the runner's) is judged by the nftables file, whose chain runs before Docker's.
  if egress_rr iptables-legacy -w -S DOCKER-USER > /dev/null 2>&1; then
    egress_fw=iptables
    egress_ipt=iptables-legacy
  elif egress_rr nft list tables > /dev/null 2>&1; then
    egress_fw=nft
  else
    echo "qualify-egress: neither nft nor an iptables-legacy DOCKER-USER chain on this host" >&2
    return 1
  fi
  egress_note "firewall applied: $egress_fw (iptables here: $(egress_rr iptables --version 2>&1 | head -1))"

  # The bundle's setup (stages 1 and 2), as deploy_hub_group does it: the same subnets, the answers without a peer.
  deploy_tmp=$(mktemp -d)
  deploy_tmp=$(cd "$deploy_tmp" && pwd -P)
  hub_pub_root=$(mktemp -d)
  hub_pub_root=$(cd "$hub_pub_root" && pwd -P)
  hub_root=$(mktemp -d)
  hub_root=$(cd "$hub_root" && pwd -P)
  local out digest_value=""
  jq -n --arg host "$hub_pub_root" '{network: "regtest", host_dir: $host, hub_private_addr: "127.0.0.1",
    public_private_addr: "127.0.0.2", checkout_base_url: "http://127.0.0.1:8080", webhook_url: "http://127.0.0.1:9/hook"}' \
    > "$deploy_tmp/pub.json"
  out=$(deploy/setup.sh "$hub_pub_root" public init -answers "$deploy_tmp/pub.json")
  check_digest "setup public init (egress)" "$out" "$hub_pub_root/setup/public-to-hub.json"
  jq -n --arg host "$hub_root" '{network: "regtest", host_dir: $host, hub_private_addr: "127.0.0.1",
    public_private_addr: "127.0.0.2", hub_admin_addr: "127.0.0.3", hub_subnet: "172.30.79.0/25", hub_ip: "172.30.79.10",
    electrum_subnet: "172.30.79.128/25"}' > "$deploy_tmp/hub.json"
  deploy/setup.sh "$hub_root" hub init -handoff "$hub_pub_root/setup/public-to-hub.json" -digest "$digest_value" \
    -answers "$deploy_tmp/hub.json" > /dev/null
  egress_hub_ip=172.30.79.10
  egress_hub_subnet=172.30.79.0/25
  egress_electrum_subnet=172.30.79.128/25
  local regtest_net=172.30.78.0/24 peer_ip=10.99.0.2 peer_host=10.99.0.1

  # The bundle takes the place of the regtest hub-satbooth: its container and relay stop, the hub's data go to the hub's
  # user, the regtest relay's pairs become the bundle's relay.json, and the override file (egress-hub.override.yml) gives
  # the namespace that hub's address and the hub its volume and the regtest chain source.
  log "egress: the bundle takes hub-satbooth's address, data and relay pairs"
  dc stop -t 60 relay-satbooth hub-satbooth > /dev/null
  dc run --rm --no-deps -T --cap-add DAC_OVERRIDE --cap-add CHOWN --cap-add FOWNER -v "$hub_root/config:/c" \
    -v satbooth-regtest_hub-satbooth-data:/hubdata --entrypoint sh volume-init -c \
    'set -eu; cp /state/relay-satbooth.json /c/relay.json; chmod 0600 /c/relay.json; chown -R 10003:10003 /hubdata'
  local ports
  ports=$(python3 -c 'import socket
for _ in range(3):
    s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
  # shellcheck disable=SC2086
  set -- $ports
  export TEST_HUB_ADMIN_PORT=$1 TEST_HUB_RELAY_PORT=$2 TEST_HUB_RPC_PORT=$3
  hub_override=dev/regtest/egress-hub.override.yml
  local admin=http://127.0.0.1:$TEST_HUB_ADMIN_PORT
  hdc config -q
  hdc build relay hubstub hubproxy > /dev/null
  # The browser's driver, built in the qualify container (vendored sources, no network), for this host's system. No VCS
  # stamping: the checkout belongs to the runner's user, and git refuses it as root (go test, which the suite runs, does not stamp).
  local goos goarch
  goos=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in x86_64) goarch=amd64 ;; aarch64 | arm64) goarch=arm64 ;; *) goarch=$(uname -m) ;; esac
  dc run --rm --no-deps -T -e "GOOS=$goos" -e "GOARCH=$goarch" -v "$egress_ev:/out" --entrypoint sh qualify -c \
    'go build -buildvcs=false -tags qualify -o /out/uicheck ./qualify/uicheck'

  # The namespace first (it creates the bundle's networks), nothing of the hub yet.
  hdc up -d netns
  local nid netns_pid hubnet_id br hub_mac outward
  nid=$(hdc ps -q netns)
  netns_pid=$(docker inspect -f '{{.State.Pid}}' "$nid")
  hubnet_id=$(docker network inspect -f '{{.Id}}' "${hub_project}_hub")
  br="br-${hubnet_id:0:12}"
  egress_rr ip link show "$br" > /dev/null || { echo "qualify-egress: no bridge $br" >&2; return 1; }
  hub_mac=$(docker inspect -f "{{(index .NetworkSettings.Networks \"${hub_project}_hub\").MacAddress}}" "$nid")
  egress_electrum_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"${hub_project}_electrum\").IPAddress}}" "$nid")
  outward=$(egress_rr ip -o route get 1.1.1.1 | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
  egress_note "hub bridge $br, hub address $egress_hub_ip (MAC $hub_mac), electrum address $egress_electrum_ip, outward interface $outward"

  # The peer: a namespace behind a veth on the host, 10.99.0.2, outside both of the bundle's networks, reached over the
  # host's forward path. A TCP relay carries 10.99.0.2:9735 to the merchant's hub (a unix socket joins the two halves:
  # the listener in the peer's namespace, the connection made inside the merchant's).
  log "egress: the peer's namespace"
  docker run -d --name satbooth-egress-peer --network none --cap-add NET_ADMIN -v "$egress_ev/relay:/relay" "$egress_tools_image" sleep infinity > /dev/null
  local peer_pid merchant_pid
  peer_pid=$(docker inspect -f '{{.State.Pid}}' satbooth-egress-peer)
  merchant_pid=$(docker inspect -f '{{.State.Pid}}' "$(container_id hub-merchant)")
  egress_rr ip link del vpeer-h > /dev/null 2>&1 || true
  egress_rr sh -c "set -e
    ip link add vpeer-h type veth peer name vpeer-n
    ip link set vpeer-n netns $peer_pid
    ip addr add $peer_host/24 dev vpeer-h; ip link set vpeer-h up
    nsenter -t $peer_pid -n ip addr add $peer_ip/24 dev vpeer-n
    nsenter -t $peer_pid -n ip link set lo up; nsenter -t $peer_pid -n ip link set vpeer-n up
    nsenter -t $peer_pid -n ip route add default via $peer_host"
  docker exec -d satbooth-egress-peer socat TCP-LISTEN:9735,fork,reuseaddr UNIX-CONNECT:/relay/sock
  docker run -d --name satbooth-egress-relay --privileged --pid=host -v "$egress_ev/relay:/relay" "$egress_tools_image" \
    nsenter -t "$merchant_pid" -n socat UNIX-LISTEN:/relay/sock,fork,mode=0777 TCP:172.30.78.20:9735 > /dev/null

  # Recorders, before any rule and before the hub starts: what reaches the hub's bridge (headers only, no DNS), every
  # DNS query on every interface of the host and inside the namespace, what reaches the peer's veth, and every new
  # connection that passed the host's firewall (a conntrack entry is confirmed only for a packet that was not dropped).
  egress_bg hubnet "$egress_ev/hubnet.log" tcpdump -i "$br" -nn -e -s 96 -U -w "$egress_ev/hubnet.pcap" 'not arp and not port 53'
  egress_bg peer "$egress_ev/peer.log" tcpdump -i vpeer-h -nn -e -s 96 -U -w "$egress_ev/peer.pcap" 'not arp'
  egress_bg dns-any "$egress_ev/dns-any.log" tcpdump -i any -nn -e -s 256 -U -w "$egress_ev/dns-any.pcap" 'port 53'
  egress_bg dns-lo "$egress_ev/dns-lo.log" nsenter -t "$netns_pid" -n tcpdump -i lo -nn -s 512 -U -w "$egress_ev/dns-lo.pcap" 'udp and host 127.0.0.11'
  egress_bg conntrack "$egress_ev/conntrack.txt" conntrack -E -e NEW -o timestamp,extended
  sleep 2

  # Control, rules off: a connection and a name lookup from the hub's namespace leave the host. The recorder must see
  # them, or its silence later means nothing.
  local off_name on_name ctl_name r
  off_name="sbdns-off-$RANDOM$RANDOM.example.net"
  on_name="sbdns-on-$RANDOM$RANDOM.example.net"
  ctl_name="sbdns-ctl-$RANDOM$RANDOM.example.net"
  egress_probe tcp 203.0.113.9 4440 > /dev/null
  egress_probe dnsctl "$ctl_name" > /dev/null
  r=$(egress_probe dns "$off_name")
  egress_note "DNS probe, rules off: $off_name from the namespace: $r"
  case "$r" in "unresolved "[0-8]) egress_ok "an unpinned name fails at once inside the namespace (rules off): $r" ;; *) egress_bad "an unpinned name did not fail fast inside the namespace (rules off): $r" ;; esac
  sleep 3

  # The printed rules, applied.
  log "egress: applying the printed rules ($egress_fw), with no peer listed"
  if [ "$egress_fw" = nft ]; then
    deploy/setup.sh "$hub_root" hub egress-rules -format nft > "$egress_ev/rules.nft"
    egress_rr nft -f "$egress_ev/rules.nft"
    egress_rr nft list ruleset > "$egress_ev/ruleset-after.txt" 2>&1 || true
  else
    deploy/setup.sh "$hub_root" hub egress-rules -format iptables > "$egress_ev/rules.sh"
    egress_rr env "IPT=$egress_ipt" sh "$egress_ev/rules.sh"
    egress_rr "$egress_ipt" -w -S > "$egress_ev/ruleset-after.txt" 2>&1 || true
  fi

  # Fence canaries before the hub runs: each must be dropped, counted and still seen leaving the namespace.
  local d0 d1 d2
  d0=$(egress_drops)
  r=$(egress_probe tcp 203.0.113.9 4443)
  if [ "$r" = blocked ]; then egress_ok "canary tcp 203.0.113.9:4443 from the hub's address: blocked"; else egress_bad "canary tcp to the internet connected with the rules on"; fi
  egress_probe udp 203.0.113.9 4444 > /dev/null
  r=$(egress_probe tcp 203.0.113.9 4445 "$egress_electrum_ip")
  if [ "$r" = blocked ]; then egress_ok "canary tcp 203.0.113.9:4445 from the electrum-network address: blocked"; else egress_bad "canary from the electrum-network address connected"; fi
  r=$(egress_probe tcp "$peer_ip" 9735)
  if [ "$r" = blocked ]; then egress_ok "the peer $peer_ip:9735 before it is listed: blocked"; else egress_bad "the unlisted peer connected"; fi
  d1=$(egress_drops)
  if [ "$d1" -gt "$d0" ]; then egress_ok "the drop counters rose from $d0 to $d1 over the canaries"; else egress_bad "the drop counters did not rise over the canaries ($d0 to $d1)"; fi
  # The printed add command lists the peer; then it connects.
  if [ "$egress_fw" = nft ]; then
    egress_rr nft add element inet satbooth_hub satbooth_hub_peers "{ $peer_ip . 9735 }"
  else
    egress_rr "$egress_ipt" -w -A SATBOOTH_HUB_PEERS -d "$peer_ip" -p tcp --dport 9735 -j ACCEPT
  fi
  r=$(egress_probe tcp "$peer_ip" 9735)
  if [ "$r" = connects ]; then egress_ok "the peer $peer_ip:9735 after it is listed (the printed add command): connects"; else egress_bad "the listed peer does not connect"; fi
  r=$(egress_probe tcp "$peer_ip" 9736)
  if [ "$r" = blocked ]; then egress_ok "the same address, port 9736 (not listed): blocked"; else egress_bad "an unlisted port of the listed peer connected"; fi
  r=$(egress_probe dns "$on_name")
  egress_note "DNS probe, rules on: $on_name from the namespace: $r"
  case "$r" in "unresolved "[0-8]) egress_ok "an unpinned name fails at once inside the namespace (rules on): $r" ;; *) egress_bad "an unpinned name did not fail fast inside the namespace (rules on): $r" ;; esac
  sleep 2

  # The hub, the relay, the proxy and the stub start now, under the rules and the recorders.
  log "egress: starting the hub, relay, proxy and stub"
  hdc up -d --build hub relay hubproxy hubstub
  local deadline=$((SECONDS + 240)) code=""
  until code=$(curl -s -o /dev/null --max-time 5 -w '%{http_code}' "$admin/api/info") && [ "$code" = 200 ]; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "qualify-egress: the hub's UI proxy does not answer after 4 minutes (last HTTP $code)" >&2; hdc logs --tail 40 hub >&2 || true; return 1; }
    sleep 3
  done
  deadline=$((SECONDS + 240))
  until curl -sS --max-time 5 "$admin/api/info" 2> /dev/null | grep -Eq '"running": *true'; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "qualify-egress: the hub is not running after 4 minutes" >&2; hdc logs --tail 40 hub >&2 || true; return 1; }
    sleep 3
  done
  egress_ok "the hub runs under the rules (through the proxy: GET /api/info 200, running true)"
  # /api/health needs the login: the password is the dev hubs' fixed string.
  local token health
  token=$(curl -sS --max-time 30 -H 'content-type: application/json' -d '{"unlockPassword":"regtest-only-unlock","permission":"full"}' "$admin/api/unlock" | jq -r '.token // empty')
  # The merchant's hub reconnects to the restarted hub by itself (a minute or so): the phases need the channel active.
  deadline=$((SECONDS + 300))
  until curl -sS --max-time 10 -H "Authorization: Bearer $token" "$admin/api/channels" 2> /dev/null | jq -e 'length > 0 and all(.[]; .active == true)' > /dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "qualify-egress: the hub's channel is not active after 5 minutes" >&2; hdc logs --tail 40 hub >&2 || true; return 1; }
    sleep 3
  done
  egress_ok "the hub's channel with the merchant is active again under the rules"
  health=$(curl -s -o /dev/null --max-time 20 -w '%{http_code}' -H "Authorization: Bearer $token" "$admin/api/health")
  token=""
  if [ "$health" = 200 ]; then egress_ok "GET /api/health through the proxy: 200"; else egress_bad "GET /api/health answered $health, want 200"; fi

  # The Lightning phases of the suite, unchanged, with the rules on (the hub is the bundle at the regtest hub's address).
  log "egress: the Lightning phases with the rules on"
  phase TestLightningPay
  phase TestRefundPay
  egress_ok "TestLightningPay and TestRefundPay passed with the rules on"

  # The hub's own dials to the peer: with a throwaway public key (the peer's relay answers the TCP connection, the Lightning
  # handshake then fails: the hub's TCP dial is what is recorded), to the listed port and to an unlisted one.
  token=$(curl -sS --max-time 30 -H 'content-type: application/json' -d '{"unlockPassword":"regtest-only-unlock","permission":"full"}' "$admin/api/unlock" | jq -r '.token // empty')
  local g=0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798
  d1=$(egress_drops)
  curl -s -o /dev/null --max-time 60 -H "Authorization: Bearer $token" -H 'content-type: application/json' \
    -d "{\"pubkey\":\"$g\",\"address\":\"$peer_ip\",\"port\":9735}" "$admin/api/peers" || true
  curl -s -o /dev/null --max-time 60 -H "Authorization: Bearer $token" -H 'content-type: application/json' \
    -d "{\"pubkey\":\"$g\",\"address\":\"$peer_ip\",\"port\":9736}" "$admin/api/peers" || true
  token=""
  d2=$(egress_drops)
  if [ "$d2" -gt "$d1" ]; then egress_ok "the hub's own dial to the unlisted port 9736 was dropped (counters $d1 to $d2)"; else egress_bad "the hub's dial to $peer_ip:9736 was not counted as dropped ($d1 to $d2)"; fi

  # The logged-in pages, through the proxy, in headless Chrome.
  local chrome="" p
  for p in google-chrome google-chrome-stable chromium chromium-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
    if command -v "$p" > /dev/null 2>&1 || [ -x "$p" ]; then
      chrome=$p
      break
    fi
  done
  if [ -z "$chrome" ]; then
    egress_bad "no Chrome on this host: the pages were not loaded"
  else
    printf 'regtest-only-unlock\n' > "$egress_ev/pw.tmp"
    if "$egress_ev/uicheck" -chrome "$chrome" -base "$admin" -password-file "$egress_ev/pw.tmp" -out "$egress_ev/uicheck.json" 2>&1 | tee "$egress_ev/uicheck.txt"; then
      egress_ok "the logged-in pages through the proxy: no request sent to another origin ($(sed -n 1p "$egress_ev/uicheck.txt"))"
    else
      egress_bad "the page check failed (uicheck.txt, uicheck.json)"
    fi
    rm -f "${egress_ev:?}/pw.tmp"
  fi
  sleep 3

  # The stub's log: the calls the hub made, as it saw them.
  hdc logs --no-color hubstub > "$egress_ev/stub.log" 2>&1 || true
  if grep -q '/api/internal/info' "$egress_ev/stub.log"; then egress_ok "the stub's log shows the hub's update check (/api/internal/info)"; else egress_bad "the stub's log shows no /api/internal/info call"; fi
  { grep -oE 'method=[A-Z]+ host=[^ ]+ path=[^ ]+' "$egress_ev/stub.log" || true; } | sort | uniq -c | sort -rn > "$egress_ev/stub-calls.txt"
  hdc logs --no-color hub > "$egress_ev/hub.log" 2>&1 || true

  # Stop the recorders, read the kernel's log, and judge.
  egress_rr dmesg > "$egress_ev/dmesg.txt" 2>&1 || egress_rr journalctl -k --no-pager > "$egress_ev/dmesg.txt" 2>&1 || true
  if [ "$egress_fw" = nft ]; then
    egress_rr nft list chain inet satbooth_hub egress > "$egress_ev/counters.txt" 2>&1 || true
  else
    egress_rr "$egress_ipt" -w -L SATBOOTH_HUB_EGRESS -n -v -x > "$egress_ev/counters.txt" 2>&1 || true
  fi
  for r in hubnet peer dns-any dns-lo conntrack; do egress_stop "$r"; done
  sleep 2
  egress_judge "$hub_mac" "$peer_ip" "$regtest_net" "$off_name" "$on_name" "$outward" "$ctl_name"
  egress_ran=1
  log "egress: $egress_findings finding(s) and failure(s); evidence in $logdir/egress (summary.txt)"
  [ "$egress_findings" -eq 0 ]
}

# egress_judge <hub mac> <peer ip> <regtest net> <name off> <name on> <outward interface>
egress_judge() {
  local hub_mac=$1 peer_ip=$2 regtest_net=$3 off_name=$4 on_name=$5 outward=$6 ctl_name=$7
  local nets=("$egress_hub_subnet" "$egress_electrum_subnet" "$regtest_net")
  local mac proto src sport dst dport line

  # 1. What passed the firewall: every new connection with the hub's address or the electrum network as its source and a
  # destination outside the bundle's networks. Only the peer's listed port is allowed (and the control before the rules).
  egress_conntrack < "$egress_ev/conntrack.txt" > "$egress_ev/conntrack-flows.txt"
  local passed=0 saw_peer=0 saw_control=0
  while read -r proto src sport dst dport; do
    if [ "$src" != "$egress_hub_ip" ] && ! egress_in_net "$src" "$egress_electrum_subnet"; then continue; fi
    egress_outside "$dst" "${nets[@]}" || continue
    if [ "$dst" = 203.0.113.9 ] && [ "$dport" = 4440 ]; then
      saw_control=1
      continue
    fi
    if [ "$dst" = "$peer_ip" ] && [ "$dport" = 9735 ] && [ "$proto" = tcp ]; then
      saw_peer=1
      continue
    fi
    passed=1
    egress_bad "a connection passed the firewall: $proto $src:$sport > $dst:$dport"
  done < "$egress_ev/conntrack-flows.txt"
  if [ "$passed" -eq 0 ]; then egress_ok "no connection from the hub's namespace to outside the bundle passed the firewall, except the listed peer"; fi
  if [ "$saw_control" -eq 1 ]; then egress_ok "the recorder saw the control connection leave before the rules (203.0.113.9:4440): it can see one"; else egress_bad "the recorder did not see the control connection before the rules: it may be blind"; fi
  if [ "$saw_peer" -eq 1 ]; then egress_ok "the recorder saw the listed peer's connection ($peer_ip:9735) pass"; else egress_bad "the listed peer's connection was not recorded passing"; fi

  # 2. What the hub tried: packets on the hub's bridge sent by the hub's address or MAC, or from the electrum network,
  # to a destination outside the bundle's networks. Dropped attempts are findings to read, not noise to hide; the
  # canaries are the expected ones.
  tcpdump -nn -e -r "$egress_ev/hubnet.pcap" 2> /dev/null | egress_flows > "$egress_ev/hubnet-flows.txt"
  : > "$egress_ev/attempts.txt"
  while read -r mac proto src sport dst dport; do
    if [ "$src" != "$egress_hub_ip" ] && [ "$mac" != "$hub_mac" ] && ! egress_in_net "$src" "$egress_electrum_subnet"; then continue; fi
    egress_outside "$dst" "${nets[@]}" || continue
    case "$dst:$dport" in 203.0.113.9:4440 | 203.0.113.9:4443 | 203.0.113.9:4444 | 203.0.113.9:4445) continue ;; esac
    [ "$dst" = "$peer_ip" ] && { [ "$dport" = 9735 ] || [ "$dport" = 9736 ]; } && continue # the peer's listed port, and the unlisted one the probes and the hub dial
    printf '%s %s > %s:%s\n' "$proto" "$src" "$dst" "$dport" >> "$egress_ev/attempts.txt"
  done < "$egress_ev/hubnet-flows.txt"
  sort "$egress_ev/attempts.txt" | uniq -c | sort -rn > "$egress_ev/attempts-unique.txt"
  if [ -s "$egress_ev/attempts-unique.txt" ]; then
    while read -r line; do egress_finding "the hub's namespace sent packets that were not a canary or for the listed peer (the rules dropped them): $line"; done < "$egress_ev/attempts-unique.txt"
  else
    egress_ok "no packet left the hub's namespace for outside the bundle but the canaries and the listed peer"
  fi
  local canary
  for canary in 4443 4444 4445; do
    if awk -v p="$canary" '$5 == "203.0.113.9" && $6 == p { f = 1 } END { exit !f }' "$egress_ev/hubnet-flows.txt"; then
      egress_ok "the recorder saw the canary to 203.0.113.9:$canary leave the namespace (and the firewall dropped it)"
    else
      egress_bad "the recorder did not see the canary to 203.0.113.9:$canary: it may be blind"
    fi
  done

  # 3. The peer's veth saw only the listed port sent to the peer.
  tcpdump -nn -e -r "$egress_ev/peer.pcap" 2> /dev/null | egress_flows > "$egress_ev/peer-flows.txt"
  if awk -v p="$peer_ip" '$5 == p && $6 != 9735 && $6 != "-" { bad = 1 } END { exit bad }' "$egress_ev/peer-flows.txt"; then
    egress_ok "the peer's interface saw only port 9735 sent to $peer_ip"
  else
    egress_bad "the peer's interface saw a port other than 9735 sent to $peer_ip"
  fi

  # 4. The firewall's own log (at most 5 lines a minute) and the DNS.
  egress_note "kernel log lines with the prefix satbooth-hub-egress: $({ grep -c 'satbooth-hub-egress:' "$egress_ev/dmesg.txt" || true; })"
  if grep -q 'satbooth-hub-egress:' "$egress_ev/dmesg.txt"; then egress_ok "the firewall logged drops with its prefix"; else egress_bad "the kernel log holds no line with the prefix satbooth-hub-egress: (dmesg.txt)"; fi

  # DNS. Docker's resolver sends its upstream queries from the HOST's namespace, where the printed rules do not see them
  # (measured on Docker Engine 29.4.2), so the bundle's netns sets dns: to the stub's address, which has no DNS port.
  # The control below, a container on the hub network without that setting, must show the host's resolver path in the
  # capture; the namespace's names (the probes' and any the hub asked) must show none: no packet on loopback (the host's
  # resolver) and none sent out of the host. A packet on a Docker bridge is the query going to the stub, inside the host.
  # The namespace's queries go to Docker's resolver on 127.0.0.11, at a port tcpdump does not decode: the names are read
  # from the packets' DNS questions.
  egress_pcap_names "$egress_ev/dns-lo.pcap" > "$egress_ev/dns-asked-in-namespace.txt"
  tcpdump -nn -e -r "$egress_ev/dns-any.pcap" 2> /dev/null > "$egress_ev/dns-host.txt" || true
  dns_host_side() { # <name>: the lines of that name on the host's loopback or sent Out by a non-Docker interface
    { grep -F "$1" "$egress_ev/dns-host.txt" || true; } | grep -vE "^[0-9:.]+ (docker0|br-[0-9a-f]+|veth[0-9a-f]+|vpeer-h) " | grep -E '^[0-9:.]+ lo |( Out )' || true
  }
  dns_via() { # <name>: where the name was seen on the host, as interface:direction counts
    { grep -F "$1" "$egress_ev/dns-host.txt" || true; } | awk '{ for (i = 1; i <= NF; i++) if ($i == "In" || $i == "Out" || $i == "B") { print $(i - 1) ":" $i; break } }' | sort | uniq -c | tr '\n' ' '
  }
  local nm
  for nm in "$ctl_name" "$off_name" "$on_name"; do egress_note "DNS: $nm seen on the host as: $(dns_via "$nm")"; done
  if [ -n "$(dns_host_side "$ctl_name")" ]; then
    egress_ok "DNS control: a container on the hub's network without dns: was seen going through the host's resolver (the recorder sees that path)"
  else
    egress_bad "DNS control: the control lookup of $ctl_name was not seen on the host's loopback or outward: the recorder may be blind to the path"
  fi
  : "$outward"
  for nm in "$off_name" "$on_name"; do
    if [ -n "$(dns_host_side "$nm")" ]; then
      egress_bad "DNS: $nm, looked up from the hub's namespace, reached the host's resolver path ($(dns_host_side "$nm" | head -1 | awk '{ print $2 " " $3 }'))"
    else
      egress_ok "DNS: $nm, looked up from the hub's namespace, did not reach the host's resolver or leave the host"
    fi
  done
  local n leaked=0
  while read -r n; do
    [ -n "$n" ] || continue
    if [ -n "$(dns_host_side "$n")" ]; then
      egress_bad "DNS: the name $n, asked from the hub's namespace, reached the host's resolver path"
      leaked=1
    fi
  done < <(grep -v -e "$off_name" -e "$on_name" "$egress_ev/dns-asked-in-namespace.txt" || true)
  if [ "$leaked" -eq 0 ]; then egress_ok "DNS: no name the hub's namespace asked other than the probes' reached the host's resolver path ($(grep -vc -e "$off_name" -e "$on_name" "$egress_ev/dns-asked-in-namespace.txt" || true) names)"; fi
}
