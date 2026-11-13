package ledger

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
)

// pend is a pending transfer 1 -> 2 on ledger 1, code 1.
func pend(id uint64, amount U128, timeout uint32) Transfer {
	x := tr(id, 1, 2, amount)
	x.Flags, x.Timeout = TransferPending, timeout
	return x
}

// resolve is a post or void of pending, with every inheritable field left zero.
func resolve(id, pending uint64, flag TransferFlags, amount U128) Transfer {
	return Transfer{ID: u(id), PendingID: u(pending), Amount: amount, Flags: flag}
}

func wantBal(t *testing.T, l *Ledger, id uint64, dp, dq, cp, cq U128) {
	t.Helper()
	b := bal(t, l, id)
	if b != (Balance{DebitsPending: dp, DebitsPosted: dq, CreditsPending: cp, CreditsPosted: cq}) {
		t.Fatalf("account %d: %+v, want dp=%v dq=%v cp=%v cq=%v", id, b, dp, dq, cp, cq)
	}
}

func TestTwoPhaseResults(t *testing.T) {
	post, void := TransferPostPendingTransfer, TransferVoidPendingTransfer
	// Setup: pending 10 (100, no timeout), pending 11 (100, timeout 60 s), single-phase 12.
	// Each case runs on a fresh ledger; before may add rows, then x is created.
	cases := []struct {
		name   string
		before func(t *testing.T, l *Ledger, clk *testClock)
		x      Transfer
		want   CreateTransferResult
	}{
		{"post ok", nil, resolve(20, 10, post, u(100)), TransferCreated},
		{"void ok", nil, resolve(20, 10, void, u(0)), TransferCreated},
		{"flags_are_mutually_exclusive post+void", nil, resolve(20, 10, post|void, u(1)), TransferFlagsAreMutuallyExclusive},
		{"flags_are_mutually_exclusive post+pending", nil, resolve(20, 10, post|TransferPending, u(1)), TransferFlagsAreMutuallyExclusive},
		{"flags_are_mutually_exclusive void+pending", nil, resolve(20, 10, void|TransferPending, u(1)), TransferFlagsAreMutuallyExclusive},
		{"mutually exclusive before pending_id check", nil, resolve(20, 0, post|void, u(1)), TransferFlagsAreMutuallyExclusive},
		{"pending_id_must_not_be_zero", nil, resolve(20, 0, post, u(1)), TransferPendingIDMustNotBeZero},
		{"pending_id_must_not_be_int_max", nil, Transfer{ID: u(20), PendingID: U128Max, Flags: post}, TransferPendingIDMustNotBeIntMax},
		{"pending_id_must_be_different", nil, resolve(20, 20, post, u(1)), TransferPendingIDMustBeDifferent},
		{"timeout_reserved_for_pending_transfer", nil, func() Transfer { x := resolve(20, 10, post, u(1)); x.Timeout = 1; return x }(), TransferTimeoutReservedForPendingTransfer},
		{"timeout checked before the lookup", nil, func() Transfer { x := resolve(20, 99, post, u(1)); x.Timeout = 1; return x }(), TransferTimeoutReservedForPendingTransfer},
		{"pending_transfer_not_found", nil, resolve(20, 99, post, u(1)), TransferPendingTransferNotFound},
		{"pending_transfer_not_pending", nil, resolve(20, 12, post, u(1)), TransferPendingTransferNotPending},
		{"pending_transfer_has_different_debit_account_id", nil, func() Transfer { x := resolve(20, 10, post, u(1)); x.DebitAccountID = u(2); return x }(), TransferPendingTransferHasDifferentDebitAccountID},
		{"pending_transfer_has_different_credit_account_id", nil, func() Transfer { x := resolve(20, 10, post, u(1)); x.CreditAccountID = u(1); return x }(), TransferPendingTransferHasDifferentCreditAccountID},
		{"debit checked before credit", nil, func() Transfer {
			x := resolve(20, 10, post, u(1))
			x.DebitAccountID, x.CreditAccountID = u(2), u(1)
			return x
		}(), TransferPendingTransferHasDifferentDebitAccountID},
		{"pending_transfer_has_different_ledger", nil, func() Transfer { x := resolve(20, 10, post, u(1)); x.Ledger = 2; return x }(), TransferPendingTransferHasDifferentLedger},
		{"pending_transfer_has_different_code", nil, func() Transfer { x := resolve(20, 10, post, u(1)); x.Code = 2; return x }(), TransferPendingTransferHasDifferentCode},
		{"ledger checked before code", nil, func() Transfer { x := resolve(20, 10, post, u(1)); x.Ledger, x.Code = 2, 2; return x }(), TransferPendingTransferHasDifferentLedger},
		{"matching fields are accepted", nil, Transfer{ID: u(20), PendingID: u(10), DebitAccountID: u(1), CreditAccountID: u(2), Ledger: 1, Code: 1, Amount: u(100), Flags: post}, TransferCreated},
		{"exceeds_pending_transfer_amount", nil, resolve(20, 10, post, u(101)), TransferExceedsPendingTransferAmount},
		{"post one above U128Max-1 does not wrap", nil, resolve(20, 10, post, mustSub(U128Max, u(1))), TransferExceedsPendingTransferAmount},
		{"void above pending amount is exceeds_pending_transfer_amount", nil, resolve(20, 10, void, u(101)), TransferExceedsPendingTransferAmount},
		{"pending_transfer_has_different_amount", nil, resolve(20, 10, void, u(50)), TransferPendingTransferHasDifferentAmount},
		{"void of the exact amount", nil, resolve(20, 10, void, u(100)), TransferCreated},
		{"void of U128Max is not 'full'", nil, resolve(20, 10, void, U128Max), TransferExceedsPendingTransferAmount},
		{"post of 0 releases everything", nil, resolve(20, 10, post, u(0)), TransferCreated},
		{"amount checked before already posted", func(t *testing.T, l *Ledger, _ *testClock) {
			mustCreate(t, l, resolve(19, 10, post, u(100)))
		}, resolve(20, 10, post, u(101)), TransferExceedsPendingTransferAmount},
		{"pending_transfer_already_posted", func(t *testing.T, l *Ledger, _ *testClock) {
			mustCreate(t, l, resolve(19, 10, post, u(100)))
		}, resolve(20, 10, post, u(100)), TransferPendingTransferAlreadyPosted},
		{"void after post", func(t *testing.T, l *Ledger, _ *testClock) {
			mustCreate(t, l, resolve(19, 10, post, u(100)))
		}, resolve(20, 10, void, u(0)), TransferPendingTransferAlreadyPosted},
		{"pending_transfer_already_voided", func(t *testing.T, l *Ledger, _ *testClock) {
			mustCreate(t, l, resolve(19, 10, void, u(0)))
		}, resolve(20, 10, post, u(100)), TransferPendingTransferAlreadyVoided},
		{"void after void", func(t *testing.T, l *Ledger, _ *testClock) {
			mustCreate(t, l, resolve(19, 10, void, u(0)))
		}, resolve(20, 10, void, u(0)), TransferPendingTransferAlreadyVoided},
		{"resolved before expiry stays resolved after it", func(t *testing.T, l *Ledger, clk *testClock) {
			mustCreate(t, l, resolve(19, 11, post, u(100)))
			clk.advance(2 * time.Minute)
		}, resolve(20, 11, post, u(100)), TransferPendingTransferAlreadyPosted},
		{"pending_transfer_expired", func(t *testing.T, l *Ledger, clk *testClock) { clk.advance(61 * time.Second) },
			resolve(20, 11, post, u(100)), TransferPendingTransferExpired},
		{"void of an expired pending", func(t *testing.T, l *Ledger, clk *testClock) { clk.advance(time.Hour) },
			resolve(20, 11, void, u(0)), TransferPendingTransferExpired},
		{"amount checked before expiry", func(t *testing.T, l *Ledger, clk *testClock) { clk.advance(time.Hour) },
			resolve(20, 11, post, u(101)), TransferExceedsPendingTransferAmount},
		{"one nanosecond before expiry", func(t *testing.T, l *Ledger, clk *testClock) {
			p, _, _ := l.LookupTransfer(context.Background(), u(11))
			clk.ns = int64(p.Timestamp) + 60_000_000_000 - 1
		}, resolve(20, 11, post, u(100)), TransferCreated},
		{"exactly at expiry", func(t *testing.T, l *Ledger, clk *testClock) {
			p, _, _ := l.LookupTransfer(context.Background(), u(11))
			clk.ns = int64(p.Timestamp) + 60_000_000_000
		}, resolve(20, 11, post, u(100)), TransferPendingTransferExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, clk := newTL(t)
			mustAccts(t, l, 1, 2)
			mustCreate(t, l, pend(10, u(100), 0))
			mustCreate(t, l, pend(11, u(100), 60))
			mustCreate(t, l, tr(12, 1, 2, u(5)))
			if c.before != nil {
				c.before(t, l, clk)
			}
			if got := create(t, l, c.x); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			// Only created rows are stored; transient failures (pending_transfer_not_found) are
			// remembered, everything else leaves no trace.
			_, found, err := l.LookupTransfer(context.Background(), c.x.ID)
			if err != nil || found != (c.want == TransferCreated) {
				t.Fatalf("found=%v err=%v", found, err)
			}
			if c.want == TransferPendingTransferNotFound {
				if got := create(t, l, c.x); got != TransferIDAlreadyFailed {
					t.Fatalf("retry of a transient failure: %q", got)
				}
				mustCreate(t, l, pend(99, u(1), 0)) // now it exists, but the id stays failed
				if got := create(t, l, c.x); got != TransferIDAlreadyFailed {
					t.Fatalf("retry after the pending transfer appeared: %q", got)
				}
			} else if c.want != TransferCreated {
				var n int
				if err := l.db.QueryRow(`SELECT count(*) FROM failed_transfers`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("failed rows %d %v", n, err)
				}
			}
		})
	}
}

