package setup

import (
	"errors"
	"fmt"
	"io"

	"github.com/satbooth/satbooth/nostr"
)

// HubInit is stage 2, on the hub host: the hub host's configuration and secrets, from its own answers and the public
// host's handoff.
func (lk *Lock) HubInit(a *HubAnswers, handoff []byte, digest string, rand io.Reader) (*Report, error) {
	ha, err := ParseHubAnswers(mustMarshal(a))
	if err != nil {
		return nil, err
	}
	_, _, err = lk.begin(StageHubInit)
	if err != nil {
		return nil, err
	}
	root := lk.root
	if err := checkDigest(handoff, digest); err != nil {
		return nil, err
	}
	in, err := ParsePublicToHub(handoff)
	if err != nil {
		return nil, err
	}
	if err := ha.CheckAgainst(in); err != nil {
		return nil, err
	}

	secret := func() (string, error) { return NewSecret(rand) }
	hubPass, err := secret()
	if err != nil {
		return nil, err
	}
	fulPass, err := secret()
	if err != nil {
		return nil, err
	}
	hubAuth, err := RPCAuthLine(rand, "hub", hubPass)
	if err != nil {
		return nil, err
	}
	fulAuth, err := RPCAuthLine(rand, "fulcrum", fulPass)
	if err != nil {
		return nil, err
	}
	files := []file{
		{name: "config/hub.env", data: HubEnv(hubPass)},
		{name: "config/fulcrum.conf", data: FulcrumConf(fulPass)},
		{name: "config/bitcoin.conf", data: BitcoinConf(ha, in, hubAuth, fulAuth)},
	}
	dirs := []string{"setup", "config", "wallet"}
	if in.Monero {
		loginPass, err := secret()
		if err != nil {
			return nil, err
		}
		walletPass, err := secret()
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, "config/wallet-rpc", "setup/carry")
		files = append(files,
			file{name: "config/monerod.conf", data: MonerodConf()},
			file{name: "config/wallet-rpc/rpc-login", data: WalletRPCLogin(in.MoneroRPCUser, loginPass)},
			file{name: "config/wallet-rpc/wallet.pass", data: line(walletPass)},
			file{name: "setup/carry/wallet-rpc.pass", data: line(loginPass)})
	}
	files = append(files, file{name: "setup/read-public-to-hub.json", data: append([]byte{}, handoff...)},
		file{name: "compose.env", data: HubComposeEnv(ha)})

	p := &plan{stage: StageHubInit, dirs: dirs, files: files,
		next: &state{Version: 1, Host: hostHub, Stage: StageHubInit, Hub: ha, Handoff: in}}
	if err := p.commit(root); err != nil {
		return nil, err
	}

	env := ha.HostDir + "/compose.env"
	dc := lk.composeCmd("hub") + " --env-file " + env
	next := []string{
		"Start the hub alone: " + dc + " up -d --build hub",
		"Open an SSH tunnel to its admin page: ssh -L 8080:" + ha.HubAdminAddr + ":8080 <hub host>   (then http://127.0.0.1:8080)",
		"Set the unlock password now, before anything else.",
		"Then open the two links stage 1 printed on the public host (the second only with refunds), and copy the " +
			"two Wallet Public Keys the hub shows (invoice, then payout)." + tick,
	}
	if in.SatboothRPCAuth != "" {
		ex := dc + " exec bitcoind bitcoin-cli -conf=/etc/bitcoin/bitcoin.conf -datadir=/data "
		next = append(next,
			"On-chain, once bitcoind runs, make the watch-only wallet: "+ex+
				"-named createwallet wallet_name=satbooth disable_private_keys=true blank=true descriptors=true load_on_startup=true",
			"Get the descriptor's checksum: "+ex+"getdescriptorinfo \"<your descriptor>\"",
			"Import it: "+ex+"-named -rpcwallet=satbooth importdescriptors "+
				"requests='[{\"desc\":\"<your descriptor with its #checksum>\",\"active\":true,\"internal\":false,\"timestamp\":\"now\"}]'")
	}
	if in.Monero {
		next = append(next,
			"With Monero, once monerod runs, make the view-only wallet: "+dc+" --profile monero run --rm --no-deps "+
				"--entrypoint monero-wallet-cli wallet-rpc --generate-from-view-key /wallet/satbooth-view "+
				"--password-file /etc/wallet-rpc/wallet.pass --restore-height <height> --daemon-address monerod:18081 --log-file /tmp/wallet-cli.log",
			"It asks for the primary address and the private view key. The wallet's password is read from the file wallet-rpc opens "+
				"the wallet with (/etc/wallet-rpc/wallet.pass in the container), so you neither type nor read it.")
	}
	next = append(next, "Then run stage 3: satbooth-setup hub pair.")
	return &Report{Digest: Digest(handoff), Next: next}, nil
}

// HubPair is stage 3, on the hub host: relay.json and the handoff back, from the two Wallet Public Keys.
func (lk *Lock) HubPair(invoiceWallet, payoutWallet string) (*Report, error) {
	if invoiceWallet == "" {
		return nil, errors.New("setup: the invoice connection's Wallet Public Key is required (64 lowercase hex characters)")
	}
	st, prev, err := lk.begin(StageHubPair)
	if err != nil {
		return nil, err
	}
	root := lk.root
	in := st.Handoff
	if (in.PayoutClient != "") != (payoutWallet != "") {
		return nil, fmt.Errorf("%w: payout Wallet Public Key (refunds are %s on the public host)", ErrMismatch, onOff(in.PayoutClient != ""))
	}
	out := &HubToPublic{Version: 1, InvoiceWallet: invoiceWallet, PayoutWallet: payoutWallet}
	for _, w := range []string{invoiceWallet, payoutWallet} {
		if w == "" {
			continue
		}
		if !isHex(w, 64) || nostr.ValidatePubKey(w) != nil {
			return nil, errors.New("setup: a Wallet Public Key must be 64 lowercase hex characters and a valid public key")
		}
		if w == in.InvoiceClient || w == in.PayoutClient {
			return nil, errors.New("setup: a Wallet Public Key is a client public key: the two are different keys")
		}
	}
	if payoutWallet != "" && invoiceWallet == payoutWallet {
		return nil, errors.New("setup: the invoice and payout Wallet Public Keys must differ")
	}
	hb := out.Marshal()
	if _, err := ParseHubToPublic(hb); err != nil {
		return nil, err
	}
	files := []file{
		{name: "config/relay.json", data: RelayJSON(st.Hub, in, out)},
		{name: "setup/hub-to-public.json", data: hb},
	}
	next := *st
	next.Stage = StageHubPair
	p := &plan{stage: StageHubPair, files: files, next: &next, prev: prev}
	if err := p.commit(root); err != nil {
		return nil, err
	}
	hd := st.Hub.HostDir
	profile := ""
	if in.Monero {
		profile = "--profile monero "
	}
	n := []string{
		"Give the public host's operator this digest to compare: " + Digest(hb),
		"Start the rest: " + lk.composeCmd("hub") + " --env-file " + st.Hub.HostDir + "/compose.env " + profile + "up -d --build",
		"Copy " + hd + "/setup/hub-to-public.json to the public host.",
	}
	if in.Monero {
		n = append(n, "Copy "+hd+"/setup/carry/wallet-rpc.pass to the public host as a 0600 file.",
			"Delete "+hd+"/setup/carry/wallet-rpc.pass on this host once it is on the public host.")
	}
	return &Report{Digest: Digest(hb), Next: n}, nil
}
