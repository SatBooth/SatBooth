package setup

import (
	"math/rand"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// hubService is the text of the hub service in a Compose file: from its "  hub:" line to the next service.
func hubService(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "\n  hub:\n")
	if i < 0 {
		t.Fatalf("%s has no hub service", path)
	}
	s = s[i+1:]
	if j := strings.Index(s, "\n  relay:\n"); j >= 0 {
		s = s[:j]
	}
	return s
}

// hubEnvironment is the hub service's environment: block as key to value, comments and quotes dropped.
func hubEnvironment(t *testing.T, path string) map[string]string {
	t.Helper()
	env := map[string]string{}
	in := false
	for _, l := range strings.Split(hubService(t, path), "\n") {
		switch {
		case l == "    environment:":
			in = true
		case in && strings.HasPrefix(l, "      ") && !strings.HasPrefix(strings.TrimSpace(l), "#"):
			k, v, ok := strings.Cut(strings.TrimSpace(l), ": ")
			if !ok {
				t.Fatalf("environment line %q", l)
			}
			if _, dup := env[k]; dup {
				t.Errorf("%s is set twice", k)
			}
			env[k] = strings.Trim(v, `"`)
		case in && !strings.HasPrefix(l, "      ") && !strings.HasPrefix(strings.TrimSpace(l), "#") && l != "":
			in = false
		}
	}
	return env
}