// What the ledger stores for a post or void: the pending transfer's accounts, ledger and code,
// the amount actually posted (or the pending amount for a void), timeout 0, and user data taken
// from the request when nonzero and else from the pending transfer.
func TestResolutionRowFields(t *testing.T) {
	ctx := context.Background()
	for _, flag := range []TransferFlags{TransferPostPendingTransfer, TransferVoidPendingTransfer} {
		for _, own := range []bool{false, true} { // request user data zero, or nonzero (wins)
			l, _ := newTL(t)
			mustAccts(t, l, 1, 2)
			p := pend(10, u(100), 60)
			p.UserData128, p.UserData64, p.UserData32 = u(7), 8, 9
			mustCreate(t, l, p)
			amount := u(30)
			if flag == TransferVoidPendingTransfer {
				amount = U128Zero
			}
			x := resolve(20, 10, flag, amount)
			if own {
				x.UserData128, x.UserData64, x.UserData32 = u(1), 2, 3
			}
			mustCreate(t, l, x)
			got, found, err := l.LookupTransfer(ctx, u(20))
			if err != nil || !found {
				t.Fatal(found, err)
			}
			want := Transfer{ID: u(20), DebitAccountID: u(1), CreditAccountID: u(2), Amount: u(30), PendingID: u(10),
				UserData128: u(7), UserData64: 8, UserData32: 9, Ledger: 1, Code: 1, Flags: flag, Timestamp: got.Timestamp}
			if own {
				want.UserData128, want.UserData64, want.UserData32 = u(1), 2, 3
			}
			if flag == TransferVoidPendingTransfer {
				want.Amount = u(100)
			}
			if got != want {
				t.Errorf("flags %d own=%v: %+v, want %+v", flag, own, got, want)
			}
		}
	}
}

