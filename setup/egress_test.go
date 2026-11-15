package setup

import (
	"math/rand"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestEgressPorts(t *testing.T) {
	got, err := EgressPorts([]int{10009, 9736, 9735, 10009})
	if err != nil || !slices.Equal(got, []int{9735, 9736, 10009}) {
		t.Errorf("%v %v", got, err)
	}
	if got, _ := EgressPorts(nil); !slices.Equal(got, []int{9735}) {
		t.Errorf("no peer ports: %v", got)
	}
	for _, bad := range [][]int{{0}, {-1}, {65536}, {53}, make([]int, 65)} {
		if _, err := EgressPorts(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// For random answers and ports: both rule sets name the namespace's address, both subnets and every port, never port
// 53, and the nft set opens only the forward chain's own drops.
func TestEgressRulesProperty(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	for i := 0; i < 300; i++ {
		a := &HubAnswers{HubIP: netip.AddrFrom4([4]byte{10, byte(r.Intn(256)), byte(r.Intn(256)), byte(2 + r.Intn(100))}).String(),
			HubSubnet: "10.1.2.0/25", ElectrumSubnet: "10.1.2.128/25"}
		var peers []int
		for j := r.Intn(5); j > 0; j-- {
			if p := 1 + r.Intn(65535); p != 53 {
				peers = append(peers, p)
			}
		}
		ports, err := EgressPorts(peers)
		if err != nil {
			t.Fatal(err)
		}
		nft, ipt := EgressNft(a, ports), EgressIptables(a, ports)
		for _, rules := range []string{nft, ipt} {
			for _, want := range []string{a.HubIP, a.HubSubnet, a.ElectrumSubnet, "satbooth-hub-egress: "} {
				if !strings.Contains(rules, want) {
					t.Fatalf("rules lack %q:\n%s", want, rules)
				}
			}
		}
		if !strings.Contains(nft, "tcp dport { "+joinInts(ports, ", ")+" } accept") ||
			!strings.Contains(ipt, "--dports "+joinInts(ports, ",")+" -j RETURN") {
			t.Fatalf("ports %v not in the rules", ports)
		}
		if slices.Contains(ports, 53) {
			t.Fatal("port 53 is in the list")
		}
		if strings.Contains(nft, "policy drop") || strings.Contains(ipt, "-P ") {
			t.Fatal("a default policy is changed: the rules are for the namespace's address only")
		}
	}
}

// The rules a hub host prints come from its state, write nothing, and refuse the public host and a missing state.
func TestHubEgressRulesFromState(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(1)
	lk, _ := LockDir(e.pubDir)
	if _, _, err := lk.HubEgressRules(nil); err == nil {
		t.Error("the public host printed hub rules")
	}
	lk.Close()
	e = newEnv(t, fxEveryRail(t))
	e.upTo(2)
	before := snapshot(t, e.hubDir)
	lk, _ = LockDir(e.hubDir)
	defer lk.Close()
	nft, ipt, err := lk.HubEgressRules([]int{10009})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nft, e.hub.HubIP) || !strings.Contains(ipt, e.hub.ElectrumSubnet) || !strings.Contains(nft, "9735, 10009") {
		t.Errorf("%s\n%s", nft, ipt)
	}
	if after := snapshot(t, e.hubDir); len(after) != len(before) {
		t.Error("the command wrote a file")
	}
	if _, _, err := lk.HubEgressRules([]int{53}); err == nil {
		t.Error("port 53 accepted")
	}
}
