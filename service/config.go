// Package service wires the SatBooth service: the ledger, the order store, the NWC connection, the
// invoice service, the webhook deliverers, the two HTTP listeners and, when configured, the on-chain rail, the
// Monero rail and the rate fetcher. cmd/satbooth is a thin wrapper around Run; the package exists so that the
// qualification suite can start the whole service in-process.
package service

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"bytes"

	"github.com/satbooth/satbooth/api"
	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/refund"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
)

const (
	maxConfigBytes = 1 << 20
	maxURLBytes    = 512
)

// Config is the service's configuration file. It holds no secrets: key and secret files (the custom rate URL, which may
// carry a token, among them) are named, never included. Every key is matched exactly; unknown, duplicate and case-variant keys are refused.
type Config struct {
	PublicListen    string    `json:"public_listen"`
	PrivateListen   string    `json:"private_listen"`
	CheckoutBaseURL string    `json:"checkout_base_url"`
	DataDir         string    `json:"data_dir"`
	NWC             NWCConfig `json:"nwc"`
	APIKeysFile     string    `json:"api_keys_file"`
	// Webhooks are the merchant's webhook receivers, 1 to MaxWebhookEndpoints: every event goes to each, delivered on its own.
	Webhooks             []WebhookEndpoint `json:"webhooks"`
	Limits               LimitsConfig      `json:"limits"`
	InvoiceExpiryMinutes int               `json:"invoice_expiry_minutes"`
	// AllowSwap lets the service start when memory cannot be locked (Linux). Set it only if swap is
	// encrypted or absent. Default false.
	AllowSwap bool `json:"allow_swap"`
	// Alerts is the operator's alert receiver (plan 10): every ALERT: line is also posted there, signed with its own
	// secret. Absent (or null): nothing is queued and the lines are only logged.
	Alerts *WebhookConfig `json:"alerts"`
	// Onchain is the on-chain rail. Absent (or null): the service has no on-chain rail, and an order with an
	// on-chain amount is refused.
	Onchain *OnchainConfig `json:"onchain"`
	// Monero is the Monero rail. Absent (or null): the service has no Monero rail, and an order with a Monero amount is
	// refused.
	Monero *MoneroConfig `json:"monero"`
	// Rates is fiat pricing. Absent (or null): a priced order is refused (fiat_disabled).
	Rates *RatesConfig `json:"rates"`
	// Payout is Lightning refunds. Absent (or null): refunds are off, and every refund route answers refunds_disabled.
	Payout *PayoutConfig `json:"payout"`
}

