package refund_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	randv2 "math/rand/v2"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/refund"
	"github.com/satbooth/satbooth/refund/refundtest"
	"github.com/satbooth/satbooth/store"
)

const amt = 1_000_000 // the refund amount most tests use

func hexID(n int) string { b := id16(n); return hex.EncodeToString(b[:]) }

func inv(n int) string { return refundtest.InvoiceFor(net, n, amt) }

// tx is a lookup answer of the refund n's own invoice and amount, outgoing, in state.
func tx(n int, state string) nwc.Transaction {
	return nwc.Transaction{Type: nwc.TypeOutgoing, State: state, Invoice: inv(n), AmountMsat: amt}
}

func (e *env) send() { e.t.Helper(); refund.SendRound(e.svc, context.Background()) }

func (e *env) resolve() { e.t.Helper(); refund.ResolveRound(e.svc, context.Background()) }

// quiesce runs sender and resolver rounds until the open refunds stop changing.
func (e *env) quiesce() {
	e.t.Helper()
	prev := ""
	for i := 0; i < 20; i++ {
		e.send()
		e.resolve()
		open, err := e.st.OpenRefunds(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		now := fmt.Sprint(open)
		if now == prev {
			return
		}
		prev = now
	}
	e.t.Fatal("the workers never went quiet")
}

func (e *env) state(n int) store.RefundState {
	e.t.Helper()
	st, found, err := e.st.RefundStateByID(context.Background(), hexID(n))
	if err != nil || !found {
		e.t.Fatalf("refund %d: %v %v", n, found, err)
	}
	return st
}

// want asserts refund n's state ("claimed", "sent", "settled", "failed") and, when failed, its reason.
func (e *env) want(n int, state, reason string) {
	e.t.Helper()
	v, found, err := e.svc.Get(context.Background(), hexID(n))
	if err != nil || !found {
		e.t.Fatalf("refund %d: %v %v", n, found, err)
	}
	if v.State != state || v.Reason != reason {
		e.t.Fatalf("refund %d: %s (%s), want %s (%s)", n, v.State, v.Reason, state, reason)
	}
}

// ledgerOf reports whether refund n's pending transfer exists and whether it is posted or voided.
func (e *env) ledgerOf(n int) (pending, posted, voided bool) {
	e.t.Helper()
	ctx := context.Background()
	look := func(id ledger.U128) bool {
		_, found, err := e.lg.LookupTransfer(ctx, id)
		if err != nil {
			e.t.Fatal(err)
		}
		return found
	}
	id := id16(n)
	return look(refund.RefundTransferID(id)), look(refund.PostTransferID(id)), look(refund.VoidTransferID(id))
}

// wantLedger asserts refund n's ledger: "none" (no pending), "pending", "posted" or "voided".
func (e *env) wantLedger(n int, want string) {
	e.t.Helper()
	p, po, vo := e.ledgerOf(n)
	got := "none"
	switch {
	case po && vo:
		got = "posted and voided"
	case po:
		got = "posted"
	case vo:
		got = "voided"
	case p:
		got = "pending"
	}
	if got != want {
		e.t.Fatalf("refund %d ledger: %s, want %s", n, got, want)
	}
}

func (e *env) claimed(n int, order string) {
	e.t.Helper()
	if _, err := e.claim(n, order, amt); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) voidApproval(n int, expires time.Time) string {
	return approval.SignVoid(approval.Void{Network: net, KeyID: approval.KeyID(e.pub), RefundID: id16(n), ExpiresAt: expires.Unix()}, e.priv)
}

func (e *env) void(n int) (refund.View, error) {
	return e.svc.Void(context.Background(), hexID(n), e.voidApproval(n, e.clk.Now().Add(time.Hour)))
}

func (e *env) later(d time.Duration) { e.clk.Set(e.clk.Now().Add(d)) }

func (e *env) alerts(msg string, n int) int { return e.logs.count(msg, hexID(n)) }

// TestCrashTable: crash right after each step, restart a new service on the same files, run to
// quiescence; at most one pay_invoice per refund, and the table's final state.
func TestCrashTable(t *testing.T) {
	type row struct {
		name, step string
		script     func(e *env)
		afterRun   func(e *env) // what a person does once the workers are quiet
		state      string
		reason     string
		ledger     string
		pays       int
	}
	settles := func(e *env) { e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateSettled))) }
	rows := []row{
		{name: "claim row", step: "", script: settles, state: "settled", ledger: "posted", pays: 1},
		{name: "pre-checks passed", step: refund.StepPreChecks, script: settles, state: "settled", ledger: "posted", pays: 1},
		{name: "ledger pending created", step: refund.StepPending, script: settles, state: "settled", ledger: "posted", pays: 1},
		{name: "marker written, before the request", step: refund.StepMarker, script: settles,
			afterRun: func(e *env) {
				if e.alerts(refund.AlertNotFound, 1) != 1 {
					e.t.Fatal("no NOT_FOUND alert")
				}
				e.want(1, "sent", "")
				if _, err := e.void(1); !errors.Is(err, refund.ErrInFlight) {
					e.t.Fatalf("void before VoidAfter: %v", err)
				}
				e.later(store.RefundVoidWait)
				if v, err := e.void(1); err != nil || v.State != "failed" || v.Reason != "voided" {
					e.t.Fatalf("void: %+v %v", v, err)
				}
			}, state: "failed", reason: "voided", ledger: "voided", pays: 0},
		{name: "request sent", step: refund.StepSent, script: settles, state: "settled", ledger: "posted", pays: 1},
		{name: "ledger post", step: refund.StepPost, script: settles, state: "settled", ledger: "posted", pays: 1},
		{name: "ledger void after hub failed", step: refund.StepVoid,
			script: func(e *env) { e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateFailed))) },
			state:  "failed", reason: "recovered", ledger: "voided", pays: 1},
		{name: "ledger void after relay refused", step: refund.StepVoid,
			script: func(e *env) { e.w.Pay(inv(1), nil, &nwc.RelayRefusedError{Reason: "no"}) },
			state:  "failed", reason: "recovered", ledger: "voided", pays: 1},
		{name: "ledger void after expiry", step: refund.StepVoid,
			script: func(e *env) {
				// A crash after the pending, then the approval expires before the next round: expired_unsent voids it.
				refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepPending })
				e.send()
				e.later(2 * time.Hour)
			},
			state: "failed", reason: "recovered", ledger: "voided", pays: 0},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			o := e.order(10_000_000)
			e.claimed(1, o)
			r.script(e)
			crashed := false
			refund.SetCrash(e.svc, func(s string) bool {
				crashed = crashed || s == r.step
				return crashed
			})
			// The first process runs until it crashes at the step, and not a moment longer.
			for i := 0; r.step != "" && !crashed; i++ {
				if i == 5 {
					t.Fatalf("never reached step %s", r.step)
				}
				if e.send(); !crashed {
					e.resolve()
				}
			}
			e.later(time.Minute)
			restartedAt := e.clk.Now()
			e.restart()
			if r.step == refund.StepPost || r.step == refund.StepVoid {
				// Recovery in New, before any round and with no hub call: the result row from the ledger, stamped now.
				calls := e.w.TotalCalls()
				e.want(1, r.state, r.reason)
				if st := e.state(1); !st.Result.At.Equal(restartedAt) {
					t.Fatalf("resolved at %v, want the restart's now %v", st.Result.At, restartedAt)
				}
				if e.w.TotalCalls() != calls {
					t.Fatal("recovery called the hub")
				}
			}
			e.quiesce()
			if r.afterRun != nil {
				r.afterRun(e)
				e.quiesce()
			}
			if got := e.w.PayCount(inv(1)); got != r.pays {
				t.Fatalf("pay_invoice calls: %d, want %d", got, r.pays)
			}
			e.want(1, r.state, r.reason)
			e.wantLedger(1, r.ledger)
		})
	}
}

// TestExpiredIsAmbiguous: expired proves neither outcome; one ALERT, no transition, until a
// person voids.
func TestExpiredIsAmbiguous(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateExpired)))
	e.send()
	for i := 0; i < 3; i++ {
		e.later(time.Hour)
		e.resolve()
	}
	e.want(1, "sent", "")
	e.wantLedger(1, "pending")
	if n := e.alerts(refund.AlertExpired, 1); n != 1 {
		t.Fatalf("expired alerts: %d, want 1", n)
	}
	if v, err := e.void(1); err != nil || v.State != "failed" || v.Reason != "voided" {
		t.Fatalf("void: %+v %v", v, err)
	}
	e.wantLedger(1, "voided")
	if e.w.PayCount(inv(1)) != 1 {
		t.Fatal(e.w.PayCount(inv(1)))
	}
}

func TestFailedClosesAndFrees(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(amt) // exactly one refund's worth
	e.claimed(1, o)
	e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateFailed)))
	if _, err := e.claim(2, o, amt); !errors.Is(err, refund.ErrExceedsRefundable) {
		t.Fatalf("while open: %v", err)
	}
	e.quiesce()
	e.want(1, "failed", "failed")
	e.wantLedger(1, "voided")
	// The approval is spent: the same one is refused; a new one may take the freed amount.
	spent := e.sign(1, o, amt, e.clk.Now().Add(time.Hour))
	if _, err := e.svc.Claim(ctx, spent, inv(3)); !errors.Is(err, refund.ErrRefundExists) {
		t.Fatalf("spent approval: %v", err)
	}
	if _, err := e.claim(2, o, amt); err != nil {
		t.Fatalf("freed amount: %v", err)
	}
	e.w.AfterPay(inv(2), refundtest.Tx(nwc.Transaction{Type: nwc.TypeOutgoing, State: nwc.StateSettled, Invoice: inv(2), AmountMsat: amt}))
	e.quiesce()
	e.want(2, "settled", "")
	e.wantLedger(2, "posted")
	if e.w.PayCount(inv(1)) != 1 || e.w.PayCount(inv(2)) != 1 {
		t.Fatal("pay counts")
	}
}

func TestKeyRevokedBeforeSend(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	// The operator replaced the key and restarted.
	other := strings.Repeat("3b", 32)
	keys, err := approval.NewKeys([]string{other})
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Keys = keys
	e.restart()
	e.quiesce()
	e.want(1, "failed", "key_revoked")
	e.wantLedger(1, "none")
	if e.w.TotalCalls() != 0 {
		t.Fatalf("hub calls: %d", e.w.TotalCalls())
	}
}

func TestFailureVoidsExistingPending(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepPending })
	e.send()
	e.wantLedger(1, "pending")
	// Now the hub knows the invoice (the claimant paid it elsewhere through the same hub, say).
	e.w.Lookup(inv(1), refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StatePending, Invoice: inv(1)}))
	e.restart()
	e.quiesce()
	e.want(1, "failed", "invoice_known")
	e.wantLedger(1, "voided")
	if e.w.PayCount(inv(1)) != 0 {
		t.Fatal("paid")
	}
}

// sentRefund claims refund 1 and sends it, with lookups after the payment answering answers.
func sentRefund(t *testing.T, answers ...refundtest.Answer) *env {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	if len(answers) > 0 {
		e.w.AfterPay(inv(1), answers...)
	}
	e.send()
	e.want(1, "sent", "")
	return e
}

func TestConcurrentTransitions(t *testing.T) {
	t.Run("sender, resolver and void race", func(t *testing.T) {
		for _, state := range []string{nwc.StateSettled, nwc.StateFailed, nwc.StateExpired} {
			t.Run(state, func(t *testing.T) {
				e := sentRefund(t, refundtest.Tx(tx(1, state)))
				e.later(store.RefundVoidWait)
				var wg sync.WaitGroup
				var mu sync.Mutex
				voided := 0
				start := make(chan struct{})
				for i := 0; i < 8; i++ {
					wg.Add(3)
					go func() { defer wg.Done(); <-start; e.send() }()
					go func() { defer wg.Done(); <-start; e.resolve() }()
					go func() {
						defer wg.Done()
						<-start
						v, err := e.void(1)
						switch {
						case err == nil:
							mu.Lock()
							voided++
							mu.Unlock()
							if v.State == "sent" || v.State == "claimed" {
								t.Errorf("void answered %+v", v)
							}
						case !errors.Is(err, refund.ErrState):
							t.Errorf("void: %v", err)
						}
					}()
				}
				close(start)
				wg.Wait()
				st := e.state(1)
				if !st.Resolved {
					t.Fatal("not resolved")
				}
				switch state {
				case nwc.StateSettled:
					e.want(1, "settled", "")
					e.wantLedger(1, "posted")
				case nwc.StateFailed:
					e.want(1, "failed", "failed")
					e.wantLedger(1, "voided")
				case nwc.StateExpired: // only a void ends it
					e.want(1, "failed", "voided")
					e.wantLedger(1, "voided")
					if voided != 1 {
						t.Fatalf("%d voids succeeded", voided)
					}
				}
				if e.w.PayCount(inv(1)) != 1 {
					t.Fatal(e.w.PayCount(inv(1)))
				}
			})
		}
	})
	// The ledger already holds a resolution when the transition runs.
	resolveInLedger := func(e *env, post bool) {
		id := id16(1)
		x := ledger.Transfer{ID: refund.VoidTransferID(id), PendingID: refund.RefundTransferID(id), Flags: ledger.TransferVoidPendingTransfer}
		if post {
			x = ledger.Transfer{ID: refund.PostTransferID(id), PendingID: refund.RefundTransferID(id), Amount: ledger.U128Max,
				Flags: ledger.TransferPostPendingTransfer}
		}
		if r, err := e.lg.CreateTransfers(context.Background(), []ledger.Transfer{x}); err != nil || r[0] != ledger.TransferCreated {
			t.Fatalf("%v %v", r, err)
		}
	}
	t.Run("already voided when posting", func(t *testing.T) {
		e := sentRefund(t, refundtest.Tx(tx(1, nwc.StateSettled)))
		resolveInLedger(e, false)
		e.resolve()
		e.resolve()
		e.want(1, "sent", "")
		if e.alerts(refund.AlertLedgerOpposite, 1) != 1 {
			t.Fatal("no single alert")
		}
	})
	t.Run("already posted when voiding", func(t *testing.T) {
		e := sentRefund(t, refundtest.Tx(tx(1, nwc.StateFailed)))
		resolveInLedger(e, true)
		e.resolve()
		e.want(1, "sent", "")
		e.later(store.RefundVoidWait)
		if _, err := e.void(1); err == nil {
			t.Fatal("void wrote over a posted pending")
		}
		e.want(1, "sent", "")
		if e.alerts(refund.AlertLedgerOpposite, 1) != 1 {
			t.Fatal("no single alert")
		}
	})
	t.Run("exists", func(t *testing.T) {
		e := sentRefund(t, refundtest.Tx(tx(1, nwc.StateSettled)))
		resolveInLedger(e, true)
		e.resolve()
		e.want(1, "settled", "")
		e.wantLedger(1, "posted")
	})
}

