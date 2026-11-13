package setup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
)

// The renderers return the exact bytes of one file each. JSON files are built from structs (the key order is the
// struct's), two-space indent, one final newline; the .conf and env files line by line. None takes or returns a
// secret it does not need to write.

// Fixed ports and addresses of the bundles.
const (
	relayHubListen    = "127.0.0.1:7447"
	relayClientPort   = "7448"
	bitcoindRPCPort   = "8332"
	walletRPCPort     = "18083"
	satboothSubnet    = "172.30.78.0/24"
	satboothIP        = "172.30.78.40"
	satboothEtc       = "/etc/satbooth/"
	satboothPublicLn  = "172.30.78.40:8080"
	satboothPrivateLn = "172.30.78.40:8081"
)

// The three rpcwhitelist lines of deploy/hub/config-example/bitcoin.conf, byte for byte (TestWhitelistsMatchTheBundle).
const (
	whitelistHub      = "rpcwhitelist=hub:getblockchaininfo,getblockheader,getblock,getblockhash,gettxout,getrawmempool,getmempoolentry,getrawtransaction,getmempoolinfo,estimatesmartfee,sendrawtransaction"
	whitelistFulcrum  = "rpcwhitelist=fulcrum:getblockchaininfo,getnetworkinfo,getzmqnotifications,uptime,help,getblockhash,getblock,getblockheader,getrawmempool,getrawtransaction,getmempoolinfo,estimatesmartfee,sendrawtransaction,submitpackage,validateaddress"
	whitelistSatbooth = "rpcwhitelist=satbooth:getnewaddress,getaddressinfo,deriveaddresses,listsinceblock,gettransaction,getmempoolentry,getblockchaininfo,getwalletinfo,listdescriptors,getdescriptorinfo,getmempoolinfo,gettxspendingprevout"
)

// RelayURL, BitcoindURL and WalletRPCURL are the URLs stage 4 builds from the operator's own answer, never from a
// handoff. The addresses are IPv4 (JoinHostPort would bracket an IPv6 one).
func RelayURL(hubPrivateAddr string) string {
	return "ws://" + net.JoinHostPort(hubPrivateAddr, relayClientPort)
}
func BitcoindURL(hubPrivateAddr string) string {
	return "http://" + net.JoinHostPort(hubPrivateAddr, bitcoindRPCPort)
}
func WalletRPCURL(hubPrivateAddr string) string {
	return "http://" + net.JoinHostPort(hubPrivateAddr, walletRPCPort)
}

func encodeJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // a URL's '&' stays '&'
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil { // the structs below hold strings, ints, floats and bools
		panic("setup: " + err.Error())
	}
	return buf.Bytes()
}

// hubNetwork is HUB_NETWORK for an answers network.
func hubNetwork(n string) string {
	if n == "mainnet" {
		return "bitcoin"
	}
	return n
}

// RelayJSON is relay.json: the pairs in the order invoice, payout. From the handoff it takes only the two client
// keys: the address is the hub host's own answer. The caller must have run a.CheckAgainst(in) and checked that
// out.PayoutWallet is set exactly when in.PayoutClient is (else the relay refuses an empty wallet key).
func RelayJSON(a *HubAnswers, in *PublicToHub, out *HubToPublic) []byte {
	type pair struct {
		Wallet string `json:"wallet"`
		Client string `json:"client"`
	}
	v := struct {
		HubListen      string `json:"hub_listen"`
		ClientListen   string `json:"client_listen"`
		ClientRelayURL string `json:"client_relay_url"`
		Pairs          []pair `json:"pairs"`
	}{relayHubListen, net.JoinHostPort(a.HubIP, relayClientPort), RelayURL(a.HubPrivateAddr),
		[]pair{{out.InvoiceWallet, in.InvoiceClient}}}
	if in.PayoutClient != "" {
		v.Pairs = append(v.Pairs, pair{out.PayoutWallet, in.PayoutClient})
	}
	return encodeJSON(v)
}

