package ledger

import (
	"context"
	"math/big"
	"math/rand"
	"testing"
	"time"
)

// clone is a deep copy of the model, for a batch that may be thrown away.
func (m *tModel) clone() *tModel {
	c := newTModel()
	for id, a := range m.accts {
		c.accts[id] = &mAcct{ledger: a.ledger, flags: a.flags, dp: new(big.Int).Set(a.dp), dq: new(big.Int).Set(a.dq),
			cp: new(big.Int).Set(a.cp), cq: new(big.Int).Set(a.cq)}
	}
	for id, x := range m.stored {
		c.stored[id] = x
	}
	for id := range m.failed {
		c.failed[id] = true
	}
	return c
}

// batchModel is CreateTransfers in the model: the items applied in order to a copy, which replaces the model only if
// every item is created or exists; otherwise the first refused item keeps its code, the others are
// linked_event_failed, and the model is left as it was (a transient refusal is not recorded either). undone reports a
// refused batch with an item created before the refused one: the ledger must roll that item back.
func (m *tModel) batch(xs []Transfer) (next *tModel, out []CreateTransferResult, undone bool) {
	c := m.clone()
	out = make([]CreateTransferResult, len(xs))
	for i, x := range xs {
		r := c.create(x)
		if r == TransferCreated || r == TransferExists {
			out[i] = r
			undone = undone || r == TransferCreated
			continue
		}
		for j := range out {
			out[j] = TransferLinkedEventFailed
		}
		out[i] = r
		return m, out, undone
	}
	return c, out, false
}

// goodTransfer is a transfer the ledger usually creates: ledger 1's accounts (1, 2, 3, 5), small amounts, code 1;
// the limited accounts 2 and 3 still refuse some (exceeds_credits, exceeds_debits).
func goodTransfer(rng *rand.Rand) Transfer {
	accts := []uint64{1, 2, 3, 5}
	d := accts[rng.Intn(len(accts))]
	c := accts[rng.Intn(len(accts))]
	for c == d {
		c = accts[rng.Intn(len(accts))]
	}
	x := tr(uint64(rng.Intn(400)+200), d, c, u(uint64(rng.Intn(20)+1)))
	if rng.Intn(5) == 0 {
		x.Flags, x.Timeout = TransferPending, 1000
	}
	return x
}

