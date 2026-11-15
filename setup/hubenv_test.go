package setup

import (
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
		"BOLTZ_API":   "https://hubstub",
		"MEMPOOL_API": "https://hubstub/api",
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
		for _, bad := range []string{"getalby", "getalbypro", "mempool.space", "boltz.exchange", "megalithic"} {
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