// PayoutConfig is Lightning refunds (plan 9): the payout wallet connection, which must be a second NWC connection
// (another wallet key and another client key than the invoice connection), the network and keys that approve refunds,
// and the limits. It holds no secret: the client key is named by file. The approval keys are public keys, inline;
// rotating them is an edit and a restart. TimeoutSeconds is 10 to 600; 0 means 70.
type PayoutConfig struct {
	RelayURL       string   `json:"relay_url"`
	WalletPubKey   string   `json:"wallet_pubkey"`
	ClientKeyFile  string   `json:"client_key_file"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Network        string   `json:"network"`       // mainnet, testnet, signet or regtest
	ApprovalKeys   []string `json:"approval_keys"` // 1 to 8 Ed25519 public keys, 64 lowercase hex characters each
	MaxMsat        int64    `json:"max_msat"`      // the most one refund may pay; a multiple of 1000
	// DailyMsat: the most the refunds claimed in one UTC day may total; at least max_msat; a multiple of 1000.
	DailyMsat       int64 `json:"daily_msat"`
	BudgetAlertMsat int64 `json:"budget_alert_msat"` // default 0 (no alert): alert when the hub budget's headroom falls below it
}

// validate checks the block and returns the refund service's configuration. Each text names its key and none carries a
// value. ParseConfig opens no secret file: CheckFiles loads the client key. Run calls it too: a Config built by struct
// literal never went through ParseConfig (and gets no timeout default).
func (p *PayoutConfig) validate() (refund.Config, error) {
	if _, err := plainURL("payout.relay_url", p.RelayURL, false, "ws", "wss"); err != nil {
		return refund.Config{}, err
	}
	if err := nostr.ValidatePubKey(p.WalletPubKey); err != nil {
		return refund.Config{}, errors.New("config: payout.wallet_pubkey is not a valid public key")
	}
	if p.ClientKeyFile == "" {
		return refund.Config{}, errors.New("config: payout.client_key_file is missing")
	}
	// Compare the plain integers: converting first would let a huge value wrap into range.
	if p.TimeoutSeconds < int(nwc.MinTimeout/time.Second) || p.TimeoutSeconds > int(nwc.MaxTimeout/time.Second) {
		return refund.Config{}, errors.New("config: payout.timeout_seconds must be 10 to 600")
	}
	net, err := approval.ParseNetwork(p.Network)
	if err != nil {
		return refund.Config{}, errors.New("config: payout.network must be mainnet, testnet, signet or regtest")
	}
	keys, err := approval.NewKeys(p.ApprovalKeys)
	if err != nil {
		return refund.Config{}, errors.New("config: payout.approval_keys must be 1 to 8 distinct Ed25519 public keys of 64 lowercase hex characters")
	}
	if p.MaxMsat <= 0 || p.MaxMsat > p.DailyMsat || p.DailyMsat > approval.MaxAmountMsat || p.MaxMsat%1000 != 0 || p.DailyMsat%1000 != 0 {
		return refund.Config{}, errors.New("config: payout.max_msat and payout.daily_msat are required: 0 < max_msat <= daily_msat <= 2^53, multiples of 1000")
	}
	if p.BudgetAlertMsat < 0 || p.BudgetAlertMsat > approval.MaxAmountMsat {
		return refund.Config{}, errors.New("config: payout.budget_alert_msat must be 0 to 2^53")
	}
	return refund.Config{Network: net, Keys: keys, MaxMsat: p.MaxMsat, DailyMsat: p.DailyMsat,
		BudgetAlertMsat: p.BudgetAlertMsat, PayTimeout: time.Duration(p.TimeoutSeconds) * time.Second}, nil
}

// RatesConfig is fiat pricing. Absent (or null): a priced order is refused (fiat_disabled).
type RatesConfig struct {
	Providers        []string `json:"providers"`          // of kraken, coinbase, yadio, bitfinex; each at most once
	CustomURL        string   `json:"custom_url"`         // optional: the custom rate API, https, without a query
	CustomURLFile    string   `json:"custom_url_file"`    // optional, instead of custom_url: a 0600 file holding it (a query allowed)
	Currencies       []string `json:"currencies"`         // 1 to 32 codes of 3 uppercase letters, not BTC or XMR
	MarginPercent    string   `json:"margin_percent"`     // default "0"; -50 to 50, at most 2 decimals
	MinSources       int      `json:"min_sources"`        // default 2; 2 to 5 (never 1: at least 2 providers)
	MaxSpreadPercent string   `json:"max_spread_percent"` // default "2"; 0.01 to 50, at most 2 decimals
	MaxAgeSeconds    int      `json:"max_age_seconds"`    // default 300; 30 to 3600
	FetchSeconds     int      `json:"fetch_seconds"`      // default 60; 10 to 600, at most max_age_seconds / 2
}

// The bounds and defaults of the rates block.
const (
	maxRateCurrencies  = 32
	minRateSources     = 2 // the custom API never prices alone
	maxRateSources     = 5 // the four built-ins and the custom API
	maxSpreadBps       = 5000
	minRateAgeSeconds  = 30
	maxRateAgeSeconds  = 3600
	minFetchSeconds    = 10
	maxFetchSeconds    = 600
	defaultMargin      = "0"
	defaultMinSources  = 2
	defaultSpread      = "2"
	defaultAgeSeconds  = 300
	defaultFetchSecond = 60
)

// builtinProviders are the rates package's fixed-host providers, by configuration name.
var builtinProviders = map[string]func(*http.Client) rates.Provider{
	rates.ProviderKraken: rates.Kraken, rates.ProviderCoinbase: rates.Coinbase, rates.ProviderYadio: rates.Yadio,
	rates.ProviderBitfinex: rates.Bitfinex,
}

// tableFiats are the providers that quote BTC from a fixed pair table, in the order errRatesReach names them, each with
// its BTC codes (sorted for the text).
var tableFiats = []struct {
	name  string
	codes []string
}{{rates.ProviderKraken, slices.Sorted(slices.Values(rates.KrakenFiats()))},
	{rates.ProviderBitfinex, slices.Sorted(slices.Values(rates.BitfinexFiats()))}}

// errRatesReach is the startup refusal of a currency that fewer than min_sources of the configured sources can ever quote
// for BTC (rates.Quotes: Kraken and Bitfinex by their pair tables; Coinbase, Yadio and the custom API quote many codes,
// which cannot be known before a fetch, and are taken to quote any). Its text names the pair-table providers among
// providers with their lists (the rule can fail only when one of them is configured: every other source counts for
// every code), never the code. XMR has no startup rule (ruling 8-A): a currency too few sources quote for XMR is not
// fetched, and a priced order with a Monero amount in it is refused (invalid_currency).
func errRatesReach(providers []string) error {
	var lists []string
	for _, t := range tableFiats {
		if slices.Contains(providers, t.name) {
			lists = append(lists, t.name+" quotes only "+strings.Join(t.codes[:len(t.codes)-1], ", ")+" and "+t.codes[len(t.codes)-1])
		}
	}
	msg := "config: rates.currencies has a code that fewer than min_sources providers quote for BTC"
	if len(lists) > 0 {
		msg += " (" + strings.Join(lists, "; ") + ")"
	}
	return errors.New(msg)
}

// applyDefaults fills the zero fields that have a default; ParseConfig runs it before validate.
func (r *RatesConfig) applyDefaults() {
	if r.MarginPercent == "" {
		r.MarginPercent = defaultMargin
	}
	if r.MinSources == 0 {
		r.MinSources = defaultMinSources
	}
	if r.MaxSpreadPercent == "" {
		r.MaxSpreadPercent = defaultSpread
	}
	if r.MaxAgeSeconds == 0 {
		r.MaxAgeSeconds = defaultAgeSeconds
	}
	if r.FetchSeconds == 0 {
		r.FetchSeconds = defaultFetchSecond
	}
}

var (
	errRatesQuery = errors.New("config: rates.custom_url must not have a query: a URL that carries a token goes in rates.custom_url_file, a file with mode 0600")
	errRatesBoth  = errors.New("config: rates.custom_url and rates.custom_url_file exclude each other: set one")
)

// hasCustom reports whether the block configures the custom rate API, by URL or by file.
func (r *RatesConfig) hasCustom() bool { return r.CustomURL != "" || r.CustomURLFile != "" }

// validate checks the block. Each text names its key and none carries a value. The URL in custom_url_file is checked
// when the file is loaded (CheckFiles, Run), never here: ParseConfig opens no secret file.
// rates.Config has no validator of its own (rates.New's errors are for programmers), so the bounds live here.
func (r *RatesConfig) validate() error {
	seen := map[string]bool{}
	for _, p := range r.Providers {
		if builtinProviders[p] == nil || seen[p] {
			return errors.New("config: rates.providers must list kraken, coinbase, yadio or bitfinex, each at most once")
		}
		seen[p] = true
	}
	if r.CustomURL != "" && r.CustomURLFile != "" { // empty is absent, for each
		return errRatesBoth
	}
	if r.CustomURL != "" {
		if _, err := plainURL("rates.custom_url", r.CustomURL, true, "https"); err != nil {
			// A URL that is only wrong by its query gets the query's own message, which says where a URL with a token
			// goes; any other flaw gets the generic one, which names the query as refused.
			if u, e2 := plainURL("rates.custom_url", r.CustomURL, false, "https"); e2 == nil && (u.RawQuery != "" || u.ForceQuery) {
				return errRatesQuery
			}
			return err
		}
	}
	if len(r.Currencies) < 1 || len(r.Currencies) > maxRateCurrencies {
		return errors.New("config: rates.currencies must be 1 to 32 distinct codes of 3 uppercase letters, not BTC or XMR")
	}
	codes := map[string]bool{}
	for _, c := range r.Currencies {
		if !rates.ValidCurrency(c) || codes[c] {
			return errors.New("config: rates.currencies must be 1 to 32 distinct codes of 3 uppercase letters, not BTC or XMR")
		}
		codes[c] = true
	}
	if m, err := rates.ParsePercent(r.MarginPercent); err != nil || m < rates.MinMarginBps || m > rates.MaxMarginBps {
		return errors.New("config: rates.margin_percent must be -50 to 50 with at most 2 decimals")
	}
	if r.MinSources < minRateSources || r.MinSources > maxRateSources {
		return errors.New("config: rates.min_sources must be 2 to 5")
	}
	if s, err := rates.ParsePercent(r.MaxSpreadPercent); err != nil || s < 1 || s > maxSpreadBps {
		return errors.New("config: rates.max_spread_percent must be 0.01 to 50 with at most 2 decimals")
	}
	if r.MaxAgeSeconds < minRateAgeSeconds || r.MaxAgeSeconds > maxRateAgeSeconds {
		return errors.New("config: rates.max_age_seconds must be 30 to 3600")
	}
	// Compare the plain integers; the product is formed only once fetch_seconds is at most 600.
	if r.FetchSeconds < minFetchSeconds || r.FetchSeconds > maxFetchSeconds || 2*r.FetchSeconds > r.MaxAgeSeconds {
		return errors.New("config: rates.fetch_seconds must be 10 to 600 and at most half of max_age_seconds")
	}
	sources := len(r.Providers)
	if r.hasCustom() {
		sources++
	}
	if sources < r.MinSources {
		return errors.New("config: rates needs at least min_sources providers (providers plus custom_url or custom_url_file)")
	}
	// The BTC reach rule: every code must be quotable for BTC by min_sources of the sources.
	for _, c := range r.Currencies {
		n := 0
		for _, p := range r.Providers {
			if rates.Quotes(p, rates.AssetBTC, c) {
				n++
			}
		}
		if r.hasCustom() { // the custom API is taken to quote every pair
			n++
		}
		if n < r.MinSources {
			return errRatesReach(r.Providers)
		}
	}
	return nil
}

// ratesConfig checks the block with validate (a Config built by struct literal never went through ParseConfig, and
// gets no defaults: a zero field fails), then maps it to a rates.Config: the providers in the configured order, then
// the custom API at custom when the block has one, all over one client on rt (nil: the real transport). custom is the
// block's custom_url, or the URL its custom_url_file holds (customURL loads it). The assets are BTC, and XMR too when
// withXMR (the Monero rail is configured). Its errors are validate's fixed texts.
func (r *RatesConfig) ratesConfig(custom string, rt http.RoundTripper, now func() time.Time, log *slog.Logger, withXMR bool) (rates.Config, error) {
	if err := r.validate(); err != nil {
		return rates.Config{}, err
	}
	if (custom != "") != r.hasCustom() || r.CustomURL != "" && custom != r.CustomURL {
		return rates.Config{}, errors.New("config: the custom rate URL does not match the rates block")
	}
	margin, err := rates.ParsePercent(r.MarginPercent)
	if err != nil { // validate refused it already: kept so that no error is ever dropped
		return rates.Config{}, errors.New("config: rates.margin_percent must be -50 to 50 with at most 2 decimals")
	}
	spread, err := rates.ParsePercent(r.MaxSpreadPercent)
	if err != nil {
		return rates.Config{}, errors.New("config: rates.max_spread_percent must be 0.01 to 50 with at most 2 decimals")
	}
	client := rates.NewClient(rt)
	var ps []rates.Provider
	for _, p := range r.Providers {
		ps = append(ps, builtinProviders[p](client)) // validate allows only the built-in names
	}
	if custom != "" {
		ps = append(ps, rates.Custom(client, custom))
	}
	assets := []string{rates.AssetBTC}
	if withXMR {
		assets = append(assets, rates.AssetXMR)
	}
	return rates.Config{
		Providers:  ps,
		Assets:     assets,
		Currencies: append([]string(nil), r.Currencies...),
		Interval:   time.Duration(r.FetchSeconds) * time.Second,
		Policy:     rates.Policy{MinSources: r.MinSources, MaxSpreadBps: spread, MaxAge: time.Duration(r.MaxAgeSeconds) * time.Second},
		MarginBps:  margin,
		Now:        now,
		Log:        log,
	}, nil
}

// OnchainConfig is the connection to bitcoind and the rail's settings. It holds no secret: the RPC password is
// named by file. Zero numeric fields and an empty confirmations list take the onchain package's defaults;
// poll_seconds is 2 to 60, timeout_seconds 1 to 600, and network is mainnet, testnet, signet or regtest.
type OnchainConfig struct {
	RPCURL          string       `json:"rpc_url"`
	RPCUser         string       `json:"rpc_user"`
	RPCPasswordFile string       `json:"rpc_password_file"`
	Wallet          string       `json:"wallet"`
	Descriptor      string       `json:"descriptor"`
	Network         string       `json:"network"`
	Confirmations   []TierConfig `json:"confirmations"`
	GapAlert        int64        `json:"gap_alert"`
	PollSeconds     int          `json:"poll_seconds"`
	PollRate        float64      `json:"poll_rate"`
	TimeoutSeconds  int          `json:"timeout_seconds"`
}

// TierConfig is one confirmation tier: orders of up to UpToSat sat (0 for the last tier: no bound) need
// Confirmations confirmations, 0 to 100 (0: paid as soon as the payment is in the mempool). The confirmations key is
// required in every tier, so an omitted key never reads as 0-conf (Ruling 6.1-5).
type TierConfig struct {
	UpToSat       int64 `json:"up_to_sat"`
	Confirmations *int  `json:"confirmations"`
}

// railConfig maps the block to an onchain.Config, parsing the descriptor, and checks it with the owner's
// validator (which owns the ranges and defaults). The errors are fixed text: none carries a value from the input.
func (o *OnchainConfig) railConfig() (onchain.Config, error) {
	d, err := descriptor.Parse(o.Descriptor)
	if err != nil {
		return onchain.Config{}, fmt.Errorf("config: onchain.descriptor: %w", err) // a sentinel: never the input
	}
	// Compare the plain integers: converting first would let a huge value wrap into range.
	if o.PollSeconds < 0 || o.PollSeconds > int(onchain.MaxPollInterval/time.Second) {
		return onchain.Config{}, errors.New("config: onchain.poll_seconds must be 2 to 60")
	}
	if o.TimeoutSeconds < 0 || o.TimeoutSeconds > int(onchain.MaxCallTimeout/time.Second) {
		return onchain.Config{}, errors.New("config: onchain.timeout_seconds must be 1 to 600")
	}
	var tiers []onchain.Tier // an empty list takes the default
	for _, t := range o.Confirmations {
		if t.Confirmations == nil {
			return onchain.Config{}, errors.New("config: onchain.confirmations: every tier needs a confirmations key (0 to 100)")
		}
		tiers = append(tiers, onchain.Tier{UpTo: t.UpToSat, Confirmations: *t.Confirmations})
	}
	c := onchain.Config{Descriptor: d, Network: o.Network, Tiers: tiers, GapAlert: o.GapAlert, PollRate: o.PollRate,
		PollInterval: time.Duration(o.PollSeconds) * time.Second, CallTimeout: time.Duration(o.TimeoutSeconds) * time.Second}
	if err := c.Validate(); err != nil {
		// The owner's texts name Go fields; an operator reads the configuration keys.
		return onchain.Config{}, fmt.Errorf("config: %s", keyNames.Replace(err.Error()))
	}
	return c, nil
}

var keyNames = strings.NewReplacer("onchain: ", "onchain: ", "GapAlert", "gap_alert", "PollInterval", "poll_seconds",
	"PollRate", "poll_rate", "CallTimeout", "timeout_seconds", "Network", "network", "Tiers", "confirmations",
	"Confirmations", "confirmations", "UpTo", "up_to_sat")

func (o *OnchainConfig) validate() error {
	if !btcrpc.ValidURL(o.RPCURL) { // the one URL rule: btcrpc's, not a second copy
		return fmt.Errorf("config: onchain.rpc_url must be an http or https URL with a host, without user info, query or fragment, spaces or control characters, at most %d bytes", maxURLBytes)
	}
	if !btcrpc.ValidName(o.RPCUser) {
		return errors.New("config: onchain.rpc_user must be 1 to 64 characters of A-Z a-z 0-9 . _ -")
	}
	if !btcrpc.ValidWallet(o.Wallet) {
		return errors.New("config: onchain.wallet must be 1 to 64 characters of A-Z a-z 0-9 . _ - and not . or ..")
	}
	if o.RPCPasswordFile == "" {
		return errors.New("config: onchain.rpc_password_file is missing")
	}
	_, err := o.railConfig()
	return err
}

// MoneroConfig is the Monero rail: the connection to monero-wallet-rpc, which holds the merchant's view-only wallet, and
// the rail's settings. Absent (or null): no Monero rail. It holds no secret: the Digest password is named by file. Zero
// numeric fields and an empty confirmations list take the monero package's defaults.
type MoneroConfig struct {
	WalletRPCURL    string             `json:"wallet_rpc_url"`    // http or https on the private link, no query
	RPCUser         string             `json:"rpc_user"`          // btcrpc.ValidName
	RPCPasswordFile string             `json:"rpc_password_file"` // 0600, the Digest password
	Address         string             `json:"address"`           // the wallet's primary address (account 0)
	Network         string             `json:"network"`           // mainnet, testnet, stagenet or regtest
	AccountIndex    uint32             `json:"account_index"`     // default 0
	Confirmations   []MoneroTierConfig `json:"confirmations"`     // default 1 / 3 / 10 at 1 XMR / 10 XMR / above
	UnusedAlert     int64              `json:"unused_alert"`      // default 100; 1 to 10000
	PollSeconds     int                `json:"poll_seconds"`      // default 10; 2 to 60
	PollRate        float64            `json:"poll_rate"`         // default 5; 1 to 10000; mapped to monero.Config.PollEvery = time.Second / rate
	TimeoutSeconds  int                `json:"timeout_seconds"`   // default 30; 1 to 600
}

// MoneroTierConfig is one confirmation tier: orders of up to UpToPiconero piconero (0 for the last tier: no bound) need
// Confirmations confirmations, 0 to 100. The confirmations key is required in every tier, as on-chain's.
type MoneroTierConfig struct {
	UpToPiconero  int64 `json:"up_to_piconero"`
	Confirmations *int  `json:"confirmations"`
}

// maxPollRate is the most tracked-payment calls a second, as on-chain's.
const maxPollRate = 10000

// railConfig maps the block to a monero.Config and checks it with the owner's validator, which owns the ranges and
// the defaults. poll_rate becomes the pause PollEvery = time.Second / rate here, the one place that holds the float64:
// monero is in the no-float scan. The errors are fixed text: none carries a value from the input.
func (x *MoneroConfig) railConfig() (monero.Config, error) {
	// Compare the plain integers: converting first would let a huge value wrap into range.
	if x.PollSeconds < 0 || x.PollSeconds > int(monero.MaxPollInterval/time.Second) {
		return monero.Config{}, errors.New("config: monero.poll_seconds must be 2 to 60")
	}
	if x.TimeoutSeconds < 0 || x.TimeoutSeconds > int(monero.MaxCallTimeout/time.Second) {
		return monero.Config{}, errors.New("config: monero.timeout_seconds must be 1 to 600")
	}
	var every time.Duration // 0: the default
	if x.PollRate != 0 {
		// Written so that NaN fails too (a Config built by struct literal never went through JSON). From 1, the pause is
		// at most monero.MaxPollEvery (1 s); Validate below keeps its own bound as the belt.
		if !(x.PollRate >= 1 && x.PollRate <= maxPollRate) {
			return monero.Config{}, errors.New("config: monero.poll_rate must be 1 to 10000")
		}
		every = time.Duration(float64(time.Second) / x.PollRate)
	}
	var tiers []rail.Tier // an empty list takes the default
	for _, t := range x.Confirmations {
		if t.Confirmations == nil {
			return monero.Config{}, errors.New("config: monero.confirmations: every tier needs a confirmations key (0 to 100)")
		}
		tiers = append(tiers, rail.Tier{UpTo: t.UpToPiconero, Confirmations: *t.Confirmations})
	}
	c := monero.Config{Address: x.Address, Network: x.Network, AccountIndex: x.AccountIndex, Tiers: tiers, UnusedAlert: x.UnusedAlert,
		PollInterval: time.Duration(x.PollSeconds) * time.Second, PollEvery: every, CallTimeout: time.Duration(x.TimeoutSeconds) * time.Second}
	if err := c.Validate(); err != nil {
		// The owner's texts name Go fields; an operator reads the configuration keys.
		return monero.Config{}, fmt.Errorf("config: %s", moneroKeyNames.Replace(err.Error()))
	}
	return c, nil
}

var moneroKeyNames = strings.NewReplacer("Address", "address", "Network", "network", "UnusedAlert", "unused_alert",
	"PollInterval", "poll_seconds", "PollEvery", "poll_rate", "CallTimeout", "timeout_seconds", "Tiers", "confirmations",
	"Confirmations", "confirmations", "UpTo", "up_to_piconero")

// validate checks the block, each rule by its owner: the URL and the user by btcrpc's rules (xmrrpc.New applies the
// same), the rest by railConfig, whose monero.Config it returns. ParseConfig opens no secret file: CheckFiles loads the
// password.
func (x *MoneroConfig) validate() (monero.Config, error) {
	if !btcrpc.ValidURL(x.WalletRPCURL) {
		return monero.Config{}, fmt.Errorf("config: monero.wallet_rpc_url must be an http or https URL with a host, without user info, query or fragment, spaces or control characters, at most %d bytes", maxURLBytes)
	}
	if !btcrpc.ValidName(x.RPCUser) {
		return monero.Config{}, errors.New("config: monero.rpc_user must be 1 to 64 characters of A-Z a-z 0-9 . _ -")
	}
	if x.RPCPasswordFile == "" {
		return monero.Config{}, errors.New("config: monero.rpc_password_file is missing")
	}
	return x.railConfig()
}

// NWCConfig is the wallet connection. TimeoutSeconds is 10 to 600; 0 means 70. Network is the network of the hub's
// invoices (mainnet, testnet, signet or regtest), required: SatBooth refuses every invoice of another network. With an
// onchain or a payout block, their network must equal it.
type NWCConfig struct {
	RelayURL       string `json:"relay_url"`
	WalletPubKey   string `json:"wallet_pubkey"`
	ClientKeyFile  string `json:"client_key_file"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Network        string `json:"network"`
}

