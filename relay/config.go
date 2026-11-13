// Package relay is the NWC-only Nostr relay of SatBooth.
package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

// Config limits and defaults.
const (
	maxConfigBytes = 1 << 20
	maxClientURL   = 512
	MaxPairs       = 64

	minWindowSeconds, maxWindowSeconds, defWindowSeconds = 60, 21600, 3600
	minWindowEvents, maxWindowEvents, defWindowEvents    = 100, 1000000, 10000
	minWindowBytes, maxWindowBytes, defWindowBytes       = 1 << 20, 1 << 30, 1 << 26
)

// Pair joins one wallet key (the hub's per-connection key) to one client key (SatBooth's).
type Pair struct {
	Wallet string `json:"wallet"`
	Client string `json:"client"`
}

// Config is the relay's configuration file. It holds no secrets.
type Config struct {
	HubListen       string `json:"hub_listen"`
	ClientListen    string `json:"client_listen"`
	ClientRelayURL  string `json:"client_relay_url"`
	Pairs           []Pair `json:"pairs"`
	WindowSeconds   int    `json:"window_seconds"`
	WindowMaxEvents int    `json:"window_max_events"`
	WindowMaxBytes  int64  `json:"window_max_bytes"`
}

func (c *Config) lifetime() time.Duration { return time.Duration(c.WindowSeconds) * time.Second }

// ParseConfig parses and validates a configuration. Absent window keys get their defaults.
func ParseConfig(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("config: trailing data after the object")
	}
	if c.WindowSeconds == 0 {
		c.WindowSeconds = defWindowSeconds
	}
	if c.WindowMaxEvents == 0 {
		c.WindowMaxEvents = defWindowEvents
	}
	if c.WindowMaxBytes == 0 {
		c.WindowMaxBytes = defWindowBytes
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadConfig reads at most 1 MiB from path and parses it.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(b) > maxConfigBytes {
		return nil, errors.New("config: file is larger than 1 MiB")
	}
	return ParseConfig(b)
}

// PairByWallet returns the pair whose wallet key is pub.
func (c *Config) PairByWallet(pub string) (Pair, bool) {
	for _, p := range c.Pairs {
		if p.Wallet == pub {
			return p, true
		}
	}
	return Pair{}, false
}

// PairByClient returns the pair whose client key is pub.
func (c *Config) PairByClient(pub string) (Pair, bool) {
	for _, p := range c.Pairs {
		if p.Client == pub {
			return p, true
		}
	}
	return Pair{}, false
}

func splitPort(field, addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("config: %s is missing", field)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("config: %s must be host:port", field)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("config: %s has an invalid port", field)
	}
	return host, nil
}

// sameHostPort reports whether two host:port strings (already checked by splitPort) name the same address:
// equal ports and equal hosts, comparing IP literals by value rather than spelling.
func sameHostPort(a, b string) bool {
	ah, ap, _ := net.SplitHostPort(a)
	bh, bp, _ := net.SplitHostPort(b)
	if ap != bp {
		return false
	}
	ai, bi := net.ParseIP(ah), net.ParseIP(bh)
	if ai != nil && bi != nil {
		return ai.Equal(bi)
	}
	return ah == bh
}

func (c *Config) validate() error {
	host, err := splitPort("hub_listen", c.HubListen)
	if err != nil {
		return err
	}
	if host != "127.0.0.1" && host != "::1" {
		return errors.New("config: hub_listen must be on loopback (127.0.0.1 or ::1)")
	}
	clientHost, err := splitPort("client_listen", c.ClientListen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(clientHost); ip == nil || ip.IsUnspecified() {
		return errors.New("config: client_listen must be a specific IP address (not a hostname or wildcard)")
	}
	if sameHostPort(c.ClientListen, c.HubListen) {
		return errors.New("config: client_listen must differ from hub_listen")
	}
	if !isClientURL(c.ClientRelayURL) {
		return errors.New("config: client_relay_url must be a ws:// or wss:// URL of printable ASCII without spaces or backslashes, at most 512 bytes")
	}
	if len(c.Pairs) == 0 || len(c.Pairs) > MaxPairs {
		return fmt.Errorf("config: pairs must hold 1 to %d entries", MaxPairs)
	}
	wallets := map[string]bool{}
	clients := map[string]bool{}
	for i, p := range c.Pairs {
		if err := nostr.ValidatePubKey(p.Wallet); err != nil {
			return fmt.Errorf("config: pairs[%d].wallet is not a valid public key", i)
		}
		if err := nostr.ValidatePubKey(p.Client); err != nil {
			return fmt.Errorf("config: pairs[%d].client is not a valid public key", i)
		}
		if wallets[p.Wallet] {
			return fmt.Errorf("config: pairs[%d].wallet appears twice", i)
		}
		if clients[p.Client] {
			return fmt.Errorf("config: pairs[%d].client appears twice", i)
		}
		wallets[p.Wallet], clients[p.Client] = true, true
	}
	for i, p := range c.Pairs {
		if wallets[p.Client] {
			return fmt.Errorf("config: pairs[%d].client is also used as a wallet", i)
		}
		if clients[p.Wallet] {
			return fmt.Errorf("config: pairs[%d].wallet is also used as a client", i)
		}
	}
	if c.WindowSeconds < minWindowSeconds || c.WindowSeconds > maxWindowSeconds {
		return fmt.Errorf("config: window_seconds must be %d to %d", minWindowSeconds, maxWindowSeconds)
	}
	if c.WindowMaxEvents < minWindowEvents || c.WindowMaxEvents > maxWindowEvents {
		return fmt.Errorf("config: window_max_events must be %d to %d", minWindowEvents, maxWindowEvents)
	}
	if c.WindowMaxBytes < minWindowBytes || c.WindowMaxBytes > maxWindowBytes {
		return fmt.Errorf("config: window_max_bytes must be %d to %d", int64(minWindowBytes), int64(maxWindowBytes))
	}
	return nil
}

// isClientURL implements ^wss?://[!#-\[\]-~]+$ with a 512-byte cap. It repeats the nostr package's
// unexported AUTH relay check, so the two cannot disagree on what the configured URL may be.
func isClientURL(s string) bool {
	if len(s) > maxClientURL {
		return false
	}
	var rest string
	switch {
	case len(s) > 6 && s[:6] == "wss://":
		rest = s[6:]
	case len(s) > 5 && s[:5] == "ws://":
		rest = s[5:]
	default:
		return false
	}
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; c <= ' ' || c > '~' || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}
