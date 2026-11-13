package setup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
)

type PublicToHub struct {
	Version           int    `json:"version"`             // 1
	Network           string `json:"network"`             // mainnet | signet | regtest
	HubPrivateAddr    string `json:"hub_private_addr"`    // an IP address
	PublicPrivateAddr string `json:"public_private_addr"` // an IP address
	InvoiceClient     string `json:"invoice_client"`      // 64 lowercase hex
	PayoutClient      string `json:"payout_client"`       // 64 lowercase hex, or "" (no refunds)
	SatboothRPCAuth   string `json:"satbooth_rpcauth"`    // an rpcauth line for user satbooth, or "" (no on-chain)
	Monero            bool   `json:"monero"`
	MoneroRPCUser     string `json:"monero_rpc_user"` // [A-Za-z0-9._-]{1,64} when Monero is on, else ""
}

type HubToPublic struct {
	Version       int    `json:"version"`        // 1
	InvoiceWallet string `json:"invoice_wallet"` // 64 lowercase hex
	PayoutWallet  string `json:"payout_wallet"`  // 64 lowercase hex, or ""
}

var (
	ErrHandoffSize    = errors.New("setup: the handoff file is empty or larger than 4096 bytes")
	ErrHandoffFormat  = errors.New("setup: the handoff file is not a handoff of this version")
	ErrHandoffKind    = errors.New("setup: this is the other host's handoff file")
	ErrHandoffContent = errors.New("setup: the handoff file holds a value that is not allowed")
)

const maxHandoff = 4096

// Marshal returns the canonical bytes: the keys in the order of the struct, two-space indent, one final newline.
func (h *PublicToHub) Marshal() []byte { return marshal(h) }
func (h *HubToPublic) Marshal() []byte { return marshal(h) }

func marshal(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil { // a struct of strings, an int and a bool cannot fail
		panic("setup: " + err.Error())
	}
	return append(b, '\n')
}

// ParsePublicToHub and ParseHubToPublic accept only what Marshal can produce in content: at most 4096 bytes, one
// JSON object, every key present exactly once, no other key, version 1, every field under its rule, the two client
// keys different, the two wallet keys different. Each refusal is one of the fixed errors.
func ParsePublicToHub(b []byte) (*PublicToHub, error) {
	h, err := parsePublicToHub(b)
	if err != nil && err != ErrHandoffSize {
		if _, other := parseHubToPublic(b); other == nil {
			return nil, ErrHandoffKind
		}
	}
	return h, err
}

func ParseHubToPublic(b []byte) (*HubToPublic, error) {
	h, err := parseHubToPublic(b)
	if err != nil && err != ErrHandoffSize {
		if _, other := parsePublicToHub(b); other == nil {
			return nil, ErrHandoffKind
		}
	}
	return h, err
}

func parsePublicToHub(b []byte) (*PublicToHub, error) {
	var h PublicToHub
	if err := strictObject(b, &h, "version", "network", "hub_private_addr", "public_private_addr", "invoice_client",
		"payout_client", "satbooth_rpcauth", "monero", "monero_rpc_user"); err != nil {
		return nil, err
	}
	if h.Version != 1 {
		return nil, ErrHandoffFormat
	}
	switch h.Network {
	case "mainnet", "signet", "regtest":
	default:
		return nil, ErrHandoffContent
	}
	if linkAddr(h.HubPrivateAddr, h.Network) != nil || linkAddr(h.PublicPrivateAddr, h.Network) != nil ||
		sameAddr(h.HubPrivateAddr, h.PublicPrivateAddr) {
		return nil, ErrHandoffContent
	}
	if !isHex(h.InvoiceClient, 64) || (h.PayoutClient != "" && !isHex(h.PayoutClient, 64)) || h.InvoiceClient == h.PayoutClient {
		return nil, ErrHandoffContent
	}
	if h.SatboothRPCAuth != "" && !satboothRPCAuth(h.SatboothRPCAuth) {
		return nil, ErrHandoffContent
	}
	if h.Monero != (h.MoneroRPCUser != "") || (h.Monero && !validName(h.MoneroRPCUser)) {
		return nil, ErrHandoffContent
	}
	return &h, nil
}

