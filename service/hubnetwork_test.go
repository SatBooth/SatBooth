package service

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/nwc"
)

var testNetworks = []string{"mainnet", "testnet", "signet", "regtest"}

// Words a hub could send that are not one of the four: none of them may appear in a message.
var unknownNetworks = []string{"bitcoin", "testnet4", "Mainnet", "MAINNET", " mainnet", "mainnet\n", `reg"test`,
	"main\nnet", "SECRETISH-VALUE-123", strings.Repeat("x", 4096), "mainnet\x00", "régtest"}

// checkNetwork: only the configured name passes; a known other name is named; nothing else is ever echoed.
func TestCheckNetwork(t *testing.T) {
	for _, conn := range []string{"invoice", "payout"} {
		for _, want := range testNetworks {
			if err := checkNetwork(&nwc.Info{Network: want}, conn, want); err != nil {
				t.Errorf("%s/%s: %v", conn, want, err)
			}
			err := checkNetwork(&nwc.Info{}, conn, want)
			wantMsg := "the " + conn + " connection's get_info does not state the network; pair it with satbooth-pair -connection " + conn
			if err == nil || err.Error() != wantMsg {
				t.Errorf("empty %s/%s: %v", conn, want, err)
			}
			for _, got := range testNetworks {
				if got == want {
					continue
				}
				err := checkNetwork(&nwc.Info{Network: got}, conn, want)
				exact := "the " + conn + ` connection's hub is on the network "` + got + `", not nwc.network "` + want + `"`
				if err == nil || err.Error() != exact {
					t.Errorf("%s: hub %s, want %s: %v", conn, got, want, err)
				}
			}
			for _, got := range unknownNetworks {
				err := checkNetwork(&nwc.Info{Network: got}, conn, want)
				exact := "the " + conn + " connection's hub reports a network SatBooth does not know"
				if err == nil || err.Error() != exact {
					t.Errorf("%s: unknown %q: %v", conn, got, err)
				}
			}
		}
	}
}

// A seeded property: a state of any bytes passes only if it is byte-equal to the configured name, and the message
// never holds a long state (a short one could be a part of the fixed text) unless it is one of the four names.
func TestCheckNetworkProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(14))
	alphabet := []byte("abcmainetstgrbc\"\n\x00 \xff0123456789MAINET")
	for range 20000 {
		b := make([]byte, rng.Intn(40))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		got, want := string(b), testNetworks[rng.Intn(4)]
		err := checkNetwork(&nwc.Info{Network: got}, "invoice", want)
		if (err == nil) != (got == want) {
			t.Fatalf("%q against %s: %v", got, want, err)
		}
		if err != nil && len(got) >= 20 && !slices.Contains(testNetworks, got) && strings.Contains(err.Error(), got) {
			t.Fatalf("%q echoed: %v", got, err)
		}
	}
}

// The invoice and the payout connection are each refused unless their get_info states the configured network; a
// refused start records no network and starts nothing; the configured one starts.
func TestStartChecksTheHubNetwork(t *testing.T) {
	check := func(t *testing.T, reason string, got string, text string) {
		t.Helper()
		cfg := payoutFiles(t, true, nil)
		fw, pw := newFakeWallet(), newPayoutWallet()
		if reason == "invoice_connection" {
			fw.network = got
		} else {
			pw.network = got
		}
		err, logs := startRefuses(t, cfg, reason, text, WithWallet(fw), WithPayoutWallet(pw))
		if len(got) >= 20 && !slices.Contains(testNetworks, got) && (strings.Contains(err.Error(), got) || strings.Contains(logs, got)) {
			t.Fatalf("%q echoed: %v / %s", got, err, logs)
		}
		if n := recordedNetwork(t, cfg.DataDir); n != "" {
			t.Fatalf("a refused start recorded %q", n)
		}
	}
	for _, c := range []struct{ reason, conn string }{{"invoice_connection", "invoice"}, {"payout_connection", "payout"}} {
		t.Run(c.conn, func(t *testing.T) {
			check(t, c.reason, "", "does not state the network; pair it with satbooth-pair -connection "+c.conn)
			check(t, c.reason, "mainnet", `hub is on the network "mainnet", not nwc.network "regtest"`)
			check(t, c.reason, "bitcoin", "reports a network SatBooth does not know")
			check(t, c.reason, "main\nnet", "reports a network SatBooth does not know")
			check(t, c.reason, "SECRETISH-"+strings.Repeat("Q", 64), "reports a network SatBooth does not know")
		})
	}
	t.Run("the configured network starts", func(t *testing.T) {
		cfg := payoutFiles(t, true, nil)
		if err, logs, _ := runAgain(t, cfg, WithPayoutWallet(newPayoutWallet())); err != nil {
			t.Fatalf("%v\n%s", err, logs)
		}
		if n := recordedNetwork(t, cfg.DataDir); n != "regtest" {
			t.Fatalf("recorded %q", n)
		}
	})
	t.Run("a bad method wins over a wrong network", func(t *testing.T) {
		fw := newFakeWallet()
		fw.network, fw.methods = "mainnet", []string{"make_invoice", "lookup_invoice", "pay_invoice"}
		startRefuses(t, payoutFiles(t, false, nil), "invoice_connection", `holds the method "pay_invoice"`, WithWallet(fw))
	})
	t.Run("a bad payout method wins over a wrong network", func(t *testing.T) {
		pw := newPayoutWallet()
		pw.network, pw.methods = "mainnet", append(slices.Clone(goodPayoutMethods), "make_invoice")
		startRefuses(t, payoutFiles(t, true, nil), "payout_connection", `holds the method "make_invoice"`,
			WithWallet(newFakeWallet()), WithPayoutWallet(pw))
	})
	t.Run("the invoice wallet's network is the configured one under every network", func(t *testing.T) {
		for _, n := range testNetworks {
			cfg := withNetwork(payoutFiles(t, true, nil), n)
			if err, logs, _ := runAgain(t, cfg, WithPayoutWallet(payoutOn(n))); err != nil {
				t.Fatalf("%s: %v\n%s", n, err, logs)
			}
			if got := recordedNetwork(t, cfg.DataDir); got != n {
				t.Fatalf("%s: recorded %q", n, got)
			}
		}
	})
}

func payoutOn(n string) payoutWallet {
	w := newPayoutWallet()
	w.network = n
	return w
}