// Seeded property: CreateTransfers equals the model's all-or-nothing batch over CreateTransfer's own model, mixed with
// single creations (so failed ids recorded by a single reach a batch, and a batch's ids reach a single). Batches of 1
// to 6 items repeat ids within the batch now and then (the later item sees the earlier one). Every stored transfer
// and every balance matches the model at the end, and a refused batch leaves the ledger as it was (its ids unstored;
// its balances at the model's when it created an item before its refusal, at least 100 of those).
func TestCreateTransfersProperty(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	m := newTModel()
	flags := []AccountFlags{0, AccountDebitsMustNotExceedCredits, AccountCreditsMustNotExceedDebits, 0, 0, AccountDebitsMustNotExceedCredits}
	ledgers := []uint32{1, 1, 1, 2, 1, 2}
	for i := range flags {
		mustAcct(t, l, uint64(i+1), ledgers[i], flags[i])
		m.accts[uint64(i+1)] = &mAcct{ledger: ledgers[i], flags: flags[i], dp: new(big.Int), dq: new(big.Int), cp: new(big.Int), cq: new(big.Int)}
	}
	// sameBalances checks every account's balance against the model's.
	sameBalances := func(i int) {
		t.Helper()
		for id, a := range m.accts {
			b := bal(t, l, id)
			if toBig(b.DebitsPending).Cmp(a.dp) != 0 || toBig(b.DebitsPosted).Cmp(a.dq) != 0 ||
				toBig(b.CreditsPending).Cmp(a.cp) != 0 || toBig(b.CreditsPosted).Cmp(a.cq) != 0 {
				t.Fatalf("%d: account %d balance %+v vs model", i, id, b)
			}
		}
	}
	rng := rand.New(rand.NewSource(101))
	seen := map[CreateTransferResult]int{}
	for i := 0; i < 1200; i++ {
		if rng.Intn(3) == 0 {
			x := genTransfer(rng)
			want := m.create(x)
			if got, err := l.CreateTransfer(ctx, x); err != nil || got != want {
				t.Fatalf("%d single %+v: got %q %v want %q", i, x, got, err, want)
			}
			continue
		}
		xs := make([]Transfer, 1+rng.Intn(4))
		for j := range xs {
			xs[j] = goodTransfer(rng)
			if rng.Intn(10) == 0 {
				xs[j] = genTransfer(rng)
			}
			if j > 0 && rng.Intn(5) == 0 {
				xs[j] = xs[rng.Intn(j)] // the same item again: exists, unless an earlier one was refused
				if rng.Intn(2) == 0 {
					xs[j].Amount = u(xs[j].Amount.Lo + 1) // or exists_with_different_amount
				}
			}
		}
		next, want, undone := m.batch(xs)
		got, err := l.CreateTransfers(ctx, xs)
		if err != nil || len(got) != len(want) {
			t.Fatalf("%d batch %+v: %v %v", i, xs, got, err)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("%d batch %+v: item %d got %q want %q (all %v, want %v)", i, xs, j, got[j], want[j], got, want)
			}
		}
		if next == m {
			seen["refused"]++
			// A refused batch leaves every balance at the model's, which it did not change. The six balance scans (each
			// over the whole table) run only when the batch created an item before its refusal, the case a rollback must
			// undo: a batch refused at its first new item wrote nothing to undo.
			if undone {
				seen["undone"]++
				sameBalances(i)
			}
			for _, x := range xs {
				if _, stored := m.stored[x.ID]; stored {
					continue
				}
				if _, found, err := l.LookupTransfer(ctx, x.ID); err != nil || found {
					t.Fatalf("%d: a refused batch stored %v (%v)", i, x.ID, err)
				}
			}
		} else {
			seen["applied"]++
		}
		m = next
	}
	t.Logf("outcomes %v", seen)
	if seen["refused"] < 100 || seen["applied"] < 100 || seen["undone"] < 100 {
		t.Fatalf("the batches did not cross both outcomes: %v", seen)
	}
	for id, e := range m.stored {
		got, found, err := l.LookupTransfer(ctx, e.ID)
		e.Timestamp = got.Timestamp
		if err != nil || !found || got != e || got.Timestamp == 0 {
			t.Fatalf("lookup %v: %+v vs %+v (%v)", id, got, e, err)
		}
	}
	for id, a := range m.accts {
		b := bal(t, l, id)
		if toBig(b.DebitsPending).Cmp(a.dp) != 0 || toBig(b.DebitsPosted).Cmp(a.dq) != 0 ||
			toBig(b.CreditsPending).Cmp(a.cp) != 0 || toBig(b.CreditsPosted).Cmp(a.cq) != 0 {
			t.Fatalf("account %d balance %+v vs model", id, b)
		}
	}
}

