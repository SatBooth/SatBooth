package ledger

import (
	"context"
	"testing"
)

// fuzzSeed is one fuzz input. PendingID is used as given: a seed names the pending transfer it
// resolves, and the fuzzer explores every other value.
type fuzzSeed struct {
	idHi, idLo, dHi, dLo, cLo, amtHi, amtLo uint64
	ledger, timeout                         uint32
	code, flags                             uint16
	ts, pidHi, pidLo                        uint64
}

// fuzzAccts: 1 and 2 are unlimited, 3 may not debit more than it has been credited, 4 may not
// credit more than it has debited (the fuzz transfers use accounts 0..4).
var fuzzAccts = []pAcct{{1, 1, 0}, {2, 1, 0}, {3, 1, AccountDebitsMustNotExceedCredits}, {4, 1, AccountCreditsMustNotExceedDebits}}

// fuzzLedger returns a small funded ledger: the four accounts; 3 and 4 funded with 1000; pending
// transfers 1001, 1002 (1 -> 2, 50, no timeout), 1003 (same, 1 s timeout, which the fuzz clock
// never reaches), and, on the limited accounts, 1101 (3 -> 1, 600), 1102 (1 -> 4, 600),
// 1103 (3 -> 1, 100) and 1104 (1 -> 4, 100), none expiring.
func fuzzLedger(tb testing.TB, dir string) *Ledger {
	tb.Helper()
	ctx := context.Background()
	l, err := Open(ctx, dir+"/l.db", WithClock((&testClock{ns: clockStart}).now))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { l.Close() })
	for _, a := range fuzzAccts {
		if r, err := l.CreateAccount(ctx, Account{ID: u(a.id), Ledger: a.ledger, Code: 1, Flags: a.flags}); err != nil || r != AccountCreated {
			tb.Fatal(r, err)
		}
	}
	pend := func(id, d, c, amount uint64, timeout uint32) Transfer {
		x := tr(id, d, c, u(amount))
		x.Flags, x.Timeout = TransferPending, timeout
		return x
	}
	for _, x := range []Transfer{
		tr(901, 1, 3, u(1000)), tr(902, 4, 2, u(1000)), // fund 3 with credits and 4 with debits
		pend(1001, 1, 2, 50, 0), pend(1002, 1, 2, 50, 0), pend(1003, 1, 2, 50, 1),
		pend(1101, 3, 1, 600, 0), pend(1102, 1, 4, 600, 0), pend(1103, 3, 1, 100, 0), pend(1104, 1, 4, 100, 0),
	} {
		if r, err := l.CreateTransfer(ctx, x); err != nil || r != TransferCreated {
			tb.Fatal(x.ID, r, err)
		}
	}
	return l
}

// fuzzSeeds lists the seeds in the order they run (they share one ledger, so a resolution seed
// finds its pending transfer unresolved only until an earlier seed resolves it; an already
// resolved one still reaches the post/void path).
func fuzzSeeds() []fuzzSeed {
	post, void, pending := uint16(TransferPostPendingTransfer), uint16(TransferVoidPendingTransfer), uint16(TransferPending)
	seeds := []fuzzSeed{
		{idLo: 1, dLo: 2, amtHi: 0, amtLo: 5, ledger: 1, code: 1},
		{idHi: ^uint64(0), idLo: ^uint64(0), dHi: ^uint64(0), dLo: ^uint64(0), cLo: 1, amtHi: ^uint64(0), amtLo: ^uint64(0), ledger: 1, timeout: 9, code: 1, flags: 2},
		// Resolutions of the unlimited pending transfers.
		{idLo: 2000, amtLo: 50, flags: post, pidLo: 1001},
		{idLo: 2001, dLo: 1, cLo: 2, amtHi: ^uint64(0), amtLo: ^uint64(0), ledger: 1, code: 1, flags: post, pidLo: 1002},
		{idLo: 2002, flags: void, pidLo: 1002},
		{idLo: 2003, amtLo: 7, flags: void, pidLo: 1003},
		{idLo: 2004, amtLo: 7, timeout: 3, code: 2, flags: post | void | pending, pidLo: 2004},
		{idLo: 2005, amtLo: 7, flags: post, ts: 1},
	}
	// Amounts against the limited accounts: 3 debiting (exceeds_credits) and 4
	// crediting (exceeds_debits), single-phase and pending; then a post of the 600 pending
	// transfers on them with each edge amount (exceeds_pending_transfer_amount).
	edges := []U128{u(1<<63 - 1), p2(63), u(^uint64(0)), p2(64)}
	for i, amt := range edges {
		id := uint64(3000 + 10*i)
		seeds = append(seeds,
			fuzzSeed{idLo: id, dLo: 3, cLo: 1, amtHi: amt.Hi, amtLo: amt.Lo, ledger: 1, code: 1},
			fuzzSeed{idLo: id + 1, dLo: 1, cLo: 4, amtHi: amt.Hi, amtLo: amt.Lo, ledger: 1, code: 1},
			fuzzSeed{idLo: id + 2, dLo: 3, cLo: 1, amtHi: amt.Hi, amtLo: amt.Lo, ledger: 1, timeout: 30, code: 1, flags: pending},
			fuzzSeed{idLo: id + 3, dLo: 1, cLo: 4, amtHi: amt.Hi, amtLo: amt.Lo, ledger: 1, timeout: 30, code: 1, flags: pending},
			fuzzSeed{idLo: id + 4, amtHi: amt.Hi, amtLo: amt.Lo, flags: post, pidLo: 1101},
			fuzzSeed{idLo: id + 5, amtHi: amt.Hi, amtLo: amt.Lo, flags: post, pidLo: 1102},
		)
	}
	return append(seeds,
		fuzzSeed{idLo: 3100, amtLo: 0, flags: void, pidLo: 1101},                             // void, 0 = full amount
		fuzzSeed{idLo: 3101, amtLo: 600, flags: post, pidLo: 1102},                           // exact
		fuzzSeed{idLo: 3102, amtHi: ^uint64(0), amtLo: ^uint64(0), flags: post, pidLo: 1103}, // full post
		fuzzSeed{idLo: 3103, amtLo: 0, flags: post, pidLo: 1104},                             // post of zero
		fuzzSeed{idLo: 3104, amtLo: 600, flags: void, pidLo: 1101},                           // already voided
	)
}

