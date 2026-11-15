package setup

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// The hub, the relay and the UI proxy share one network namespace, the one of the netns container, whose address on
// the bundle's hub network is HUB_IP. Everything that leaves that namespace towards the internet crosses the host's
// forward path with HUB_IP as its source, and these rules bound it there: the hub's outside calls that are not pinned
// to the stub fail at the firewall, not only at the TLS handshake. bitcoind is another container (other addresses)
// and is not bound: it needs the internet for its peers.
const (
	lnPort     = 9735
	logPrefix  = "satbooth-hub-egress: "
	nftTable   = "satbooth_hub"
	iptChain   = "SATBOOTH_HUB_EGRESS"
	maxPeerLen = 64
)

// EgressPorts is the sorted list of TCP ports the hub's namespace may reach on the internet: 9735 (Lightning) and the
// peer ports the operator names. A port outside 1 to 65535, or 53 (DNS is never allowed out), is refused.
func EgressPorts(peerPorts []int) ([]int, error) {
	ports := []int{lnPort}
	if len(peerPorts) > maxPeerLen {
		return nil, fmt.Errorf("setup: at most %d peer ports", maxPeerLen)
	}
	for _, p := range peerPorts {
		switch {
		case p < 1 || p > 65535:
			return nil, fmt.Errorf("setup: peer port %d is not between 1 and 65535", p)
		case p == 53:
			return nil, errors.New("setup: port 53 is DNS: the hub never reaches an outside resolver")
		}
		if !slices.Contains(ports, p) {
			ports = append(ports, p)
		}
	}
	slices.Sort(ports)
	return ports, nil
}

func joinInts(ports []int, sep string) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, sep)
}

// EgressNft is the nftables rule set for the answers: a table of its own with a forward chain before Docker's, which
// only drops (what it accepts, Docker's own rules still judge). Loading the file replaces the table, so it can be
// loaded again.
func EgressNft(a *HubAnswers, ports []int) string {
	ip := a.HubIP
	return fmt.Sprintf(`# SatBooth hub host: what the hub's network namespace may send out. Load as root: nft -f <this file>
# The namespace (hub, relay, UI proxy) has the address %[1]s. It may reach the bundle's own two networks, answer
# connections made to it, and open TCP connections to Lightning peers on port(s) %[2]s. Everything else, outside DNS
# included, is logged and dropped. bitcoind is another container and is not bound.
add table inet %[3]s
delete table inet %[3]s
table inet %[3]s {
	chain egress {
		type filter hook forward priority -10; policy accept;
		ip saddr %[1]s ct state established,related accept
		ip saddr %[1]s ip daddr { %[4]s, %[5]s } accept
		ip saddr %[1]s tcp dport { %[6]s } accept
		ip saddr %[1]s limit rate 5/minute burst 5 packets log prefix "%[7]s"
		ip saddr %[1]s counter drop
	}
}
`, ip, joinInts(ports, ", "), nftTable, a.HubSubnet, a.ElectrumSubnet, joinInts(ports, ", "), logPrefix)
}

// EgressIptables is the same policy for a host whose Docker uses iptables-legacy: a chain of its own that DOCKER-USER
// jumps to for the namespace's address. Allowed packets RETURN, so Docker's own rules still judge them. IPT names the
// binary (iptables-legacy by default). Running it again rebuilds the chain and adds the jump once.
func EgressIptables(a *HubAnswers, ports []int) string {
	var b strings.Builder
	w := func(f string, args ...any) { fmt.Fprintf(&b, f+"\n", args...) }
	w("#!/bin/sh")
	w("# SatBooth hub host: what the hub's network namespace may send out. Run as root: sh <this file>")
	w("# The namespace (hub, relay, UI proxy) has the address %s. It may reach the bundle's own two networks, answer", a.HubIP)
	w("# connections made to it, and open TCP connections to Lightning peers on port(s) %s. Everything else, outside DNS", joinInts(ports, ", "))
	w("# included, is logged and dropped. bitcoind is another container and is not bound.")
	w("set -eu")
	w(`IPT=${IPT:-iptables-legacy}`)
	w(`"$IPT" -w -N %s 2>/dev/null || "$IPT" -w -F %s`, iptChain, iptChain)
	w(`"$IPT" -w -A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN`, iptChain)
	w(`"$IPT" -w -A %s -d %s -j RETURN`, iptChain, a.HubSubnet)
	w(`"$IPT" -w -A %s -d %s -j RETURN`, iptChain, a.ElectrumSubnet)
	w(`"$IPT" -w -A %s -p tcp -m multiport --dports %s -j RETURN`, iptChain, joinInts(ports, ","))
	w(`"$IPT" -w -A %s -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "%s"`, iptChain, logPrefix)
	w(`"$IPT" -w -A %s -j DROP`, iptChain)
	w(`"$IPT" -w -C DOCKER-USER -s %[1]s -j %[2]s 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s %[1]s -j %[2]s`, a.HubIP, iptChain)
	return b.String()
}

// HubEgressRules reads the hub host's answers and returns both rule sets. It writes nothing and runs nothing.
func (l *Lock) HubEgressRules(peerPorts []int) (nft, iptables string, err error) {
	ports, err := EgressPorts(peerPorts)
	if err != nil {
		return "", "", err
	}
	st, err := l.hubState()
	if err != nil {
		return "", "", err
	}
	a := st.Hub
	for _, f := range []struct{ name, v string }{{"hub_subnet", a.HubSubnet}, {"electrum_subnet", a.ElectrumSubnet}} {
		if _, err := netip.ParsePrefix(f.v); err != nil {
			return "", "", fmt.Errorf("setup: %s in setup/state.json is not a network", f.name)
		}
	}
	if _, err := netip.ParseAddr(a.HubIP); err != nil {
		return "", "", errors.New("setup: hub_ip in setup/state.json is not an address")
	}
	return EgressNft(a, ports), EgressIptables(a, ports), nil
}