func TestPendingAlertsOnceAfterAnHour(t *testing.T) {
	e := sentRefund(t, refundtest.Tx(tx(1, nwc.StatePending)))
	e.later(refund.PendingAlertAfter - time.Second)
	e.resolve()
	if e.alerts(refund.AlertPending, 1) != 0 {
		t.Fatal("alert before an hour")
	}
	e.later(time.Second)
	e.resolve()
	e.later(time.Hour)
	e.resolve()
	if n := e.alerts(refund.AlertPending, 1); n != 1 {
		t.Fatalf("alerts: %d", n)
	}
	e.want(1, "sent", "")
	e.wantLedger(1, "pending")
}

func TestAnomalyChangesNothing(t *testing.T) {
	for name, a := range map[string]refundtest.Answer{
		"another invoice": refundtest.Tx(nwc.Transaction{Type: nwc.TypeOutgoing, State: nwc.StateSettled, Invoice: inv(2), AmountMsat: amt}),
		"another amount":  refundtest.Tx(nwc.Transaction{Type: nwc.TypeOutgoing, State: nwc.StateSettled, Invoice: inv(1), AmountMsat: amt + 1000}),
		"type incoming":   refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StateSettled, Invoice: inv(1), AmountMsat: amt}),
		"failed incoming": refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StateFailed, Invoice: inv(1), AmountMsat: amt}),
		"state accepted":  refundtest.Tx(tx(1, nwc.StateAccepted)),
		"state unknown":   refundtest.Tx(tx(1, "weird")),
		"empty invoice":   refundtest.Tx(nwc.Transaction{Type: nwc.TypeOutgoing, State: nwc.StateSettled, AmountMsat: amt}),
		"malformed":       refundtest.Err(fmt.Errorf("%w: bad", nwc.ErrMalformed)),
	} {
		t.Run(name, func(t *testing.T) {
			e := sentRefund(t, a)
			e.resolve()
			e.later(time.Hour)
			e.resolve()
			e.want(1, "sent", "")
			e.wantLedger(1, "pending")
			if n := e.alerts(refund.AlertAnomaly, 1); n != 1 {
				t.Fatalf("anomaly alerts: %d", n)
			}
		})
	}
	t.Run("invoice in capitals counts", func(t *testing.T) {
		e := sentRefund(t, refundtest.Tx(nwc.Transaction{Type: nwc.TypeOutgoing, State: nwc.StateSettled, Invoice: strings.ToUpper(inv(1)), AmountMsat: amt}))
		e.resolve()
		e.want(1, "settled", "")
	})
}

func TestRelayRefusedFails(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	e.w.Pay(inv(1), nil, fmt.Errorf("wrapped: %w", &nwc.RelayRefusedError{Reason: "rate limited"}))
	e.quiesce()
	e.want(1, "failed", "not_sent")
	e.wantLedger(1, "voided")
	if e.w.PayCount(inv(1)) != 1 {
		t.Fatal(e.w.PayCount(inv(1)))
	}
}

func TestOutcomeUnknownStaysSent(t *testing.T) {
	for name, err := range map[string]error{
		"no response":         nwc.ErrNoResponse,
		"wrapped no response": fmt.Errorf("%w: %w", context.DeadlineExceeded, nwc.ErrNoResponse),
		"closed":              nwc.ErrClosed,
		"canceled":            context.Canceled,
		"deadline":            context.DeadlineExceeded,
		"wallet internal":     &nwc.WalletError{Code: "INTERNAL", Message: "there is already a payment pending for this invoice"},
		"wallet not found":    &nwc.WalletError{Code: "NOT_FOUND", Message: "x"},
		"other":               errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			e.claimed(1, e.order(10_000_000))
			e.w.Pay(inv(1), nil, err)
			e.quiesce()
			e.later(time.Hour)
			e.quiesce()
			e.want(1, "sent", "")
			e.wantLedger(1, "pending")
			if e.w.PayCount(inv(1)) != 1 {
				t.Fatal(e.w.PayCount(inv(1)))
			}
		})
	}
	t.Run("the run's context ends at the call", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		ctx, cancel := context.WithCancel(context.Background())
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepMarker {
				cancel() // the process is shutting down just after the marker
			}
			return false
		})
		refund.SendRound(e.svc, ctx)
		e.want(1, "sent", "")
		if e.w.PayCount(inv(1)) != 1 { // the attempt is counted, though its context had ended
			t.Fatal(e.w.PayCount(inv(1)))
		}
		e.restart()
		e.quiesce()
		e.want(1, "sent", "")
		if e.w.PayCount(inv(1)) != 1 {
			t.Fatal("paid again after the restart")
		}
	})
}

func TestInvoiceKnownFails(t *testing.T) {
	n := 0
	for _, typ := range []string{nwc.TypeIncoming, nwc.TypeOutgoing} {
		for _, state := range []string{nwc.StatePending, nwc.StateSettled, nwc.StateFailed, nwc.StateAccepted, nwc.StateExpired} {
			n++
			t.Run(typ+" "+state, func(t *testing.T) {
				e := newEnv(t, 5_000_000, 50_000_000)
				e.claimed(1, e.order(10_000_000))
				e.w.Lookup(inv(1), refundtest.Tx(nwc.Transaction{Type: typ, State: state, Invoice: inv(1), AmountMsat: amt}))
				e.quiesce()
				e.want(1, "failed", "invoice_known")
				e.wantLedger(1, "none")
				if e.w.PayCount(inv(1)) != 0 {
					t.Fatal("paid")
				}
			})
		}
	}
	t.Run("the invoice in capitals", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.w.Lookup(inv(1), refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StateSettled,
			Invoice: strings.ToUpper(inv(1))}))
		e.send()
		e.want(1, "failed", "invoice_known")
	})
}

// TestLookupOtherInvoice: a pre-send lookup answered with a row of another invoice (or of none) is an anomaly: one
// ALERT, no transition, and the refund is skipped like one whose lookup failed; a later NOT_FOUND sends it.
func TestLookupOtherInvoice(t *testing.T) {
	for name, other := range map[string]string{"another invoice": inv(2), "no invoice": ""} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			o := e.order(10_000_000)
			e.claimed(1, o)
			e.claimed(2, o)
			odd := refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StateSettled, Invoice: other})
			e.w.Lookup(inv(1), odd, odd, refundtest.NotFound())
			e.send() // refund 1: the odd row
			e.want(1, "claimed", "")
			e.wantLedger(1, "none")
			e.send() // refund 1 is skipped: refund 2 is sent
			e.want(1, "claimed", "")
			e.want(2, "sent", "")
			if e.w.LookupCount(inv(1)) != 1 {
				t.Fatalf("refund 1 looked up %d times before refund 2 went", e.w.LookupCount(inv(1)))
			}
			e.send() // only refund 1 is left: the odd row again, no second ALERT
			e.want(1, "claimed", "")
			if e.alerts(refund.AlertLookupOther, 1) != 1 || e.logs.count(refund.LogRetry, hexID(1)) != 1 {
				t.Fatalf("alerts %d, retry lines %d", e.alerts(refund.AlertLookupOther, 1),
					e.logs.count(refund.LogRetry, hexID(1)))
			}
			e.send() // NOT_FOUND: sent as usual
			e.want(1, "sent", "")
			if e.w.PayCount(inv(1)) != 1 {
				t.Fatalf("%d pay calls", e.w.PayCount(inv(1)))
			}
		})
	}
}

func TestBudget(t *testing.T) {
	const a = 2_000_000 // headroom: a + max(a/100, 10,000) = 2,020,000
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(10_000_000)
	if _, err := e.claim(1, o, a); err != nil {
		t.Fatal(err)
	}
	e.w.Budget(&nwc.Budget{Set: true, UsedMsat: 7_000, TotalMsat: 7_000 + 2_020_000}, nil)
	e.send()
	e.want(1, "sent", "")
	if _, err := e.claim(2, o, a); err != nil {
		t.Fatal(err)
	}
	e.w.Budget(&nwc.Budget{Set: true, UsedMsat: 7_000, TotalMsat: 7_000 + 2_020_000 - 1}, nil)
	e.send()
	e.send()
	e.want(2, "claimed", "")
	if e.alerts(refund.AlertBudgetShort, 2) != 1 {
		t.Fatal("no single budget alert")
	}
	e.wantLedger(2, "none")
	e.later(time.Hour) // the approval expires
	e.send()
	e.want(2, "failed", "expired_unsent")
	if e.w.PayCount(inv(2)) != 0 {
		t.Fatal("paid")
	}

	t.Run("small amount: the 10,000 msat floor", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(10_000_000)
		if _, err := e.claim(1, o, 5_000); err != nil {
			t.Fatal(err)
		}
		e.w.Budget(&nwc.Budget{Set: true, TotalMsat: 14_999}, nil)
		e.send()
		e.want(1, "claimed", "")
		e.w.Budget(&nwc.Budget{Set: true, TotalMsat: 15_000}, nil)
		e.send()
		e.want(1, "sent", "")
	})
	t.Run("no budget set", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.w.Budget(&nwc.Budget{}, nil)
		e.send()
		e.want(1, "claimed", "")
		if e.alerts(refund.AlertBudgetUnset, 1) != 1 {
			t.Fatal("no alert")
		}
	})
	t.Run("budget alert threshold", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.cfg.BudgetAlertMsat = 500_000
		e.newService()
		e.claimed(1, e.order(10_000_000))
		e.w.Budget(&nwc.Budget{Set: true, TotalMsat: amt + 499_999}, nil) // headroom is there; what is left after is below
		e.send()
		e.want(1, "sent", "")
		if e.alerts(refund.AlertBudgetLow, 1) != 1 {
			t.Fatal("no low-budget alert")
		}
	})
}

func TestHubErrorKeepsClaimed(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	boom := errors.New("boom")
	e.w.FailOnce(refundtest.CallLookup, boom)
	e.send()
	e.want(1, "claimed", "")
	e.w.FailOnce(refundtest.CallLookup, &nwc.WalletError{Code: "INTERNAL", Message: "x"})
	e.send()
	e.want(1, "claimed", "")
	e.w.FailOnce(refundtest.CallLookup, nwc.ErrNoResponse)
	e.send()
	e.want(1, "claimed", "")
	e.w.FailOnce(refundtest.CallBudget, boom)
	e.send()
	e.want(1, "claimed", "")
	e.wantLedger(1, "none")
	if e.w.PayCount(inv(1)) != 0 {
		t.Fatal("paid")
	}
	e.send()
	e.want(1, "sent", "")
}

func TestSerialSender(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(10_000_000)
	const n = 6
	for i := 1; i <= n; i++ {
		e.claimed(i, o)
	}
	e.w.BeforePay(func(context.Context, string) { time.Sleep(5 * time.Millisecond) })
	refund.SetTiming(e.svc, time.Millisecond, time.Hour, 0)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- e.svc.Run(ctx) }()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.send() }()
	}
	wg.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		open, err := e.st.OpenRefunds(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sent := 0
		for _, st := range open {
			if st.Sent {
				sent++
			}
		}
		if sent == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d sent", sent)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if m := e.w.MaxPaysInFlight(); m != 1 {
		t.Fatalf("%d pay_invoice calls in flight at once", m)
	}
	for i := 1; i <= n; i++ {
		if e.w.PayCount(inv(i)) != 1 {
			t.Fatalf("refund %d: %d pays", i, e.w.PayCount(inv(i)))
		}
	}
}