// lightningNetwork maps nwc.network to its Network, the one mapping of the configuration's network names, and checks
// that the onchain and payout blocks, when present, name the same network (Run calls it too: a Config built by struct
// literal never went through ParseConfig).
func (c *Config) lightningNetwork() (approval.Network, error) {
	n, err := approval.ParseNetwork(c.NWC.Network)
	if err != nil {
		return 0, errors.New("config: nwc.network is required and must be mainnet, testnet, signet or regtest")
	}
	if c.Onchain != nil && c.Onchain.Network != c.NWC.Network {
		return 0, errors.New("config: onchain.network must equal nwc.network")
	}
	if c.Payout != nil && c.Payout.Network != c.NWC.Network {
		return 0, errors.New("config: payout.network must equal nwc.network")
	}
	return n, nil
}

// MaxWebhookEndpoints is the most webhook endpoints a configuration lists.
const MaxWebhookEndpoints = 8

// errWebhookReplaced refuses the former single `webhook` key. It names the new key and carries no value.
var errWebhookReplaced = errors.New("config: webhook is replaced by webhooks (a list of endpoints)")

// WebhookEndpoint is one of the merchant's webhook receivers: a name (the store's endpoint name rule), the URL and the
// file holding its own secret.
type WebhookEndpoint struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SecretFile string `json:"secret_file"`
}