// BitcoinConf is bitcoin.conf. The chain and the public host's address are the answers'; the handoff gives only
// SatBooth's rpcauth line. hubAuth and fulcrumAuth are the two rpcauth lines ("rpcauth=user:salt$hash"); the
// handoff's line for SatBooth is written only with on-chain. The caller must have run a.CheckAgainst(in).
func BitcoinConf(a *HubAnswers, in *PublicToHub, hubAuth, fulcrumAuth string) []byte {
	chain, section := "main", "main"
	switch a.Network {
	case "signet":
		chain, section = "signet", "signet"
	case "regtest":
		chain, section = "regtest", "regtest"
	}
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }
	line("chain=" + chain)
	line("server=1")
	line("txindex=1")
	line("")
	line("[" + section + "]")
	line("rpcbind=0.0.0.0")
	line("rpcport=" + bitcoindRPCPort)
	line("rpcallowip=" + a.HubSubnet)
	line("rpcallowip=" + a.ElectrumSubnet)
	if in.SatboothRPCAuth != "" {
		line("rpcallowip=" + hostPrefix(a.PublicPrivateAddr))
	}
	line(hubAuth)
	line(whitelistHub)
	line(fulcrumAuth)
	line(whitelistFulcrum)
	if in.SatboothRPCAuth != "" {
		line(in.SatboothRPCAuth)
		line(whitelistSatbooth)
	}
	line("rpcwhitelistdefault=0")
	return []byte(b.String())
}

// hostPrefix is addr/32: the answers' addresses are IPv4.
func hostPrefix(addr string) string { return addr + "/32" }

// HubEnv is hub.env.
func HubEnv(hubPassword string) []byte {
	return []byte("LDK_BITCOIND_RPC_PASSWORD=" + hubPassword + "\n")
}

// FulcrumConf is fulcrum.conf.
func FulcrumConf(fulcrumPassword string) []byte {
	return []byte("rpcuser = fulcrum\nrpcpassword = " + fulcrumPassword + "\npeering = false\nannounce = false\n")
}

// MonerodConf is monerod.conf: deploy/hub/config-example/monerod.conf without its comment header (TestMonerodConf).
func MonerodConf() []byte {
	return []byte(`# A pruned node is enough for SatBooth's view-only wallet.
prune-blockchain=1
no-igd=1

# The RPC is for wallet-rpc only: monerod is alone with it on the bundle's monero network, the RPC is not published, and
# compose.yml adds --restricted-rpc. monerod listens on every address of its container.
rpc-bind-ip=0.0.0.0
rpc-bind-port=18081
confirm-external-bind=1

log-level=0
`)
}

// WalletRPCLogin is wallet-rpc/rpc-login.
func WalletRPCLogin(user, password string) []byte { return []byte(user + ":" + password + "\n") }

// HubComposeEnv is the hub bundle's env file, the keys in the order of deploy/hub/example.env.
func HubComposeEnv(a *HubAnswers) []byte {
	return []byte("HUB_PRIVATE_ADDR=" + a.HubPrivateAddr + "\n" +
		"HUB_ADMIN_ADDR=" + a.HubAdminAddr + "\n" +
		"HUB_SUBNET=" + a.HubSubnet + "\n" +
		"HUB_IP=" + a.HubIP + "\n" +
		"ELECTRUM_SUBNET=" + a.ElectrumSubnet + "\n" +
		"HUB_NETWORK=" + hubNetwork(a.Network) + "\n" +
		"HUB_CONFIG_DIR=" + a.HostDir + "/config\n" +
		"MONERO_WALLET_DIR=" + a.HostDir + "/wallet\n")
}

// PublicComposeEnv is the public bundle's env file, the keys in the order of deploy/public/example.env.
func PublicComposeEnv(a *PublicAnswers) []byte {
	return []byte("PUBLIC_ADDR=" + a.PublicAddr + "\n" +
		"PUBLIC_PORT=" + strconv.Itoa(a.PublicPort) + "\n" +
		"SATBOOTH_SUBNET=" + satboothSubnet + "\n" +
		"SATBOOTH_IP=" + satboothIP + "\n" +
		"SATBOOTH_ETC=" + a.HostDir + "/etc\n" +
		"SATBOOTH_BACKUPS=" + a.HostDir + "/backups\n")
}

