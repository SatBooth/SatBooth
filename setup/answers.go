package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/descriptor"
)

// maxAnswers is the largest answers file.
const maxAnswers = 16 << 10

// PublicAnswers is what the operator gives at stage 1. Parsed strictly (unknown and duplicate keys refused, at most
// 16 KiB). The descriptor and the Monero address are checked for shape only, by the rules of service.ParseConfig.
// The package imports neither service nor any package that opens a connection or a database (it imports net for
// address formatting only), so each rule is mirrored here; TestAcceptedAnswersAreAcceptedByService holds the two
// together. With refunds on (approval_keys) the operator gives every cap: nothing about refunds is defaulted.
type PublicAnswers struct {
	Network           string   `json:"network"`             // mainnet | signet | regtest
	HostDir           string   `json:"host_dir"`            // this host's base directory: absolute, only [A-Za-z0-9/._-]
	HubPrivateAddr    string   `json:"hub_private_addr"`    // IP
	PublicPrivateAddr string   `json:"public_private_addr"` // IP, different from the hub's
	PublicAddr        string   `json:"public_addr"`         // IP the checkout is published on (default 127.0.0.1)
	PublicPort        int      `json:"public_port"`         // 1..65535 (default 8080)
	CheckoutBaseURL   string   `json:"checkout_base_url"`
	WebhookURL        string   `json:"webhook_url"`
	AlertsURL         string   `json:"alerts_url"`        // "" = no alerts block
	Descriptor        string   `json:"descriptor"`        // "" = no on-chain rail
	MoneroAddress     string   `json:"monero_address"`    // "" = no Monero rail
	MoneroNetwork     string   `json:"monero_network"`    // mainnet (the hub bundle runs Monero on mainnet only); "" without Monero
	ApprovalKeys      []string `json:"approval_keys"`     // empty = no refunds; else 1..8 Ed25519 public keys
	PayoutBudgetSat   uint64   `json:"payout_budget_sat"` // the payout connection's budget; required with refunds
	PayoutMaxSat      uint64   `json:"payout_max_sat"`    // the most one refund may pay; required with refunds
	PayoutDailySat    uint64   `json:"payout_daily_sat"`  // the most refunds may pay in a UTC day; required with refunds
	PayoutAlertSat    uint64   `json:"payout_alert_sat"`  // alert when the budget's headroom falls below it (0: no alert); the key is required with refunds
	Currencies        []string `json:"currencies"`        // empty = no rates block
}

// MarshalJSON writes the answers as ParsePublicAnswers reads them back: the four refund keys only with approval_keys
// (without refunds a present cap key is refused, even as 0), and all four with it (0 is a value for the alert).
func (a PublicAnswers) MarshalJSON() ([]byte, error) {
	type plain PublicAnswers
	// The outer fields shadow the embedded ones of the same JSON name (the shallower field wins).
	type tagged struct {
		*plain
		Budget *uint64 `json:"payout_budget_sat,omitempty"`
		Max    *uint64 `json:"payout_max_sat,omitempty"`
		Daily  *uint64 `json:"payout_daily_sat,omitempty"`
		Alert  *uint64 `json:"payout_alert_sat,omitempty"`
	}
	t := tagged{plain: (*plain)(&a)}
	if len(a.ApprovalKeys) > 0 {
		t.Budget, t.Max, t.Daily, t.Alert = &a.PayoutBudgetSat, &a.PayoutMaxSat, &a.PayoutDailySat, &a.PayoutAlertSat
	}
	return json.Marshal(t)
}

// HubAnswers is what the operator gives at stage 2.
type HubAnswers struct {
	// The first three are typed by the hub host's operator, never taken from the handoff: a handoff may not decide
	// the network or an address. CheckAgainst compares them with the handoff.
	Network           string `json:"network"`             // mainnet | signet | regtest
	HubPrivateAddr    string `json:"hub_private_addr"`    // IP
	PublicPrivateAddr string `json:"public_private_addr"` // IP, different from the hub's
	HostDir           string `json:"host_dir"`
	HubAdminAddr      string `json:"hub_admin_addr"`  // default 127.0.0.1; never the private or a public address
	HubSubnet         string `json:"hub_subnet"`      // default 172.30.78.0/25
	HubIP             string `json:"hub_ip"`          // default 172.30.78.10, inside HubSubnet
	ElectrumSubnet    string `json:"electrum_subnet"` // default 172.30.78.128/25, not overlapping HubSubnet
}