// WebhookConfig is the shape of the operator's alert receiver.
type WebhookConfig struct {
	URL        string `json:"url"`
	SecretFile string `json:"secret_file"`
}

// LimitsConfig mirrors limits.Config. A zero field takes the limiter's default, except DailyMsat and
// MaxMsat, which are required.
type LimitsConfig struct {
	RatePerSec float64 `json:"rate_per_sec"`
	Burst      int     `json:"burst"`
	MaxOpen    int     `json:"max_open"`
	NewShare   float64 `json:"new_share"`
	DailyCount int     `json:"daily_count"`
	DailyMsat  int64   `json:"daily_msat"`
	MinMsat    int64   `json:"min_msat"`
	MaxMsat    int64   `json:"max_msat"`
	// MaxOpenPerAccount is the one per-account limit: open invoices one account_id may hold.
	MaxOpenPerAccount int `json:"max_open_per_account"`
}

func (l LimitsConfig) limits() limits.Config {
	return limits.Config{RatePerSec: l.RatePerSec, Burst: l.Burst, MaxOpen: l.MaxOpen, NewShare: l.NewShare,
		DailyCount: l.DailyCount, DailyMsat: l.DailyMsat, MinMsat: l.MinMsat, MaxMsat: l.MaxMsat,
		MaxOpenPerAccount: l.MaxOpenPerAccount}
}