func TestRunWakesOnClaim(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(10_000_000)
	e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
	refund.SetTiming(e.svc, time.Hour, 10*time.Millisecond, 0) // only a claim can wake the sender in time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- e.svc.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // the sender's first round has run and is waiting
	e.claimed(1, o)
	deadline := time.Now().Add(10 * time.Second)
	for e.state(1).Result.Outcome != store.RefundOutcomeSettled {
		if time.Now().After(deadline) {
			t.Fatal("never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// lookupWatch wraps the wallet: it records each lookup's deadline and can cancel the caller's context right after
// a lookup answers.
type lookupWatch struct {
	*refundtest.Wallet
	mu       sync.Mutex
	deadline time.Duration
	cancel   context.CancelFunc
	during   func() // runs once, before the next lookup is answered
}

func (l *lookupWatch) LookupInvoiceByString(ctx context.Context, invoice string) (*nwc.Transaction, error) {
	l.mu.Lock()
	during := l.during
	l.during = nil
	l.mu.Unlock()
	if during != nil {
		during()
	}
	tx, err := l.Wallet.LookupInvoiceByString(ctx, invoice)
	l.mu.Lock()
	defer l.mu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		l.deadline = time.Until(d)
	}
	if l.cancel != nil {
		l.cancel()
	}
	return tx, err
}

// TestVoidRules pins the rules of the void.
func TestVoidRules(t *testing.T) {
	ctx := context.Background()
	t.Run("refusals", func(t *testing.T) {
		e := sentRefund(t)
		now := e.clk.Now()
		otherPub, otherKey, _ := ed25519Key(3)
		cases := []struct {
			name, id, ap string
			want         error
		}{
			{"garbage", hexID(1), "zz", refund.ErrApproval},
			{"refund approval", hexID(1), e.sign(1, e.state(1).OrderID, amt, now.Add(time.Hour)), refund.ErrApproval},
			{"uppercase", hexID(1), strings.ToUpper(e.voidApproval(1, now.Add(time.Hour))), refund.ErrApproval},
			{"expired", hexID(1), e.voidApproval(1, now), refund.ErrApprovalExpired},
			{"over 30 days", hexID(1), e.voidApproval(1, now.Add(31*24*time.Hour)), refund.ErrApproval},
			{"another refund's", hexID(1), e.voidApproval(2, now.Add(time.Hour)), refund.ErrApproval},
			{"unknown key", hexID(1), approval.SignVoid(approval.Void{Network: net, KeyID: approval.KeyID(otherPub), RefundID: id16(1),
				ExpiresAt: now.Add(time.Hour).Unix()}, otherKey), refund.ErrApproval},
			{"other network", hexID(1), approval.SignVoid(approval.Void{Network: approval.Mainnet, KeyID: approval.KeyID(e.pub), RefundID: id16(1),
				ExpiresAt: now.Add(time.Hour).Unix()}, e.priv), refund.ErrApproval},
			{"unknown refund", hexID(99), e.voidApproval(99, now.Add(time.Hour)), refund.ErrNotFound},
			{"in flight", hexID(1), e.voidApproval(1, now.Add(time.Hour)), refund.ErrInFlight},
		}
		calls := e.w.LookupCount(inv(1))
		for _, c := range cases {
			if _, err := e.svc.Void(ctx, c.id, c.ap); !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", c.name, err, c.want)
			}
		}
		if e.w.LookupCount(inv(1)) != calls {
			t.Fatal("a refused void looked up the hub")
		}
		e.later(store.RefundVoidWait - time.Second)
		if _, err := e.void(1); !errors.Is(err, refund.ErrInFlight) {
			t.Fatalf("one second early: %v", err)
		}
		e.want(1, "sent", "")
	})
	answers := []struct {
		name          string
		a             refundtest.Answer
		err           error
		state, reason string
		ledger        string
	}{
		{"not found", refundtest.NotFound(), nil, "failed", "voided", "voided"},
		{"expired", refundtest.Tx(tx(1, nwc.StateExpired)), nil, "failed", "voided", "voided"},
		{"failed", refundtest.Tx(tx(1, nwc.StateFailed)), nil, "failed", "failed", "voided"},
		{"settled", refundtest.Tx(tx(1, nwc.StateSettled)), nil, "settled", "", "posted"},
		{"pending", refundtest.Tx(tx(1, nwc.StatePending)), refund.ErrInFlight, "sent", "", "pending"},
		{"error", refundtest.Err(errors.New("boom")), refund.ErrHubUnavailable, "sent", "", "pending"},
		{"no response", refundtest.Err(nwc.ErrNoResponse), refund.ErrHubUnavailable, "sent", "", "pending"},
		{"anomaly", refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StateSettled, Invoice: inv(1), AmountMsat: amt}),
			refund.ErrHubUnavailable, "sent", "", "pending"},
	}
	for _, c := range answers {
		t.Run("lookup "+c.name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			watch := &lookupWatch{Wallet: e.w}
			e.wallet = watch
			e.newService()
			e.claimed(1, e.order(10_000_000))
			e.w.AfterPay(inv(1), c.a)
			e.send()
			e.later(store.RefundVoidWait)
			// The caller hangs up as soon as the lookup answers: the writes still happen.
			cctx, cancel := context.WithCancel(ctx)
			watch.mu.Lock()
			watch.cancel = cancel
			watch.mu.Unlock()
			v, err := e.svc.Void(cctx, hexID(1), e.voidApproval(1, e.clk.Now().Add(time.Hour)))
			if !errors.Is(err, c.err) {
				t.Fatalf("void: %v, want %v", err, c.err)
			}
			if err == nil && (v.State != c.state || v.Reason != c.reason || !v.ResolvedAt.Equal(e.clk.Now())) {
				t.Fatalf("view %+v", v)
			}
			e.want(1, c.state, c.reason)
			e.wantLedger(1, c.ledger)
			if watch.deadline <= 0 || watch.deadline > refund.VoidLookupTimeout {
				t.Fatalf("lookup deadline %v", watch.deadline)
			}
			if c.name == "anomaly" && e.alerts(refund.AlertAnomaly, 1) != 1 {
				t.Fatal("no anomaly alert")
			}
			if err == nil {
				if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
					t.Fatalf("second void: %v", err)
				}
			}
		})
	}
}

// TestNoSendAfterMarker: a marker with no result never leads to a pay call, whatever the lookup says.
func TestNoSendAfterMarker(t *testing.T) {
	for name, a := range map[string]refundtest.Answer{
		"not found": refundtest.NotFound(),
		"settled":   refundtest.Tx(tx(1, nwc.StateSettled)),
		"failed":    refundtest.Tx(tx(1, nwc.StateFailed)),
		"pending":   refundtest.Tx(tx(1, nwc.StatePending)),
		"expired":   refundtest.Tx(tx(1, nwc.StateExpired)),
		"anomaly":   refundtest.Tx(nwc.Transaction{Type: nwc.TypeIncoming, State: nwc.StateSettled, Invoice: inv(1), AmountMsat: amt}),
		"error":     refundtest.Err(errors.New("boom")),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			e.claimed(1, e.order(10_000_000))
			refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepMarker })
			e.send()
			e.w.Lookup(inv(1), a)
			for i := 0; i < 3; i++ {
				e.restart()
				e.quiesce()
				e.later(time.Hour)
			}
			if e.w.PayCount(inv(1)) != 0 {
				t.Fatalf("%d pay calls after the marker", e.w.PayCount(inv(1)))
			}
		})
	}
}

// TestRefusedResultRetriedSame: a result the store refuses (the clock stepped back) raises an ALERT and is written
// later, the same result, even if the hub's answer changes meanwhile.
func TestRefusedResultRetriedSame(t *testing.T) {
	e := sentRefund(t, refundtest.Tx(tx(1, nwc.StateFailed)))
	sentAt := e.state(1).SentAt
	e.clk.Set(sentAt.Add(-time.Minute)) // the clock steps back before the send
	e.resolve()
	e.want(1, "sent", "")
	e.wantLedger(1, "voided")
	if e.alerts(refund.AlertResultRefused, 1) != 1 {
		t.Fatal("no alert")
	}
	e.resolve() // still refused; still one alert
	if e.alerts(refund.AlertResultRefused, 1) != 1 {
		t.Fatal("alert repeated")
	}
	// The hub now says settled (its restart exception): the decided result stands.
	e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
	e.clk.Set(sentAt.Add(time.Minute))
	e.later(store.RefundVoidWait)
	if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
		t.Fatalf("void over a waiting result: %v", err)
	}
	e.resolve()
	e.want(1, "failed", "failed")
	e.wantLedger(1, "voided")
	if e.state(1).Result.At.Before(sentAt) {
		t.Fatal("result before the send")
	}
}

// TestRecoveryAfterRefusedResult: a refused result is lost with the process; recovery reads the ledger again.
func TestRecoveryAfterRefusedResult(t *testing.T) {
	e := sentRefund(t, refundtest.Tx(tx(1, nwc.StateSettled)))
	sentAt := e.state(1).SentAt
	e.clk.Set(sentAt.Add(-time.Minute))
	e.resolve()
	e.wantLedger(1, "posted")
	e.restart() // still behind: recovery's write is refused too and waits
	e.want(1, "sent", "")
	e.clk.Set(sentAt.Add(time.Minute))
	e.send()
	e.want(1, "settled", "")
	if !e.state(1).Result.At.Equal(sentAt.Add(time.Minute)) {
		t.Fatal("not stamped now")
	}
}

func ed25519Key(seed int64) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.New(rand.NewSource(seed)))
}

// TestTransitionRereads: a transition re-reads the refund under the mutex after its hub call and does nothing if
// another transition changed it meanwhile. The crash hook serves as the point where the other transition cuts in.
func TestTransitionRereads(t *testing.T) {
	ctx := context.Background()
	t.Run("marker written during the pre-checks", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepPreChecks {
				if err := e.st.AddRefundSend(ctx, hexID(1), e.clk.Now()); err != nil {
					t.Error(err)
				}
			}
			return false
		})
		e.send()
		e.wantLedger(1, "none") // no pending for a refund that was no longer claimed
		if e.w.PayCount(inv(1)) != 0 || e.alerts(refund.AlertSendRefused, 1) != 0 {
			t.Fatal("acted on a stale state")
		}
	})
	t.Run("settled while the pay call ran", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.w.Pay(inv(1), nil, &nwc.RelayRefusedError{Reason: "no"})
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepSent { // the resolver settled it first (posted, then the result)
				id := id16(1)
				if r, err := e.lg.CreateTransfers(ctx, []ledger.Transfer{{ID: refund.PostTransferID(id), PendingID: refund.RefundTransferID(id),
					Amount: ledger.U128Max, Flags: ledger.TransferPostPendingTransfer}}); err != nil || r[0] != ledger.TransferCreated {
					t.Error(r, err)
				}
				if err := e.st.AddRefundResult(ctx, store.RefundResult{RefundID: hexID(1), Outcome: "settled", Reason: "settled", At: e.clk.Now()}); err != nil {
					t.Error(err)
				}
			}
			return false
		})
		e.send()
		e.want(1, "settled", "")
		e.wantLedger(1, "posted")
		if e.alerts(refund.AlertLedgerOpposite, 1) != 0 {
			t.Fatal("tried to void a refund that was already settled")
		}
	})
	t.Run("a waiting result is never replaced", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		watch := &lookupWatch{Wallet: e.w}
		e.wallet = watch
		e.newService()
		e.claimed(1, e.order(10_000_000))
		e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateFailed)), refundtest.NotFound())
		e.send()
		sentAt := e.state(1).SentAt
		e.clk.Set(sentAt.Add(store.RefundVoidWait))
		watch.mu.Lock()
		watch.during = func() { // during the void's lookup: the resolver decides failed, and the store refuses it
			e.clk.Set(sentAt.Add(-time.Minute))
			e.resolve()
			e.clk.Set(sentAt.Add(store.RefundVoidWait))
		}
		watch.mu.Unlock()
		if _, err := e.void(1); !errors.Is(err, refund.ErrState) { // the void's lookup read NOT_FOUND: voided
			t.Fatalf("void: %v", err)
		}
		e.want(1, "sent", "")
		e.resolve()
		e.want(1, "failed", "failed")
	})
}

// TestLedgerResolvedBeforeSendNeverPays: a claimed refund whose pending the ledger already shows resolved (and no
// result decided in this process) is never sent: ALERT, nothing changes.
func TestLedgerResolvedBeforeSendNeverPays(t *testing.T) {
	ctx := context.Background()
	for _, post := range []bool{false, true} {
		t.Run(fmt.Sprint("posted ", post), func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			o := e.order(10_000_000)
			e.claimed(1, o)
			id := id16(1)
			ud, _ := ledger.U128FromBytes(func() []byte { b := orderBytes(t, o); return b[:] }())
			res := ledger.Transfer{ID: refund.VoidTransferID(id), PendingID: refund.RefundTransferID(id), Flags: ledger.TransferVoidPendingTransfer}
			if post {
				res = ledger.Transfer{ID: refund.PostTransferID(id), PendingID: refund.RefundTransferID(id), Amount: ledger.U128Max,
					Flags: ledger.TransferPostPendingTransfer}
			}
			r, err := e.lg.CreateTransfers(ctx, []ledger.Transfer{{ID: refund.RefundTransferID(id), DebitAccountID: lnpay.MerchantLightning,
				CreditAccountID: lnpay.LightningHub, Amount: ledger.U128From64(amt), UserData128: ud, Ledger: 1, Code: 5,
				Flags: ledger.TransferPending}, res})
			if err != nil || r[0] != ledger.TransferCreated || r[1] != ledger.TransferCreated {
				t.Fatal(r, err)
			}
			e.send()
			e.send()
			e.want(1, "claimed", "")
			if e.w.PayCount(inv(1)) != 0 || e.alerts(refund.AlertLedgerResolved, 1) != 1 {
				t.Fatal("sent, or not one alert")
			}
		})
	}
}

