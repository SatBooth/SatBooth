package setup

import (
	"errors"
	"strings"
	"testing"
)

func TestHubInitMismatchIsRefused(t *testing.T) {
	for _, c := range []struct {
		field string
		edit  func(*HubAnswers)
	}{
		{"network", func(a *HubAnswers) { a.Network = "signet" }},
		{"hub_private_addr", func(a *HubAnswers) { a.HubPrivateAddr = "192.0.2.21" }},
		{"public_private_addr", func(a *HubAnswers) { a.PublicPrivateAddr = "192.0.2.11" }},
	} {
		e := newEnv(t, fxEveryRail(t))
		e.upTo(1)
		h := e.read(e.pubDir, "setup/public-to-hub.json")
		c.edit(&e.hub)
		before := snapshot(t, e.hubDir)
		_, err := HubInit(e.hubDir, &e.hub, h, Digest(h), detRand(1))
		if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), c.field) {
			t.Fatalf("%s: %v", c.field, err)
		}
		sameTree(t, c.field, before, snapshot(t, e.hubDir))
	}
}

func TestHubPairRules(t *testing.T) {
	cases := []struct {
		name     string
		fx       func(*testing.T) *fixture
		inv, pay string
		mismatch bool
	}{
		{"payout wallet without a payout client", fxLightning, curveKeys[2], curveKeys[3], true},
		{"no payout wallet with a payout client", fxEveryRail, curveKeys[2], "", true},
		{"the two wallets equal", fxEveryRail, curveKeys[2], curveKeys[2], false},
		{"not hex", fxEveryRail, "zz", curveKeys[3], false},
		{"uppercase", fxEveryRail, strings.ToUpper(curveKeys[2]), curveKeys[3], false},
		{"not a point", fxEveryRail, strings.Repeat("0", 64), curveKeys[3], false},
		{"empty invoice wallet", fxEveryRail, "", curveKeys[3], false},
	}
	for _, c := range cases {
		e := newEnv(t, c.fx(t))
		e.upTo(2)
		before := snapshot(t, e.hubDir)
		_, err := HubPair(e.hubDir, c.inv, c.pay)
		if err == nil || errors.Is(err, ErrMismatch) != c.mismatch {
			t.Fatalf("%s: %v", c.name, err)
		}
		sameTree(t, c.name, before, snapshot(t, e.hubDir))
	}
	// A wallet key equal to a client key.
	e := newEnv(t, fxEveryRail(t))
	e.upTo(2)
	in, _ := ParsePublicToHub(e.read(e.pubDir, "setup/public-to-hub.json"))
	for _, c := range [][2]string{{in.InvoiceClient, curveKeys[3]}, {curveKeys[2], in.PayoutClient}, {in.PayoutClient, curveKeys[3]}} {
		before := snapshot(t, e.hubDir)
		if _, err := HubPair(e.hubDir, c[0], c[1]); err == nil {
			t.Fatal("a client key was taken as a wallet key")
		}
		sameTree(t, "client key", before, snapshot(t, e.hubDir))
	}
}

// An empty invoice Wallet Public Key is refused with its own message, before anything is read or written.
func TestHubPairRequiresTheInvoiceKey(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(2)
	before := snapshot(t, e.hubDir)
	for _, pay := range []string{"", curveKeys[3]} {
		_, err := HubPair(e.hubDir, "", pay)
		if err == nil || !strings.Contains(err.Error(), "invoice connection's Wallet Public Key is required") ||
			strings.Contains(err.Error(), "handoff") {
			t.Fatalf("%v", err)
		}
	}
	// Even where the stage could not run at all.
	if _, err := HubPair(baseDir(t), "", ""); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("%v", err)
	}
	sameTree(t, "refused", before, snapshot(t, e.hubDir))
}