// APIKeysJSON is api-keys.json: the SHA-256 of each key, never the key. The three slices must have the same length,
// at least one entry, and no key may be empty; the loader checks the ids and capabilities.
func APIKeysJSON(ids, capabilities, keys []string) ([]byte, error) {
	if len(ids) == 0 || len(ids) != len(capabilities) || len(ids) != len(keys) {
		return nil, errors.New("setup: the API key ids, capabilities and keys must be lists of one equal, nonzero length")
	}
	type entry struct {
		ID         string `json:"id"`
		Capability string `json:"capability"`
		SHA256     string `json:"sha256"`
	}
	v := struct {
		Keys []entry `json:"keys"`
	}{Keys: make([]entry, len(ids))}
	for i := range ids {
		if keys[i] == "" {
			return nil, errors.New("setup: an API key is empty")
		}
		sum := sha256.Sum256([]byte(keys[i]))
		v.Keys[i] = entry{ids[i], capabilities[i], hex.EncodeToString(sum[:])}
	}
	return encodeJSON(v), nil
}

type tier struct {
	UpToSat       int64 `json:"up_to_sat"`
	Confirmations int   `json:"confirmations"`
}
type moneroTier struct {
	UpToPiconero  int64 `json:"up_to_piconero"`
	Confirmations int   `json:"confirmations"`
}
type webhookBlock struct {
	URL        string `json:"url"`
	SecretFile string `json:"secret_file"`
}

type webhookEndpointBlock struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SecretFile string `json:"secret_file"`
}