// TestSentWithoutPendingAlerts: a sent refund always has a ledger pending; one without is a state that should not
// exist, so neither a failure nor a settlement resolves it.
func TestSentWithoutPendingAlerts(t *testing.T) {
	for _, state := range []string{nwc.StateFailed, nwc.StateSettled} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			e.claimed(1, e.order(10_000_000))
			if err := e.st.AddRefundSend(context.Background(), hexID(1), e.clk.Now()); err != nil {
				t.Fatal(err)
			}
			e.w.Lookup(inv(1), refundtest.Tx(tx(1, state)))
			e.resolve()
			e.resolve()
			e.want(1, "sent", "")
			e.wantLedger(1, "none")
			if e.alerts(refund.AlertNoPending, 1) != 1 {
				t.Fatal("not one alert")
			}
		})
	}
}

// TestSenderHubCallsBounded: however many refunds are claimed, a sender round makes one get_budget, and a lookup only
// for the one refund about to be sent; with the budget covering none or the hub down, one call a round and no
// repeated log lines. An expired approval still ends its refund on every round, with no hub call for it.
func TestSenderHubCallsBounded(t *testing.T) {
	const n = 8
	setup := func(t *testing.T) (*env, string) {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(int64(n+1) * amt)
		for i := 1; i <= n; i++ {
			e.claimed(i, o)
		}
		return e, o
	}
	lookups := func(e *env) int {
		c := 0
		for i := 1; i <= 2*n; i++ {
			c += e.w.LookupCount(inv(i))
		}
		return c
	}
	t.Run("budget short", func(t *testing.T) {
		e, o := setup(t)
		e.w.Budget(&nwc.Budget{Set: true, TotalMsat: amt}, nil) // below amt + 10,000 msat
		for r := 1; r <= 5; r++ {
			before := e.w.TotalCalls()
			e.send()
			if got := e.w.TotalCalls() - before; got != 1 {
				t.Fatalf("round %d: %d hub calls, want 1 get_budget", r, got)
			}
		}
		if lookups(e) != 0 || len(e.w.Pays()) != 0 {
			t.Fatalf("lookups %d, pays %d", lookups(e), len(e.w.Pays()))
		}
		for i := 1; i <= n; i++ {
			if e.alerts(refund.AlertBudgetShort, i) != 1 {
				t.Fatalf("refund %d: %d budget ALERTs over 5 rounds, want 1", i, e.alerts(refund.AlertBudgetShort, i))
			}
		}
		if e.logs.count(refund.LogRetry, "") != 0 {
			t.Fatal("a short budget is not a retry")
		}
		// A refund whose approval expires sooner fails expired_unsent on the next round, with no hub call for it.
		v, err := e.svc.Claim(context.Background(), e.sign(n+1, o, amt, e.clk.Now().Add(time.Minute)), inv(n+1))
		if err != nil || v.State != "claimed" {
			t.Fatalf("claim: %+v %v", v, err)
		}
		e.later(2 * time.Minute)
		before := e.w.TotalCalls()
		e.send()
		e.want(n+1, "failed", "expired_unsent")
		if e.w.TotalCalls()-before != 1 || e.w.LookupCount(inv(n+1)) != 0 {
			t.Fatalf("the expiry round made %d hub calls", e.w.TotalCalls()-before)
		}
		// Every claimed refund's approval expires: each round ends them on the store alone, and no hub call is made.
		e.later(2 * time.Hour)
		before = e.w.TotalCalls()
		e.send()
		for i := 1; i <= n; i++ {
			e.want(i, "failed", "expired_unsent")
		}
		if e.w.TotalCalls() != before {
			t.Fatal("a round with nothing to send called the hub")
		}
	})
	t.Run("budget room: one refund a round", func(t *testing.T) {
		e, _ := setup(t)
		for r := 1; r <= n; r++ {
			before, lb := e.w.TotalCalls(), lookups(e)
			e.send()
			// get_budget, the lookup of refund r, and its pay_invoice.
			if got := e.w.TotalCalls() - before; got != 3 || lookups(e)-lb != 1 || e.w.LookupCount(inv(r)) != 1 {
				t.Fatalf("round %d: %d hub calls, %d lookups", r, got, lookups(e)-lb)
			}
			e.want(r, "sent", "")
			if r < n {
				e.want(r+1, "claimed", "")
			}
		}
	})
	t.Run("hub down", func(t *testing.T) {
		e, _ := setup(t)
		e.w.Budget(nil, errors.New("boom"))
		for r := 1; r <= 5; r++ {
			before := e.w.TotalCalls()
			e.send()
			if got := e.w.TotalCalls() - before; got != 1 {
				t.Fatalf("round %d: %d hub calls, want 1", r, got)
			}
		}
		if lookups(e) != 0 {
			t.Fatal("looked up while the budget read failed")
		}
		if got := e.logs.count(refund.LogRetry, ""); got != 1 {
			t.Fatalf("%d retry lines over 5 rounds, want 1", got)
		}
		// The budget answers and the lookups fail: the sender passes from one refund to the next, and each failure is
		// logged once for its refund, however many rounds it fails in.
		e.w.Budget(&nwc.Budget{Set: true, TotalMsat: 1 << 50}, nil)
		for r := 0; r < 2*n; r++ {
			before := e.w.TotalCalls()
			e.w.FailOnce(refundtest.CallLookup, errors.New("boom"))
			e.send()
			if got := e.w.TotalCalls() - before; got != 2 {
				t.Fatalf("lookup round %d: %d hub calls, want 2", r, got)
			}
		}
		for i := 1; i <= n; i++ {
			if e.w.LookupCount(inv(i)) != 2 || e.logs.count(refund.LogRetry, hexID(i)) != 1 {
				t.Fatalf("refund %d: %d lookups, %d retry lines", i, e.w.LookupCount(inv(i)),
					e.logs.count(refund.LogRetry, hexID(i)))
			}
		}
		// The hub recovers: refunds are sent one a round, oldest first.
		e.send()
		e.want(1, "sent", "")
		e.want(2, "claimed", "")
	})
}

// TestVoidAfter: the void wait is strictly longer than the longest expiration a pay_invoice request can carry, and the
// store enforces the same wait.
func TestVoidAfter(t *testing.T) {
	if refund.VoidAfter <= nwc.MaxTimeout {
		t.Fatalf("VoidAfter %v is not above nwc.MaxTimeout %v", refund.VoidAfter, nwc.MaxTimeout)
	}
	if refund.VoidAfter != nwc.MaxTimeout+time.Minute || store.RefundVoidWait != refund.VoidAfter {
		t.Fatalf("VoidAfter %v, store.RefundVoidWait %v, want both nwc.MaxTimeout + 1 min", refund.VoidAfter,
			store.RefundVoidWait)
	}
}

// TestRecoveryPostedUnsent: at start-up, a claimed refund whose pending the ledger shows posted (a post only ever
// follows a send) raises its own ALERT and is not settled; nothing is sent.
func TestRecoveryPostedUnsent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(10_000_000)
	e.claimed(1, o)
	id := id16(1)
	ud, _ := ledger.U128FromBytes(func() []byte { b := orderBytes(t, o); return b[:] }())
	r, err := e.lg.CreateTransfers(ctx, []ledger.Transfer{{ID: refund.RefundTransferID(id),
		DebitAccountID: lnpay.MerchantLightning, CreditAccountID: lnpay.LightningHub, Amount: ledger.U128From64(amt),
		UserData128: ud, Ledger: 1, Code: 5, Flags: ledger.TransferPending},
		{ID: refund.PostTransferID(id), PendingID: refund.RefundTransferID(id), Amount: ledger.U128Max,
			Flags: ledger.TransferPostPendingTransfer}})
	if err != nil || r[0] != ledger.TransferCreated || r[1] != ledger.TransferCreated {
		t.Fatal(r, err)
	}
	e.restart()
	if e.alerts(refund.AlertPostedUnsent, 1) != 1 || e.alerts(refund.AlertResultRefused, 1) != 0 {
		t.Fatal("want the posted-unsent ALERT, not the refused-result one")
	}
	e.send()
	e.send()
	e.want(1, "claimed", "")
	if e.w.PayCount(inv(1)) != 0 || e.alerts(refund.AlertResultRefused, 1) != 0 {
		t.Fatal("sent, or a result was queued")
	}
}

// TestNotFoundSilentAtFirst: right after the send marker, NOT_FOUND may only mean the request has not reached the hub
// yet: the resolver raises the ALERT only from sent_at + HubCallTimeout.
func TestNotFoundSilentAtFirst(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	e.send()
	e.want(1, "sent", "")
	e.w.Lookup(inv(1), refundtest.NotFound())
	e.later(refund.HubCallTimeout - time.Second)
	e.resolve()
	if e.alerts(refund.AlertNotFound, 1) != 0 {
		t.Fatal("NOT_FOUND alerted before sent_at + HubCallTimeout")
	}
	e.later(time.Second)
	e.resolve()
	if e.alerts(refund.AlertNotFound, 1) != 1 {
		t.Fatal("no NOT_FOUND alert at sent_at + HubCallTimeout")
	}
	e.want(1, "sent", "")
}

// TestApprovalRecheckAtSend: the send-time re-check of the stored approval. An unknown key ends the refund
// key_revoked (TestKeyRevokedBeforeSend); an expired approval ends it expired_unsent; any other failure (here another
// network, and a clock stepped back past the 30-day window) raises AlertApproval and changes nothing, until the
// approval's expiry ends it expired_unsent. No hub call is made for it.
func TestApprovalRecheckAtSend(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.later(time.Hour)
		e.send()
		e.want(1, "failed", "expired_unsent")
		if e.w.TotalCalls() != 0 {
			t.Fatal("hub called")
		}
	})
	t.Run("other network", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.cfg.Network = approval.Testnet
		e.restart()
		e.send()
		e.send()
		e.want(1, "claimed", "")
		if e.alerts(refund.AlertApproval, 1) != 1 || e.w.TotalCalls() != 0 {
			t.Fatalf("alerts %d, hub calls %d", e.alerts(refund.AlertApproval, 1), e.w.TotalCalls())
		}
		e.later(time.Hour)
		e.send()
		e.want(1, "failed", "expired_unsent")
	})
	t.Run("clock stepped back", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.later(-31 * 24 * time.Hour)
		e.send()
		e.want(1, "claimed", "")
		if e.alerts(refund.AlertApproval, 1) != 1 || e.alerts(refund.AlertResultRefused, 1) != 0 || e.w.TotalCalls() != 0 {
			t.Fatal("want one AlertApproval, no transition and no hub call")
		}
	})
}