// fuzzOne runs one input against the ledger, checks everything that must hold whatever the input
// is, and returns the result.
func fuzzOne(t testing.TB, l *Ledger, names map[CreateTransferResult]string, s fuzzSeed) CreateTransferResult {
	t.Helper()
	ctx := context.Background()
	x := Transfer{
		ID: U128{s.idHi, s.idLo}, DebitAccountID: U128{s.dHi, s.dLo % 5}, CreditAccountID: U128{0, s.cLo % 5},
		Amount: U128{s.amtHi, s.amtLo}, Ledger: s.ledger % 3, Code: s.code % 3, Timeout: s.timeout,
		Flags: TransferFlags(s.flags), Timestamp: s.ts % 2, PendingID: U128{s.pidHi, s.pidLo},
	}
	r, err := l.CreateTransfer(ctx, x)
	if err != nil {
		t.Fatalf("%+v: %v", x, err)
	}
	if _, ok := names[r]; !ok {
		t.Fatalf("%+v: unknown result %q", x, r)
	}
	got, found, err := l.LookupTransfer(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r == TransferCreated {
		x.Timestamp = got.Timestamp
		if !found {
			t.Fatalf("created but not found: %+v", x)
		}
		if x.Flags&(TransferPostPendingTransfer|TransferVoidPendingTransfer) != 0 {
			// A resolution stores the pending transfer's accounts, ledger and code, and
			// the posted amount: check the fields that are the request's own.
			if got.ID != x.ID || got.PendingID != x.PendingID || got.Flags != x.Flags || got.Timeout != 0 ||
				got.Ledger == 0 || got.Code == 0 || got.DebitAccountID.IsZero() || got.CreditAccountID.IsZero() {
				t.Fatalf("resolution %+v stored as %+v", x, got)
			}
		} else if got != x {
			t.Fatalf("created but lookup says %+v", got)
		}
	}
	if r == TransferIDAlreadyFailed && found {
		t.Fatalf("failed id %v is stored", x.ID)
	}
	// The books balance, nothing is resolved twice and no limit is broken, whatever came in.
	bals := make([]Balance, len(fuzzAccts))
	for i, a := range fuzzAccts {
		if bals[i], err = l.Balance(ctx, u(a.id)); err != nil {
			t.Fatal(err)
		}
	}
	checkBooks(t, fuzzAccts, bals)
	checkResolutions(t, l)
	return r
}

// FuzzCreateTransfer feeds arbitrary field values: the result is always one of TigerBeetle's
// codes for our subset, never an error, and a created transfer is found and a refused one is not.
func FuzzCreateTransfer(f *testing.F) {
	l := fuzzLedger(f, f.TempDir())
	names := transferResultNames(f)
	for _, s := range fuzzSeeds() {
		f.Add(s.idHi, s.idLo, s.dHi, s.dLo, s.cLo, s.amtHi, s.amtLo, s.ledger, s.timeout, s.code, s.flags, s.ts, s.pidHi, s.pidLo)
	}
	f.Fuzz(func(t *testing.T, idHi, idLo, dHi, dLo, cLo, amtHi, amtLo uint64, ledger, timeout uint32, code, flags uint16, ts, pidHi, pidLo uint64) {
		fuzzOne(t, l, names, fuzzSeed{idHi, idLo, dHi, dLo, cLo, amtHi, amtLo, ledger, timeout, code, flags, ts, pidHi, pidLo})
	})
}

// TestFuzzSeedsReach runs every seed once, in order, and checks that each post/void seed gets
// past the pending-id checks to a real pending transfer, and that the edge amounts are refused by
// the limit they target, so a mistyped seed cannot pass silently.
func TestFuzzSeedsReach(t *testing.T) {
	l := fuzzLedger(t, t.TempDir())
	names := transferResultNames(t)
	seen := map[CreateTransferResult]int{}
	for i, s := range fuzzSeeds() {
		r := fuzzOne(t, l, names, s)
		seen[r]++
		resolves := s.flags&(uint16(TransferPostPendingTransfer)|uint16(TransferVoidPendingTransfer)) != 0
		if resolves && s.flags&uint16(TransferPending) == 0 && s.idLo != 2005 &&
			(r == TransferPendingIDMustNotBeZero || r == TransferPendingTransferNotFound) {
			t.Errorf("seed %d (%+v) does not reach a pending transfer: %q", i, s, r)
		}
		if s.idLo >= 3000 && s.idLo < 3100 {
			var want CreateTransferResult
			switch {
			case s.flags == uint16(TransferPostPendingTransfer):
				want = TransferExceedsPendingTransferAmount
			case s.dLo == 3 && s.flags == 0:
				want = TransferExceedsCredits
			case s.cLo == 4 && s.flags == 0:
				want = TransferExceedsDebits
			}
			if want != "" && r != want {
				t.Errorf("seed %d (%+v): %q, want %q", i, s, r, want)
			}
		}
	}
	for _, want := range []CreateTransferResult{TransferCreated, TransferExceedsPendingTransferAmount, TransferExceedsCredits, TransferExceedsDebits} {
		if seen[want] == 0 {
			t.Errorf("no seed gives %q: %v", want, seen)
		}
	}
}
