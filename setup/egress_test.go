package setup

import (
	"fmt"
	"math/rand"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestParseLightningPeers(t *testing.T) {
	got, err := ParseLightningPeers([]string{"203.0.113.5:10009", "198.51.100.7:9735", "198.51.100.7:9000"})
	if err != nil || !slices.Equal(got, []string{"198.51.100.7:9000", "198.51.100.7:9735", "203.0.113.5:10009"}) {
		t.Errorf("%v %v", got, err)
	}
	if got, err := ParseLightningPeers(nil); err != nil || got != nil {
		t.Errorf("none: %v %v", got, err)
	}
	for _, bad := range []string{"", "example.com:9735", "203.0.113.5", "203.0.113.5:0", "203.0.113.5:65536", "203.0.113.5:-1",
		"203.0.113.5:09735", "203.0.113.05:9735", "[2001:db8::1]:9735", "2001:db8::1", "[::ffff:203.0.113.5]:9735", " 203.0.113.5:9735",
		"203.0.113.5:9735 ", "127.0.0.1:9735", "0.0.0.0:9735", "224.0.0.1:9735", "169.254.1.1:9735", "255.255.255.255:9735",
		"203.0.113.5:9735,198.51.100.7:9735", "203.0.113.5:9735\n", "203.0.113.5:x"} {
		if _, err := ParseLightningPeers([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseLightningPeers([]string{"203.0.113.5:9735", "203.0.113.5:9735"}); err == nil {
		t.Error("a repeated peer accepted")
	}
	big := make([]string, 257)
	for i := range big {
		big[i] = fmt.Sprintf("203.0.%d.%d:9735", i/250, i%250+1)
	}
	if _, err := ParseLightningPeers(big); err == nil {
		t.Error("257 peers accepted")
	}
}

// Seeded property: a public IPv4 address with any port from 1 to 65535 is accepted and comes back as it was written;
// any IPv6 literal, any hostname and any port outside the range is refused; the result is sorted and its elements are
// the input's.
func TestParseLightningPeersProperty(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	for i := 0; i < 3000; i++ {
		a := netip.AddrFrom4([4]byte{byte(1 + r.Intn(222)), byte(r.Intn(256)), byte(r.Intn(256)), byte(1 + r.Intn(254))})
		if reservedV4(a) || a.IsLoopback() {
			continue
		}
		port := 1 + r.Intn(65535)
		in := fmt.Sprintf("%s:%d", a, port)
		out, err := ParseLightningPeers([]string{in})
		if err != nil || len(out) != 1 || out[0] != in {
			t.Fatalf("%s: %v %v", in, out, err)
		}
		for _, bad := range []string{
			fmt.Sprintf("[2001:db8::%x]:%d", 1+r.Intn(60000), port),
			fmt.Sprintf("host%d.example.com:%d", r.Intn(1000), port),
			fmt.Sprintf("%s:%d", a, 65536+r.Intn(100000)),
			fmt.Sprintf("%s:0", a),
		} {
			if _, err := ParseLightningPeers([]string{bad}); err == nil {
				t.Fatalf("%q accepted", bad)
			}
		}
	}
	// A random list: sorted, same members.
	for i := 0; i < 200; i++ {
		var in []string
		seen := map[string]bool{}
		for j := r.Intn(8); j > 0; j-- {
			s := fmt.Sprintf("%d.%d.%d.%d:%d", 11+r.Intn(100), r.Intn(256), r.Intn(256), 1+r.Intn(254), 1+r.Intn(65535))
			if !seen[s] {
				seen[s] = true
				in = append(in, s)
			}
		}
		out, err := ParseLightningPeers(in)
		if err != nil || len(out) != len(in) {
			t.Fatalf("%v %v", in, err)
		}
		for _, s := range out {
			if !seen[s] {
				t.Fatalf("%s is not an input", s)
			}
		}
		if !slices.IsSortedFunc(out, func(x, y string) int {
			ax, ay := netip.MustParseAddrPort(x), netip.MustParseAddrPort(y)
			if c := ax.Addr().Compare(ay.Addr()); c != 0 {
				return c
			}
			return int(ax.Port()) - int(ay.Port())
		}) {
			t.Fatalf("not sorted: %v", out)
		}
	}
}

// The answers' peers reach the parser: a hostname, an IPv6 address or a bad port is refused naming the field.
func TestHubAnswersLightningPeers(t *testing.T) {
	base := `{"network":"mainnet","hub_private_addr":"192.0.2.20","public_private_addr":"192.0.2.10","host_dir":"/srv/h"`
	a, err := ParseHubAnswers([]byte(base + `,"lightning_peers":["203.0.113.5:10009","198.51.100.7:9735"]}`))
	if err != nil || !slices.Equal(a.LightningPeers, []string{"198.51.100.7:9735", "203.0.113.5:10009"}) {
		t.Fatalf("%v %v", a, err)
	}
	if a, err = ParseHubAnswers([]byte(base + `}`)); err != nil || a.LightningPeers != nil {
		t.Errorf("default: %v %v", a, err)
	}
	if a, err = ParseHubAnswers([]byte(base + `,"lightning_peers":[]}`)); err != nil || a.LightningPeers != nil {
		t.Errorf("empty: %v %v", a, err)
	}
	for _, bad := range []string{`["peer.example.com:9735"]`, `["[2001:db8::1]:9735"]`, `["203.0.113.5:0"]`, `"203.0.113.5:9735"`, `[1]`} {
		if _, err := ParseHubAnswers([]byte(base + `,"lightning_peers":` + bad + `}`)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// The rule sets for zero, one and several peers: the namespace's address is the only source, both networks are
// allowed, the peers are the only internet destinations, no port is allowed on its own and nothing opens port 53.
func TestEgressRulesShape(t *testing.T) {
	for _, peers := range [][]string{nil, {"203.0.113.5:10009"}, {"198.51.100.7:9735", "203.0.113.5:10009", "203.0.113.5:9735"}} {
		a := &HubAnswers{HubIP: "172.30.78.10", HubSubnet: "172.30.78.0/25", ElectrumSubnet: "172.30.78.128/25", LightningPeers: peers}
		nft, ipt := EgressNft(a), EgressIptables(a)
		for _, rules := range []string{nft, ipt} {
			for _, want := range []string{"172.30.78.10", "172.30.78.0/25", "172.30.78.128/25", "satbooth-hub-egress: ", "203.0.113.7"} {
				if !strings.Contains(rules, want) {
					t.Fatalf("rules lack %q:\n%s", want, rules)
				}
			}
			for _, bad := range []string{"policy drop", " -P ", "dport 9735 accept", "dport { 9735", "multiport", "dport 53", "dport 443"} {
				if strings.Contains(rules, bad) {
					t.Fatalf("rules hold %q:\n%s", bad, rules)
				}
			}
		}
		for _, p := range peers {
			ap := netip.MustParseAddrPort(p)
			if !strings.Contains(nft, fmt.Sprintf("%s . %d", ap.Addr(), ap.Port())) ||
				!strings.Contains(ipt, fmt.Sprintf("-d %s -p tcp --dport %d -j ACCEPT", ap.Addr(), ap.Port())) {
				t.Errorf("peer %s is not in both rule sets", p)
			}
		}
		if n := strings.Count(nft, "\nadd element inet satbooth_hub satbooth_hub_peers {"); (n == 1) != (len(peers) > 0) || n > 1 {
			t.Errorf("%d element lines for %d peers", n, len(peers))
		}
	}
}

// The rules a hub host prints come from its state, write nothing, include the answers' peers and refuse the public host.
func TestHubEgressRulesFromState(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(1)
	lk, _ := LockDir(e.pubDir)
	if _, _, err := lk.HubEgressRules(); err == nil {
		t.Error("the public host printed hub rules")
	}
	lk.Close()
	e = newEnv(t, fxEveryRail(t))
	e.hub.LightningPeers = []string{"203.0.113.5:10009"}
	e.upTo(2)
	before := snapshot(t, e.hubDir)
	lk, _ = LockDir(e.hubDir)
	defer lk.Close()
	nft, ipt, err := lk.HubEgressRules()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nft, e.hub.HubIP) || !strings.Contains(ipt, e.hub.ElectrumSubnet) || !strings.Contains(nft, "203.0.113.5 . 10009") {
		t.Errorf("%s\n%s", nft, ipt)
	}
	if after := snapshot(t, e.hubDir); len(after) != len(before) {
		t.Error("the command wrote a file")
	}
}
