package setup

import (
	"errors"
	"strings"
	"testing"
)

func TestPublicInitRefusals(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	before := snapshot(t, e.pubDir)
	for _, u := range []string{"", "ftp://x", "http://h/?q=1", "http://u:p@h"} {
		if _, err := PublicInit(e.pubDir, &e.pub, u, detRand(1)); !errors.Is(err, ErrHubURL) {
			t.Fatalf("%q: %v", u, err)
		}
	}
	bad := e.pub
	bad.HostDir = "/srv/a b"
	if _, err := PublicInit(e.pubDir, &bad, "http://127.0.0.1:8080", detRand(1)); err == nil {
		t.Fatal("a host directory with a space")
	}
	if _, err := PublicInit(e.pubDir, &e.pub, "http://127.0.0.1:8080", strings.NewReader("short")); err == nil {
		t.Fatal("a short random source")
	}
	if _, err := PublicInit(e.pubDir+"/missing", &e.pub, "http://127.0.0.1:8080", detRand(1)); err == nil {
		t.Fatal("a missing base directory")
	}
	sameTree(t, "refusals", before, snapshot(t, e.pubDir))
}

func TestPublicFinishRules(t *testing.T) {
	run := func(name string, fx func(*testing.T) *fixture, edit func(h *HubToPublic, pass *[]byte), want error) {
		t.Helper()
		e := newEnv(t, fx(t))
		e.upTo(3)
		h, _ := ParseHubToPublic(e.read(e.hubDir, "setup/hub-to-public.json"))
		var pass []byte
		if e.pub.MoneroAddress != "" {
			pass = e.read(e.hubDir, "setup/carry/wallet-rpc.pass")
		}
		edit(h, &pass)
		hb := h.Marshal()
		before := snapshot(t, e.pubDir)
		_, err := PublicFinish(e.pubDir, hb, Digest(hb), pass)
		if err == nil || want != nil && !errors.Is(err, want) {
			t.Fatalf("%s: %v", name, err)
		}
		sameTree(t, name, before, snapshot(t, e.pubDir))
	}
	run("payout wallet without refunds", fxLightning, func(h *HubToPublic, _ *[]byte) { h.PayoutWallet = curveKeys[3] }, ErrMismatch)
	run("no payout wallet with refunds", fxEveryRail, func(h *HubToPublic, _ *[]byte) { h.PayoutWallet = "" }, ErrMismatch)
	run("no password file with Monero", fxEveryRail, func(_ *HubToPublic, p *[]byte) { *p = nil }, ErrMismatch)
	run("a password file without Monero", fxLightning, func(_ *HubToPublic, p *[]byte) { *p = []byte(strings.Repeat("a", 64) + "\n") }, ErrMismatch)
	run("a short password file", fxEveryRail, func(_ *HubToPublic, p *[]byte) { *p = []byte("abc\n") }, nil)
	run("a password file without its newline", fxEveryRail, func(_ *HubToPublic, p *[]byte) { *p = []byte(strings.Repeat("a", 64)) }, nil)
	run("an uppercase password", fxEveryRail, func(_ *HubToPublic, p *[]byte) { *p = []byte(strings.Repeat("A", 64) + "\n") }, nil)
	run("an empty password file", fxEveryRail, func(_ *HubToPublic, p *[]byte) { *p = []byte{} }, nil)
	run("a wallet that is not a point", fxEveryRail, func(h *HubToPublic, _ *[]byte) { h.InvoiceWallet = strings.Repeat("0", 64) }, nil)

	// A wallet key equal to a client key.
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	h := &HubToPublic{Version: 1, InvoiceWallet: clientKey(t, e.pubDir, "nwc-client.key"), PayoutWallet: curveKeys[3]}
	hb := h.Marshal()
	if _, err := PublicFinish(e.pubDir, hb, Digest(hb), e.read(e.hubDir, "setup/carry/wallet-rpc.pass")); err == nil {
		t.Fatal("a client key as a wallet key")
	}
}

func TestAnswerErrorsNameTheirField(t *testing.T) {
	for field, edit := range map[string]func(*PublicAnswers){
		"network":             func(a *PublicAnswers) { a.Network = "testnet" },
		"public_private_addr": func(a *PublicAnswers) { a.PublicPrivateAddr = a.HubPrivateAddr },
		"public_port":         func(a *PublicAnswers) { a.PublicPort = 70000 },
		"alerts_url":          func(a *PublicAnswers) { a.AlertsURL = a.WebhookURL },
		"descriptor":          func(a *PublicAnswers) { a.Descriptor = "wpkh(nope)" },
		"monero_network":      func(a *PublicAnswers) { a.MoneroNetwork = "mainnet" },
		"approval_keys":       func(a *PublicAnswers) { a.ApprovalKeys = []string{"zz"} },
		"currencies":          func(a *PublicAnswers) { a.Currencies = []string{"usd"} },
		"host_dir":            func(a *PublicAnswers) { a.HostDir = "/srv/a b" },
		"checkout_base_url":   func(a *PublicAnswers) { a.CheckoutBaseURL = "ftp://x" },
	} {
		a := baseAnswers()
		edit(&a)
		_, err := ParsePublicAnswers(mustJSON(a))
		var ae *AnswerError
		if !errors.As(err, &ae) || ae.Field != field || err.Error() != "setup: answers: "+ae.Field+" "+ae.Rule || ae.Rule == "" {
			t.Fatalf("%s: %v", field, err)
		}
	}
	h := baseHub()
	h.HubIP = "10.0.0.1"
	_, err := ParseHubAnswers(mustJSON(h))
	var ae *AnswerError
	if !errors.As(err, &ae) || ae.Field != "hub_ip" {
		t.Fatalf("%v", err)
	}
	// A refusal about the whole document is not one field's.
	for _, b := range [][]byte{nil, []byte("[]"), []byte(`{"network":"mainnet","network":"mainnet"}`), []byte(`{"nope":1}`)} {
		if _, err := ParsePublicAnswers(b); err == nil || errors.As(err, &ae) {
			t.Fatalf("%q: %v", b, err)
		}
	}
	if _, err := ParsePublicAnswers([]byte(`{"nope":1}`)); errors.As(err, &ae) {
		t.Fatal("an unknown key is a refusal of the document")
	}
	// The mismatch with the other host wraps ErrMismatch and keeps the text the documentation quotes.
	in := &PublicToHub{Version: 1, Network: "signet", HubPrivateAddr: "192.0.2.20", PublicPrivateAddr: "192.0.2.10", InvoiceClient: curveKeys[0]}
	h = baseHub()
	err = h.CheckAgainst(in)
	if !errors.Is(err, ErrMismatch) || err.Error() != "setup: the two hosts were given different values: network" {
		t.Fatalf("%v", err)
	}
}
