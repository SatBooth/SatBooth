package setup

import (
	"errors"
	"fmt"
	"io"

	"github.com/satbooth/satbooth/nostr"
)

// The connection names the pairing links carry.
const (
	invoiceConnName = "SatBooth invoice"
	payoutConnName  = "SatBooth payout"
	moneroRPCUser   = "satbooth"
)

// PublicInit is stage 1, on the public host: SatBooth's secrets and the handoff for the hub host. rand is the source
// of every secret, the two NWC client keys included.
func (lk *Lock) PublicInit(a *PublicAnswers, hubURL string, rand io.Reader) (*Report, error) {
	pa, err := ParsePublicAnswers(mustMarshal(a)) // the same validation as the answers file
	if err != nil {
		return nil, err
	}
	hub, err := ParseHub(hubURL)
	if err != nil {
		return nil, err
	}
	_, _, err = lk.begin(StagePublicInit)
	if err != nil {
		return nil, err
	}
	root := lk.root

	invoiceHex, invoicePub, err := newClientKey(rand)
	if err != nil {
		return nil, err
	}
	h := &PublicToHub{Version: 1, Network: pa.Network, HubPrivateAddr: pa.HubPrivateAddr,
		PublicPrivateAddr: pa.PublicPrivateAddr, InvoiceClient: invoicePub}
	files := []file{{name: "etc/nwc-client.key", data: line(invoiceHex)}}
	hasPayout := len(pa.ApprovalKeys) > 0
	if hasPayout {
		payoutHex, payoutPub, err := newClientKey(rand)
		if err != nil {
			return nil, err
		}
		h.PayoutClient = payoutPub
		files = append(files, file{name: "etc/payout.key", data: line(payoutHex)})
	}

	secret := func() (string, error) { return NewSecret(rand) }
	webhookSecret, err := secret()
	if err != nil {
		return nil, err
	}
	files = append(files, file{name: "etc/webhook.secret", data: line(webhookSecret)})
	if pa.AlertsURL != "" {
		s, err := secret()
		if err != nil {
			return nil, err
		}
		files = append(files, file{name: "etc/alerts.secret", data: line(s)})
	}
	if pa.Descriptor != "" {
		pass, err := secret()
		if err != nil {
			return nil, err
		}
		if h.SatboothRPCAuth, err = RPCAuthLine(rand, "satbooth", pass); err != nil {
			return nil, err
		}
		files = append(files, file{name: "etc/bitcoind.pass", data: line(pass)})
	}
	if pa.MoneroAddress != "" {
		h.Monero, h.MoneroRPCUser = true, moneroRPCUser
	}

	ids := []string{"orders-write", "orders-read"}
	caps := []string{"orders:write", "orders:read"}
	if len(pa.ApprovalKeys) > 0 {
		ids = append(ids, "refunds-write", "refunds-read")
		caps = append(caps, "refunds:write", "refunds:read")
	}
	keys := make([]string, len(ids))
	for i := range ids {
		if keys[i], err = secret(); err != nil {
			return nil, err
		}
	}
	apiKeys, err := APIKeysJSON(ids, caps, keys)
	if err != nil {
		return nil, err
	}
	files = append(files, file{name: "etc/api-keys.json", data: apiKeys})
	for i, id := range ids {
		files = append(files, file{name: "setup/api-keys/" + id + ".key", data: line(keys[i])})
	}
	hb := h.Marshal()
	if _, err := ParsePublicToHub(hb); err != nil {
		return nil, err
	}
	files = append(files, file{name: "setup/public-to-hub.json", data: hb})

	links := make([]string, 0, 2)
	l, err := InvoiceLink(hub, h.InvoiceClient, invoiceConnName)
	if err != nil {
		return nil, err
	}
	links = append(links, l)
	if hasPayout {
		if l, err = PayoutLink(hub, h.PayoutClient, payoutConnName, pa.PayoutBudgetSat); err != nil {
			return nil, err
		}
		links = append(links, l)
	}

	p := &plan{stage: StagePublicInit, dirs: []string{"setup", "etc", "backups", "setup/api-keys"}, files: files,
		next: &state{Version: 1, Host: hostPublic, Stage: StagePublicInit, Public: pa, HubURL: hub,
			InvoiceClient: h.InvoiceClient, PayoutClient: h.PayoutClient}}
	if err := p.commit(root); err != nil {
		return nil, err
	}
	hd := pa.HostDir
	next := []string{
		"Open the two links above only after the hub's unlock password is set (stage 2, on the hub host)." +
			tick,
		"Copy " + hd + "/setup/public-to-hub.json to the hub host, and give the hub host this digest to compare: " + Digest(hb),
		"The plain API keys are in " + hd + "/setup/api-keys/ (one file per key) and the webhook secret is in " + hd +
			"/etc/webhook.secret: give them to the merchant's application.",
	}
	if len(links) == 1 {
		next[0] = "Open the link above only after the hub's unlock password is set (stage 2, on the hub host)." + tick
	}
	return &Report{Digest: Digest(hb), Links: links, Next: next}, nil
}

