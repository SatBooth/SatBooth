package setup

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// The hub, the relay and the UI proxy share one network namespace, the one of the netns container, whose address on
// the bundle's hub network is HUB_IP. Everything that leaves that namespace towards the internet crosses the host's
// forward path with HUB_IP as its source, and these rules bound it there: the hub's outside calls that are not pinned
// to the stub fail at the firewall, not only at the TLS handshake. (Docker's embedded resolver is not covered: it sends
// its upstream queries from the host, which these rules do not see; the bundle's netns service sets dns: to the stub's
// address so that they never reach the host's resolver.) The one thing it may open to the internet is a TCP
// connection to a listed Lightning peer, an address and a port (peers do not all listen on 9735). bitcoind is another
// container (other addresses) and is not bound: it needs the internet for its peers. The namespace has a second
// address, on the internal electrum network; a process that bound to it would leave by the default route with that
// source, so the rules also drop what comes from the electrum network and is not for it.
const (
	logPrefix = "satbooth-hub-egress: "
	nftTable  = "satbooth_hub"
	nftSet    = "satbooth_hub_peers"
	iptChain  = "SATBOOTH_HUB_EGRESS"
	iptElec   = "SATBOOTH_HUB_ELECTRUM"
	iptPeers  = "SATBOOTH_HUB_PEERS"
)

// nftElements is "a . p, b . q" for peers already checked by ParseLightningPeers.
func nftElements(peers []string) string {
	var e []string
	for _, s := range peers {
		ap := netip.MustParseAddrPort(s)
		e = append(e, fmt.Sprintf("%s . %d", ap.Addr(), ap.Port()))
	}
	return strings.Join(e, ", ")
}

// EgressNft is the nftables rule set for the answers: a table of its own, a set of peers (address . port), and a
// forward chain before Docker's that only drops (what it accepts, Docker's own rules still judge). The file can be
// loaded again: it rebuilds the chain, keeps the set and its elements, and adds the answers' peers if they are missing.
func EgressNft(a *HubAnswers) string {
	ip := a.HubIP
	var b strings.Builder
	w := func(f string, args ...any) { fmt.Fprintf(&b, f+"\n", args...) }
	w("# SatBooth hub host: what the hub's network namespace may send out. Load as root: nft -f <this file>")
	w("# The namespace (hub, relay, UI proxy) has the address %s. It may reach the bundle's own two networks, answer", ip)
	w("# connections made to it, and open TCP connections to the Lightning peers in the set %s, and nothing else:", nftSet)
	w("# everything else it sends is logged and dropped. bitcoind is another container and is not bound. DNS is not covered here:")
	w("# Docker's resolver sends its queries from the host, not from the namespace; the bundle's netns service sets dns: for that.")
	w("# What leaves with an address on the electrum network (%s) for anywhere outside it is dropped too.", a.ElectrumSubnet)
	w("# Add a peer BEFORE connecting to it or opening a channel with it, and remove it when it is gone:")
	w("#   nft add element inet %s %s { 203.0.113.7 . 9735 }", nftTable, nftSet)
	w("#   nft delete element inet %s %s { 203.0.113.7 . 9735 }", nftTable, nftSet)
	w("# List them: nft list set inet %s %s", nftTable, nftSet)
	w("add table inet %s", nftTable)
	w("add set inet %s %s { type ipv4_addr . inet_service; }", nftTable, nftSet)
	if len(a.LightningPeers) > 0 {
		w("add element inet %s %s { %s }", nftTable, nftSet, nftElements(a.LightningPeers))
	}
	w("add chain inet %s egress { type filter hook forward priority -10; policy accept; }", nftTable)
	w("flush chain inet %s egress", nftTable)
	for _, r := range []string{
		"ct state established,related accept",
		fmt.Sprintf("ip daddr { %s, %s } accept", a.HubSubnet, a.ElectrumSubnet),
		fmt.Sprintf("ip daddr . tcp dport @%s accept", nftSet),
		fmt.Sprintf(`limit rate 5/minute burst 5 packets log prefix "%s"`, logPrefix),
		"counter drop",
	} {
		w("add rule inet %s egress ip saddr %s %s", nftTable, ip, r)
	}
	for _, r := range []string{
		"ct state established,related accept",
		fmt.Sprintf("ip daddr %s accept", a.ElectrumSubnet),
		fmt.Sprintf(`limit rate 5/minute burst 5 packets log prefix "%s"`, logPrefix),
		"counter drop",
	} {
		w("add rule inet %s egress ip saddr %s %s", nftTable, a.ElectrumSubnet, r)
	}
	return b.String()
}