// The defaults of the optional answers.
const (
	defPublicAddr     = "127.0.0.1"
	defPublicPort     = 8080
	defHubAdminAddr   = "127.0.0.1"
	defHubSubnet      = "172.30.78.0/25"
	defHubIP          = "172.30.78.10"
	defElectrumSubnet = "172.30.78.128/25"
	maxURLBytes       = 512
	maxCurrencies     = 32
	// maxPayoutSat is the most a refund cap or the alert threshold may be: service takes at most 2^53 msat for each of
	// payout.max_msat, daily_msat and budget_alert_msat.
	maxPayoutSat = approval.MaxAmountMsat / 1000
	// nullSuffix marks, in the set strictAnswers returns, a key whose value was JSON null.
	nullSuffix = "\x00null"
)

// AnswerError is a refusal of one field of the answers. Rule never carries a value of the input.
type AnswerError struct {
	Field string // the JSON name of the field
	Rule  string // what the field must be, in words
}

func (e *AnswerError) Error() string { return "setup: answers: " + e.Field + " " + e.Rule }

// answerErr is a refusal that names a field and never carries a value.
func answerErr(field, rule string) error { return &AnswerError{Field: field, Rule: rule} }

// ParsePublicAnswers parses and validates the stage 1 answers. No error carries a value of the input.
func ParsePublicAnswers(b []byte) (*PublicAnswers, error) {
	var a PublicAnswers
	seen, err := strictAnswers(b, &a, "network", "host_dir", "hub_private_addr", "public_private_addr", "public_addr",
		"public_port", "checkout_base_url", "webhook_url", "alerts_url", "descriptor", "monero_address",
		"monero_network", "approval_keys", "payout_budget_sat", "payout_max_sat", "payout_daily_sat", "payout_alert_sat", "currencies")
	if err != nil {
		return nil, err
	}
	if !seen["public_addr"] {
		a.PublicAddr = defPublicAddr
	}
	if !seen["public_port"] {
		a.PublicPort = defPublicPort
	}
	switch a.Network {
	case "mainnet", "signet", "regtest":
	case "testnet":
		return nil, answerErr("network", "testnet is not supported here: set up testnet by hand (deploy/README.md)")
	default:
		return nil, answerErr("network", "must be mainnet, signet or regtest")
	}
	if err := checkHostDir(a.HostDir); err != nil {
		return nil, err
	}
	if err := checkPrivateAddr("hub_private_addr", a.HubPrivateAddr, a.Network); err != nil {
		return nil, err
	}
	if err := checkPrivateAddr("public_private_addr", a.PublicPrivateAddr, a.Network); err != nil {
		return nil, err
	}
	if sameAddr(a.HubPrivateAddr, a.PublicPrivateAddr) {
		return nil, answerErr("public_private_addr", "must differ from hub_private_addr")
	}
	bundle := netip.MustParsePrefix(satboothSubnet)
	for _, f := range [][2]string{{"hub_private_addr", a.HubPrivateAddr}, {"public_private_addr", a.PublicPrivateAddr}} {
		if ip, _ := netip.ParseAddr(f[1]); bundle.Contains(ip) {
			return nil, answerErr(f[0], "is inside the bundle's own Docker network ("+satboothSubnet+"): give the address of the private link")
		}
	}
	if err := checkPublicAddr(a.PublicAddr); err != nil {
		return nil, err
	}
	if a.PublicPort < 1 || a.PublicPort > 65535 {
		return nil, answerErr("public_port", "must be 1 to 65535")
	}
	if _, err := checkURL("checkout_base_url", a.CheckoutBaseURL, true, "http", "https"); err != nil {
		return nil, err
	}
	wu, err := checkURL("webhook_url", a.WebhookURL, false, "http", "https")
	if err != nil {
		return nil, err
	}
	if a.AlertsURL != "" {
		au, err := checkURL("alerts_url", a.AlertsURL, false, "http", "https")
		if err != nil {
			return nil, err
		}
		if sameEndpoint(au, wu) {
			return nil, answerErr("alerts_url", "must differ from webhook_url")
		}
	}
	if a.Descriptor != "" {
		d, err := descriptor.Parse(a.Descriptor)
		if err != nil { // the descriptor package's errors are sentinels; none is wrapped here, so none can echo input
			return nil, answerErr("descriptor", "is not a descriptor SatBooth accepts (wpkh or tr, one extended public key, /0/*; never a private key)")
		}
		if d.Testnet != (a.Network != "mainnet") {
			return nil, answerErr("descriptor", "does not match the network (xpub for mainnet, tpub otherwise)")
		}
	}
	if a.MoneroAddress == "" {
		if a.MoneroNetwork != "" {
			return nil, answerErr("monero_network", "is set without monero_address")
		}
	} else {
		switch a.MoneroNetwork {
		case "mainnet":
		case "":
			return nil, answerErr("monero_network", "is required with monero_address (mainnet)")
		case "stagenet", "testnet", "regtest":
			return nil, answerErr("monero_network", "is "+a.MoneroNetwork+": the hub bundle runs Monero on mainnet only: set up a stagenet or testnet monerod and wallet-rpc by hand "+
				"(docs/monero.md, \"Setting up monerod and monero-wallet-rpc (Monero)\") and add the monero block to satbooth.json")
		default:
			return nil, answerErr("monero_network", "must be mainnet: the hub bundle runs Monero on mainnet only")
		}
		if !moneroAddressShape(a.MoneroAddress) {
			return nil, answerErr("monero_address", "is not a mainnet primary address (95 base58 characters starting with 4)")
		}
	}
	capKeys := []struct {
		key string
		v   uint64
	}{{"payout_budget_sat", a.PayoutBudgetSat}, {"payout_max_sat", a.PayoutMaxSat}, {"payout_daily_sat", a.PayoutDailySat}, {"payout_alert_sat", a.PayoutAlertSat}}
	if len(a.ApprovalKeys) == 0 {
		for _, f := range capKeys {
			if seen[f.key] { // present, whatever its value (0 included): refunds are off
				return nil, answerErr(f.key, "is set without approval_keys")
			}
		}
	} else {
		if _, err := approval.NewKeys(a.ApprovalKeys); err != nil {
			return nil, answerErr("approval_keys", "must be 1 to 8 distinct Ed25519 public keys of 64 lowercase hex characters")
		}
		// Refunds are money leaving: every cap is the operator's to give, none is defaulted. The bounds are those of
		// service's payout block (budget_alert_msat, max_msat and daily_msat are each at most 2^53 msat, multiples of
		// 1000: whole sats), and the budget the hub's own (the largest whole msat amount; it is not written to
		// satbooth.json, only into the pairing link).
		for _, f := range capKeys {
			if seen[f.key+nullSuffix] {
				msg := "is required with approval_keys, not null"
				if f.key == "payout_alert_sat" {
					msg += " (give 0 for no alert)"
				}
				return nil, answerErr(f.key, msg)
			}
		}
		if a.PayoutBudgetSat < 1 || a.PayoutBudgetSat > maxAmountSat {
			return nil, answerErr("payout_budget_sat", "is required with approval_keys: 1 to the largest budget the hub takes")
		}
		if a.PayoutMaxSat < 1 || a.PayoutMaxSat > maxPayoutSat {
			return nil, answerErr("payout_max_sat", "is required with approval_keys: 1 to 9007199254740 sat")
		}
		if a.PayoutDailySat < 1 || a.PayoutDailySat > maxPayoutSat {
			return nil, answerErr("payout_daily_sat", "is required with approval_keys: 1 to 9007199254740 sat")
		}
		if !seen["payout_alert_sat"] || a.PayoutAlertSat > maxPayoutSat {
			return nil, answerErr("payout_alert_sat", "is required with approval_keys (0 for no alert): at most 9007199254740 sat")
		}
		if a.PayoutMaxSat > a.PayoutDailySat {
			return nil, answerErr("payout_max_sat", "must be at most payout_daily_sat")
		}
		if a.PayoutMaxSat > a.PayoutBudgetSat {
			return nil, answerErr("payout_max_sat", "must be at most payout_budget_sat")
		}
		if a.PayoutAlertSat >= a.PayoutBudgetSat {
			return nil, answerErr("payout_alert_sat", "must be below payout_budget_sat")
		}
	}
	if len(a.Currencies) > maxCurrencies {
		return nil, answerErr("currencies", "must hold at most 32 codes")
	}
	codes := map[string]bool{}
	for _, c := range a.Currencies {
		if !validCurrency(c) || codes[c] {
			return nil, answerErr("currencies", "must be distinct codes of 3 uppercase letters, not BTC or XMR")
		}
		codes[c] = true
	}
	return &a, nil
}