// ParseConfig parses and validates a configuration. It does not look at the files the configuration
// names: LoadConfig does. Errors never contain a value from the input.
func ParseConfig(b []byte) (*Config, error) {
	// The former key gets its own message (checkKeys would call it unknown). Only the key is looked at, never its value.
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) == nil {
		if _, old := top["webhook"]; old {
			return nil, errWebhookReplaced
		}
	}
	if err := checkKeys(b, reflect.TypeOf(Config{})); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("config: not a valid configuration object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("config: trailing data after the object")
	}
	if c.InvoiceExpiryMinutes == 0 {
		c.InvoiceExpiryMinutes = int(lnpay.DefaultInvoiceExpiry / time.Minute)
	}
	if c.NWC.TimeoutSeconds == 0 {
		c.NWC.TimeoutSeconds = int(nwc.DefaultTimeout / time.Second)
	}
	if c.Rates != nil {
		c.Rates.applyDefaults()
	}
	if c.Payout != nil && c.Payout.TimeoutSeconds == 0 {
		c.Payout.TimeoutSeconds = int(nwc.DefaultTimeout / time.Second)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadConfig reads at most 1 MiB from path, parses it and checks the files it names: each must exist
// and pass its loader (the client key, the API keys, every endpoint's secret, with a payout block its client key, with an onchain block the RPC password, with a
// monero block the wallet-rpc password, and with rates.custom_url_file the custom rate URL). It loads secrets, so the
// caller must have run Lock first; cmd/satbooth calls ReadConfig, Lock and CheckFiles in that order.
func LoadConfig(path string) (*Config, error) {
	c, err := ReadConfig(path)
	if err != nil {
		return nil, err
	}
	if err := c.CheckFiles(); err != nil {
		return nil, err
	}
	return c, nil
}

// ReadConfig reads at most 1 MiB from path and parses it. It opens no secret file.
func ReadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("config: cannot open the configuration file")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, errors.New("config: cannot read the configuration file")
	}
	if len(b) > maxConfigBytes {
		return nil, errors.New("config: file is larger than 1 MiB")
	}
	return ParseConfig(b)
}