// SatboothJSON is satbooth.json. The values the answers do not decide are those of cmd/satbooth/testdata/config-example.json
// (TestDefaultsMatchTheExample), except margin_percent, which is the documented default "0": a margin is the
// merchant's pricing decision. The blocks of a rail or feature the answers leave out are left out. The descriptor is
// written as the operator gave it. The caller must have checked that h.PayoutWallet is set exactly when the answers
// have approval keys (a payout block without a wallet key is refused by the service).
func SatboothJSON(a *PublicAnswers, h *HubToPublic) []byte {
	type nwcBlock struct {
		RelayURL       string `json:"relay_url"`
		WalletPubKey   string `json:"wallet_pubkey"`
		ClientKeyFile  string `json:"client_key_file"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Network        string `json:"network"`
	}
	type limitsBlock struct {
		RatePerSec        float64 `json:"rate_per_sec"`
		Burst             int     `json:"burst"`
		MaxOpen           int     `json:"max_open"`
		NewShare          float64 `json:"new_share"`
		DailyCount        int     `json:"daily_count"`
		DailyMsat         int64   `json:"daily_msat"`
		MinMsat           int64   `json:"min_msat"`
		MaxMsat           int64   `json:"max_msat"`
		MaxOpenPerAccount int     `json:"max_open_per_account,omitempty"`
	}
	type onchainBlock struct {
		RPCURL          string `json:"rpc_url"`
		RPCUser         string `json:"rpc_user"`
		RPCPasswordFile string `json:"rpc_password_file"`
		Wallet          string `json:"wallet"`
		Descriptor      string `json:"descriptor"`
		Network         string `json:"network"`
		Confirmations   []tier `json:"confirmations"`
		GapAlert        int64  `json:"gap_alert"`
		PollSeconds     int    `json:"poll_seconds"`
		PollRate        int    `json:"poll_rate"`
		TimeoutSeconds  int    `json:"timeout_seconds"`
	}
	type moneroBlock struct {
		WalletRPCURL    string       `json:"wallet_rpc_url"`
		RPCUser         string       `json:"rpc_user"`
		RPCPasswordFile string       `json:"rpc_password_file"`
		Address         string       `json:"address"`
		Network         string       `json:"network"`
		AccountIndex    int          `json:"account_index"`
		Confirmations   []moneroTier `json:"confirmations"`
		UnusedAlert     int64        `json:"unused_alert"`
		PollSeconds     int          `json:"poll_seconds"`
		PollRate        int          `json:"poll_rate"`
		TimeoutSeconds  int          `json:"timeout_seconds"`
	}
	type payoutBlock struct {
		RelayURL        string   `json:"relay_url"`
		WalletPubKey    string   `json:"wallet_pubkey"`
		ClientKeyFile   string   `json:"client_key_file"`
		TimeoutSeconds  int      `json:"timeout_seconds"`
		Network         string   `json:"network"`
		ApprovalKeys    []string `json:"approval_keys"`
		MaxMsat         int64    `json:"max_msat"`
		DailyMsat       int64    `json:"daily_msat"`
		BudgetAlertMsat int64    `json:"budget_alert_msat"`
	}
	type ratesBlock struct {
		Providers        []string `json:"providers"`
		Currencies       []string `json:"currencies"`
		MarginPercent    string   `json:"margin_percent"`
		MinSources       int      `json:"min_sources"`
		MaxSpreadPercent string   `json:"max_spread_percent"`
		MaxAgeSeconds    int      `json:"max_age_seconds"`
		FetchSeconds     int      `json:"fetch_seconds"`
	}
	v := struct {
		PublicListen         string                 `json:"public_listen"`
		PrivateListen        string                 `json:"private_listen"`
		CheckoutBaseURL      string                 `json:"checkout_base_url"`
		DataDir              string                 `json:"data_dir"`
		NWC                  nwcBlock               `json:"nwc"`
		APIKeysFile          string                 `json:"api_keys_file"`
		Webhooks             []webhookEndpointBlock `json:"webhooks"`
		Alerts               *webhookBlock          `json:"alerts,omitempty"`
		Limits               limitsBlock            `json:"limits"`
		InvoiceExpiryMinutes int                    `json:"invoice_expiry_minutes"`
		AllowSwap            bool                   `json:"allow_swap"`
		Onchain              *onchainBlock          `json:"onchain,omitempty"`
		Monero               *moneroBlock           `json:"monero,omitempty"`
		Payout               *payoutBlock           `json:"payout,omitempty"`
		Rates                *ratesBlock            `json:"rates,omitempty"`
	}{
		PublicListen:    satboothPublicLn,
		PrivateListen:   satboothPrivateLn,
		CheckoutBaseURL: a.CheckoutBaseURL,
		DataDir:         "/data",
		NWC: nwcBlock{RelayURL: RelayURL(a.HubPrivateAddr), WalletPubKey: h.InvoiceWallet,
			ClientKeyFile: satboothEtc + "nwc-client.key", TimeoutSeconds: 70, Network: a.Network},
		APIKeysFile: satboothEtc + "api-keys.json",
		Webhooks:    []webhookEndpointBlock{{"main", a.WebhookURL, satboothEtc + "webhook.secret"}},
		Limits: limitsBlock{RatePerSec: 1, Burst: 10, MaxOpen: 1000, NewShare: 0.2, DailyCount: 5000,
			DailyMsat: 100000000000, MinMsat: 1000, MaxMsat: 10000000000},
		InvoiceExpiryMinutes: 20,
	}
	if a.AlertsURL != "" {
		v.Alerts = &webhookBlock{a.AlertsURL, satboothEtc + "alerts.secret"}
	}
	if a.Descriptor != "" {
		v.Onchain = &onchainBlock{RPCURL: BitcoindURL(a.HubPrivateAddr), RPCUser: "satbooth",
			RPCPasswordFile: satboothEtc + "bitcoind.pass", Wallet: "satbooth", Descriptor: a.Descriptor,
			Network:       a.Network,
			Confirmations: []tier{{1000000, 1}, {10000000, 3}, {0, 6}},
			GapAlert:      1000, PollSeconds: 10, PollRate: 5, TimeoutSeconds: 30}
	}
	if a.MoneroAddress != "" {
		v.Monero = &moneroBlock{WalletRPCURL: WalletRPCURL(a.HubPrivateAddr), RPCUser: "satbooth",
			RPCPasswordFile: satboothEtc + "wallet-rpc.pass", Address: a.MoneroAddress, Network: a.MoneroNetwork,
			Confirmations: []moneroTier{{1000000000000, 1}, {10000000000000, 3}, {0, 10}},
			UnusedAlert:   100, PollSeconds: 10, PollRate: 5, TimeoutSeconds: 30}
	}
	if len(a.ApprovalKeys) > 0 {
		v.Payout = &payoutBlock{RelayURL: RelayURL(a.HubPrivateAddr), WalletPubKey: h.PayoutWallet,
			ClientKeyFile: satboothEtc + "payout.key", TimeoutSeconds: 70, Network: a.Network,
			ApprovalKeys: a.ApprovalKeys, MaxMsat: int64(a.PayoutMaxSat) * 1000,
			DailyMsat: int64(a.PayoutDailySat) * 1000, BudgetAlertMsat: int64(a.PayoutAlertSat) * 1000}
	}
	if len(a.Currencies) > 0 {
		v.Rates = &ratesBlock{Providers: []string{"kraken", "coinbase", "yadio", "bitfinex"}, Currencies: a.Currencies,
			MarginPercent: "0", MinSources: 2, MaxSpreadPercent: "2", MaxAgeSeconds: 300, FetchSeconds: 60}
	}
	return encodeJSON(v)
}