// Balances follow the example tables of two-phase-transfers.md.
func TestPostAmounts(t *testing.T) {
	post := TransferPostPendingTransfer
	cases := []struct {
		name   string
		amount U128
		posted U128
	}{
		{"exactly the pending amount", u(100), u(100)},
		{"U128Max posts the full amount", U128Max, u(100)},
		{"less: the remainder is released", u(40), u(40)},
		{"zero: all released", u(0), u(0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, _ := newTL(t)
			mustAccts(t, l, 1, 2)
			mustCreate(t, l, pend(10, u(100), 0))
			wantBal(t, l, 1, u(100), u(0), u(0), u(0))
			wantBal(t, l, 2, u(0), u(0), u(100), u(0))
			mustCreate(t, l, resolve(20, 10, post, c.amount))
			wantBal(t, l, 1, u(0), c.posted, u(0), u(0))
			wantBal(t, l, 2, u(0), u(0), u(0), c.posted)
			got, _, _ := l.LookupTransfer(context.Background(), u(20))
			if got.Amount != c.posted {
				t.Fatalf("stored amount %v, want %v", got.Amount, c.posted)
			}
		})
	}
	t.Run("void adds nothing", func(t *testing.T) {
		l, _ := newTL(t)
		mustAccts(t, l, 1, 2)
		mustCreate(t, l, pend(10, u(100), 0))
		mustCreate(t, l, resolve(20, 10, TransferVoidPendingTransfer, u(0)))
		wantBal(t, l, 1, u(0), u(0), u(0), u(0))
		wantBal(t, l, 2, u(0), u(0), u(0), u(0))
	})
	t.Run("amounts at the 64 bit edge", func(t *testing.T) {
		for _, a := range []U128{u(1<<63 - 1), p2(63), u(^uint64(0)), p2(64), mustSub(U128Max, u(1)), U128Max} {
			l, _ := newTL(t)
			mustAccts(t, l, 1, 2)
			mustCreate(t, l, pend(10, a, 0))
			wantBal(t, l, 1, a, u(0), u(0), u(0))
			mustCreate(t, l, resolve(20, 10, post, U128Max))
			wantBal(t, l, 1, u(0), a, u(0), u(0))
			wantBal(t, l, 2, u(0), u(0), u(0), a)
		}
	})
	t.Run("another pending and posted amount on the same accounts", func(t *testing.T) {
		l, _ := newTL(t)
		mustAccts(t, l, 1, 2)
		mustCreate(t, l, tr(1, 1, 2, u(7)))
		mustCreate(t, l, pend(10, u(100), 0))
		mustCreate(t, l, pend(11, u(30), 0))
		wantBal(t, l, 1, u(130), u(7), u(0), u(0))
		mustCreate(t, l, resolve(20, 10, post, u(40)))
		wantBal(t, l, 1, u(30), u(47), u(0), u(0))
		wantBal(t, l, 2, u(0), u(0), u(30), u(47))
		mustCreate(t, l, resolve(21, 11, post, U128Max))
		wantBal(t, l, 1, u(0), u(77), u(0), u(0))
		wantBal(t, l, 2, u(0), u(0), u(0), u(77))
	})
	t.Run("a released limit can be used again", func(t *testing.T) {
		l, _ := newTL(t)
		mustAccts(t, l, 2, 3)
		mustAcct(t, l, 1, 1, AccountDebitsMustNotExceedCredits)
		mustCreate(t, l, tr(1, 2, 1, u(100)))
		p := tr(2, 1, 3, u(100))
		p.Flags = TransferPending
		mustCreate(t, l, p)
		if r := create(t, l, tr(3, 1, 3, u(1))); r != TransferExceedsCredits {
			t.Fatal(r)
		}
		mustCreate(t, l, resolve(4, 2, post, u(60)))
		if r := create(t, l, tr(5, 1, 3, u(41))); r != TransferExceedsCredits {
			t.Fatal(r)
		}
		mustCreate(t, l, tr(6, 1, 3, u(40)))
	})
}

// The exists rule for post and void is fn post_or_void_pending_transfer_exists
// (state_machine.zig:4296-4377). It is stricter than the prose of create_transfers.md for post:
// the request amount must equal the stored amount, or be U128Max when the stored amount is the
// whole pending amount.
func TestPostExistsRule(t *testing.T) {
	post, void := TransferPostPendingTransfer, TransferVoidPendingTransfer
	setup := func(t *testing.T) *Ledger {
		l, _ := newTL(t)
		mustAccts(t, l, 1, 2)
		mustCreate(t, l, pend(10, u(100), 0))
		return l
	}
	t.Run("full post", func(t *testing.T) {
		l := setup(t)
		mustCreate(t, l, resolve(20, 10, post, U128Max))
		for _, c := range []struct {
			amount U128
			want   CreateTransferResult
		}{
			{U128Max, TransferExists}, {u(100), TransferExists},
			{u(99), TransferExistsWithDifferentAmount}, {u(101), TransferExistsWithDifferentAmount},
			{u(0), TransferExistsWithDifferentAmount},
		} {
			if got := create(t, l, resolve(20, 10, post, c.amount)); got != c.want {
				t.Errorf("amount %v: %q, want %q", c.amount, got, c.want)
			}
		}
	})
	t.Run("partial post", func(t *testing.T) {
		l := setup(t)
		mustCreate(t, l, resolve(20, 10, post, u(40)))
		for _, c := range []struct {
			amount U128
			want   CreateTransferResult
		}{
			{u(40), TransferExists},
			{U128Max, TransferExistsWithDifferentAmount}, // would mean 100
			{u(100), TransferExistsWithDifferentAmount},
			{u(41), TransferExistsWithDifferentAmount},
		} {
			if got := create(t, l, resolve(20, 10, post, c.amount)); got != c.want {
				t.Errorf("amount %v: %q, want %q", c.amount, got, c.want)
			}
		}
	})
	t.Run("void", func(t *testing.T) {
		l := setup(t)
		mustCreate(t, l, resolve(20, 10, void, u(0)))
		for _, c := range []struct {
			amount U128
			want   CreateTransferResult
		}{{u(0), TransferExists}, {u(100), TransferExists}, {u(50), TransferExistsWithDifferentAmount}, {U128Max, TransferExistsWithDifferentAmount}} {
			if got := create(t, l, resolve(20, 10, void, c.amount)); got != c.want {
				t.Errorf("amount %v: %q, want %q", c.amount, got, c.want)
			}
		}
	})
	t.Run("other fields", func(t *testing.T) {
		l := setup(t)
		x := resolve(20, 10, post, u(100))
		x.UserData64 = 5
		mustCreate(t, l, x)
		mod := func(f func(*Transfer)) CreateTransferResult { y := x; f(&y); return create(t, l, y) }
		checks := []struct {
			name string
			f    func(*Transfer)
			want CreateTransferResult
		}{
			{"same", func(*Transfer) {}, TransferExists},
			{"flags", func(y *Transfer) { y.Flags = void }, TransferExistsWithDifferentFlags},
			{"pending_id", func(y *Transfer) { y.PendingID = u(11) }, TransferExistsWithDifferentPendingID},
			{"timeout", func(y *Transfer) { y.Timeout = 1 }, TransferExistsWithDifferentTimeout},
			{"debit zero is inherited", func(y *Transfer) { y.DebitAccountID = u(1) }, TransferExists},
			{"debit", func(y *Transfer) { y.DebitAccountID = u(2) }, TransferExistsWithDifferentDebitAccountID},
			{"credit", func(y *Transfer) { y.CreditAccountID = u(1) }, TransferExistsWithDifferentCreditAccountID},
			{"user_data_128 zero must equal the pending's", func(y *Transfer) { y.UserData128 = u(1) }, TransferExistsWithDifferentUserData128},
			{"user_data_64 differs", func(y *Transfer) { y.UserData64 = 6 }, TransferExistsWithDifferentUserData64},
			{"user_data_64 zero must equal the pending's", func(y *Transfer) { y.UserData64 = 0 }, TransferExistsWithDifferentUserData64},
			{"user_data_32", func(y *Transfer) { y.UserData32 = 1 }, TransferExistsWithDifferentUserData32},
			{"ledger", func(y *Transfer) { y.Ledger = 2 }, TransferExistsWithDifferentLedger},
			{"code", func(y *Transfer) { y.Code = 2 }, TransferExistsWithDifferentCode},
		}
		for _, c := range checks {
			if got := mod(c.f); got != c.want {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
			}
		}
	})
	t.Run("exists is judged before the pending transfer's state", func(t *testing.T) {
		l, clk := newTL(t)
		mustAccts(t, l, 1, 2)
		mustCreate(t, l, pend(10, u(100), 60))
		mustCreate(t, l, resolve(20, 10, post, u(100)))
		clk.advance(time.Hour)
		if got := create(t, l, resolve(20, 10, post, u(100))); got != TransferExists {
			t.Fatal(got)
		}
	})
}