// ParseHubAnswers parses and validates the stage 2 answers.
func ParseHubAnswers(b []byte) (*HubAnswers, error) {
	var a HubAnswers
	seen, err := strictAnswers(b, &a, "network", "hub_private_addr", "public_private_addr", "host_dir", "hub_admin_addr", "hub_subnet", "hub_ip", "electrum_subnet")
	if err != nil {
		return nil, err
	}
	if !seen["hub_admin_addr"] {
		a.HubAdminAddr = defHubAdminAddr
	}
	if !seen["hub_subnet"] {
		a.HubSubnet = defHubSubnet
	}
	if !seen["hub_ip"] {
		a.HubIP = defHubIP
	}
	if !seen["electrum_subnet"] {
		a.ElectrumSubnet = defElectrumSubnet
	}
	switch a.Network {
	case "mainnet", "signet", "regtest":
	case "testnet":
		return nil, answerErr("network", "testnet is not supported here: set up testnet by hand (deploy/README.md)")
	default:
		return nil, answerErr("network", "must be mainnet, signet or regtest")
	}
	if err := checkPrivateAddr("hub_private_addr", a.HubPrivateAddr, a.Network); err != nil {
		return nil, err
	}
	if err := checkPrivateAddr("public_private_addr", a.PublicPrivateAddr, a.Network); err != nil {
		return nil, err
	}
	if sameAddr(a.HubPrivateAddr, a.PublicPrivateAddr) {
		return nil, answerErr("public_private_addr", "must differ from hub_private_addr")
	}
	if err := checkHostDir(a.HostDir); err != nil {
		return nil, err
	}
	hub, err := ipv4Prefix(a.HubSubnet)
	if err != nil {
		return nil, answerErr("hub_subnet", "must be an IPv4 network address in CIDR form with a prefix of /16 to /29, such as 172.30.78.0/25")
	}
	ele, err := ipv4Prefix(a.ElectrumSubnet)
	if err != nil {
		return nil, answerErr("electrum_subnet", "must be an IPv4 network address in CIDR form with a prefix of /16 to /29, such as 172.30.78.128/25")
	}
	if hub.Overlaps(ele) {
		return nil, answerErr("electrum_subnet", "must not overlap hub_subnet")
	}
	ip, err := netip.ParseAddr(a.HubIP)
	if err != nil || !ip.Is4() {
		return nil, answerErr("hub_ip", "must be an IPv4 address")
	}
	// A usable host address: not the network address, not the first host address (Docker's gateway), not the broadcast.
	first := hub.Addr().Next()
	last := lastAddr(hub)
	if !hub.Contains(ip) || ip == hub.Addr() || ip == first || ip == last {
		return nil, answerErr("hub_ip", "must be a host address inside hub_subnet, not its network, first (gateway) or broadcast address")
	}
	for _, f := range [][2]string{{"hub_private_addr", a.HubPrivateAddr}, {"public_private_addr", a.PublicPrivateAddr}} {
		pa, _ := netip.ParseAddr(f[1])
		if hub.Contains(pa) || ele.Contains(pa) {
			return nil, answerErr(f[0], "is inside the bundle's own Docker network (hub_subnet or electrum_subnet): give the address of the private link")
		}
	}
	admin, err := netip.ParseAddr(a.HubAdminAddr)
	if err != nil || !admin.Is4() || admin.Zone() != "" || reservedV4(admin) {
		return nil, answerErr("hub_admin_addr", "must be an IPv4 address (not a hostname, a zone, or a reserved, wildcard, multicast or link-local address)")
	}
	if !admin.IsLoopback() && !admin.IsPrivate() {
		return nil, answerErr("hub_admin_addr", "must be a loopback or a private address: the admin UI controls the node's funds")
	}
	if hub.Contains(admin) || ele.Contains(admin) {
		return nil, answerErr("hub_admin_addr", "must not be inside hub_subnet or electrum_subnet")
	}
	if sameAddr(a.HubAdminAddr, a.HubPrivateAddr) || sameAddr(a.HubAdminAddr, a.PublicPrivateAddr) {
		return nil, answerErr("hub_admin_addr", "must differ from hub_private_addr and public_private_addr: the admin UI controls the node's funds")
	}
	return &a, nil
}