// EgressIptables is the same policy for a host whose Docker uses iptables-legacy: a chain of its own that DOCKER-USER
// jumps to for the namespace's address, and a chain of peers (SATBOOTH_HUB_PEERS) that holds one ACCEPT rule for each
// peer. Allowed packets to the two networks RETURN, so Docker's own rules still judge them; a packet to a listed peer
// is accepted there, which it would be anyway on its way out to the internet. IPT names the binary (iptables-legacy by
// default). Running the script again replaces the egress chains in one step (never an empty chain behind the jump),
// keeps the peers chain, adds the answers' peers that are missing, and adds each jump once. A second chain
// (SATBOOTH_HUB_ELECTRUM) holds what comes from the electrum network. A peer is ACCEPTed in its chain, which ends the
// traversal of DOCKER-USER: rules of the operator's after the jump do not see the packets to a listed peer.
func EgressIptables(a *HubAnswers) string {
	var b strings.Builder
	w := func(f string, args ...any) { fmt.Fprintf(&b, f+"\n", args...) }
	w("#!/bin/sh")
	w("# SatBooth hub host: what the hub's network namespace may send out. Run as root: sh <this file>")
	w("# The namespace (hub, relay, UI proxy) has the address %s. It may reach the bundle's own two networks, answer", a.HubIP)
	w("# connections made to it, and open TCP connections to the Lightning peers in the chain %s, and nothing else:", iptPeers)
	w("# everything else it sends is logged and dropped. bitcoind is another container and is not bound. DNS is not covered here:")
	w("# Docker's resolver sends its queries from the host, not from the namespace; the bundle's netns service sets dns: for that.")
	w("# What leaves with an address on the electrum network (%s) for anywhere outside it is dropped too.", a.ElectrumSubnet)
	w("# Add a peer BEFORE connecting to it or opening a channel with it, and remove it when it is gone:")
	w("#   iptables-legacy -w -A %s -d 203.0.113.7 -p tcp --dport 9735 -j ACCEPT", iptPeers)
	w("#   iptables-legacy -w -D %s -d 203.0.113.7 -p tcp --dport 9735 -j ACCEPT", iptPeers)
	w("# List them: iptables-legacy -w -S %s", iptPeers)
	w("set -eu")
	w(`IPT=${IPT:-iptables-legacy}`)
	w(`"$IPT" -w -N %s 2>/dev/null || true`, iptPeers)
	for _, s := range a.LightningPeers {
		ap := netip.MustParseAddrPort(s)
		w(`"$IPT" -w -C %[1]s -d %[2]s -p tcp --dport %[3]d -j ACCEPT 2>/dev/null || "$IPT" -w -A %[1]s -d %[2]s -p tcp --dport %[3]d -j ACCEPT`,
			iptPeers, ap.Addr(), ap.Port())
	}
	// The two chains are written by one iptables-restore --noflush, which replaces them in a single step: a rerun has no
	// moment with an empty chain behind a live jump. The peers chain is not in it (it keeps what was added by hand).
	w(`"${IPT}-restore" --noflush -w <<'RULES'`)
	w("*filter")
	w(":%s - [0:0]", iptChain)
	w("-F %s", iptChain)
	w("-A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN", iptChain)
	w("-A %s -d %s -j RETURN", iptChain, a.HubSubnet)
	w("-A %s -d %s -j RETURN", iptChain, a.ElectrumSubnet)
	w("-A %s -p tcp -j %s", iptChain, iptPeers)
	w(`-A %s -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "%s"`, iptChain, logPrefix)
	w("-A %s -j DROP", iptChain)
	w(":%s - [0:0]", iptElec)
	w("-F %s", iptElec)
	w("-A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN", iptElec)
	w("-A %s -d %s -j RETURN", iptElec, a.ElectrumSubnet)
	w(`-A %s -m limit --limit 5/minute --limit-burst 5 -j LOG --log-prefix "%s"`, iptElec, logPrefix)
	w("-A %s -j DROP", iptElec)
	w("COMMIT")
	w("RULES")
	w(`"$IPT" -w -C DOCKER-USER -s %[1]s -j %[2]s 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s %[1]s -j %[2]s`, a.HubIP, iptChain)
	w(`"$IPT" -w -C DOCKER-USER -s %[1]s -j %[2]s 2>/dev/null || "$IPT" -w -I DOCKER-USER 1 -s %[1]s -j %[2]s`, a.ElectrumSubnet, iptElec)
	return b.String()
}

// HubEgressRules reads the hub host's answers and returns both rule sets. It writes nothing and runs nothing.
func (l *Lock) HubEgressRules() (nft, iptables string, err error) {
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
	if _, err := ParseLightningPeers(a.LightningPeers); err != nil {
		return "", "", errors.New("setup: lightning_peers in setup/state.json: " + err.Error())
	}
	return EgressNft(a), EgressIptables(a), nil
}