// TestSenderSkipsStuckRefund: a refund that cannot proceed does not hold back the others. An oldest refund the budget
// does not cover is skipped while a smaller younger one is sent; a refund whose lookup keeps failing is passed over
// for the next one, and is tried again once the others have had their turn. Still at most two hub calls a round
// before the send.
func TestSenderSkipsStuckRefund(t *testing.T) {
	t.Run("budget covers a younger, smaller refund", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(10_000_000)
		if _, err := e.claim(1, o, 3_000_000); err != nil {
			t.Fatal(err)
		}
		e.claimed(2, o) // 1,000,000 msat
		e.w.Budget(&nwc.Budget{Set: true, TotalMsat: 2_000_000}, nil)
		before := e.w.TotalCalls()
		e.send()
		if got := e.w.TotalCalls() - before; got != 3 { // get_budget, lookup of refund 2, its pay_invoice
			t.Fatalf("%d hub calls", got)
		}
		e.want(1, "claimed", "")
		e.want(2, "sent", "")
		if e.alerts(refund.AlertBudgetShort, 1) != 1 || e.alerts(refund.AlertBudgetShort, 2) != 0 ||
			e.w.LookupCount(inv(1)) != 0 {
			t.Fatal("want the budget ALERT for refund 1 only, and no lookup of it")
		}
		before = e.w.TotalCalls()
		e.send()
		if e.w.TotalCalls()-before != 1 {
			t.Fatal("a round where the budget covers none made more than one call")
		}
	})
	t.Run("a lookup that keeps failing", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(10_000_000)
		for i := 1; i <= 3; i++ {
			e.claimed(i, o)
		}
		boom := &nwc.WalletError{Code: "INTERNAL", Message: "x"}
		e.w.Lookup(inv(1), refundtest.Err(boom))
		round := func() int {
			before := e.w.TotalCalls()
			e.send()
			return e.w.TotalCalls() - before
		}
		// Round 1: refund 1's lookup fails (2 calls). Rounds 2 and 3 send refunds 2 and 3 (3 calls each).
		if c := round(); c != 2 {
			t.Fatalf("round 1: %d calls", c)
		}
		e.want(1, "claimed", "")
		for _, n := range []int{2, 3} {
			if c := round(); c != 3 {
				t.Fatalf("refund %d: %d calls", n, c)
			}
			e.want(n, "sent", "")
		}
		if e.w.LookupCount(inv(1)) != 1 {
			t.Fatal("refund 1 looked up again before the others had their turn")
		}
		// Only refund 1 is left: it is tried again, every round, one lookup each.
		for r := 0; r < 3; r++ {
			if c := round(); c != 2 {
				t.Fatalf("retry round %d: %d calls", r, c)
			}
		}
		if e.w.LookupCount(inv(1)) != 4 || e.logs.count(refund.LogRetry, hexID(1)) != 1 {
			t.Fatalf("lookups %d, retry lines %d", e.w.LookupCount(inv(1)), e.logs.count(refund.LogRetry, hexID(1)))
		}
		// The hub answers (after the failure already scripted): refund 1 is sent.
		e.w.Lookup(inv(1), refundtest.NotFound())
		e.send()
		e.send()
		e.want(1, "sent", "")
	})
	// A refund whose prepare stops with an ALERT (here its ledger pending is already voided, with no marker) is
	// skipped like one whose lookup failed, so the younger ones are sent; it comes round again when only it is left.
	t.Run("a prepare that raises an ALERT", func(t *testing.T) {
		ctx := context.Background()
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(10_000_000)
		for i := 1; i <= 3; i++ {
			e.claimed(i, o)
		}
		id := id16(1)
		ud, _ := ledger.U128FromBytes(func() []byte { b := orderBytes(t, o); return b[:] }())
		r, err := e.lg.CreateTransfers(ctx, []ledger.Transfer{{ID: refund.RefundTransferID(id),
			DebitAccountID: lnpay.MerchantLightning, CreditAccountID: lnpay.LightningHub, Amount: ledger.U128From64(amt),
			UserData128: ud, Ledger: 1, Code: 5, Flags: ledger.TransferPending},
			{ID: refund.VoidTransferID(id), PendingID: refund.RefundTransferID(id), Flags: ledger.TransferVoidPendingTransfer}})
		if err != nil || r[0] != ledger.TransferCreated || r[1] != ledger.TransferCreated {
			t.Fatal(r, err)
		}
		e.send() // refund 1: lookup NOT_FOUND, then prepare raises the ALERT
		e.send() // refund 2 is sent
		e.send() // refund 3 is sent
		e.want(1, "claimed", "")
		e.want(2, "sent", "")
		e.want(3, "sent", "")
		if e.w.LookupCount(inv(1)) != 1 || e.alerts(refund.AlertLedgerResolved, 1) != 1 {
			t.Fatalf("refund 1: %d lookups, %d alerts", e.w.LookupCount(inv(1)), e.alerts(refund.AlertLedgerResolved, 1))
		}
		e.send() // only refund 1 is left: it is tried again (the ALERT is not repeated) and still never paid
		if e.w.LookupCount(inv(1)) != 2 || e.alerts(refund.AlertLedgerResolved, 1) != 1 || e.w.PayCount(inv(1)) != 0 {
			t.Fatalf("refund 1 again: %d lookups, %d alerts, %d pays", e.w.LookupCount(inv(1)),
				e.alerts(refund.AlertLedgerResolved, 1), e.w.PayCount(inv(1)))
		}
	})
	t.Run("a failed refund comes round again after the others", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(10_000_000)
		for i := 1; i <= 3; i++ {
			e.claimed(i, o)
		}
		boom := &nwc.WalletError{Code: "INTERNAL", Message: "x"}
		e.w.Lookup(inv(1), refundtest.Err(boom))
		e.w.Lookup(inv(2), refundtest.Err(boom))
		e.send() // refund 1 fails
		e.send() // refund 2 fails
		e.send() // refund 3 is sent
		e.want(3, "sent", "")
		e.send() // every one left has failed: start again from the oldest
		if e.w.LookupCount(inv(1)) != 2 || e.w.LookupCount(inv(2)) != 1 {
			t.Fatalf("lookups: 1 %d, 2 %d", e.w.LookupCount(inv(1)), e.w.LookupCount(inv(2)))
		}
		e.send()
		if e.w.LookupCount(inv(2)) != 2 {
			t.Fatalf("refund 2 not tried in turn: %d lookups", e.w.LookupCount(inv(2)))
		}
	})
}

// TestPayAmountIsApproved: pay_invoice always carries the approved amount, which equals the invoice's own (in any
// unit).
func TestPayAmountIsApproved(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(10_000_000)
	invs := []string{refundtest.InvoiceWithAmount(net, 80, "10000n"), refundtest.InvoiceWithAmount(net, 81, "10u")}
	for i, in := range invs {
		if _, err := e.svc.Claim(context.Background(), e.sign(80+i, o, amt, e.clk.Now().Add(time.Hour)), in); err != nil {
			t.Fatal(err)
		}
		e.send()
	}
	pays := e.w.Pays()
	if len(pays) != 2 {
		t.Fatalf("%d pays", len(pays))
	}
	for i, p := range pays {
		if p.Invoice != invs[i] || p.AmountMsat != amt {
			t.Fatalf("pay %d: %+v, want %s with %d msat", i, p, invs[i], amt)
		}
	}
}

// TestPayRequestDeadAtVoid: the pay request expires at sent_at + PayTimeout, so a sender that stalls after the marker
// past that expiry sends nothing; and while it is stalled, a void is refused.
func TestPayRequestDeadAtVoid(t *testing.T) {
	t.Run("a void while the sender is stalled", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		var verr error
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepMarker {
				e.later(refund.VoidAfter) // the sender stalls past the void wait (or the clock steps forward)
				_, verr = e.void(1)       // refused: the sender can still publish
				e.want(1, "sent", "")
				e.wantLedger(1, "pending")
			}
			return false // the sender resumes
		})
		e.send()
		if !errors.Is(verr, refund.ErrInFlight) {
			t.Fatalf("void while the sender is stalled: %v", verr)
		}
		if n := e.w.PayCount(inv(1)); n != 0 {
			t.Fatalf("%d pay calls past the request's expiry", n)
		}
		e.want(1, "failed", "not_sent")
		e.wantLedger(1, "voided")
	})
	t.Run("the request would be dead on arrival", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepMarker {
				e.later(payTimeout)
			}
			return false
		})
		e.send()
		if n := e.w.PayCount(inv(1)); n != 0 {
			t.Fatalf("%d pay calls past the request's expiry", n)
		}
		e.want(1, "failed", "not_sent")
		e.wantLedger(1, "voided")
	})
	t.Run("one second before", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepMarker {
				e.later(payTimeout - time.Second)
			}
			return false
		})
		e.send()
		if n := e.w.PayCount(inv(1)); n != 1 {
			t.Fatalf("%d pay calls", n)
		}
		e.want(1, "sent", "")
		// The clock moved between the marker and the call: the expiration still counts from the stored sent_at.
		if got, want := e.w.Pays()[0].ExpiresAt, e.state(1).SentAt.Add(payTimeout); !got.Equal(want) {
			t.Fatalf("expiration %v, want %v", got, want)
		}
	})
	t.Run("the wallet refuses it as expired", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		e.w.Pay(inv(1), nil, nwc.ErrExpiredBeforeSend) // the wallet's clock is ahead of the service's
		e.send()
		e.want(1, "failed", "not_sent")
		e.wantLedger(1, "voided")
	})
}

// TestVoidWhileSenderCanPublish: Void refuses a refund (ErrInFlight, nothing changed) from the send marker until the
// sender's pay_invoice call has returned, whatever the clock reads.
func TestVoidWhileSenderCanPublish(t *testing.T) {
	// The sequence: the sender pauses after the marker, the clock steps past the void wait, a void comes, the clock
	// steps back to 5 s after the marker and the sender resumes.
	t.Run("the clock steps forward past the void wait and back", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		var verr error
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepMarker {
				sentAt := e.state(1).SentAt
				e.clk.Set(sentAt.Add(refund.VoidAfter + time.Second))
				_, verr = e.void(1) // the hub knows nothing of the refund: NOT_FOUND, were the void to look
				e.want(1, "sent", "")
				e.wantLedger(1, "pending")
				e.clk.Set(sentAt.Add(5 * time.Second))
			}
			return false
		})
		e.send()
		if !errors.Is(verr, refund.ErrInFlight) {
			t.Fatalf("void while the sender can publish: %v", verr)
		}
		if n := e.w.PayCount(inv(1)); n != 1 { // allowed: the refund is still sent, its one pay_invoice
			t.Fatalf("%d pay calls", n)
		}
		e.want(1, "sent", "")
		e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateSettled))) // and then it resolves as usual
		e.resolve()
		e.want(1, "settled", "")
		e.wantLedger(1, "posted")
	})
	t.Run("a void while pay_invoice is in progress", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		e.w.BeforePay(func(context.Context, string) { close(entered); <-release })
		go func() { defer close(done); refund.SendRound(e.svc, context.Background()) }()
		<-entered
		e.later(refund.VoidAfter) // so only the mark can refuse it
		if _, err := e.void(1); !errors.Is(err, refund.ErrInFlight) {
			close(release)
			<-done
			t.Fatalf("void during pay_invoice: %v", err)
		}
		e.want(1, "sent", "")
		e.wantLedger(1, "pending")
		close(release)
		<-done
		v, err := e.void(1) // the call has returned: the void looks up (NOT_FOUND) and ends it
		if err != nil || v.State != "failed" || v.Reason != "voided" {
			t.Fatalf("void after pay_invoice returned: %+v, %v", v, err)
		}
		e.wantLedger(1, "voided")
		if n := e.w.PayCount(inv(1)); n != 1 {
			t.Fatalf("%d pay calls", n)
		}
	})
	// A prepare that raises an ALERT after a send marker exists (here the store refuses the marker because one was
	// written behind the service's back) leaves no mark: a later void is not refused on its account.
	t.Run("a prepare that raises an ALERT leaves no mark", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepPending {
				if err := e.st.AddRefundSend(context.Background(), hexID(1), e.clk.Now()); err != nil {
					t.Fatal(err)
				}
			}
			return false
		})
		e.send()
		if e.alerts(refund.AlertSendRefused, 1) != 1 || e.w.PayCount(inv(1)) != 0 {
			t.Fatalf("%d alerts, %d pays", e.alerts(refund.AlertSendRefused, 1), e.w.PayCount(inv(1)))
		}
		e.want(1, "sent", "")
		e.later(refund.VoidAfter)
		v, err := e.void(1)
		if err != nil || v.State != "failed" || v.Reason != "voided" {
			t.Fatalf("void: %+v, %v", v, err)
		}
	})
}

// TestPayRequestExpiration: the request's expiration is exactly the stored sent_at + PayTimeout.
func TestPayRequestExpiration(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.claimed(1, e.order(10_000_000))
	e.later(17 * time.Second)
	e.send()
	pays := e.w.Pays()
	st := e.state(1)
	if len(pays) != 1 || !st.Sent {
		t.Fatalf("%d pays, sent %v", len(pays), st.Sent)
	}
	if want := st.SentAt.Add(payTimeout); !pays[0].ExpiresAt.Equal(want) {
		t.Fatalf("expiration %v, want %v", pays[0].ExpiresAt, want)
	}
	if refund.VoidAfter <= nwc.MaxTimeout {
		t.Fatal("VoidAfter must outlast the longest pay request")
	}
}

// TestAlertApprovalText pins AlertApproval's exact text: operators match on it, and it is written as concatenated
// constants to keep the lines short.
func TestAlertApprovalText(t *testing.T) {
	const want = "ALERT: refund: the stored approval no longer verifies, for a reason other than its key or its expiry " +
		"(has the clock or payout.network changed?); nothing is sent, the refund ends expired_unsent at its expiry"
	if refund.AlertApproval != want {
		t.Fatalf("AlertApproval = %q", refund.AlertApproval)
	}
}

// ---- Plan 11: the widened void (rows V1a to V1e) ----

// alertCount is how many ALERT lines were logged.
func (r *recorder) alertCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.recs {
		if strings.HasPrefix(x.msg, "ALERT") {
			n++
		}
	}
	return n
}

// putLedger writes refund n's ledger transfers for order: each step is "pending", "post" or "void".
func (e *env) putLedger(n int, order string, steps ...string) {
	e.t.Helper()
	id := id16(n)
	ud, _ := ledger.U128FromBytes(func() []byte { b := orderBytes(e.t, order); return b[:] }())
	for _, s := range steps {
		tr := ledger.Transfer{ID: refund.RefundTransferID(id), DebitAccountID: lnpay.MerchantLightning,
			CreditAccountID: lnpay.LightningHub, Amount: ledger.U128From64(amt), UserData128: ud, Ledger: lnpay.LedgerMsat,
			Code: 5, Flags: ledger.TransferPending}
		switch s {
		case "post":
			tr = ledger.Transfer{ID: refund.PostTransferID(id), PendingID: refund.RefundTransferID(id), Amount: ledger.U128Max,
				Ledger: lnpay.LedgerMsat, Code: 5, Flags: ledger.TransferPostPendingTransfer}
		case "void":
			tr = ledger.Transfer{ID: refund.VoidTransferID(id), PendingID: refund.RefundTransferID(id),
				Ledger: lnpay.LedgerMsat, Code: 5, Flags: ledger.TransferVoidPendingTransfer}
		}
		if r, err := e.lg.CreateTransfers(context.Background(), []ledger.Transfer{tr}); err != nil || r[0] != ledger.TransferCreated {
			e.t.Fatalf("ledger step %s: %v %v", s, r, err)
		}
	}
}