// CheckFiles loads the client key, the API keys, the webhook secrets, (with a payout block) its client key, (with an on-chain block) the RPC password, (with a
// monero block) the wallet-rpc password and (with rates.custom_url_file) the custom rate URL, to prove that each exists
// and is acceptable. The key, the secret, the passwords and the URL are dropped at once.
func (c *Config) CheckFiles() error {
	k, err := nostr.LoadSecretKeyFile(c.NWC.ClientKeyFile)
	if err != nil {
		return fmt.Errorf("config: nwc.client_key_file: %w", err)
	}
	k.Zero()
	if _, err := api.LoadKeys(c.APIKeysFile); err != nil {
		return fmt.Errorf("config: api_keys_file: %w", err)
	}
	secrets := make([][]byte, len(c.Webhooks))
	defer func() {
		for _, s := range secrets {
			clear(s)
		}
	}()
	for i, w := range c.Webhooks {
		s, err := webhook.LoadSecret(w.SecretFile)
		if err != nil {
			return fmt.Errorf("config: webhooks[%d].secret_file: %w", i, err)
		}
		secrets[i] = s
	}
	var alertSecret []byte
	if c.Alerts != nil {
		as, err := webhook.LoadSecret(c.Alerts.SecretFile)
		if err != nil {
			return fmt.Errorf("config: alerts.secret_file: %w", err)
		}
		defer clear(as)
		alertSecret = as
	}
	if _, msg := distinctSecrets(secrets, alertSecret); msg != "" {
		return errors.New("config: " + msg)
	}
	if c.Onchain != nil {
		pw, err := btcrpc.LoadPassword(c.Onchain.RPCPasswordFile)
		if err != nil {
			return fmt.Errorf("config: onchain.rpc_password_file: %w", err)
		}
		clear(pw)
	}
	if c.Monero != nil {
		pw, err := btcrpc.LoadPassword(c.Monero.RPCPasswordFile) // the one password loader
		if err != nil {
			return fmt.Errorf("config: monero.rpc_password_file: %w", err)
		}
		clear(pw)
	}
	if c.Payout != nil {
		k, err := nostr.LoadSecretKeyFile(c.Payout.ClientKeyFile)
		if err != nil {
			return fmt.Errorf("config: payout.client_key_file: %w", err)
		}
		k.Zero()
	}
	if c.Rates != nil && c.Rates.CustomURLFile != "" {
		if _, err := loadCustomURL(c.Rates.CustomURLFile); err != nil {
			return fmt.Errorf("config: rates.custom_url_file: %w", err)
		}
	}
	return nil
}

// errCustomURLFile means the custom URL file was refused. The content is never in the error.
var errCustomURLFile = errors.New("custom URL file refused: it must be a regular file with mode 0600 or stricter holding one https URL " +
	"with a host, without user info or fragment, spaces or control characters, at most 512 bytes (a query is allowed), and an optional final newline")

// errCustomURLAsset means the custom URL file's URL has a query that does not parse, or one that already has an
// asset parameter: SatBooth adds its own to every request (rates.Custom), and two (or one a lenient server reads from
// a malformed pair) would leave the choice to the operator's server. The URL is never in the error.
var errCustomURLAsset = errors.New("custom URL file refused: its URL's query must parse (no ';', every '%' followed by two hex " +
	"digits) and must not carry an asset parameter (SatBooth adds asset=<asset> to every request)")

// loadCustomURL reads the custom rate API's URL from path, as btcrpc.LoadPassword reads the RPC password: a regular file
// (a symbolic link is refused, and opening a FIFO does not wait) with mode 0600 or stricter, holding one https URL that
// passes the custom URL rules, a query allowed (one that parses, with no asset parameter), and an optional final newline,
// which is stripped. Neither the URL nor the path is ever in an error, a log or a response: the URL may carry a token.
func loadCustomURL(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return "", errors.New("opening the custom URL file: " + pe.Err.Error())
		}
		return "", errors.New("opening the custom URL file")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", errors.New("reading the custom URL file")
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&^0o600 != 0 {
		return "", errCustomURLFile
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxURLBytes+2)) // the URL, a newline, and one byte too many
	defer clear(raw)
	if err != nil {
		return "", errors.New("reading the custom URL file")
	}
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	u := string(raw)
	pu, err := plainURL("rates.custom_url_file", u, false, "https")
	if err != nil {
		return "", errCustomURLFile
	}
	// url.Query skips a pair it cannot parse (a bad escape, a ';'), which a lenient server may still read as an asset:
	// a query that does not parse is refused too.
	if q, err := url.ParseQuery(pu.RawQuery); err != nil || q.Has("asset") {
		return "", errCustomURLAsset
	}
	return u, nil
}

