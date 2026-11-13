package onchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/store"
)

// fakeHub is a minimal lnpay.Wallet: an invoice stays pending until settle. looked receives one value per
// LookupInvoice answer.
type fakeHub struct {
	mu     sync.Mutex
	seq    int
	invs   map[string]*nwc.Transaction
	ch     chan nwc.Notification
	looked chan struct{}
}

func (w *fakeHub) Notifications() <-chan nwc.Notification { return w.ch }

func (w *fakeHub) MakeInvoice(_ context.Context, p nwc.MakeInvoiceParams) (*nwc.Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("invoice %d", w.seq)))
	h := hex.EncodeToString(sum[:])
	inv := refundtest.InvoiceLong(approval.Regtest, w.seq, strconv.FormatInt(p.AmountMsat*10, 10)+"p", 290)
	tx := &nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: inv,
		Description: p.Description, PaymentHash: h, AmountMsat: p.AmountMsat}
	w.invs[h] = tx
	c := *tx
	return &c, nil
}

func (w *fakeHub) LookupInvoice(_ context.Context, h string) (*nwc.Transaction, error) {
	w.mu.Lock()
	tx, ok := w.invs[h]
	var c nwc.Transaction
	if ok {
		c = *tx
	}
	w.mu.Unlock()
	select {
	case w.looked <- struct{}{}:
	default:
	}
	if !ok {
		return nil, &nwc.WalletError{Code: "NOT_FOUND", Message: "not found"}
	}
	return &c, nil
}

// settle marks the invoice paid at the hub.
func (w *fakeHub) settle(hash string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.invs[hash].State = nwc.StateSettled
}

// newWatchEnvRealLN is the watch env with a real lnpay as its Lightning, over the env's store and ledger, sharing the
// settle lock with the rail as the service wires it.
func newWatchEnvRealLN(t *testing.T) (*env, *lnpay.Service, *fakeHub) {
	t.Helper()
	e := newEnv(t)
	hub := &fakeHub{invs: map[string]*nwc.Transaction{}, ch: make(chan nwc.Notification, 8), looked: make(chan struct{}, 64)}
	lim, err := limits.New(limits.Config{RatePerSec: 1e6, Burst: 1 << 20, MaxMsat: 1_000_000_000, DailyMsat: 1 << 50}, e.clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := lnpay.New(lnpay.Config{PollRate: 1000, Network: approval.Regtest}, e.st, e.lg, hub, lim, e.clk.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	mu := &sync.Mutex{}
	ln.SetSettleLock(mu)
	e.lnReal = ln
	e.wire = func(s *Service) { s.SetPeer(nil, mu) }
	e.mod = func(c *Config) { c.PollRate, c.Tiers = MaxPollRate, nil }
	e.svc = e.newService()
	e.ready()
	return e, ln, hub
}

// mustInvoice makes the order's Lightning invoice.
func mustInvoice(t *testing.T, ln *lnpay.Service, o store.Order) store.Invoice {
	t.Helper()
	inv, err := ln.Pay(bg, o.Token)
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	return inv
}

// lnTransfer is the ledger transfer of the invoice's payment.
func lnTransfer(e *env, inv store.Invoice) ledger.Transfer {
	e.t.Helper()
	tr, _ := e.lookup(lnpay.PaidTransferID(inv.PaymentHash))
	return tr
}

// lnTransferExists reports whether the invoice's payment is in the ledger.
func lnTransferExists(e *env, inv store.Invoice) bool {
	e.t.Helper()
	_, ok := e.lookup(lnpay.PaidTransferID(inv.PaymentHash))
	return ok
}