func parseHubToPublic(b []byte) (*HubToPublic, error) {
	var h HubToPublic
	if err := strictObject(b, &h, "version", "invoice_wallet", "payout_wallet"); err != nil {
		return nil, err
	}
	if h.Version != 1 {
		return nil, ErrHandoffFormat
	}
	if !isHex(h.InvoiceWallet, 64) || (h.PayoutWallet != "" && !isHex(h.PayoutWallet, 64)) || h.InvoiceWallet == h.PayoutWallet {
		return nil, ErrHandoffContent
	}
	return &h, nil
}

// linkAddr is the one rule for a private-link address: s must be an IPv4 literal as net/netip reads it (so no
// IPv6 of any form, no IPv4-mapped form, no zone, no port, prefix or brackets), and not in 0.0.0.0/8 (this network,
// the unspecified address), 224.0.0.0/4 (multicast), 240.0.0.0/4 (reserved, the broadcast address included) or
// 169.254.0.0/16 (link-local). Loopback (127.0.0.0/8) is accepted only when network is "regtest". The error never
// holds s unless s is a well-formed address.
func linkAddr(s, network string) error {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return errors.New("setup: not an IP address")
	}
	if !a.Is4() {
		return errors.New("setup: " + a.String() + " is not an IPv4 address")
	}
	if a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.As4()[0] == 0 || a.As4()[0] >= 240 {
		return errors.New("setup: " + s + " is a reserved address")
	}
	if a.IsLoopback() && network != "regtest" {
		return errors.New("setup: " + s + " is a loopback address, allowed on regtest only")
	}
	return nil
}

// sameAddr reports whether two valid address literals name the same address. A handoff only compares IPv4 values;
// for other forms it compares the parsed values as they are (an IPv4-mapped address is not unmapped).
func sameAddr(x, y string) bool {
	a, errA := netip.ParseAddr(x)
	b, errB := netip.ParseAddr(y)
	return errA == nil && errB == nil && a == b
}

// satboothRPCAuth reports whether s is exactly rpcauth=satbooth:<32 lowercase hex>$<64 lowercase hex>.
func satboothRPCAuth(s string) bool {
	rest, ok := strings.CutPrefix(s, "rpcauth=satbooth:")
	if !ok {
		return false
	}
	salt, mac, ok := strings.Cut(rest, "$")
	return ok && isHex(salt, 32) && isHex(mac, 64)
}

// strictObject decodes a handoff: b must be exactly one JSON object of at most 4096 bytes in which every key in keys is
// present exactly once, matched exactly (the case too), and no other key is. See strictWalk.
func strictObject(b []byte, v any, keys ...string) error {
	return strictWalk(b, v, maxHandoff, keys, nil, nil)
}

// strictWalk decodes b, which must be exactly one JSON object of at most max bytes, into v. Every key in required
// must be present, every key in optional may be; each is matched exactly (the case too), appears at most once and
// no other key is allowed. A null value is refused except for the keys in nullable (null would leave a field at its
// zero value, as an absent key does). Nothing but white space may follow the object. Size and syntax problems are
// ErrHandoffSize and ErrHandoffFormat. The walk by tokens is how service/config.go refuses unknown and duplicate
// keys; the state file and the journal use it too.
func strictWalk(b []byte, v any, max int, required, optional, nullable []string) error {
	if len(b) == 0 || len(b) > max {
		return ErrHandoffSize
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ErrHandoffFormat
	}
	want := map[string]bool{}
	for _, k := range required {
		want[k] = true
	}
	for _, k := range optional {
		want[k] = true
	}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return ErrHandoffFormat
		}
		key, ok := kt.(string)
		if !ok || !want[key] || seen[key] {
			return ErrHandoffFormat
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil || string(skip) == "null" && !slices.Contains(nullable, key) {
			return ErrHandoffFormat
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return ErrHandoffFormat
	}
	if _, err := dec.Token(); err != io.EOF { // nothing may follow the object
		return ErrHandoffFormat
	}
	for _, k := range required {
		if !seen[k] {
			return ErrHandoffFormat
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil { // a value of the wrong type
		return ErrHandoffFormat
	}
	return nil
}

// Digest returns the SHA-256 of b as eight groups of eight lowercase hex characters separated by spaces.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	parts := make([]string, 8)
	for i := range parts {
		parts[i] = h[i*8 : i*8+8]
	}
	return strings.Join(parts, " ")
}
