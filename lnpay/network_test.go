package lnpay

import (
	"errors"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund/refundtest"
)

var allNetworks = []approval.Network{approval.Mainnet, approval.Testnet, approval.Signet, approval.Regtest}

// Every invoice the hub makes is checked against the configured network and the order's amount: a refused invoice is
// not stored and not shown, the slot is free again, the caller gets ErrUnknown, one ALERT line is logged and it
// holds no part of the invoice.
func TestMakeInvoiceNetworkAndAmountCheck(t *testing.T) {
	const amount = 1_000_000
	wrongInvoices := map[string]func(net approval.Network, msat int64) string{}
	for _, other := range allNetworks {
		wrongInvoices["network "+string(rune('0'+int(other)))] = func(net approval.Network, msat int64) string {
			if other == net {
				return "skip"
			}
			return refundtest.InvoiceFor(other, 7, msat)
		}
	}
	wrongInvoices["another amount"] = func(net approval.Network, msat int64) string { return refundtest.InvoiceFor(net, 7, msat+1000) }
	wrongInvoices["no amount"] = func(net approval.Network, msat int64) string { return refundtest.Invoice(net, 7) }
	wrongInvoices["not an invoice"] = func(approval.Network, int64) string { return "has space" }
	wrongInvoices["empty"] = func(approval.Network, int64) string { return "" }
	wrongInvoices["uppercase"] = func(net approval.Network, msat int64) string {
		return strings.ToUpper(refundtest.InvoiceFor(net, 7, msat))
	}
	wrongInvoices["bad checksum"] = func(net approval.Network, msat int64) string {
		s := refundtest.InvoiceFor(net, 7, msat)
		last := byte('q')
		if s[len(s)-1] == 'q' {
			last = 'p'
		}
		return s[:len(s)-1] + string(last)
	}
	for _, net := range allNetworks {
		for name, mk := range wrongInvoices {
			text := mk(net, amount)
			if text == "skip" {
				continue
			}
			t.Run(string(rune('0'+int(net)))+" "+name, func(t *testing.T) {
				e := newEnv(t, func(e *env) { e.cfg.Network = net })
				o := e.order("a", amount)
				e.svc.w = &mutatingWallet{fakeWallet: e.w, mut: func(tx *nwc.Transaction) { tx.Invoice = text }}
				if _, err := e.pay(o); !errors.Is(err, ErrUnknown) {
					t.Fatalf("got %v, want ErrUnknown", err)
				}
				if n := strings.Count(e.log.String(), logMakeWrongInvoice); n != 1 {
					t.Fatalf("the ALERT line was logged %d times: %s", n, e.log.String())
				}
				logged := e.log.String()
				if text != "" && strings.Contains(logged, text) {
					t.Fatal("the log carries the invoice")
				}
				for i := 0; i+20 <= len(text); i++ {
					if strings.Contains(logged, text[i:i+20]) {
						t.Fatalf("the log carries a 20-character window of the invoice (at %d)", i)
					}
				}
				if len(e.open_(o)) != 0 {
					t.Fatal("an invoice was stored")
				}
				if e.svc.total != 0 {
					t.Fatalf("the slot was not given back: %d", e.svc.total)
				}
				if s := e.status(o); s.Invoice != nil {
					t.Fatal("an invoice is shown")
				}
				// The wallet answers well next: the same order gets its invoice.
				good := refundtest.InvoiceFor(net, 9, amount)
				e.svc.w = &mutatingWallet{fakeWallet: e.w, mut: func(tx *nwc.Transaction) { tx.Invoice = good }}
				if _, err := e.pay(o); err != nil {
					t.Fatalf("the following press: %v", err)
				}
			})
		}
	}
}

// The right network and amount are accepted on each network, and the stored text is the hub's, unchanged.
func TestMakeInvoiceAcceptsConfiguredNetwork(t *testing.T) {
	for _, net := range allNetworks {
		for _, msat := range []int64{1000, 21_000_000, 10_000_000_000} { // 1 sat, 21,000 sat, 0.1 BTC
			e := newEnv(t, func(e *env) { e.cfg.Network = net; e.limCfg.MaxMsat = 20_000_000_000 })
			o := e.order("a", msat)
			want := refundtest.InvoiceFor(net, 3, msat)
			e.svc.w = &mutatingWallet{fakeWallet: e.w, mut: func(tx *nwc.Transaction) { tx.Invoice = want }}
			inv, err := e.pay(o)
			if err != nil || inv.Bolt11 != want {
				t.Fatalf("network %d, %d msat: %v", net, msat, err)
			}
			if strings.Contains(e.log.String(), "ALERT") {
				t.Fatalf("an alert for a good invoice: %s", e.log.String())
			}
		}
	}
	// The human-readable amount in the forms a hub uses: 1 sat is 10n, 21,000 sat 210u, 0.1 BTC 100m.
	e := newEnv(t, func(e *env) { e.limCfg.MaxMsat = 20_000_000_000 })
	for amt, msat := range map[string]int64{"10n": 1000, "210u": 21_000_000, "100m": 10_000_000_000} {
		o := e.order("a", msat)
		text := refundtest.InvoiceWithAmount(approval.Regtest, len(amt)+int(msat%97), amt)
		e.svc.w = &mutatingWallet{fakeWallet: e.w, mut: func(tx *nwc.Transaction) { tx.Invoice = text }}
		if inv, err := e.pay(o); err != nil || inv.Bolt11 != text {
			t.Fatalf("amount %s: %v", amt, err)
		}
	}
}
