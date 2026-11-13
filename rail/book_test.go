package rail

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/satbooth/satbooth/ledger"
)

var testBook = Book{Ledger: 9, Wallet: ledger.U128From64(0x91), Merchant: ledger.U128From64(0x92)}

// fakeLedger is a map of transfers by id; failOn, when set, fails the lookup of that id.
type fakeLedger struct {
	m      map[ledger.U128]ledger.Transfer
	failOn ledger.U128
	reads  int
}

func (f *fakeLedger) LookupTransfer(_ context.Context, id ledger.U128) (ledger.Transfer, bool, error) {
	f.reads++
	if !f.failOn.IsZero() && id == f.failOn {
		return ledger.Transfer{}, false, errFake
	}
	t, ok := f.m[id]
	return t, ok, nil
}

func fwdID(ep uint32) ledger.U128                  { return ledger.U128From64(0x1000 + uint64(ep)) }
func revID(ep uint32) ledger.U128                  { return ledger.U128From64(0x2000 + uint64(ep)) }
func walkIDs(ep uint32) (ledger.U128, ledger.U128) { return fwdID(ep), revID(ep) }

var walkUD = ledger.U128From64(0xabcd)

func TestBookTransfersPassTheirChecks(t *testing.T) {
	for _, counted := range []bool{true, false} {
		f := testBook.Forward(fwdID(0), walkUD, 700, counted)
		if !testBook.ForwardOK(f, walkUD, 700, counted) || testBook.ForwardOK(f, walkUD, 700, !counted) {
			t.Errorf("counted %v: forward %+v", counted, f)
		}
		wantCode := CodeLate
		if counted {
			wantCode = CodePaid
		}
		if f.Code != wantCode || f.DebitAccountID != testBook.Wallet || f.CreditAccountID != testBook.Merchant || f.Ledger != 9 {
			t.Errorf("counted %v: forward %+v", counted, f)
		}
	}
	r := testBook.Reversal(revID(0), walkUD, 700, 5)
	if !testBook.ReversalOK(r, walkUD, 700) || r.UserData64 != 5 || r.Code != CodeReversed ||
		r.DebitAccountID != testBook.Merchant || r.CreditAccountID != testBook.Wallet {
		t.Errorf("reversal %+v", r)
	}
}

// Every field the checks read, changed alone, fails the check (seeded, over random field values).
func TestBookChecksRefuseEveryField(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	other := func(u ledger.U128) ledger.U128 {
		return ledger.U128{Lo: u.Lo ^ (1 + uint64(rng.Intn(1<<20))), Hi: u.Hi}
	}
	for i := 0; i < 200; i++ {
		amount := 1 + rng.Int63n(1<<53-1)
		ud := ledger.U128{Lo: rng.Uint64(), Hi: rng.Uint64()}
		counted := rng.Intn(2) == 0
		f := testBook.Forward(fwdID(0), ud, amount, counted)
		r := testBook.Reversal(revID(0), ud, amount, uint64(rng.Intn(10)))
		if !testBook.ForwardOK(f, ud, amount, counted) || !testBook.ReversalOK(r, ud, amount) {
			t.Fatalf("own transfers refused: %+v %+v", f, r)
		}
		for name, m := range map[string]func(*ledger.Transfer){
			"ledger":  func(x *ledger.Transfer) { x.Ledger++ },
			"debit":   func(x *ledger.Transfer) { x.DebitAccountID = other(x.DebitAccountID) },
			"credit":  func(x *ledger.Transfer) { x.CreditAccountID = other(x.CreditAccountID) },
			"amount":  func(x *ledger.Transfer) { x.Amount = ledger.U128From64(x.Amount.Lo + 1) },
			"amtHi":   func(x *ledger.Transfer) { x.Amount.Hi = 1 },
			"ud":      func(x *ledger.Transfer) { x.UserData128 = other(x.UserData128) },
			"code":    func(x *ledger.Transfer) { x.Code = 1 },
			"flags":   func(x *ledger.Transfer) { x.Flags = 1 },
			"pending": func(x *ledger.Transfer) { x.PendingID = ledger.U128From64(1) },
		} {
			fx, rx := f, r
			m(&fx)
			m(&rx)
			if testBook.ForwardOK(fx, ud, amount, counted) {
				t.Errorf("forward with a changed %s passes: %+v", name, fx)
			}
			if testBook.ReversalOK(rx, ud, amount) {
				t.Errorf("reversal with a changed %s passes: %+v", name, rx)
			}
		}
	}
}