// refundIn builds refund n of a fresh order paid 10 000 000 msat, in the store kind k ("claimed", or "sent": the
// marker written, no pay call, the clock then moved to the end of the void wait) and the ledger letter l (N: no
// pending; P: pending; S: pending and post; V: pending and void). It returns the order id.
func (e *env) refundIn(n int, k, l string) string {
	e.t.Helper()
	o := e.order(10_000_000)
	e.claimed(n, o)
	switch l {
	case "N":
	case "P":
		e.putLedger(n, o, "pending")
	case "S":
		e.putLedger(n, o, "pending", "post")
	case "V":
		e.putLedger(n, o, "pending", "void")
	default:
		e.t.Fatal("ledger letter", l)
	}
	if k == "sent" {
		if err := e.st.AddRefundSend(context.Background(), hexID(n), e.clk.Now()); err != nil {
			e.t.Fatal(err)
		}
		e.later(store.RefundVoidWait)
	}
	return o
}

// voidAnswer is the lookup answer of the table's name, for refund 1.
func voidAnswer(name string) refundtest.Answer {
	other := func(f func(*nwc.Transaction)) refundtest.Answer {
		x := tx(1, nwc.StateSettled)
		f(&x)
		return refundtest.Tx(x)
	}
	switch name {
	case "settled":
		return refundtest.Tx(tx(1, nwc.StateSettled))
	case "failed":
		return refundtest.Tx(tx(1, nwc.StateFailed))
	case "pending":
		return refundtest.Tx(tx(1, nwc.StatePending))
	case "no record":
		return refundtest.NotFound()
	case "expired":
		return refundtest.Tx(tx(1, nwc.StateExpired))
	case "other invoice":
		return other(func(x *nwc.Transaction) { x.Invoice = inv(2) })
	case "other amount":
		return other(func(x *nwc.Transaction) { x.AmountMsat = amt + 1000 })
	case "incoming":
		return other(func(x *nwc.Transaction) { x.Type = nwc.TypeIncoming })
	case "malformed":
		return refundtest.Err(nwc.ErrMalformed)
	case "error":
		return refundtest.Err(errors.New("boom"))
	}
	panic(name)
}

// noHub is a wallet that fails the test on any call.
type noHub struct{ t *testing.T }

func (h noHub) PayInvoiceUntil(context.Context, string, int64, time.Time) (*nwc.PayResult, error) {
	h.t.Error("pay_invoice called")
	return nil, errors.New("no hub")
}
func (h noHub) LookupInvoiceByString(context.Context, string) (*nwc.Transaction, error) {
	h.t.Error("lookup_invoice called")
	return nil, errors.New("no hub")
}
func (h noHub) GetBudget(context.Context) (*nwc.Budget, error) {
	h.t.Error("get_budget called")
	return nil, errors.New("no hub")
}

// hookWallet runs lookup, when set, at the start of every lookup_invoice call.
type hookWallet struct {
	refund.Wallet
	lookup func()
}

func (h *hookWallet) LookupInvoiceByString(ctx context.Context, invoice string) (*nwc.Transaction, error) {
	if h.lookup != nil {
		h.lookup()
	}
	return h.Wallet.LookupInvoiceByString(ctx, invoice)
}

const errVoidIncomplete = "refund: the void could not be completed"

// TestVoidTable pins V1: every cell of {claimed, sent} x {N, P, S, voided} x the lookup answers (the answer axis for
// sent only) gives its row's view or error, its result, its ledger transfers, its hub calls and its ALERTs. A cell
// outside V1 writes nothing.
func TestVoidTable(t *testing.T) {
	rows := map[string]string{"claimed N": "V1a", "claimed P": "V1b", "claimed S": "V1c", "claimed V": "",
		"sent N": "V1e", "sent P": "V1d", "sent S": "", "sent V": ""}
	for cell, want := range rows {
		if got := refund.VoidRow(strings.Fields(cell)[0], strings.Fields(cell)[1][0]); got != want {
			t.Errorf("VoidRow(%s) = %q, want %q", cell, got, want)
		}
	}
	type want struct {
		err           error
		state, reason string // the view's
		ledger        string
		alerts        int
		lookups       int
	}
	run := func(t *testing.T, k, l, answer string) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, k, l)
		if answer != "" {
			e.w.Lookup(inv(1), voidAnswer(answer))
		}
		var w want
		switch {
		case k == "claimed" && l == "N":
			w = want{nil, "failed", "cancelled", "voided", 0, 0} // the pending is created, then voided
		case k == "claimed" && l == "P":
			w = want{nil, "failed", "cancelled", "voided", 0, 0}
		case k == "claimed" && l == "S":
			w = want{nil, "settled", "", "posted", 0, 0}
		case k == "claimed":
			w = want{refund.ErrState, "claimed", "", "voided", 1, 0}
		case l == "S":
			w = want{refund.ErrState, "sent", "", "posted", 1, 0}
		case l == "V":
			w = want{refund.ErrState, "sent", "", "voided", 1, 0}
		default:
			stay, closed := "pending", "voided"
			if l == "N" {
				stay, closed = "none", "none"
			}
			switch answer {
			case "settled":
				w = want{nil, "settled", "", "posted", 0, 1}
			case "failed":
				w = want{nil, "failed", "failed", closed, 0, 1}
			case "no record", "expired":
				w = want{nil, "failed", "voided", closed, 0, 1}
			case "pending":
				w = want{refund.ErrInFlight, "sent", "", stay, 0, 1}
			case "other invoice", "other amount", "incoming", "malformed":
				w = want{refund.ErrHubUnavailable, "sent", "", stay, 1, 1}
			case "error":
				w = want{refund.ErrHubUnavailable, "sent", "", stay, 0, 1}
			}
		}
		v, err := e.void(1)
		if !errors.Is(err, w.err) {
			t.Fatalf("void: %v, want %v", err, w.err)
		}
		if err == nil && (v.State != w.state || v.Reason != w.reason) {
			t.Fatalf("view %+v, want %s %s", v, w.state, w.reason)
		}
		e.want(1, w.state, w.reason)
		e.wantLedger(1, w.ledger)
		if p, po, vo := e.ledgerOf(1); k == "claimed" && l == "N" && !(p && !po && vo) {
			t.Fatalf("V1a ledger: pending %v posted %v voided %v", p, po, vo)
		}
		if n := e.logs.alertCount(); n != w.alerts {
			t.Fatalf("%d ALERT lines, want %d", n, w.alerts)
		}
		if n := e.w.TotalCalls(); n != w.lookups || e.w.PayCount(inv(1)) != 0 {
			t.Fatalf("%d hub calls (want %d), %d pays", n, w.lookups, e.w.PayCount(inv(1)))
		}
		if w.alerts > 0 && w.err == refund.ErrState && e.alerts(refund.AlertVoidLedgerResolved, 1) != 1 {
			t.Fatal("no AlertVoidLedgerResolved")
		}
		if w.err == refund.ErrHubUnavailable && w.alerts > 0 && e.alerts(refund.AlertAnomaly, 1) != 1 {
			t.Fatal("no AlertAnomaly")
		}
		if err == nil {
			res := e.state(1).Result
			if (k == "claimed") != (res.Reason == "cancelled" || res.Reason == "ledger_paid") {
				t.Fatalf("stored reason %q", res.Reason)
			}
			if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
				t.Fatalf("second void: %v", err)
			}
		}
	}
	for _, k := range []string{"claimed", "sent"} {
		for _, l := range []string{"N", "P", "S", "V"} {
			if k == "claimed" {
				t.Run(k+" "+l, func(t *testing.T) { run(t, k, l, "") })
				continue
			}
			for _, a := range []string{"settled", "failed", "pending", "no record", "expired", "other invoice", "other amount",
				"incoming", "malformed", "error"} {
				t.Run(k+" "+l+" "+a, func(t *testing.T) { run(t, k, l, a) })
			}
		}
	}
}

// TestVoidClaimedNoHubCall: V1a to V1c make no hub call.
func TestVoidClaimedNoHubCall(t *testing.T) {
	for name, c := range map[string]struct{ ledger, state, reason, want string }{
		"V1a": {"N", "failed", "cancelled", "voided"}, "V1b": {"P", "failed", "cancelled", "voided"},
		"V1c": {"S", "settled", "", "posted"}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			e.wallet = noHub{t}
			e.newService()
			e.refundIn(1, "claimed", c.ledger)
			if _, err := e.void(1); err != nil {
				t.Fatal(err)
			}
			e.want(1, c.state, c.reason)
			e.wantLedger(1, c.want)
		})
	}
}

// TestVoidClaimedThenSender: the sender never pays a refund a void cancelled, and a void of a refund the sender had
// only prepared voids its pending.
func TestVoidClaimedThenSender(t *testing.T) {
	t.Run("void, then the sender", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		if v, err := e.void(1); err != nil || v.State != "failed" || v.Reason != "cancelled" {
			t.Fatalf("void: %+v %v", v, err)
		}
		e.quiesce()
		e.quiesce()
		e.want(1, "failed", "cancelled")
		e.wantLedger(1, "voided")
		if e.w.PayCount(inv(1)) != 0 || e.count() != 0 {
			t.Fatalf("%d pays, %d open", e.w.PayCount(inv(1)), e.count())
		}
	})
	t.Run("the sender stops at the pending, then void", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.claimed(1, e.order(10_000_000))
		refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepPending })
		e.send()
		e.wantLedger(1, "pending")
		e.want(1, "claimed", "")
		refund.SetCrash(e.svc, nil)
		if v, err := e.void(1); err != nil || v.State != "failed" || v.Reason != "cancelled" {
			t.Fatalf("void: %+v %v", v, err)
		}
		e.wantLedger(1, "voided")
		e.quiesce()
		if e.w.PayCount(inv(1)) != 0 || e.logs.alertCount() != 0 {
			t.Fatalf("%d pays, %d ALERT lines", e.w.PayCount(inv(1)), e.logs.alertCount())
		}
	})
}