// Every setting of the hub's environment that turns off or redirects one of Alby's defaults, each by value.
func TestHubEnvironmentDisablesOutsideDefaults(t *testing.T) {
	env := hubEnvironment(t, "../deploy/hub/compose.yml")
	for k, want := range map[string]string{
		// The swap provider and the mempool API are the bundle's own stub (the hubstub service); a non-empty BOLTZ_API
		// is also what stops the hub asking getalby.com which swap provider to use.
		"BOLTZ_API":             "https://hubstub",
		"REBALANCE_SERVICE_URL": "https://hubstub",
		"LDK_VSS_URL":           "https://hubstub",
		"MEMPOOL_API":           "https://hubstub/api",
		// An empty value would not disable the hub's built-in liquidity provider; a non-empty one it cannot parse does.
		"LDK_LSPS2_ADDRESSES": "none",
		// Empty: no rapid gossip sync server, gossip from connected peers only.
		"LDK_GOSSIP_SOURCE":      "",
		"SEND_EVENTS_TO_ALBY":    "false",
		"AUTO_LINK_ALBY_ACCOUNT": "false",
		"HIDE_UPDATE_BANNER":     "true",
		// Only the bundle's own certificate authority; the directory setting stops the system roots being read.
		"SSL_CERT_FILE":       "/etc/satbooth/ca.pem",
		"SSL_CERT_DIR":        "/etc/satbooth/no-roots",
		"LDK_ELECTRUM_SERVER": "${HUB_ELECTRUM_SERVER:?set HUB_ELECTRUM_SERVER}",
		"RELAY":               "ws://127.0.0.1:7447",
	} {
		got, ok := env[k]
		if !ok {
			t.Errorf("the hub's environment does not set %s", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// No setting of the hub's environment names a host of Alby's, of mempool.space, of Boltz or of Megalith.
func TestHubEnvironmentNamesNoAlbyHost(t *testing.T) {
	for k, v := range hubEnvironment(t, "../deploy/hub/compose.yml") {
		for _, bad := range []string{"getalby", "mempool.space", "boltz", "satsrouting", "megalith", "second.tech", "albylabs"} {
			if strings.Contains(v, bad) {
				t.Errorf("%s = %q names %s", k, v, bad)
			}
		}
	}
}

// The hub image is the release pinned by digest, in the bundle and in the regtest hubs alike.
func TestHubImagePinsAgree(t *testing.T) {
	const want = "ghcr.io/getalby/hub:v1.24.7@sha256:7e3de6dac330fb8a426c02a247d81ebaf07deece22634e97d8f20dd48c3a8e38"
	b, err := os.ReadFile("../deploy/hub/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "    image: "+want+"\n") {
		t.Error("the bundle's hub image is not the v1.24.7 pin")
	}
	r, err := os.ReadFile("../dev/regtest/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(r), "image: "+want+"\n"); n != 2 {
		t.Errorf("%d regtest hubs run the v1.24.7 pin, want 2", n)
	}
	for _, f := range [][]byte{b, r} {
		if strings.Contains(string(f), "getalby/hub:v1.24.6") {
			t.Error("a compose file still pins v1.24.6")
		}
	}
}

// serviceText is the text of one service of a Compose file: from its "  <name>:" line to the next service or the
// top-level key after the services.
func serviceText(t *testing.T, path, name string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "\n  "+name+":\n")
	if i < 0 {
		t.Fatalf("%s has no service %s", path, name)
	}
	lines := strings.SplitAfter(s[i+1:], "\n")
	out := lines[0]
	for _, l := range lines[1:] {
		if l != "\n" && !strings.HasPrefix(l, "   ") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			break
		}
		out += l
	}
	return out
}

// The names pinned to the stub are exactly the names of its certificate, each to HUB_STUB_IP; the stub is on the
// internal network alone with its own files read-only, at 443 as a non-root user through the namespaced sysctl; the hub
// mounts the authority's certificate at SSL_CERT_FILE and nothing else of the authority's.
func TestHubStubWiring(t *testing.T) {
	const c = "../deploy/hub/compose.yml"
	netns := serviceText(t, c, "netns")
	var pinned []string
	for _, l := range strings.Split(netns, "\n") {
		if n, ok := strings.CutPrefix(strings.TrimSpace(l), `- "`); ok && strings.Contains(n, ":${HUB_STUB_IP:?") {
			name, rest, _ := strings.Cut(n, ":")
			if rest != "${HUB_STUB_IP:?set HUB_STUB_IP}\"" {
				t.Errorf("%s is pinned to %s", name, rest)
			}
			pinned = append(pinned, name)
		}
	}
	if strings.Join(pinned, ",") != strings.Join(StubNames(), ",") {
		t.Errorf("pinned names %v, want %v", pinned, StubNames())
	}
	// Docker's resolver sends queries for unpinned names from the host: the namespace's only upstream is the stub's
	// address (which has no DNS port), set on netns alone (the hub, the relay and the proxy share its resolver).
	if !strings.Contains(netns, "dns:\n      - \"${HUB_STUB_IP:?set HUB_STUB_IP}\"\n") {
		t.Error("netns lacks dns: [HUB_STUB_IP]: unpinned names would go to the host's resolver")
	}
	for _, svc := range []string{"hub", "relay", "hubproxy", "hubstub"} {
		if strings.Contains(serviceText(t, c, svc), "\n    dns:") {
			t.Errorf("%s sets dns: it shares or has no resolver of its own to set", svc)
		}
	}
	if strings.Contains(serviceText(t, c, "hub"), "extra_hosts") {
		t.Error("the hub shares netns's hosts file: extra_hosts on it is refused by Docker")
	}

	// The egress rules are IPv4 only: the two networks the namespace is on must not get IPv6 from the daemon's default.
	raw, err := os.ReadFile(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"hub", "electrum"} {
		_, rest, _ := strings.Cut(string(raw), "\nnetworks:\n")
		_, blk, ok := strings.Cut(rest, "\n  "+n+":\n")
		if !ok {
			t.Fatalf("no %s network", n)
		}
		blk, _, _ = strings.Cut("\n"+blk, "\n  # ")
		blk, _, _ = strings.Cut(blk, "\nvolumes:")
		if !strings.Contains(blk, "\n    enable_ipv6: false\n") {
			t.Errorf("the %s network does not set enable_ipv6: false", n)
		}
	}

	stub := serviceText(t, c, "hubstub")
	for _, want := range []string{
		"image: *relay_image", `entrypoint: ["/satbooth-hubstub"]`, "net.ipv4.ip_unprivileged_port_start", "<<: *hardening",
		"/stub-cert.pem\n        target: /etc/satbooth/stub-cert.pem\n        read_only: true",
		"/stub-key.pem\n        target: /etc/satbooth/stub-key.pem\n        read_only: true",
		"ipv4_address: ${HUB_STUB_IP:?set HUB_STUB_IP}",
	} {
		if !strings.Contains(stub, want) {
			t.Errorf("the stub service lacks %q", want)
		}
	}
	for _, bad := range []string{"ports:", "cap_add", "user: \"0", "hub-ca-key", "ca-key", "network_mode", "hub:\n", "privileged:"} {
		if strings.Contains(stub, bad) {
			t.Errorf("the stub service has %q", bad)
		}
	}
	if !strings.Contains(serviceText(t, c, "relay"), "image: &relay_image ghcr.io/satbooth/satbooth-relay:") {
		t.Error("the relay does not carry the anchor of the one pin line")
	}
	if n := strings.Count(mustRead(t, c), "ghcr.io/satbooth/"); n != 1 {
		t.Errorf("%d pin lines of the SatBooth namespace in the hub bundle, want 1", n)
	}
	// Nothing is mounted over the directory that stands for "no system roots".
	if strings.Contains(mustRead(t, c), "target: /etc/satbooth/no-roots") {
		t.Error("a volume targets /etc/satbooth/no-roots")
	}

	hub := serviceText(t, c, "hub")
	if !strings.Contains(hub, "/hub-ca.pem\n        target: /etc/satbooth/ca.pem\n        read_only: true") {
		t.Error("the hub does not mount hub-ca.pem read-only at SSL_CERT_FILE")
	}
	if env := hubEnvironment(t, c); env["SSL_CERT_FILE"] != "/etc/satbooth/ca.pem" || strings.Contains(hub, "ca-key") ||
		strings.Contains(hub, "stub-") {
		t.Error("the hub is given more than the authority's certificate")
	}

	proxy := serviceText(t, c, "hubproxy")
	for _, want := range []string{"image: *relay_image", `entrypoint: ["/satbooth-hubproxy"]`, "network_mode: service:netns",
		`"-listen", ":8080", "-upstream", "http://127.0.0.1:8081"`, "<<: *hardening"} {
		if !strings.Contains(proxy, want) {
			t.Errorf("the proxy service lacks %q", want)
		}
	}
	if strings.Contains(proxy, "ports:") || strings.Contains(proxy, "cap_add") {
		t.Error("the proxy publishes a port or adds a capability")
	}
	// The hub moves off the published port: the proxy is the only thing on it.
	if env := hubEnvironment(t, c); env["PORT"] != "8081" {
		t.Errorf("the hub's PORT is %q, want 8081", env["PORT"])
	}
	if !strings.Contains(netns, `"${HUB_ADMIN_ADDR:?set HUB_ADMIN_ADDR}:8080:8080"`) {
		t.Error("netns no longer publishes 8080 on HUB_ADMIN_ADDR")
	}
	if b := serviceText(t, "../deploy/hub/compose.build.yml", "hubproxy"); !strings.Contains(b, "target: satbooth-relay") {
		t.Error("compose.build.yml does not build the proxy")
	}

	vi := serviceText(t, c, "volume-init")
	for _, want := range []string{
		"chown 10003:10003 /config/hub-ca.pem", "chown 65532:65532 /config/stub-cert.pem /config/stub-key.pem"} {
		if !strings.Contains(vi, want) {
			t.Errorf("volume-init lacks %q", want)
		}
	}
	b := serviceText(t, "../deploy/hub/compose.build.yml", "hubstub")
	if !strings.Contains(b, "target: satbooth-relay") || !strings.Contains(b, "image: satbooth-relay:local") {
		t.Error("compose.build.yml does not build the stub from the relay image")
	}
}

// The stub's address is the last host address of the internal network, inside it, and never the gateway.
func TestStubIP(t *testing.T) {
	for in, want := range map[string]string{"172.30.78.128/25": "172.30.78.254", "10.3.0.0/24": "10.3.0.254",
		"192.168.0.0/29": "192.168.0.6", "10.0.0.0/16": "10.0.255.254"} {
		if got := StubIP(in); got != want {
			t.Errorf("StubIP(%s) = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"", "x", "fd00::/64", "10.0.0.0/30"} {
		if StubIP(in) != "" {
			t.Errorf("StubIP(%q) is not empty", in)
		}
	}
	r := rand.New(rand.NewSource(5))
	for i := 0; i < 500; i++ {
		bits := 16 + r.Intn(14)
		a := netip.AddrFrom4([4]byte{10, byte(r.Intn(256)), byte(r.Intn(256)), 0})
		p, _ := a.Prefix(bits)
		ip := netip.MustParseAddr(StubIP(p.String()))
		if !p.Contains(ip) || ip == p.Addr() || ip == p.Addr().Next() || ip == lastAddr(p) {
			t.Fatalf("%s: stub address %s", p, ip)
		}
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