// CheckAgainst checks the hub answers against the handoff of the public host. The two hosts' operators type the
// network and the two private addresses each: a handoff may not decide them, so a difference is a refusal, never a
// choice (the error wraps ErrMismatch and names the field). The admin UI is never published on the private link's address.
func (a *HubAnswers) CheckAgainst(in *PublicToHub) error {
	if in.Network != a.Network {
		return fmt.Errorf("%w: network", ErrMismatch)
	}
	if !sameAddr(a.HubPrivateAddr, in.HubPrivateAddr) {
		return fmt.Errorf("%w: hub_private_addr", ErrMismatch)
	}
	if !sameAddr(a.PublicPrivateAddr, in.PublicPrivateAddr) {
		return fmt.Errorf("%w: public_private_addr", ErrMismatch)
	}
	if sameAddr(a.HubAdminAddr, a.HubPrivateAddr) || sameAddr(a.HubAdminAddr, a.PublicPrivateAddr) {
		return answerErr("hub_admin_addr", "must differ from hub_private_addr and public_private_addr: the admin UI controls the node's funds")
	}
	return nil
}

// checkPrivateAddr decides with linkAddr, the one rule for a private-link address (handoff.go), and names the field.
func checkPrivateAddr(field, s, network string) error {
	if linkAddr(s, network) == nil {
		return nil
	}
	if a, err := netip.ParseAddr(s); err == nil && !a.Is4() {
		return answerErr(field, "is an IPv6 address: set up an IPv6 private link by hand (deploy/README.md)")
	}
	return answerErr(field, "must be an IPv4 address of the private link (no hostname, zone, wildcard, multicast, link-local, reserved or broadcast address; loopback only on regtest)")
}