// customURL is the block's custom rate URL: custom_url, or the URL custom_url_file holds, loaded in-process (after
// Lock: the URL may carry a token), or "" when the block has neither.
func (r *RatesConfig) customURL() (string, error) {
	if r.CustomURLFile == "" {
		return r.CustomURL, nil
	}
	return loadCustomURL(r.CustomURLFile)
}

// checkKeys walks the JSON tokens against the type: every object key must be exactly a json tag of
// the target struct (so no unknown, case-variant or duplicate key gets through, at any depth).
func checkKeys(b []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := walk(dec, t); err != nil {
		return err
	}
	return nil
}

var errShape = errors.New("config: unknown, duplicate or misspelt key, or a value of the wrong type")

func walk(dec *json.Decoder, t reflect.Type) error {
	for t.Kind() == reflect.Pointer { // an optional block: *struct
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return errors.New("config: not valid JSON")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar; the decoder checks its type
	}
	switch d {
	case '[':
		if t.Kind() != reflect.Slice {
			return errShape
		}
		for dec.More() {
			if err := walk(dec, t.Elem()); err != nil {
				return err
			}
		}
	case '{':
		if t.Kind() != reflect.Struct {
			return errShape
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
				fields[name] = t.Field(i).Type
			}
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return errors.New("config: not valid JSON")
			}
			key, _ := kt.(string)
			ft, known := fields[key]
			if !known || seen[key] {
				return errShape
			}
			seen[key] = true
			if err := walk(dec, ft); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the closing delimiter
		return errors.New("config: not valid JSON")
	}
	return nil
}

// splitPort checks a host:port and returns the host.
func splitPort(field, addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("config: %s is missing", field)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("config: %s must be host:port", field)
	}
	if !btcrpc.ValidPort(port) {
		return "", fmt.Errorf("config: %s has an invalid port", field)
	}
	return host, nil
}

// specificListen checks that addr is an IP literal (not a hostname, no zone) that is not a wildcard.
func specificListen(field, addr string) error {
	host, err := splitPort(field, addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("config: %s must be a specific IP address (not a hostname or wildcard)", field)
	}
	return nil
}

// sameHostPort reports whether two checked host:port strings name the same address, comparing IP
// literals by value rather than spelling.
func sameHostPort(a, b string) bool {
	ah, ap, _ := net.SplitHostPort(a)
	bh, bp, _ := net.SplitHostPort(b)
	return ap == bp && net.ParseIP(ah).Equal(net.ParseIP(bh))
}

// portOK reports whether the host part of a URL (host, host:port or [ip]:port) has no port, or a valid one: a colon
// with nothing after it is refused.
func portOK(host string) bool {
	i := strings.LastIndex(host, ":")
	if i < 0 || i < strings.LastIndex(host, "]") {
		return true // no port
	}
	return btcrpc.ValidPort(host[i+1:])
}

// plainURL checks an absolute URL of the given schemes with a host, no user info, no fragment and no
// spaces or control characters, at most 512 bytes. A port, when present, is 1 to 65535 (btcrpc.ValidPort). noQuery also
// refuses a query (even an empty one). Its refusal says which of the two it applies.
func plainURL(field, s string, noQuery bool, schemes ...string) (*url.URL, error) {
	rule := "without user info, query or fragment, spaces or control characters, at most %d bytes"
	if !noQuery {
		rule = "without user info or fragment, spaces or control characters, at most %d bytes (a query is allowed)"
	}
	bad := fmt.Errorf("config: %s must be a %s URL with a host, "+rule, field, strings.Join(schemes, " or "), maxURLBytes)
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
	okScheme := false
	for _, sc := range schemes {
		okScheme = okScheme || u.Scheme == sc
	}
	if !okScheme || (noQuery && (u.RawQuery != "" || u.ForceQuery)) {
		return nil, bad
	}
	return u, nil
}

// msgAlertSecret refuses an alerts secret equal to endpoint i's: whoever runs the alert receiver could then sign merchant
// webhooks. It names the keys by position and no value.
func msgAlertSecret(i int) string {
	return fmt.Sprintf("alerts.secret_file must differ from webhooks[%d].secret_file: use a secret of its own", i)
}

// distinctSecrets returns the log reason and the refusal (without the "config: " prefix) of two endpoints whose secrets are one HMAC key, or
// of an alerts secret (nil: none) equal to an endpoint's, or "", "" when every secret has a key of its own. It names the
// keys by position and no value. secrets[i] belongs to Webhooks[i].
func distinctSecrets(secrets [][]byte, alert []byte) (reason, msg string) {
	for i := range secrets {
		for j := i + 1; j < len(secrets); j++ {
			if sameHMACKey(secrets[i], secrets[j]) {
				return "webhook_secret_file", fmt.Sprintf("webhooks[%d].secret_file and webhooks[%d].secret_file hold the same secret", i, j)
			}
		}
	}
	if alert != nil {
		for i := range secrets {
			if sameHMACKey(alert, secrets[i]) {
				return "alerts_secret_file", msgAlertSecret(i)
			}
		}
	}
	return "", ""
}