func TestExpiry(t *testing.T) {
	l, clk := newTL(t)
	mustAccts(t, l, 1, 2)
	mustCreate(t, l, pend(10, u(100), 60))
	p, _, _ := l.LookupTransfer(context.Background(), u(10))
	clk.ns = int64(p.Timestamp) + 59_000_000_000
	wantBal(t, l, 1, u(100), u(0), u(0), u(0))
	clk.ns = int64(p.Timestamp) + 60_000_000_000
	wantBal(t, l, 1, u(0), u(0), u(0), u(0))
	wantBal(t, l, 2, u(0), u(0), u(0), u(0))
	if got := create(t, l, resolve(20, 10, TransferPostPendingTransfer, u(100))); got != TransferPendingTransferExpired {
		t.Fatal(got)
	}
	// Expiry is final once ledger time has passed it in a committed row: a clock stepped back
	// afterwards does not revive it.
	mustCreate(t, l, tr(30, 1, 2, u(1)))
	clk.ns = int64(p.Timestamp) + 30_000_000_000
	if got := create(t, l, resolve(21, 10, TransferPostPendingTransfer, u(100))); got != TransferPendingTransferExpired {
		t.Fatal(got)
	}
	wantBal(t, l, 1, u(0), u(1), u(0), u(0))
}

// The same pending transfer resolved by 32 goroutines with distinct ids.
func TestConcurrentResolution(t *testing.T) {
	l, _ := newTL(t)
	mustAccts(t, l, 1, 2)
	mustCreate(t, l, pend(10, u(100), 0))
	const n = 32
	results := make([]CreateTransferResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			flag, amount := TransferPostPendingTransfer, u(100)
			if i%2 == 1 {
				flag, amount = TransferVoidPendingTransfer, u(0)
			}
			<-start
			results[i], errs[i] = l.CreateTransfer(context.Background(), resolve(uint64(100+i), 10, flag, amount))
		}()
	}
	close(start)
	wg.Wait()
	created, winner := 0, -1
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		switch r {
		case TransferCreated:
			created++
			winner = i
		case TransferPendingTransferAlreadyPosted, TransferPendingTransferAlreadyVoided:
		default:
			t.Errorf("goroutine %d: %q", i, r)
		}
	}
	if created != 1 {
		t.Fatalf("%d created", created)
	}
	// The losers name what the winner did.
	wantLoser := TransferPendingTransferAlreadyPosted
	if winner%2 == 1 {
		wantLoser = TransferPendingTransferAlreadyVoided
	}
	for i, r := range results {
		if i != winner && r != wantLoser {
			t.Errorf("goroutine %d: %q, want %q", i, r, wantLoser)
		}
	}
	var rows int
	if err := l.db.QueryRow(`SELECT count(*) FROM transfers WHERE (flags & 12) != 0`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("resolution rows %d %v", rows, err)
	}
	if winner%2 == 0 {
		wantBal(t, l, 1, u(0), u(100), u(0), u(0))
	} else {
		wantBal(t, l, 1, u(0), u(0), u(0), u(0))
	}
}

// A restart (close and reopen) keeps the resolution: the second resolution is still refused.
func TestResolutionSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{ns: clockStart}
	path := t.TempDir() + "/l.db"
	l, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	mustAccts(t, l, 1, 2)
	mustCreate(t, l, pend(10, u(100), 0))
	mustCreate(t, l, resolve(20, 10, TransferPostPendingTransfer, u(100)))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := create(t, l, resolve(21, 10, TransferVoidPendingTransfer, u(0))); got != TransferPendingTransferAlreadyPosted {
		t.Fatal(got)
	}
	wantBal(t, l, 1, u(0), u(100), u(0), u(0))
}