// tick is added to the two steps that tell the operator to open the links (stages 1 and 2): the hub's form pre-ticks "Read your node info" when the link lists
// get_info, and a connection without it states no network, which SatBooth refuses at start.
const tick = " " + TickSentence

// TickSentence is the one text that tells the operator to leave the node-info permission ticked on the hub's page: the
// setup steps add it (tick) and satbooth-pair prints it after its link.
const TickSentence = "Leave every permission ticked on the hub's page, \"Read your node info\" included: without it SatBooth refuses to start."

// PublicFinish is stage 4, on the public host: satbooth.json from the hub's two Wallet Public Keys. The carried
// password file, when Monero is on, must be exactly what stage 2 writes: 64 lowercase hex characters and one final
// newline (stricter than the loaders, on purpose: a file another tool touched is refused).
func (lk *Lock) PublicFinish(handoff []byte, digest string, walletRPCPass []byte) (*Report, error) {
	st, prev, err := lk.begin(StagePublicDone)
	if err != nil {
		return nil, err
	}
	root := lk.root
	if err := checkDigest(handoff, digest); err != nil {
		return nil, err
	}
	out, err := ParseHubToPublic(handoff)
	if err != nil {
		return nil, err
	}
	if (st.PayoutClient != "") != (out.PayoutWallet != "") {
		return nil, fmt.Errorf("%w: payout_wallet (refunds are %s on this host)", ErrMismatch, onOff(st.PayoutClient != ""))
	}
	for _, w := range []string{out.InvoiceWallet, out.PayoutWallet} {
		if w != "" && (w == st.InvoiceClient || w == st.PayoutClient) {
			return nil, errors.New("setup: a Wallet Public Key is a client public key: the two are different keys")
		}
	}
	for _, w := range []string{out.InvoiceWallet, out.PayoutWallet} {
		if w != "" && nostr.ValidatePubKey(w) != nil {
			return nil, errors.New("setup: a Wallet Public Key is not a valid public key")
		}
	}
	monero := st.Public.MoneroAddress != ""
	if monero != (walletRPCPass != nil) {
		return nil, fmt.Errorf("%w: wallet-rpc password file (Monero is %s on this host)", ErrMismatch, onOff(monero))
	}
	files := []file{{name: "etc/satbooth.json", data: SatboothJSON(st.Public, out)}}
	if monero {
		pass, ok := secretLine(walletRPCPass)
		if !ok {
			return nil, errors.New("setup: the carried password file must be 64 lowercase hex characters and a final newline")
		}
		files = append(files, file{name: "etc/wallet-rpc.pass", data: line(pass)})
	}
	files = append(files, file{name: "setup/read-hub-to-public.json", data: append([]byte{}, handoff...)},
		file{name: "compose.env", data: PublicComposeEnv(st.Public)})
	next := *st
	next.Stage = StagePublicDone
	p := &plan{stage: StagePublicDone, files: files, next: &next, prev: prev}
	if err := p.commit(root); err != nil {
		return nil, err
	}
	n := []string{"Start SatBooth: docker compose -f deploy/public/compose.yml --env-file " + st.Public.HostDir +
		"/compose.env up -d --build"}
	if monero {
		n = append(n, "Delete the carried password file from the channel you copied it through.")
	}
	return &Report{Digest: Digest(handoff), Next: n}, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// line is a secret and a final newline.
func line(s string) []byte { return []byte(s + "\n") }

// secretLine returns the 64 hex characters of b, which must be exactly those and one final newline.
func secretLine(b []byte) (string, bool) {
	if len(b) != 65 || b[64] != '\n' || !isHex(string(b[:64]), 64) {
		return "", false
	}
	return string(b[:64]), true
}

// newClientKey draws an NWC client key from rand: 64 hex characters that nostr.ParseSecretKeyHex accepts (a draw
// outside 1..n-1, a chance of about 2^-128, is drawn again, at most 8 times). The file holds the hex and one final
// newline, which is what nostr.WriteNewSecretKeyFile writes and nostr.LoadSecretKeyFile reads.
func newClientKey(rand io.Reader) (secretHex, publicHex string, err error) {
	for i := 0; i < 8; i++ {
		s, err := NewSecret(rand)
		if err != nil {
			return "", "", err
		}
		k, perr := nostr.ParseSecretKeyHex(s)
		if perr != nil {
			continue
		}
		pub := k.PublicKeyHex()
		k.Zero()
		return s, pub, nil
	}
	return "", "", errors.New("setup: cannot draw a valid client key")
}