// reservedV4 reports whether a is in a range no host address comes from: 0.0.0.0/8, 224.0.0.0/4 and above
// (multicast, reserved, broadcast) or link-local (the ranges linkAddr refuses).
func reservedV4(a netip.Addr) bool {
	return a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.As4()[0] == 0 || a.As4()[0] >= 240
}

// checkPublicAddr accepts the IPv4 address the checkout is published on: loopback or unicast, never a wildcard.
func checkPublicAddr(s string) error {
	a, err := netip.ParseAddr(s)
	if err == nil && a.Zone() == "" && !a.Is4() {
		return answerErr("public_addr", "is an IPv6 address: set up an IPv6 listener by hand (deploy/README.md)")
	}
	if err == nil && a.IsUnspecified() {
		return answerErr("public_addr", "must be the address the checkout is served on: 127.0.0.1 behind a reverse proxy, or the host's own address, never 0.0.0.0")
	}
	if err != nil || a.Zone() != "" || reservedV4(a) {
		return answerErr("public_addr", "must be an IPv4 address (loopback or unicast; no hostname, zone, reserved, multicast, link-local or broadcast address)")
	}
	return nil
}

// lastAddr is the last (broadcast) address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// ipv4Prefix parses an IPv4 CIDR that is already in canonical (masked) form.
func ipv4Prefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Addr().Zone() != "" || p.Masked() != p || p.Bits() < 16 || p.Bits() > 29 {
		return netip.Prefix{}, errors.New("prefix")
	}
	return p, nil
}