// An expiry the ledger has decided is final (TigerBeetle commits an expired status), whatever the clock does afterwards, and
// no row is committed between the observation and the step back in these tests except the
// expiry record itself.
func TestExpiryIsFinal(t *testing.T) {
	post, void := TransferPostPendingTransfer, TransferVoidPendingTransfer
	count := func(t *testing.T, l *Ledger, q string) (n int) {
		t.Helper()
		if err := l.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	setup := func(t *testing.T) (*Ledger, *testClock, int64) {
		l, clk := newTL(t)
		mustAccts(t, l, 1, 2)
		mustCreate(t, l, pend(10, u(100), 60))
		p, _, _ := l.LookupTransfer(context.Background(), u(10))
		return l, clk, int64(p.Timestamp)
	}
	t.Run("a: observed by a void", func(t *testing.T) {
		l, clk, pts := setup(t)
		clk.ns = pts + 61_000_000_000
		if got := create(t, l, resolve(20, 10, void, u(0))); got != TransferPendingTransferExpired {
			t.Fatal(got)
		}
		clk.ns = pts + 30_000_000_000
		if got := create(t, l, resolve(21, 10, post, u(100))); got != TransferPendingTransferExpired {
			t.Fatalf("after the step back: %q", got)
		}
		wantBal(t, l, 1, u(0), u(0), u(0), u(0))
		wantBal(t, l, 2, u(0), u(0), u(0), u(0))
		if n := count(t, l, `SELECT count(*) FROM expired_transfers`); n != 1 {
			t.Fatalf("%d expiry records", n)
		}
		// Not transient: nothing in failed_transfers, and the retry says the same.
		if n := count(t, l, `SELECT count(*) FROM failed_transfers`); n != 0 {
			t.Fatalf("%d failed rows", n)
		}
		if got := create(t, l, resolve(20, 10, void, u(0))); got != TransferPendingTransferExpired {
			t.Fatalf("retry: %q", got)
		}
	})
	t.Run("b: observed by Balance", func(t *testing.T) {
		l, clk, pts := setup(t)
		clk.ns = pts + 61_000_000_000
		wantBal(t, l, 1, u(0), u(0), u(0), u(0))
		clk.ns = pts + 30_000_000_000
		if got := create(t, l, resolve(20, 10, post, u(100))); got != TransferPendingTransferExpired {
			t.Fatalf("after the step back: %q", got)
		}
		wantBal(t, l, 1, u(0), u(0), u(0), u(0))
		wantBal(t, l, 2, u(0), u(0), u(0), u(0))
		if n := count(t, l, `SELECT count(*) FROM expired_transfers`); n != 1 {
			t.Fatalf("%d expiry records", n)
		}
	})
	t.Run("c: a resolved pending is never recorded as expired", func(t *testing.T) {
		l, clk, pts := setup(t)
		mustCreate(t, l, resolve(20, 10, post, u(100)))
		clk.ns = pts + 120_000_000_000
		wantBal(t, l, 1, u(0), u(100), u(0), u(0))
		if n := count(t, l, `SELECT count(*) FROM expired_transfers`); n != 0 {
			t.Fatalf("%d expiry records", n)
		}
	})
	t.Run("a live pending is not recorded", func(t *testing.T) {
		l, clk, pts := setup(t)
		clk.ns = pts + 59_000_000_000
		wantBal(t, l, 1, u(100), u(0), u(0), u(0))
		if n := count(t, l, `SELECT count(*) FROM expired_transfers`); n != 0 {
			t.Fatalf("%d expiry records", n)
		}
	})
	t.Run("creating a transfer records the expiries it sees", func(t *testing.T) {
		l, clk, pts := setup(t)
		clk.ns = pts + 61_000_000_000
		mustCreate(t, l, tr(30, 1, 2, u(1)))
		clk.ns = pts + 10_000_000_000
		if got := create(t, l, resolve(31, 10, post, u(100))); got != TransferPendingTransferExpired {
			t.Fatal(got)
		}
	})
	t.Run("ledger time includes expiry records", func(t *testing.T) {
		l, clk, pts := setup(t)
		clk.ns = pts + 61_000_000_000
		wantBal(t, l, 1, u(0), u(0), u(0), u(0))
		var rec int64
		if err := l.db.QueryRow(`SELECT timestamp FROM expired_transfers`).Scan(&rec); err != nil {
			t.Fatal(err)
		}
		clk.ns = 5
		mustCreate(t, l, tr(30, 1, 2, u(1)))
		got, _, _ := l.LookupTransfer(context.Background(), u(30))
		if int64(got.Timestamp) <= rec {
			t.Fatalf("transfer timestamp %d is not after the expiry record %d", got.Timestamp, rec)
		}
	})
}

// A clock that advances on every read (one clock read per transaction): the expiry
// Balance reports is the expiry it records, even when the pending transfer expires between two
// reads of the clock.
func TestExpiryOneClockReadPerTransaction(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	ns := int64(clockStart)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); ns += 2; return time.Unix(0, ns) }
	set := func(v int64) { mu.Lock(); ns = v; mu.Unlock() }
	// The first read of the transaction lands just before the expiry (offsets in ns), so a second read would be after it.
	for _, start := range []int64{-12, -7, -6, -5, -4, -3, -2, -1, 0, 1} {
		l, err := Open(ctx, t.TempDir()+"/l.db", WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		set(clockStart)
		mustAccts(t, l, 1, 2)
		mustCreate(t, l, pend(10, u(100), 60))
		p, _, _ := l.LookupTransfer(ctx, u(10))
		set(int64(p.Timestamp) + 60_000_000_000 + start)
		b := bal(t, l, 1)
		var n int
		if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if b.DebitsPending.IsZero() != (n == 1) {
			t.Fatalf("start %d: Balance pending %v but %d expiry records", start, b.DebitsPending, n)
		}
		set(int64(p.Timestamp) + 30_000_000_000)
		got := create(t, l, resolve(20, 10, TransferPostPendingTransfer, u(100)))
		want := TransferCreated
		if n == 1 {
			want = TransferPendingTransferExpired
		}
		if got != want {
			t.Fatalf("start %d: post after step back %q, want %q", start, got, want)
		}
	}
}

// A database at schema version 2 (before expired_transfers) upgrades to 3 with its data, the new
// guards are in place, and an expiry is then final.
func TestUpgradeV2ToV3(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/l.db"
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`PRAGMA journal_mode=wal`, `CREATE TABLE schema_version (version INTEGER NOT NULL) STRICT`,
		migrations[0], migrations[1], `INSERT INTO schema_version (version) VALUES (2)`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Rows written by the v2 schema: two accounts and a pending transfer with a timeout.
	z8, z16 := make([]byte, 8), make([]byte, 16)
	for i := range 2 {
		id := u(uint64(i + 1)).Bytes()
		if _, err := raw.Exec(`INSERT INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, 0, 1, 1, 0, ?)`, id[:], z16, z8, clockStart+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	pidA, amtA, d1, c2 := u(10).Bytes(), u(100).Bytes(), u(1).Bytes(), u(2).Bytes()
	pid, amt := pidA[:], amtA[:]
	pts := int64(clockStart + 10)
	if _, err := raw.Exec(`INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id, user_data_128,
		user_data_64, user_data_32, timeout, ledger, code, flags, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 60, 1, 1, 2, ?)`,
		pid, d1[:], c2[:], amt, z16, z16, z8, pts); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	clk := &testClock{ns: pts + 1}
	l, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var v int
	if err := l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 4 || len(migrations) != 4 {
		t.Fatalf("version %d err %v", v, err)
	}
	var n int
	if err := l.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND (name LIKE 'expired\_transfers\_%' ESCAPE '\' OR name = 'transfers_not_after_expiry')`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("%d new triggers, err %v", n, err)
	}
	wantBal(t, l, 1, u(100), u(0), u(0), u(0))
	clk.ns = pts + 61_000_000_000
	wantBal(t, l, 1, u(0), u(0), u(0), u(0))
	if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d expiry records, err %v", n, err)
	}
	clk.ns = pts + 30_000_000_000
	if got := create(t, l, resolve(20, 10, TransferPostPendingTransfer, u(100))); got != TransferPendingTransferExpired {
		t.Fatal(got)
	}
	_, err = l.db.Exec(`UPDATE expired_transfers SET timestamp = 1`)
	wantErr(t, err, "immutable", "update expired_transfers")
}

// The guards of expired_transfers hold on a raw connection, like those of the other tables.
func TestExpiredTransfersGuards(t *testing.T) {
	l, path := freshLedger(t)
	seedRows(t, l)
	z := make([]byte, 16)
	// 0x50: pending with a timeout; 0x58: pending without; 0x51 resolves 0x59; 0x59 pending with timeout.
	const pendSQL = `INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id, user_data_128,
		user_data_64, user_data_32, timeout, ledger, code, flags, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, 1, 1, ?, ?)`
	for _, r := range []struct {
		id      byte
		timeout int
		flags   int
		pending []byte
		ts      int
	}{{0x50, 60, 2, z, 10}, {0x58, 0, 2, z, 11}, {0x59, 60, 2, z, 12}, {0x51, 0, 4, id16(0x59), 13}, {0x5A, 60, 2, z, 14}} {
		if _, err := l.db.Exec(pendSQL, id16(r.id), id16(1), id16(2), z, r.pending, id16(0), make([]byte, 8), r.timeout, r.flags, r.ts); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := exec0(conn, `PRAGMA foreign_keys=off`); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) error { _, err := conn.ExecContext(ctx, q, args...); return err }
	const ins = `INSERT INTO expired_transfers (pending_id, timestamp) VALUES (?, ?)`
	if err := exec(ins, id16(0x50), 100); err != nil {
		t.Fatal(err)
	}
	wantErr(t, exec(ins, id16(0x50), 101), "immutable", "second record for one pending")
	wantErr(t, exec(`INSERT OR REPLACE INTO expired_transfers (pending_id, timestamp) VALUES (?, ?)`, id16(0x50), 102), "immutable", "replace by pending_id")
	wantErr(t, exec(`INSERT OR REPLACE INTO expired_transfers (pending_id, timestamp) VALUES (?, ?)`, id16(0x5A), 100), "immutable", "replace by timestamp")
	wantErr(t, exec(ins, id16(0x58), 103), "not an unresolved pending", "pending without timeout")
	wantErr(t, exec(ins, id16(0x03), 104), "not an unresolved pending", "not a pending transfer")
	wantErr(t, exec(ins, id16(0x77), 105), "not an unresolved pending", "unknown transfer")
	wantErr(t, exec(ins, id16(0x59), 106), "not an unresolved pending", "already resolved")
	wantErr(t, exec(`UPDATE expired_transfers SET timestamp = 5`), "immutable", "update")
	wantErr(t, exec(`DELETE FROM expired_transfers`), "immutable", "delete")
	wantErr(t, exec(`REPLACE INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id, user_data_128,
		user_data_64, user_data_32, timeout, ledger, code, flags, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 1, 4, 200)`,
		id16(0x60), id16(1), id16(2), z, id16(0x50), id16(0), make([]byte, 8)), "pending transfer expired", "resolve an expired pending")
}

// Two handles on one file (two Opens, as two processes would be): 16 goroutines race to post one
// pending transfer; exactly one wins.
func TestConcurrentResolutionTwoHandles(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{ns: clockStart}
	path := t.TempDir() + "/l.db"
	var ls [2]*Ledger
	for i := range ls {
		l, err := Open(ctx, path, WithClock(clk.now))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		ls[i] = l
	}
	mustAccts(t, ls[0], 1, 2)
	mustCreate(t, ls[0], pend(10, u(100), 0))
	const n = 16
	results := make([]CreateTransferResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = ls[i%2].CreateTransfer(ctx, resolve(uint64(100+i), 10, TransferPostPendingTransfer, u(100)))
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		switch r {
		case TransferCreated:
			created++
		case TransferPendingTransferAlreadyPosted:
		default:
			t.Errorf("goroutine %d: %q", i, r)
		}
	}
	if created != 1 {
		t.Fatalf("%d created", created)
	}
	wantBal(t, ls[1], 1, u(0), u(100), u(0), u(0))
}

// Seeded property: random pending (some with timeouts), single, post, void, replay (sometimes with
// one field changed) and Balance operations, with a clock that steps forward and back, give the
// results and balances of an independent model. A recorded expiry is final in the model.
func TestTwoPhaseProperty(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { twoPhaseProperty(t, seed) })
	}
}