// TestVoidClaimedInterleavings: the sender and a void meet at fixed points of the sender's round.
func TestVoidClaimedInterleavings(t *testing.T) {
	// newRound is an environment whose lookups and pay calls can run a hook; refund 1 is claimed.
	newRound := func(t *testing.T) (*env, *hookWallet) {
		e := newEnv(t, 5_000_000, 50_000_000)
		hw := &hookWallet{Wallet: e.w}
		e.wallet = hw
		e.newService()
		e.claimed(1, e.order(10_000_000))
		return e, hw
	}
	t.Run("inside the pre-send lookup: cancelled, nothing paid", func(t *testing.T) {
		e, hw := newRound(t)
		var verr error
		hw.lookup = func() { hw.lookup = nil; _, verr = e.void(1) }
		e.send()
		if verr != nil {
			t.Fatalf("void: %v", verr)
		}
		e.want(1, "failed", "cancelled")
		e.wantLedger(1, "voided")
		if e.w.PayCount(inv(1)) != 0 || e.logs.alertCount() != 0 {
			t.Fatalf("%d pays, %d ALERT lines", e.w.PayCount(inv(1)), e.logs.alertCount())
		}
	})
	t.Run("inside the pre-send lookup: no budget alert for the cancelled refund", func(t *testing.T) {
		// The budget left after the refund is below budget_alert_msat, so a send would raise AlertBudgetLow.
		for _, cancel := range []bool{true, false} {
			e, hw := newRound(t)
			e.cfg.BudgetAlertMsat = 1 << 51
			e.newService()
			if cancel {
				hw.lookup = func() { hw.lookup = nil; _, _ = e.void(1) }
			}
			e.send()
			want := map[bool]int{true: 0, false: 1}[cancel]
			if n := e.alerts(refund.AlertBudgetLow, 1); n != want {
				t.Fatalf("void in the lookup %v: %d AlertBudgetLow lines, want %d", cancel, n, want)
			}
			if cancel {
				e.want(1, "failed", "cancelled")
			} else if e.w.PayCount(inv(1)) != 1 {
				t.Fatal("not sent")
			}
		}
	})
	t.Run("between the marker and the call: refused, then paid once", func(t *testing.T) {
		e, _ := newRound(t)
		var verr error
		refund.SetCrash(e.svc, func(s string) bool {
			if s == refund.StepMarker {
				sentAt := e.state(1).SentAt
				e.clk.Set(sentAt.Add(refund.VoidAfter + time.Second)) // so only the in-flight mark can refuse it
				_, verr = e.void(1)
				e.clk.Set(sentAt.Add(5 * time.Second)) // the sender resumes before its request expires
			}
			return false
		})
		e.send()
		if !errors.Is(verr, refund.ErrInFlight) || e.w.PayCount(inv(1)) != 1 {
			t.Fatalf("void %v, %d pays", verr, e.w.PayCount(inv(1)))
		}
		e.want(1, "sent", "")
		e.wantLedger(1, "pending")
	})
	t.Run("inside the pay call: refused, paid once", func(t *testing.T) {
		e, _ := newRound(t)
		var verr error
		e.w.BeforePay(func(context.Context, string) {
			e.later(refund.VoidAfter + time.Second)
			_, verr = e.void(1)
		})
		e.send()
		if !errors.Is(verr, refund.ErrInFlight) || e.w.PayCount(inv(1)) != 1 {
			t.Fatalf("void %v, %d pays", verr, e.w.PayCount(inv(1)))
		}
		e.want(1, "sent", "")
	})
	t.Run("after the round: refused until the wait, then V1d", func(t *testing.T) {
		e, _ := newRound(t)
		e.send()
		if _, err := e.void(1); !errors.Is(err, refund.ErrInFlight) || e.w.PayCount(inv(1)) != 1 {
			t.Fatalf("void at once: %v", err)
		}
		e.w.AfterPay(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
		e.later(refund.VoidAfter)
		if v, err := e.void(1); err != nil || v.State != "settled" {
			t.Fatalf("void after the wait: %+v %v", v, err)
		}
		e.wantLedger(1, "posted")
		if e.w.PayCount(inv(1)) != 1 {
			t.Fatal("paid again")
		}
	})
}

// TestVoidClaimedRace: a void against a sender round on two goroutines, the void released from a random point inside
// the round. Either the refund was sent exactly once and the void was refused, or it is cancelled and nothing was
// paid, with no pending left open. Each arm must happen often, or the test fails: it never passes on one arm alone.
func TestVoidClaimedRace(t *testing.T) {
	const seed = 20261111
	rng := randv2.New(randv2.NewPCG(seed, seed))
	e := newEnv(t, 5_000_000, 5_000_000_000)
	hw := &hookWallet{Wallet: e.w}
	e.wallet = hw
	e.newService()
	cancelled, refused := 0, 0
	for round := 1; round <= 200; round++ {
		e.claimed(round, e.order(10_000_000))
		ctx := context.Background()
		id := hexID(round)
		if rng.IntN(4) == 0 { // the sender gets as far as the pending first
			refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepPending })
			e.send()
		}
		// The void is released at point 0 (the start), 1 (the pre-send lookup), 2 (the pending), 3 (the marker) or
		// 4 (the pay call), and always when the round ends.
		point, spin := rng.IntN(5), rng.IntN(100)
		gate, once := make(chan struct{}), sync.Once{}
		open := func() { once.Do(func() { close(gate) }) }
		if point == 0 {
			open()
		}
		hw.lookup = func() {
			if point == 1 {
				open()
			}
		}
		refund.SetCrash(e.svc, func(s string) bool {
			if (point == 2 && s == refund.StepPending) || (point == 3 && s == refund.StepMarker) {
				open()
			}
			return false
		})
		e.w.BeforePay(func(context.Context, string) {
			if point == 4 {
				open()
			}
		})
		var verr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			for i := 0; i < spin; i++ {
				runtime.Gosched()
			}
			_, verr = e.svc.Void(ctx, id, e.voidApproval(round, e.clk.Now().Add(time.Hour)))
		}()
		go func() {
			defer wg.Done()
			defer open()
			refund.SendRound(e.svc, ctx)
		}()
		wg.Wait()
		st, pays := e.state(round), e.w.PayCount(inv(round))
		p, po, vo := e.ledgerOf(round)
		switch {
		case verr == nil:
			cancelled++
			if !st.Resolved || st.Result.Reason != "cancelled" || st.Sent || pays != 0 || po || (p && !vo) {
				t.Fatalf("seed %d round %d: cancelled, but %+v pays %d ledger %v %v %v", seed, round, st, pays, p, po, vo)
			}
		case errors.Is(verr, refund.ErrInFlight):
			refused++
			if !st.Sent || st.Resolved || pays != 1 || !p || po || vo {
				t.Fatalf("seed %d round %d: refused, but %+v pays %d ledger %v %v %v", seed, round, st, pays, p, po, vo)
			}
			e.w.AfterPay(inv(round), refundtest.Tx(tx(round, nwc.StateSettled)))
			e.resolve() // settle it, so the open refunds stay under the cap
		default:
			t.Fatalf("seed %d round %d: void: %v", seed, round, verr)
		}
	}
	t.Logf("seed %d: %d cancelled, %d refused", seed, cancelled, refused)
	if cancelled < 20 || refused < 20 {
		t.Fatalf("seed %d: %d cancelled, %d refused in 200 rounds: an arm of the race is not run", seed, cancelled, refused)
	}
	if n := e.logs.alertCount(); n != 0 {
		t.Fatalf("seed %d: %d ALERT lines", seed, n)
	}
}

// TestVoidLedgerPaid: the ledger shows a claimed refund paid (the state TestRecoveryPostedUnsent builds): the void
// confirms it settled, and it still counts against the order.
func TestVoidLedgerPaid(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(1_500_000)
	e.claimed(1, o)
	e.putLedger(1, o, "pending", "post")
	e.restart()
	if e.alerts(refund.AlertPostedUnsent, 1) != 1 {
		t.Fatal("no posted-unsent ALERT")
	}
	v, err := e.void(1)
	if err != nil || v.State != "settled" || v.Reason != "" {
		t.Fatalf("void: %+v %v", v, err)
	}
	if r := e.state(1).Result; r.Outcome != "settled" || r.Reason != "ledger_paid" {
		t.Fatalf("stored result %+v", r)
	}
	e.wantLedger(1, "posted")
	if e.w.TotalCalls() != 0 {
		t.Fatal("a hub call")
	}
	if _, err := e.claim(2, o, amt); !errors.Is(err, refund.ErrExceedsRefundable) {
		t.Fatalf("a second refund of the whole amount: %v", err)
	}
	if _, err := e.claim(3, o, 500_000); err != nil {
		t.Fatalf("the rest: %v", err)
	}
	if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
		t.Fatalf("second void: %v", err)
	}
}

// TestVoidSentNoPending: a sent refund with no ledger pending (the state TestSentWithoutPendingAlerts builds), 11
// minutes on: a settled lookup writes the pending prepare would have written, then its post; a failure writes no
// transfer.
func TestVoidSentNoPending(t *testing.T) {
	ctx := context.Background()
	t.Run("settled", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.refundIn(1, "sent", "N")
		e.later(-store.RefundVoidWait)
		e.claimed(2, o)
		e.send() // refund 2 the normal way: its pending is what prepare writes
		e.later(store.RefundVoidWait)
		e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
		v, err := e.void(1)
		if err != nil || v.State != "settled" {
			t.Fatalf("void: %+v %v", v, err)
		}
		e.wantLedger(1, "posted")
		a, _, _ := e.lg.LookupTransfer(ctx, refund.RefundTransferID(id16(1)))
		b, found, _ := e.lg.LookupTransfer(ctx, refund.RefundTransferID(id16(2)))
		if !found {
			t.Fatal("refund 2 has no pending")
		}
		a.ID, a.Timestamp, b.ID, b.Timestamp = ledger.U128{}, 0, ledger.U128{}, 0
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("pending %+v, prepare's is %+v", a, b)
		}
		post, _, _ := e.lg.LookupTransfer(ctx, refund.PostTransferID(id16(1)))
		if post.PendingID != refund.RefundTransferID(id16(1)) || post.Flags != ledger.TransferPostPendingTransfer ||
			post.Amount != ledger.U128From64(amt) { // the ledger stores the whole pending amount
			t.Fatalf("post %+v", post)
		}
		if r := e.state(1).Result; r.Outcome != "settled" || r.Reason != "settled" {
			t.Fatalf("result %+v", r)
		}
		if e.logs.alertCount() != 0 {
			t.Fatal("an ALERT")
		}
	})
	for name, c := range map[string]struct {
		a             refundtest.Answer
		state, reason string
	}{
		"failed":    {refundtest.Tx(tx(1, nwc.StateFailed)), "failed", "failed"},
		"no record": {refundtest.NotFound(), "failed", "voided"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			e.refundIn(1, "sent", "N")
			e.w.Lookup(inv(1), c.a)
			if v, err := e.void(1); err != nil || v.State != c.state || v.Reason != c.reason {
				t.Fatalf("void: %+v %v", v, err)
			}
			e.wantLedger(1, "none")
			if e.logs.alertCount() != 0 {
				t.Fatal("an ALERT")
			}
		})
	}
}

// TestVoidSentNoPendingCrash: a crash inside V1e's two ledger steps leaves a state the resolver or recovery finishes.
func TestVoidSentNoPendingCrash(t *testing.T) {
	t.Run("after the pending", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "sent", "N")
		e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
		refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepVoidPending })
		if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.want(1, "sent", "")
		e.wantLedger(1, "pending")
		refund.SetCrash(e.svc, nil)
		e.resolve()
		e.want(1, "settled", "")
		e.wantLedger(1, "posted")
		if e.logs.alertCount() != 0 {
			t.Fatal("an ALERT")
		}
	})
	t.Run("after the post", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "sent", "N")
		e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
		refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepPost })
		if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.want(1, "sent", "")
		e.wantLedger(1, "posted")
		calls := e.w.TotalCalls()
		e.restart()
		e.want(1, "settled", "")
		if e.w.TotalCalls() != calls || e.logs.alertCount() != 0 {
			t.Fatal("a hub call or an ALERT")
		}
	})
}

// TestVoidSentNoPendingRefused: the ledger refuses V1e's pending: an ALERT, nothing else written, the refund stays
// sent.
func TestVoidSentNoPendingRefused(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	o := e.order(500_000) // the merchant account holds less than the refund
	e.paid.mu.Lock()
	e.paid.m[o] = 10_000_000
	e.paid.mu.Unlock()
	e.claimed(1, o)
	if err := e.st.AddRefundSend(context.Background(), hexID(1), e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	e.later(store.RefundVoidWait)
	e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
	if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
		t.Fatalf("void: %v", err)
	}
	e.want(1, "sent", "")
	e.wantLedger(1, "none")
	if e.alerts(refund.AlertLedgerRefused, 1) != 1 || e.logs.alertCount() != 1 {
		t.Fatalf("%d ALERT lines", e.logs.alertCount())
	}
}

// TestVoidResultRefusedOnce: for every row and outcome, a result the store refuses (the clock one second before the
// claim) waits with its row: a second void answers invalid_state, and the next round writes the same result, with
// the row's ledger transfers and no others, and no ALERT but the one for the refused result.
func TestVoidResultRefusedOnce(t *testing.T) {
	cases := []struct {
		name, kind, ledger, answer string
		state, reason, stored      string // the view's, and the stored reason
		ledgerWant                 string
		lookups                    int
	}{
		{"V1a", "claimed", "N", "", "failed", "cancelled", "cancelled", "voided", 0},
		{"V1b", "claimed", "P", "", "failed", "cancelled", "cancelled", "voided", 0},
		{"V1c", "claimed", "S", "", "settled", "", "ledger_paid", "posted", 0},
		{"V1d settled", "sent", "P", "settled", "settled", "", "settled", "posted", 1},
		{"V1d failed", "sent", "P", "failed", "failed", "failed", "failed", "voided", 1},
		{"V1d no record", "sent", "P", "no record", "failed", "voided", "voided", "voided", 1},
		{"V1e settled", "sent", "N", "settled", "settled", "", "settled", "posted", 1},
		{"V1e failed", "sent", "N", "failed", "failed", "failed", "failed", "none", 1},
		{"V1e no record", "sent", "N", "no record", "failed", "voided", "voided", "none", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			hw := &hookWallet{Wallet: e.w}
			e.wallet = hw
			e.newService()
			e.refundIn(1, c.kind, c.ledger)
			if c.answer != "" {
				e.w.Lookup(inv(1), voidAnswer(c.answer))
			}
			t0, behind := e.clk.Now(), e.state(1).ClaimedAt.Add(-time.Second)
			if c.kind == "claimed" {
				e.clk.Set(behind)
			} else {
				hw.lookup = func() { e.clk.Set(behind) }
			}
			if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
				t.Fatalf("void: %v", err)
			}
			e.want(1, c.kind, "")
			e.wantLedger(1, c.ledgerWant)
			if e.alerts(refund.AlertResultRefused, 1) != 1 {
				t.Fatal("no refused-result ALERT")
			}
			if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
				t.Fatalf("second void: %v", err)
			}
			e.clk.Set(t0)
			e.send()
			e.want(1, c.state, c.reason)
			if r := e.state(1).Result; r.Reason != c.stored {
				t.Fatalf("stored reason %q, want %q", r.Reason, c.stored)
			}
			e.wantLedger(1, c.ledgerWant)
			if n := e.logs.alertCount(); n != 1 {
				t.Fatalf("%d ALERT lines, want the one", n)
			}
			if n := e.w.TotalCalls(); n != c.lookups || e.w.PayCount(inv(1)) != 0 {
				t.Fatalf("%d hub calls, %d pays", n, e.w.PayCount(inv(1)))
			}
		})
	}
}