// checkHostDir accepts an absolute path of [A-Za-z0-9/._-] in its clean form that is not the root.
func checkHostDir(s string) error {
	bad := answerErr("host_dir", "must be an absolute path of letters, digits, '/', '.', '_' and '-', clean, at most 200 bytes, not '/'")
	if s == "" || len(s) > 200 || s[0] != '/' || s == "/" || path.Clean(s) != s {
		return bad
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '/' || c == '.' || c == '_' || c == '-') {
			return bad
		}
	}
	return nil
}

// validCurrency mirrors rates.ValidCurrency: 3 uppercase letters, not BTC or XMR.
func validCurrency(s string) bool {
	if len(s) != 3 || s == "BTC" || s == "XMR" {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// moneroAddressShape mirrors xmrrpc.ValidAddress for a primary address: 95 characters of the base58 alphabet and
// the network's leading character. The address is not decoded: wallet-rpc checks its checksum and keys.
func moneroAddressShape(s string) bool {
	if len(s) != 95 || s[0] != '4' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if strings.IndexByte(base58Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// portOK mirrors service's (and btcrpc.ValidPort), written out again on purpose: satbooth-setup must not link a
// package with a network capability (scripts/capslock.sh). The host part of a URL has no port, or one of 1 to 65535
// in decimal digits without a sign or a leading zero (a colon with nothing after it is refused).
func portOK(host string) bool {
	i := strings.LastIndex(host, ":")
	if i < 0 || i < strings.LastIndex(host, "]") {
		return true
	}
	port := host[i+1:]
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port
}

// checkURL mirrors service's plainURL: an absolute URL of the given schemes with a host, no user info, no fragment,
// no space or control character, at most 512 bytes; noQuery also refuses a query, even an empty one.
func checkURL(field, s string, noQuery bool, schemes ...string) (*url.URL, error) {
	bad := answerErr(field, "must be a "+strings.Join(schemes, " or ")+" URL with a host, without user info, fragment, spaces or control characters, at most 512 bytes"+
		map[bool]string{true: ", and without a query", false: " (a query is allowed)"}[noQuery])
	if s == "" || len(s) > maxURLBytes || strings.Contains(s, "#") {
		return nil, bad
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r > '~' {
			return nil, bad
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || !portOK(u.Host) {
		return nil, bad
	}
	ok := false
	for _, sc := range schemes {
		ok = ok || u.Scheme == sc
	}
	if !ok || (noQuery && (u.RawQuery != "" || u.ForceQuery)) {
		return nil, bad
	}
	return u, nil
}

// sameEndpoint mirrors service's: scheme and host without case, a default port the same as none, an empty path the
// same as "/", the rest exactly.
func sameEndpoint(a, b *url.URL) bool {
	norm := func(u *url.URL) string {
		scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
		p := u.EscapedPath()
		if p == "" {
			p = "/"
		}
		return scheme + "://" + net.JoinHostPort(host, port) + p + "?" + u.RawQuery
	}
	return norm(a) == norm(b)
}

// strictAnswers decodes b, exactly one JSON object of at most 16 KiB, into v. Each key must be one of keys, matched
// exactly, and appear once; a null leaves its field at the zero value (as an absent key does; the set holds key+nullSuffix for it, so
// the two defaults need the set). Absent keys are allowed: the returned set says which were present. No
// error carries a value or an unknown key.
func strictAnswers(b []byte, v any, keys ...string) (map[string]bool, error) {
	if len(b) == 0 || len(b) > maxAnswers {
		return nil, errors.New("setup: answers: the file is empty or larger than 16 KiB")
	}
	shape := errors.New("setup: answers: not one JSON object with known, distinct keys and values of the right type")
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, shape
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, shape
		}
		key, ok := kt.(string)
		if !ok || !want[key] {
			return nil, errors.New("setup: answers: an unknown or misspelt key")
		}
		if seen[key] {
			return nil, fmt.Errorf("setup: answers: the key %s appears twice", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, shape
		}
		if string(skip) == "null" {
			seen[key+nullSuffix] = true
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, shape
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, shape
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && want[te.Field] {
			return nil, answerErr(te.Field, "has the wrong type")
		}
		return nil, shape
	}
	return seen, nil
}