type mPend struct {
	id             uint64
	amount         *big.Int
	timeout        int64 // seconds
	d, c           uint64
	created        int64 // the pending transfer's real timestamp
	posted, voided bool
	actual         *big.Int // posted amount
	final          bool     // expiry recorded
}

type mReq struct {
	t      Transfer
	actual *big.Int
}

type pModel struct {
	pends       map[uint64]*mPend
	pendIDs     []uint64 // creation order
	singles     map[uint64]bool
	singleAccts [][2]uint64
	reqs        map[uint64]mReq
	reqIDs      []uint64
	failed      map[uint64]bool
	failIDs     []uint64
	L           int64 // the ledger's last committed timestamp, read from the database by syncL
}

// syncL reads the ledger's real last timestamp: ledger time is exact in the model.
func (m *pModel) syncL(t *testing.T, l *Ledger) {
	t.Helper()
	last, err := lastTimestamp(context.Background(), l.db)
	if err != nil {
		t.Fatal(err)
	}
	m.L = last
}

func (m *pModel) due(p *mPend, lt int64) bool {
	return !p.posted && !p.voided && p.timeout != 0 && lt-p.created >= p.timeout*1_000_000_000
}

// record marks the due pendings of the accounts final; reports whether any was.
func (m *pModel) record(lt int64, accounts ...uint64) bool {
	any := false
	for _, id := range m.pendIDs {
		p := m.pends[id]
		if p.final || !m.due(p, lt) {
			continue
		}
		for _, a := range accounts {
			if a == p.d || a == p.c {
				p.final, any = true, true
				break
			}
		}
	}
	return any
}