// TestVoidRereadsAfterLookup: the void takes the mutex again after its lookup and repeats the checks; if the refund
// or the ledger changed meanwhile it answers invalid_state and writes nothing.
func TestVoidRereadsAfterLookup(t *testing.T) {
	cases := []struct {
		name, ledger string
		change       func(e *env, order string)
		state        string
		ledgerWant   string
		alerts       int
	}{
		{"a result is written", "P", func(e *env, _ string) {
			if err := e.st.AddRefundResult(context.Background(), store.RefundResult{RefundID: hexID(1),
				Outcome: store.RefundOutcomeFailed, Reason: store.RefundReasonFailed, At: e.clk.Now()}); err != nil {
				e.t.Fatal(err)
			}
		}, "failed", "pending", 0},
		{"the pending appears", "N", func(e *env, o string) { e.putLedger(1, o, "pending") }, "sent", "pending", 0},
		{"the pending is posted", "P", func(e *env, o string) { e.putLedger(1, o, "post") }, "sent", "posted", 1},
		{"the pending is voided", "P", func(e *env, o string) { e.putLedger(1, o, "void") }, "sent", "voided", 1},
		{"a result waits for its write", "P", func(e *env, _ string) {
			// The resolver decides failed during the void's lookup and the store refuses the write (the clock is behind).
			t0 := e.clk.Now()
			e.clk.Set(e.state(1).ClaimedAt.Add(-time.Second))
			e.resolve()
			e.clk.Set(t0)
		}, "sent", "voided", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			hw := &hookWallet{Wallet: e.w}
			e.wallet = hw
			e.newService()
			o := e.refundIn(1, "sent", c.ledger)
			e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateFailed)), refundtest.Tx(tx(1, nwc.StateSettled)))
			hw.lookup = func() { hw.lookup = nil; c.change(e, o) } // the first answer is the change's (the resolver's)
			if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
				t.Fatalf("void: %v", err)
			}
			e.want(1, c.state, map[bool]string{true: "failed", false: ""}[c.state == "failed"])
			e.wantLedger(1, c.ledgerWant)
			if n := e.logs.alertCount(); n != c.alerts {
				t.Fatalf("%d ALERT lines, want %d", n, c.alerts)
			}
		})
	}
}

// TestVoidWaiting: a refund with a refused result waiting answers invalid_state to a void, in both store kinds.
func TestVoidWaiting(t *testing.T) {
	t.Run("claimed", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "claimed", "P")
		t0 := e.clk.Now()
		e.clk.Set(e.state(1).ClaimedAt.Add(-time.Second))
		if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("first void: %v", err)
		}
		e.clk.Set(t0)
		if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
			t.Fatalf("void over a waiting result: %v", err)
		}
		e.want(1, "claimed", "")
	})
	t.Run("sent", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "sent", "P")
		e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateFailed)))
		t0 := e.clk.Now()
		e.clk.Set(e.state(1).ClaimedAt.Add(-time.Second))
		e.resolve() // the resolver decides failed; the store refuses it
		if e.alerts(refund.AlertResultRefused, 1) != 1 {
			t.Fatal("no refused result")
		}
		e.clk.Set(t0)
		calls := e.w.TotalCalls()
		if _, err := e.void(1); !errors.Is(err, refund.ErrState) {
			t.Fatalf("void over a waiting result: %v", err)
		}
		e.want(1, "sent", "")
		if e.w.TotalCalls() != calls {
			t.Fatal("the void looked up the hub")
		}
	})
}

// TestVoidSentNoPendingWaits: V1e, like V1d, answers refund_in_flight before sent_at + VoidAfter, with no lookup.
func TestVoidSentNoPendingWaits(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.refundIn(1, "sent", "N")
	e.later(-time.Second)
	if _, err := e.void(1); !errors.Is(err, refund.ErrInFlight) {
		t.Fatalf("void a second early: %v", err)
	}
	if e.w.TotalCalls() != 0 || e.logs.alertCount() != 0 {
		t.Fatal("a lookup or an ALERT")
	}
	e.want(1, "sent", "")
	e.wantLedger(1, "none")
}

// TestVoidCancelCrashAfterLedgerVoid: a V1b void that stops right after it voided the pending leaves a claimed refund
// with a voided pending; start-up recovery ends it failed recovered (not cancelled), and nothing is paid.
func TestVoidCancelCrashAfterLedgerVoid(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	e.refundIn(1, "claimed", "P")
	refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepVoid })
	if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
		t.Fatalf("void: %v", err)
	}
	e.want(1, "claimed", "")
	e.wantLedger(1, "voided")
	e.restart()
	e.want(1, "failed", "recovered")
	e.wantLedger(1, "voided")
	e.quiesce()
	if e.w.PayCount(inv(1)) != 0 || e.w.TotalCalls() != 0 || e.logs.alertCount() != 0 {
		t.Fatalf("%d pays, %d hub calls, %d ALERT lines", e.w.PayCount(inv(1)), e.w.TotalCalls(), e.logs.alertCount())
	}
}

// lockLedger holds the ledger file's write lock until the returned function releases it: a write to the ledger then
// fails (after the busy timeout), a read does not.
func lockLedger(t *testing.T, e *env) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(e.dir, "ledger.db")+"?_txlock=immediate&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	return func() { tx.Rollback(); db.Close() }
}

// TestVoidLedgerErrorWaitsForNothing: a void whose ledger step fails with an error (not a refusal) answers its
// internal error and leaves nothing waiting and no retry line: a second void runs the row again.
func TestVoidLedgerErrorWaitsForNothing(t *testing.T) {
	t.Run("V1b, the void transfer", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "claimed", "P")
		release := lockLedger(t, e)
		_, err := e.void(1)
		release()
		if err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.want(1, "claimed", "")
		e.wantLedger(1, "pending")
		if refund.Waiting(e.svc, hexID(1)) || e.logs.count(refund.LogRetry, hexID(1)) != 0 || e.logs.alertCount() != 0 {
			t.Fatal("waiting, or a retry line, or an ALERT")
		}
		if v, err := e.void(1); err != nil || v.State != "failed" || v.Reason != "cancelled" {
			t.Fatalf("second void: %+v %v", v, err)
		}
		e.wantLedger(1, "voided")
	})
	t.Run("V1e, the pending", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "sent", "N")
		e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateSettled)))
		release := lockLedger(t, e)
		_, err := e.void(1)
		release()
		if err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.want(1, "sent", "")
		e.wantLedger(1, "none")
		if refund.Waiting(e.svc, hexID(1)) || e.logs.count(refund.LogRetry, hexID(1)) != 0 || e.logs.alertCount() != 0 {
			t.Fatal("waiting, or a retry line, or an ALERT")
		}
		if v, err := e.void(1); err != nil || v.State != "settled" {
			t.Fatalf("second void: %+v %v", v, err)
		}
		e.wantLedger(1, "posted") // one pending, one post: their ids are the refund's
		if e.alerts(refund.AlertLedgerRefused, 1) != 0 || e.logs.alertCount() != 0 {
			t.Fatal("an ALERT")
		}
	})
}

// TestVoidClaimedResultRefusedThenRestart: V1a and V1b move the ledger first, so a cancel whose result the store
// refuses survives a restart: recovery ends the refund failed recovered and it is never sent.
func TestVoidClaimedResultRefusedThenRestart(t *testing.T) {
	for name, l := range map[string]string{"V1a": "N", "V1b": "P"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 5_000_000, 50_000_000)
			e.refundIn(1, "claimed", l)
			t0 := e.clk.Now()
			e.clk.Set(e.state(1).ClaimedAt.Add(-time.Second))
			if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
				t.Fatalf("void: %v", err)
			}
			e.want(1, "claimed", "")
			e.wantLedger(1, "voided")
			e.clk.Set(t0)
			e.restart()
			e.want(1, "failed", "recovered")
			for i := 0; i < 3; i++ {
				e.send()
			}
			if e.w.PayCount(inv(1)) != 0 || e.w.TotalCalls() != 0 {
				t.Fatalf("%d pays, %d hub calls", e.w.PayCount(inv(1)), e.w.TotalCalls())
			}
			if e.alerts(refund.AlertResultRefused, 1) != 1 || e.logs.alertCount() != 1 {
				t.Fatalf("%d ALERT lines", e.logs.alertCount())
			}
		})
	}
}

// TestVoidClaimedCrash: V1a writes the pending and its void in one ledger batch, so there is no crash point between
// them; a void that stops right after the batch never answered, and after a restart the refund ends recovered.
func TestVoidClaimedCrash(t *testing.T) {
	t.Run("after the void transfer", func(t *testing.T) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "claimed", "N")
		refund.SetCrash(e.svc, func(s string) bool { return s == refund.StepVoid })
		if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.restart()
		e.want(1, "failed", "recovered")
		e.quiesce()
		e.wantLedger(1, "voided")
		if e.w.PayCount(inv(1)) != 0 || e.logs.alertCount() != 0 {
			t.Fatalf("%d pays, %d ALERT lines", e.w.PayCount(inv(1)), e.logs.alertCount())
		}
	})
}

// TestVoidClaimedLedgerRefused: the ledger refuses the refund's pending (the merchant account holds less than the
// refund): the void falls back to the store-only cancel, writes no transfer and raises no ALERT; a result the store
// refuses then waits in memory with no ledger step.
func TestVoidClaimedLedgerRefused(t *testing.T) {
	build := func(t *testing.T) *env {
		e := newEnv(t, 5_000_000, 50_000_000)
		o := e.order(500_000)
		e.paid.mu.Lock()
		e.paid.m[o] = 10_000_000
		e.paid.mu.Unlock()
		e.claimed(1, o)
		return e
	}
	t.Run("cancelled", func(t *testing.T) {
		e := build(t)
		if v, err := e.void(1); err != nil || v.State != "failed" || v.Reason != "cancelled" {
			t.Fatalf("void: %+v %v", v, err)
		}
		e.wantLedger(1, "none")
		if e.logs.alertCount() != 0 || e.w.TotalCalls() != 0 {
			t.Fatalf("%d ALERT lines, %d hub calls", e.logs.alertCount(), e.w.TotalCalls())
		}
	})
	t.Run("the result is refused once", func(t *testing.T) {
		e := build(t)
		t0 := e.clk.Now()
		e.clk.Set(e.state(1).ClaimedAt.Add(-time.Second))
		if _, err := e.void(1); err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.clk.Set(t0)
		e.send()
		e.want(1, "failed", "cancelled")
		e.wantLedger(1, "none")
		if e.logs.alertCount() != 1 || e.alerts(refund.AlertResultRefused, 1) != 1 {
			t.Fatalf("%d ALERT lines", e.logs.alertCount())
		}
	})
}

// TestVoidClaimedLedgerErrors: the ledger batch of V1a errors (not a refusal): the internal error, nothing written
// (the batch is applied whole or not at all, so not even the pending), nothing waiting, no retry line. After a restart
// the refund is a plain claimed one with no ledger entry, and the sender sends it; a second void runs the row again.
func TestVoidClaimedLedgerErrors(t *testing.T) {
	build := func(t *testing.T) (*env, error) {
		e := newEnv(t, 5_000_000, 50_000_000)
		e.refundIn(1, "claimed", "N")
		release := lockLedger(t, e)
		_, err := e.void(1)
		release()
		if err == nil || err.Error() != errVoidIncomplete {
			t.Fatalf("void: %v", err)
		}
		e.want(1, "claimed", "")
		e.wantLedger(1, "none")
		if refund.Waiting(e.svc, hexID(1)) || e.logs.count(refund.LogRetry, hexID(1)) != 0 || e.logs.alertCount() != 0 {
			t.Fatal("waiting, or a retry line, or an ALERT")
		}
		return e, err
	}
	t.Run("a second void", func(t *testing.T) {
		e, _ := build(t)
		if v, err := e.void(1); err != nil || v.Reason != "cancelled" {
			t.Fatalf("second void: %+v %v", v, err)
		}
		e.wantLedger(1, "voided")
	})
	t.Run("a restart, then the sender", func(t *testing.T) {
		e, _ := build(t)
		e.restart()
		e.want(1, "claimed", "")
		e.wantLedger(1, "none")
		e.send()
		e.want(1, "sent", "")
		if e.w.PayCount(inv(1)) != 1 {
			t.Fatalf("%d pays", e.w.PayCount(inv(1)))
		}
	})
}

// TestWorkerStoreErrorLogsRefundID: a worker whose store read fails after its hub call logs the retry line once, with
// the refund id.
func TestWorkerStoreErrorLogsRefundID(t *testing.T) {
	e := newEnv(t, 5_000_000, 50_000_000)
	hw := &hookWallet{Wallet: e.w}
	e.wallet = hw
	e.newService()
	e.refundIn(1, "sent", "P")
	e.w.Lookup(inv(1), refundtest.Tx(tx(1, nwc.StateFailed)))
	hw.lookup = func() { hw.lookup = nil; e.st.Close() } // the store goes away during the lookup
	e.resolve()
	if n := e.logs.count(refund.LogRetry, hexID(1)); n != 1 {
		t.Fatalf("%d retry lines with the refund id, want 1", n)
	}
	if n := e.logs.count(refund.LogRetry, ""); n != 1 {
		t.Fatalf("%d retry lines in all, want 1", n)
	}
}