// validateWebhooks checks the list: 1 to MaxWebhookEndpoints endpoints, each with a name by the store's rule, a URL by
// plainURL and a secret file, no name, URL (by sameEndpoint) or secret file (as written) twice. Positions are 0-based,
// as in the file. Each text names its key by position and carries no value. The secrets' content is compared where the
// files are loaded (CheckFiles, Run).
func (c *Config) validateWebhooks() error {
	if len(c.Webhooks) < 1 || len(c.Webhooks) > MaxWebhookEndpoints {
		return fmt.Errorf("config: webhooks must list 1 to %d endpoints", MaxWebhookEndpoints)
	}
	urls := make([]*url.URL, len(c.Webhooks))
	for i, w := range c.Webhooks {
		if !store.ValidEndpoint(w.Name) {
			return fmt.Errorf("config: webhooks[%d].name must be 1 to 64 characters of A-Z a-z 0-9 . _ -", i)
		}
		u, err := plainURL(fmt.Sprintf("webhooks[%d].url", i), w.URL, false, "http", "https")
		if err != nil {
			return err
		}
		urls[i] = u
		if w.SecretFile == "" {
			return fmt.Errorf("config: webhooks[%d].secret_file is missing", i)
		}
		for j := 0; j < i; j++ {
			p := c.Webhooks[j]
			switch {
			case p.Name == w.Name:
				return fmt.Errorf("config: webhooks[%d].name repeats webhooks[%d].name", i, j)
			case sameEndpoint(urls[j], u):
				return fmt.Errorf("config: webhooks[%d].url repeats webhooks[%d].url", i, j)
			case p.SecretFile == w.SecretFile:
				return fmt.Errorf("config: webhooks[%d].secret_file repeats webhooks[%d].secret_file", i, j)
			}
		}
	}
	return nil
}

// sameHMACKey reports, in constant time, whether two webhook secrets are one HMAC-SHA256 key. HMAC hashes a key longer
// than SHA-256's 64-byte block and pads every key with zeros to the block, so K and K followed by zero bytes sign
// alike; comparing the two key blocks catches that. LoadSecret accepts 32 to 64 bytes, so the hash is only there to
// match HMAC for any length. The blocks are cleared before it returns.
func sameHMACKey(a, b []byte) bool {
	var ka, kb [sha256.BlockSize]byte
	hmacKeyBlock(&ka, a)
	hmacKeyBlock(&kb, b)
	same := subtle.ConstantTimeCompare(ka[:], kb[:]) == 1
	clear(ka[:])
	clear(kb[:])
	return same
}

// hmacKeyBlock writes the block HMAC-SHA256 derives from key into dst: key itself, or its SHA-256 when it is longer
// than the block, padded with zeros.
func hmacKeyBlock(dst *[sha256.BlockSize]byte, key []byte) {
	if len(key) > sha256.BlockSize {
		sum := sha256.Sum256(key)
		copy(dst[:], sum[:])
		clear(sum[:])
		return
	}
	copy(dst[:], key)
}

// sameEndpoint reports whether a and b name the same endpoint: the scheme and host compared without case, a default
// port the same as none, an empty path the same as "/", and the rest exactly.
func sameEndpoint(a, b *url.URL) bool {
	norm := func(u *url.URL) string {
		scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
		path := u.EscapedPath()
		if path == "" {
			path = "/"
		}
		return scheme + "://" + net.JoinHostPort(host, port) + path + "?" + u.RawQuery
	}
	return norm(a) == norm(b)
}

// validateEndpoints checks everything the configuration says about where events are sent: the webhooks list
// (validateWebhooks) and the alert endpoint against every one of its entries. Run applies it too, for a Config built
// by struct literal.
func (c *Config) validateEndpoints() error {
	if err := c.validateWebhooks(); err != nil {
		return err
	}
	if c.Alerts == nil {
		return nil
	}
	au, err := plainURL("alerts.url", c.Alerts.URL, false, "http", "https")
	if err != nil {
		return err
	}
	if c.Alerts.SecretFile == "" {
		return errors.New("config: alerts.secret_file is missing")
	}
	for i, w := range c.Webhooks {
		if wu, err := url.Parse(w.URL); err == nil && sameEndpoint(au, wu) {
			return fmt.Errorf("config: alerts.url must differ from webhooks[%d].url", i)
		}
	}
	return nil
}

func (c *Config) validate() error {
	if err := specificListen("public_listen", c.PublicListen); err != nil {
		return err
	}
	if err := specificListen("private_listen", c.PrivateListen); err != nil {
		return err
	}
	if sameHostPort(c.PublicListen, c.PrivateListen) {
		return errors.New("config: public_listen and private_listen must differ")
	}
	if _, err := plainURL("checkout_base_url", c.CheckoutBaseURL, true, "http", "https"); err != nil {
		return err
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir is missing")
	}
	if _, err := plainURL("nwc.relay_url", c.NWC.RelayURL, false, "ws", "wss"); err != nil {
		return err
	}
	if err := nostr.ValidatePubKey(c.NWC.WalletPubKey); err != nil {
		return errors.New("config: nwc.wallet_pubkey is not a valid public key")
	}
	if c.NWC.ClientKeyFile == "" {
		return errors.New("config: nwc.client_key_file is missing")
	}
	// Compare the plain integers: converting first would let a huge value wrap into range.
	if c.NWC.TimeoutSeconds < int(nwc.MinTimeout/time.Second) || c.NWC.TimeoutSeconds > int(nwc.MaxTimeout/time.Second) {
		return errors.New("config: nwc.timeout_seconds must be 10 to 600")
	}
	if c.APIKeysFile == "" {
		return errors.New("config: api_keys_file is missing")
	}
	if err := c.validateEndpoints(); err != nil {
		return err
	}
	if _, err := limits.New(c.Limits.limits(), time.Now); err != nil {
		return errors.New("config: limits are invalid (daily_msat and max_msat are required; see the limits package for the bounds)")
	}
	if c.InvoiceExpiryMinutes < int(lnpay.MinInvoiceExpiry/time.Minute) || c.InvoiceExpiryMinutes > int(lnpay.MaxInvoiceExpiry/time.Minute) {
		return errors.New("config: invoice_expiry_minutes must be 15 to 30")
	}
	if c.Rates != nil {
		if err := c.Rates.validate(); err != nil {
			return err
		}
	}
	if c.Onchain != nil {
		if err := c.Onchain.validate(); err != nil {
			return err
		}
	}
	if c.Payout != nil {
		if _, err := c.Payout.validate(); err != nil {
			return err
		}
	}
	if c.Monero != nil {
		if _, err := c.Monero.validate(); err != nil {
			return err
		}
	}
	// Last, so that a block's own error is the one an operator sees first.
	_, err := c.lightningNetwork()
	return err
}