func twoPhaseProperty(t *testing.T, seed int64) {
	l, clk := newTL(t)
	mustAccts(t, l, 1, 2, 3)
	rng := rand.New(rand.NewSource(seed))
	m := &pModel{pends: map[uint64]*mPend{}, singles: map[uint64]bool{}, reqs: map[uint64]mReq{}, failed: map[uint64]bool{}, L: clockStart}
	edge := []U128{u(0), u(1), u(5), u(100), u(101), u(1<<63 - 1), p2(63), u(^uint64(0)), p2(64), mustSub(U128Max, u(1)), U128Max}
	amount := func() U128 {
		if rng.Intn(2) == 0 {
			return edge[rng.Intn(len(edge))]
		}
		return u(uint64(rng.Intn(150)))
	}
	nextID := uint64(1000)
	for step := 0; step < 800; step++ {
		// Step the clock: mostly forward, sometimes back, by whole seconds plus a random part.
		frac := time.Duration(rng.Intn(1000)) * time.Millisecond
		switch r := rng.Intn(10); {
		case r < 6:
			clk.advance(time.Duration(rng.Intn(3))*time.Second + frac)
		case r < 8:
			clk.advance(-time.Duration(rng.Intn(4))*time.Second - frac)
		}
		// Note: the three `<=` expiry sites (Balance's accountBalance, recordDueExpiries and the
		// post/void path) back each other up, so changing `<=` to `<` fails this property test
		// only when all of them are changed; the table tests catch each site on its own.
		// Sometimes put the clock exactly on the expiry of a random unresolved pending transfer, or
		// one nanosecond either side: the expiry boundary (timestamp + timeout*1e9 <= ledger time).
		if rng.Intn(12) == 0 {
			var cand []*mPend
			for _, id := range m.pendIDs {
				if p := m.pends[id]; !p.posted && !p.voided && !p.final && p.timeout != 0 {
					cand = append(cand, p)
				}
			}
			if len(cand) > 0 {
				p := cand[rng.Intn(len(cand))]
				clk.mu.Lock()
				clk.ns = p.created + p.timeout*1_000_000_000 + int64(rng.Intn(3)) - 1
				clk.mu.Unlock()
			}
		}
		m.syncL(t, l)
		now := clk.ns
		lt := max(now, m.L)
		nextID++
		switch k := rng.Intn(12); {
		case k < 3, k == 3: // a pending transfer (k<3) or a single-phase transfer (k==3)
			d, c := uint64(rng.Intn(3)+1), uint64(rng.Intn(3)+1)
			if d == c {
				continue
			}
			m.record(lt, d, c)
			if k == 3 {
				mustCreate(t, l, tr(nextID, d, c, u(1)))
				m.singles[nextID] = true
				m.singleAccts = append(m.singleAccts, [2]uint64{d, c})
				continue
			}
			a := uint64(rng.Intn(100))
			to := uint32(0)
			if rng.Intn(5) > 1 {
				to = uint32(rng.Intn(5) + 1)
			}
			x := tr(nextID, d, c, u(a))
			x.Flags, x.Timeout = TransferPending, to
			mustCreate(t, l, x)
			// The real timestamp: ledger time, plus a few nanoseconds when the clock is behind.
			pt, _, _ := l.LookupTransfer(context.Background(), x.ID)
			m.pends[nextID] = &mPend{id: nextID, amount: new(big.Int).SetUint64(a), timeout: int64(to), d: d, c: c, created: int64(pt.Timestamp)}
			m.pendIDs = append(m.pendIDs, nextID)
		case k == 4: // Balance of a random account
			a := uint64(rng.Intn(3) + 1)
			m.record(lt, a)
			checkProp(t, l, m, a)
		default:
			x := Transfer{ID: u(nextID), Flags: TransferPostPendingTransfer, Amount: amount()}
			if rng.Intn(2) == 0 {
				x.Flags = TransferVoidPendingTransfer
			}
			if rng.Intn(10) == 0 {
				x.Flags |= TransferPostPendingTransfer | TransferVoidPendingTransfer
			}
			switch r := rng.Intn(14); {
			case r == 0:
				x.PendingID = U128Max
			case r == 1:
				x.PendingID = u(77777)
			case r == 2 && len(m.singles) > 0:
				ids := make([]uint64, 0, len(m.singles))
				for id := range m.singles {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				x.PendingID = u(ids[rng.Intn(len(ids))])
			case len(m.pendIDs) > 0:
				x.PendingID = u(m.pendIDs[rng.Intn(len(m.pendIDs))])
			}
			if p := m.pends[x.PendingID.Lo]; p != nil && x.PendingID.Hi == 0 {
				switch rng.Intn(10) {
				case 0:
					x.DebitAccountID = u(p.d%3 + 1)
				case 1:
					x.CreditAccountID = u(p.c%3 + 1)
				case 2:
					x.Ledger = 2
				case 3:
					x.Code = 2
				case 4:
					x.DebitAccountID, x.CreditAccountID, x.Ledger, x.Code = u(p.d), u(p.c), 1, 1
				case 5:
					x.UserData64 = uint64(rng.Intn(2)) * 9
				}
			}
			if rng.Intn(15) == 0 {
				x.Timeout = 1
			}
			// Replay an earlier request (stored or failed), half of them with one field changed.
			if rng.Intn(8) == 0 && len(m.reqIDs)+len(m.failIDs) > 0 {
				if len(m.failIDs) > 0 && (len(m.reqIDs) == 0 || rng.Intn(4) == 0) {
					x.ID = u(m.failIDs[rng.Intn(len(m.failIDs))])
				} else {
					r := m.reqs[m.reqIDs[rng.Intn(len(m.reqIDs))]]
					x = r.t
					if rng.Intn(6) < 3 {
						switch rng.Intn(9) {
						case 0:
							x.Flags ^= TransferPostPendingTransfer | TransferVoidPendingTransfer
						case 1:
							x.PendingID = u(m.pendIDs[rng.Intn(len(m.pendIDs))])
						case 2:
							x.Timeout = 3
						case 3:
							x.DebitAccountID = u(1 + rng.Uint64()%3)
						case 4:
							x.CreditAccountID = u(1 + rng.Uint64()%3)
						case 5:
							x.Amount = amount()
						case 6:
							x.UserData128 = u(uint64(rng.Intn(2)))
						case 7:
							x.UserData32 = uint32(rng.Intn(2))
						case 8:
							x.Ledger, x.Code = uint32(rng.Intn(3)), uint16(rng.Intn(3))
						}
					}
				}
			}
			want, actual, _ := m.resolve(x, max(now, m.L+1)) // the timestamp a write would get
			got, err := l.CreateTransfer(context.Background(), x)
			if err != nil || got != want {
				t.Fatalf("step %d: %+v: got %q %v, want %q", step, x, got, err, want)
			}
			if got == TransferCreated {
				m.reqs[x.ID.Lo] = mReq{t: x, actual: actual}
				m.reqIDs = append(m.reqIDs, x.ID.Lo)
				p := m.pends[x.PendingID.Lo]
				if x.Flags&TransferPostPendingTransfer != 0 {
					p.posted, p.actual = true, actual
				} else {
					p.voided = true
				}
			}
			if got == TransferPendingTransferNotFound {
				m.failed[x.ID.Lo] = true
				m.failIDs = append(m.failIDs, x.ID.Lo)
			}
		}
	}
	// Final check at the current clock and after a large step forward.
	for a := uint64(1); a <= 3; a++ {
		m.syncL(t, l)
		lt := max(clk.ns, m.L)
		m.record(lt, a)
		checkProp(t, l, m, a)
	}
	clk.advance(time.Minute)
	for a := uint64(1); a <= 3; a++ {
		m.syncL(t, l)
		lt := max(clk.ns, m.L)
		m.record(lt, a)
		checkProp(t, l, m, a)
	}
}

// checkProp compares Balance with the model's sums (the model has already recorded what Balance
// records).
func checkProp(t *testing.T, l *Ledger, m *pModel, a uint64) {
	t.Helper()
	dp, dq, cp, cq := new(big.Int), new(big.Int), new(big.Int), new(big.Int)
	for _, id := range m.pendIDs {
		p := m.pends[id]
		switch {
		case p.posted:
			if p.d == a {
				dq.Add(dq, p.actual)
			}
			if p.c == a {
				cq.Add(cq, p.actual)
			}
		case p.voided || p.final:
		default:
			if p.d == a {
				dp.Add(dp, p.amount)
			}
			if p.c == a {
				cp.Add(cp, p.amount)
			}
		}
	}
	for _, ac := range m.singleAccts {
		if ac[0] == a {
			dq.Add(dq, big.NewInt(1))
		}
		if ac[1] == a {
			cq.Add(cq, big.NewInt(1))
		}
	}
	b := bal(t, l, a)
	if toBig(b.DebitsPending).Cmp(dp) != 0 || toBig(b.DebitsPosted).Cmp(dq) != 0 ||
		toBig(b.CreditsPending).Cmp(cp) != 0 || toBig(b.CreditsPosted).Cmp(cq) != 0 {
		for _, id := range m.pendIDs {
			t.Logf("%+v", *m.pends[id])
		}
		t.Fatalf("account %d: %+v vs model dp=%v dq=%v cp=%v cq=%v", a, b, dp, dq, cp, cq)
	}
}

// resolve is the model of a post or void request at ledger time lt, written again from the
// source's check order. It returns the result, the amount stored when created, and whether the
// ledger committed a row (a resolution or an expiry record).
func (m *pModel) resolve(x Transfer, lt int64) (CreateTransferResult, *big.Int, bool) {
	post, void := x.Flags&TransferPostPendingTransfer != 0, x.Flags&TransferVoidPendingTransfer != 0
	if r, ok := m.reqs[x.ID.Lo]; ok && x.ID.Hi == 0 { // exists
		e := r.t
		p := m.pends[e.PendingID.Lo]
		switch {
		case e.Flags != x.Flags:
			return TransferExistsWithDifferentFlags, nil, false
		case e.PendingID != x.PendingID:
			return TransferExistsWithDifferentPendingID, nil, false
		case e.Timeout != x.Timeout:
			return TransferExistsWithDifferentTimeout, nil, false
		case !x.DebitAccountID.IsZero() && x.DebitAccountID != u(p.d):
			return TransferExistsWithDifferentDebitAccountID, nil, false
		case !x.CreditAccountID.IsZero() && x.CreditAccountID != u(p.c):
			return TransferExistsWithDifferentCreditAccountID, nil, false
		}
		want := toBig(x.Amount)
		if (post && x.Amount.IsMax()) || (void && x.Amount.IsZero()) {
			want = p.amount
		}
		switch {
		case want.Cmp(r.actual) != 0:
			return TransferExistsWithDifferentAmount, nil, false
		case x.UserData128 != e.UserData128:
			return TransferExistsWithDifferentUserData128, nil, false
		case x.UserData64 != e.UserData64:
			return TransferExistsWithDifferentUserData64, nil, false
		case x.UserData32 != e.UserData32:
			return TransferExistsWithDifferentUserData32, nil, false
		case x.Ledger != 0 && x.Ledger != 1:
			return TransferExistsWithDifferentLedger, nil, false
		case x.Code != 0 && x.Code != 1:
			return TransferExistsWithDifferentCode, nil, false
		}
		return TransferExists, nil, false
	}
	if m.failed[x.ID.Lo] && x.ID.Hi == 0 {
		return TransferIDAlreadyFailed, nil, false
	}
	switch {
	case post && void, x.Flags&TransferPending != 0:
		return TransferFlagsAreMutuallyExclusive, nil, false
	case x.PendingID.IsZero():
		return TransferPendingIDMustNotBeZero, nil, false
	case x.PendingID.IsMax():
		return TransferPendingIDMustNotBeIntMax, nil, false
	case x.PendingID == x.ID:
		return TransferPendingIDMustBeDifferent, nil, false
	case x.Timeout != 0:
		return TransferTimeoutReservedForPendingTransfer, nil, false
	}
	p := m.pends[x.PendingID.Lo]
	if x.PendingID.Hi != 0 || (p == nil && !m.singles[x.PendingID.Lo]) {
		return TransferPendingTransferNotFound, nil, false
	}
	switch {
	case p == nil:
		return TransferPendingTransferNotPending, nil, false
	case !x.DebitAccountID.IsZero() && x.DebitAccountID != u(p.d):
		return TransferPendingTransferHasDifferentDebitAccountID, nil, false
	case !x.CreditAccountID.IsZero() && x.CreditAccountID != u(p.c):
		return TransferPendingTransferHasDifferentCreditAccountID, nil, false
	case x.Ledger != 0 && x.Ledger != 1:
		return TransferPendingTransferHasDifferentLedger, nil, false
	case x.Code != 0 && x.Code != 1:
		return TransferPendingTransferHasDifferentCode, nil, false
	}
	actual := toBig(x.Amount)
	if (void && x.Amount.IsZero()) || (post && x.Amount.IsMax()) {
		actual = p.amount
	}
	switch {
	case actual.Cmp(p.amount) > 0:
		return TransferExceedsPendingTransferAmount, nil, false
	case void && actual.Cmp(p.amount) < 0:
		return TransferPendingTransferHasDifferentAmount, nil, false
	case p.posted:
		return TransferPendingTransferAlreadyPosted, nil, false
	case p.voided:
		return TransferPendingTransferAlreadyVoided, nil, false
	case p.final:
		return TransferPendingTransferExpired, nil, false
	case m.due(p, lt):
		p.final = true
		return TransferPendingTransferExpired, nil, true
	}
	return TransferCreated, actual, true
}

// CreateTransfer reads the clock once per transaction. The clock here ticks 2 ns on every read,
// and the first read lands just before, on, or just after the expiry of a pending transfer that
// holds all of a limited account's credits. A second read would put the expiry between the two:
// the expiry would be judged at one instant (and recorded or not) and the balance check at
// another, so the new transfer's outcome and the expiry record would disagree. With one read
// they agree for every offset: the transfer succeeds exactly when the pending amount was judged
// expired, and then (and only then) the expiry is recorded and final.
func TestCreateTransferOneClockRead(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	ns := int64(clockStart)
	reads := 0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); ns += 2; reads++; return time.Unix(0, ns) }
	set := func(v int64) { mu.Lock(); ns = v; mu.Unlock() }
	for start := int64(-12); start <= 4; start++ {
		l, err := Open(ctx, t.TempDir()+"/l.db", WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		set(clockStart)
		mustAcct(t, l, 1, 1, 0)
		mustAcct(t, l, 2, 1, AccountDebitsMustNotExceedCredits)
		mustCreate(t, l, tr(5, 1, 2, u(100))) // account 2 now holds 100 credits
		p := pend(10, u(100), 60)             // pending debit of all of them
		p.DebitAccountID, p.CreditAccountID = u(2), u(1)
		mustCreate(t, l, p)
		pt, _, _ := l.LookupTransfer(ctx, u(10))
		set(int64(pt.Timestamp) + 60_000_000_000 + start)
		mu.Lock()
		reads = 0
		mu.Unlock()
		x := tr(20, 2, 1, u(1))
		got := create(t, l, x)
		mu.Lock()
		r := reads
		mu.Unlock()
		if r != 1 {
			t.Fatalf("start %d: CreateTransfer read the clock %d times, want 1", start, r)
		}
		var n int
		if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (got == TransferCreated) != (n == 1) {
			t.Fatalf("start %d: result %q but %d expiry records", start, got, n)
		}
		set(int64(pt.Timestamp) + 30_000_000_000)
		post := create(t, l, resolve(30, 10, TransferPostPendingTransfer, u(100)))
		if (post == TransferPendingTransferExpired) != (n == 1) {
			t.Fatalf("start %d: post after step back %q with %d expiry records", start, post, n)
		}
	}
}