func TestBookWalk(t *testing.T) {
	const amt = 500
	fw := func(ep uint32, counted bool) ledger.Transfer {
		f := testBook.Forward(fwdID(ep), walkUD, amt, counted)
		f.Timestamp = 100 + uint64(ep)
		return f
	}
	rv := func(ep uint32, seq uint64) ledger.Transfer { return testBook.Reversal(revID(ep), walkUD, amt, seq) }
	bad := func(t ledger.Transfer) ledger.Transfer { t.Amount = ledger.U128From64(amt + 1); return t }
	type want struct {
		recorded, reversed, foreign bool
		code                        uint16
		ts                          uint64
		epoch, next                 uint32
		seqs                        int
	}
	for name, tc := range map[string]struct {
		l       []ledger.Transfer
		counted bool
		want    want
	}{
		"nothing":                  {nil, true, want{next: 0}},
		"one forward":              {[]ledger.Transfer{fw(0, true)}, true, want{recorded: true, code: CodePaid, ts: 100}},
		"one late forward":         {[]ledger.Transfer{fw(0, false)}, false, want{recorded: true, code: CodeLate, ts: 100}},
		"reversed":                 {[]ledger.Transfer{fw(0, true), rv(0, 1)}, true, want{reversed: true, next: 1, seqs: 1}},
		"reversed, then recorded":  {[]ledger.Transfer{fw(0, true), rv(0, 1), fw(1, true)}, true, want{recorded: true, reversed: true, code: CodePaid, ts: 101, epoch: 1, seqs: 1}},
		"a late code on a counted": {[]ledger.Transfer{fw(0, false)}, true, want{foreign: true}},
		"a bad forward":            {[]ledger.Transfer{bad(fw(0, true))}, true, want{foreign: true}},
		"a bad reversal":           {[]ledger.Transfer{fw(0, true), bad(rv(0, 1))}, true, want{foreign: true}},
		"a bad second forward":     {[]ledger.Transfer{fw(0, true), rv(0, 2), bad(fw(1, true))}, true, want{foreign: true, reversed: true, seqs: 1}},
	} {
		lg := &fakeLedger{m: map[ledger.U128]ledger.Transfer{}}
		for _, x := range tc.l {
			lg.m[x.ID] = x
		}
		o := Out{Amount: amt, Counted: tc.counted}
		ep, next, err := testBook.Walk(context.Background(), lg, walkIDs, walkUD, &o, MaxEpochs)
		w := tc.want
		if err != nil || o.Recorded != w.recorded || o.Reversed != w.reversed || o.Foreign != w.foreign ||
			(w.recorded && (o.Code != w.code || o.Timestamp != w.ts || ep != w.epoch)) ||
			(!w.recorded && !w.foreign && next != w.next) || len(o.Seqs) != w.seqs {
			t.Errorf("%s: err %v, out %+v, epoch %d, next %d; want %+v", name, err, o, ep, next, w)
		}
	}
}

// The walk stops at maxEpochs (the transfers then count for nothing), and returns a lookup's error.
func TestBookWalkBoundAndError(t *testing.T) {
	lg := &fakeLedger{m: map[ledger.U128]ledger.Transfer{}}
	for ep := uint32(0); ep < 4; ep++ {
		lg.m[fwdID(ep)] = testBook.Forward(fwdID(ep), walkUD, 9, true)
		lg.m[revID(ep)] = testBook.Reversal(revID(ep), walkUD, 9, 0)
	}
	o := Out{Amount: 9, Counted: true}
	if _, _, err := testBook.Walk(context.Background(), lg, walkIDs, walkUD, &o, 3); err != nil || !o.Foreign || o.Recorded {
		t.Fatalf("bound: %v %+v", err, o)
	}
	lg.failOn = revID(1)
	o = Out{Amount: 9, Counted: true}
	if _, _, err := testBook.Walk(context.Background(), lg, walkIDs, walkUD, &o, MaxEpochs); !errors.Is(err, errFake) {
		t.Fatalf("a lookup error: %v", err)
	}
}
