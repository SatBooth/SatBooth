//go:build qualify

// Package qualify holds the regtest qualification suite (scripts/qualify.sh, dev/regtest/compose.yml): SatBooth's
// relay and NWC client against two real Alby Hub containers. The build tag keeps it out of every normal build
// and out of scripts/check.sh. Hub behaviour is pinned to getAlby/hub 921003b (image v1.24.6).
package qualify

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/relay"
)

// Fixed regtest-only values. They guard nothing outside the throwaway test network.
const (
	unlockPassword = "regtest-only-unlock"
	bitcoindURL    = "http://172.30.78.2:18443"
)

// Hub names and their addresses on the internal network (dev/regtest/compose.yml).
const (
	hubSatbooth = "satbooth"
	hubMerchant = "merchant"
)

var hubIP = map[string]string{hubSatbooth: "172.30.78.10", hubMerchant: "172.30.78.20"}

func hubURL(name string) string      { return "http://" + hubIP[name] + ":8080" }
func relayClientURL(n string) string { return "ws://" + hubIP[n] + ":7448" }

// Client key names. invoice and payout are SatBooth's two connections, merchant plays the merchant's wallet,
// stranger has no app on the hub. decoy is the client key of the app that owns the stranger's wallet key (the
// relay pairs one client with one wallet), and info is a spare connection with get_info and lookup_invoice that the
// crash phases replay a get_info request on, apart from the two connections a SatBooth service holds.
var keyNames = []string{"invoice", "payout", "merchant", "stranger", "decoy", "info"}

// hubState is what the suite remembers about one hub. The token is a regtest-only credential for a throwaway hub.
type hubState struct {
	URL    string `json:"url"`
	Token  string `json:"token"`
	NodeID string `json:"node_id"`
}

// state is /state/state.json (mode 0600).
type state struct {
	Hubs      map[string]*hubState `json:"hubs"`
	KeyFiles  map[string]string    `json:"key_files"`
	Wallets   map[string]string    `json:"wallets"` // connection name -> wallet public key
	RelayURLs map[string]string    `json:"relay_urls"`
	ChannelID string               `json:"channel_id"`
}

func stateDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("SATBOOTH_QUALIFY_STATE")
	if dir == "" {
		t.Fatal("SATBOOTH_QUALIFY_STATE is not set")
	}
	return dir
}

func (s *state) save(dir string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

func loadState(dir string) (*state, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Hubs == nil || s.KeyFiles == nil || s.Wallets == nil {
		return nil, errors.New("state.json is incomplete")
	}
	return &s, nil
}

func (s *state) key(t testing.TB, name string) *nostr.SecretKey {
	t.Helper()
	p, ok := s.KeyFiles[name]
	if !ok {
		t.Fatalf("no key file for %q in the state", name)
	}
	k, err := nostr.LoadSecretKeyFile(p)
	if err != nil {
		t.Fatalf("loading key %q: %v", name, shownErr(err))
	}
	return k
}

// writeRelayConfigs writes /state/relay-satbooth.json and /state/relay-merchant.json (the names compose.yml
// passes to the relays). Each pairs a hub connection's wallet key with its client key; the relay's config is
// public data (no secrets), so 0644 lets the relay container read it.
func writeRelayConfigs(dir string, pairs map[string][]relay.Pair) error {
	for name, ps := range pairs {
		cfg := relay.Config{
			HubListen:      "127.0.0.1:7447",
			ClientListen:   hubIP[name] + ":7448",
			ClientRelayURL: relayClientURL(name),
			Pairs:          ps,
		}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		if _, err := relay.ParseConfig(b); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "relay-"+name+".json"), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// imageUID is the numeric user the release images run as (deploy/image/Dockerfile, USER 65532:65532).
const imageUID = 65532

// writeImageFiles writes a satbooth container's files into dir (/state/image, which the container sees read-only at the
// same path, or /state/deploy for the public bundle): each secret file of secrets by name, then the configuration cfgName
// from cfg. dir is made
// 0700 and every file 0600, all owned by imageUID, as an operator prepares bind-mounted secrets for the image
// (docs/running.md
// "Secrets in Docker Compose"): SatBooth's loaders refuse anything looser. Each file is written beside its final name and
// renamed into place. No content is ever printed.
func writeImageFiles(dir, cfgName string, secrets map[string][]byte, cfg map[string]any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chown(dir, imageUID, imageUID); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for name, data := range secrets {
		if name == cfgName || filepath.Base(name) != name || name[0] == '.' {
			return errors.New("writeImageFiles: bad secret file name")
		}
		if err := writeImageFile(dir, name, data); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeImageFile(dir, cfgName, b)
}

func writeImageFile(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chown(tmp, imageUID, imageUID); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