// The cases by hand: an empty batch; a batch created whole, its timestamps strictly increasing; the same batch again
// (every item exists, nothing changes); an item that exists with other details, or that exceeds a limit, refuses the
// whole batch (the others linked_event_failed, nothing stored, no failed id recorded); a batch that posts a pending
// transfer it creates.
func TestCreateTransfersCases(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	mustAcct(t, l, 1, 1, 0)
	mustAcct(t, l, 2, 1, AccountDebitsMustNotExceedCredits)
	if got, err := l.CreateTransfers(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty batch: %v %v", got, err)
	}
	a, b := tr(10, 1, 2, u(5)), tr(11, 2, 1, u(5))
	got, err := l.CreateTransfers(ctx, []Transfer{a, b})
	if err != nil || got[0] != TransferCreated || got[1] != TransferCreated {
		t.Fatalf("a whole batch: %v %v", got, err)
	}
	ta, _, _ := l.LookupTransfer(ctx, a.ID)
	tb, _, _ := l.LookupTransfer(ctx, b.ID)
	if ta.Timestamp == 0 || tb.Timestamp <= ta.Timestamp {
		t.Fatalf("timestamps %d then %d", ta.Timestamp, tb.Timestamp)
	}
	if got, err := l.CreateTransfers(ctx, []Transfer{a, b}); err != nil || got[0] != TransferExists || got[1] != TransferExists {
		t.Fatalf("the batch again: %v %v", got, err)
	}
	before1, before2 := bal(t, l, 1), bal(t, l, 2)
	c := tr(12, 1, 2, u(3))
	diff := a
	diff.Amount = u(6)
	over := tr(13, 2, 1, u(4)) // account 2 credited 5 and debited 5: after c, 8 against 9
	for name, tc := range map[string]struct {
		xs   []Transfer
		at   int
		code CreateTransferResult
	}{
		"exists with other details": {[]Transfer{c, diff}, 1, TransferExistsWithDifferentAmount},
		"exceeds credits":           {[]Transfer{c, over, tr(14, 1, 2, u(1))}, 1, TransferExceedsCredits},
		"refused first":             {[]Transfer{tr(15, 1, 1, u(1)), c}, 0, TransferAccountsMustBeDifferent},
		"bad timestamp":             {[]Transfer{c, {ID: u(16), DebitAccountID: u(1), CreditAccountID: u(2), Amount: u(1), Ledger: 1, Code: 1, Timestamp: 1}}, 1, TransferTimestampMustBeZero},
	} {
		got, err := l.CreateTransfers(ctx, tc.xs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for j, r := range got {
			want := TransferLinkedEventFailed
			if j == tc.at {
				want = tc.code
			}
			if r != want {
				t.Fatalf("%s: item %d %q, want %q (%v)", name, j, r, want, got)
			}
		}
		for _, x := range tc.xs {
			if x.ID == a.ID {
				continue
			}
			if _, found, _ := l.LookupTransfer(ctx, x.ID); found {
				t.Fatalf("%s: stored %v", name, x.ID)
			}
		}
		if bal(t, l, 1) != before1 || bal(t, l, 2) != before2 {
			t.Fatalf("%s: balances moved", name)
		}
	}
	// The exceeds_credits item was not recorded as failed: once account 2 has credit, the same id is created.
	mustCreate(t, l, tr(20, 1, 2, u(4)))
	if r := create(t, l, over); r != TransferCreated {
		t.Fatalf("the refused batch's transient item later: %q, want created", r)
	}
	// A pending transfer and its post in one batch.
	p := tr(30, 1, 2, u(2))
	p.Flags = TransferPending
	post := Transfer{ID: u(31), PendingID: p.ID, Amount: U128Max, Flags: TransferPostPendingTransfer}
	if got, err := l.CreateTransfers(ctx, []Transfer{p, post}); err != nil || got[0] != TransferCreated || got[1] != TransferCreated {
		t.Fatalf("pending and post: %v %v", got, err)
	}
}

// FuzzCreateTransfers feeds batches of up to 4 transfers built from the fuzzer's bytes, over fuzzLedger: the results
// are TigerBeetle codes, either every item created or exists, or exactly one refused and the others
// linked_event_failed; a refused batch moves no balance and stores none of its new ids, an applied one stores every
// item it created; the books balance and nothing is resolved twice, whatever came in.
func FuzzCreateTransfers(f *testing.F) {
	l := fuzzLedger(f, f.TempDir())
	names := transferResultNames(f)
	f.Add([]byte{1, 1, 2, 5, 1, 1, 0, 0, 2, 1, 2, 5, 1, 1, 0, 0})
	f.Add([]byte{3, 3, 1, 200, 1, 1, 0, 0, 4, 1, 4, 200, 1, 1, 0, 0})
	f.Add([]byte{5, 1, 2, 7, 1, 1, 1, 0, 6, 0, 0, 255, 0, 0, 2, 5})
	f.Add([]byte{7, 1, 2, 7, 1, 1, 0, 0, 7, 1, 2, 8, 1, 1, 0, 0, 0, 1, 2, 1, 1, 1, 0, 0})
	f.Add([]byte{})
	ctx := context.Background()
	f.Fuzz(func(t *testing.T, b []byte) {
		var xs []Transfer
		for len(b) >= 8 && len(xs) < 4 {
			x := Transfer{ID: u(uint64(b[0]) + 5000), DebitAccountID: u(uint64(b[1] % 5)), CreditAccountID: u(uint64(b[2] % 5)),
				Amount: u(uint64(b[3])), Ledger: uint32(b[4] % 3), Code: uint16(b[5] % 3)}
			switch b[6] % 4 {
			case 1:
				x.Flags, x.Timeout = TransferPending, uint32(b[7])
			case 2: // resolve one of fuzzLedger's pending transfers
				x.Flags = []TransferFlags{TransferPostPendingTransfer, TransferVoidPendingTransfer}[b[7]%2]
				x.PendingID = u([]uint64{1001, 1002, 1003, 1101, 1102, 1103, 1104}[int(b[7])%7])
			case 3:
				x.Flags = TransferFlags(b[7])
			}
			if b[0] == 0 {
				x.ID = U128Zero
			}
			xs = append(xs, x)
			b = b[8:]
		}
		before := make([]Balance, len(fuzzAccts))
		for i, a := range fuzzAccts {
			before[i] = bal(t, l, a.id)
		}
		existed := map[U128]bool{}
		for _, x := range xs {
			if _, found, err := l.LookupTransfer(ctx, x.ID); err != nil {
				t.Fatal(err)
			} else if found {
				existed[x.ID] = true
			}
		}
		got, err := l.CreateTransfers(ctx, xs)
		if err != nil || len(got) != len(xs) {
			t.Fatalf("%+v: %v %v", xs, got, err)
		}
		refused, linked := 0, 0
		for _, r := range got {
			if _, ok := names[r]; !ok {
				t.Fatalf("%+v: unknown result %q", xs, r)
			}
			switch r {
			case TransferCreated, TransferExists:
			case TransferLinkedEventFailed:
				linked++
			default:
				refused++
			}
		}
		switch {
		case refused == 0 && linked == 0:
			for i, r := range got {
				if _, found, err := l.LookupTransfer(ctx, xs[i].ID); err != nil || (r == TransferCreated && !found) {
					t.Fatalf("%+v: item %d created but not found (%v)", xs, i, err)
				}
			}
		case refused == 1 && linked == len(xs)-1:
			for i, a := range fuzzAccts {
				if after := bal(t, l, a.id); after != before[i] {
					t.Fatalf("%+v: a refused batch moved account %d", xs, a.id)
				}
			}
			for _, x := range xs {
				if _, found, err := l.LookupTransfer(ctx, x.ID); err != nil || (found && !existed[x.ID]) {
					t.Fatalf("%+v: a refused batch stored %v (%v)", xs, x.ID, err)
				}
			}
		default:
			t.Fatalf("%+v: results %v are neither applied nor one refusal", xs, got)
		}
		bals := make([]Balance, len(fuzzAccts))
		for i, a := range fuzzAccts {
			bals[i] = bal(t, l, a.id)
		}
		checkBooks(t, fuzzAccts, bals)
		checkResolutions(t, l)
	})
}

// CreateTransfers: an expiry the ledger observes is final, whatever the batch. A batch whose first
// item posts a pending transfer past its timeout (pending_transfer_expired, which CreateTransfer records in
// expired_transfers) and whose second item is refused is rolled back, but the expiry stays recorded, as
// CreateTransfer leaves it: the first refusal is reported, and the expiry's row is there. So is an expiry recorded
// on the way by another item's balance check (recordDueExpiries).
func TestCreateTransfersKeepsObservedExpiries(t *testing.T) {
	ctx := context.Background()
	l, clk := newTL(t)
	mustAccts(t, l, 1, 2)
	p := tr(40, 1, 2, u(5))
	p.Flags, p.Timeout = TransferPending, 1
	q := tr(41, 1, 2, u(5))
	q.Flags, q.Timeout = TransferPending, 1
	mustCreate(t, l, p)
	mustCreate(t, l, q)
	clk.advance(2 * time.Second)
	expired := func(id uint64) bool {
		t.Helper()
		var n int
		if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers WHERE pending_id = ?`, idBytes(u(id))).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	post := Transfer{ID: u(42), PendingID: p.ID, Amount: U128Max, Flags: TransferPostPendingTransfer}
	bad := tr(43, 1, 1, u(1)) // accounts_must_be_different
	got, err := l.CreateTransfers(ctx, []Transfer{post, bad})
	if err != nil || got[0] != TransferPendingTransferExpired || got[1] != TransferLinkedEventFailed {
		t.Fatalf("batch: %v %v", got, err)
	}
	if !expired(40) {
		t.Fatal("the batch reported pending_transfer_expired but its expiry was rolled back")
	}
	// recordDueExpiries: a transfer between the accounts records q's expiry before its balance check; the batch's
	// next item is refused.
	got, err = l.CreateTransfers(ctx, []Transfer{tr(44, 1, 2, u(1)), bad})
	if err != nil || got[0] != TransferLinkedEventFailed || got[1] != TransferAccountsMustBeDifferent {
		t.Fatalf("batch: %v %v", got, err)
	}
	if !expired(41) {
		t.Fatal("an expiry observed by the batch was rolled back")
	}
	if _, found, _ := l.LookupTransfer(ctx, u(44)); found {
		t.Fatal("the refused batch stored its transfer")
	}
	// The same post again, alone: expired, as recorded.
	if r := create(t, l, Transfer{ID: u(45), PendingID: p.ID, Flags: TransferVoidPendingTransfer}); r != TransferPendingTransferExpired {
		t.Fatalf("a void after the batch: %q", r)
	}
}
