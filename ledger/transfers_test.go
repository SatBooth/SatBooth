package ledger

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is a settable clock.
type testClock struct {
	mu sync.Mutex
	ns int64
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(0, c.ns)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.ns += int64(d)
	c.mu.Unlock()
}

const clockStart = 1_700_000_000_000_000_000

func newTL(t *testing.T) (*Ledger, *testClock) {
	t.Helper()
	clk := &testClock{ns: clockStart}
	l, err := Open(context.Background(), t.TempDir()+"/l.db", WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, clk
}

func mustAcct(t *testing.T, l *Ledger, id uint64, ledger uint32, flags AccountFlags) {
	t.Helper()
	r, err := l.CreateAccount(context.Background(), Account{ID: U128From64(id), Ledger: ledger, Code: 1, Flags: flags})
	if err != nil || r != AccountCreated {
		t.Fatalf("account %d: %q %v", id, r, err)
	}
}

func mustAccts(t *testing.T, l *Ledger, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		mustAcct(t, l, id, 1, 0)
	}
}

func tr(id, d, c uint64, amount U128) Transfer {
	return Transfer{ID: U128From64(id), DebitAccountID: U128From64(d), CreditAccountID: U128From64(c), Amount: amount, Ledger: 1, Code: 1}
}

func create(t *testing.T, l *Ledger, x Transfer) CreateTransferResult {
	t.Helper()
	r, err := l.CreateTransfer(context.Background(), x)
	if err != nil {
		t.Fatalf("create %+v: %v", x, err)
	}
	return r
}

func mustCreate(t *testing.T, l *Ledger, x Transfer) {
	t.Helper()
	if r := create(t, l, x); r != TransferCreated {
		t.Fatalf("create %+v: %q", x, r)
	}
}

func bal(t *testing.T, l *Ledger, id uint64) Balance {
	t.Helper()
	b, err := l.Balance(context.Background(), U128From64(id))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func u(v uint64) U128 { return U128From64(v) }

// p2 returns 2^n as a U128, for n < 128.
func p2(n uint) U128 {
	if n >= 64 {
		return U128{1 << (n - 64), 0}
	}
	return U128{0, 1 << n}
}

func mustSub(a, b U128) U128 {
	r, ok := a.Sub(b)
	if !ok {
		panic("underflow")
	}
	return r
}

func mustAdd(a, b U128) U128 {
	r, ok := a.Add(b)
	if !ok {
		panic("overflow")
	}
	return r
}

func TestCreateTransferResults(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	// 1, 2 plain on ledger 1; 3 debits_must_not_exceed_credits; 4 credits_must_not_exceed_debits;
	// 5 plain on ledger 2. 10..19 are scratch accounts for the overflow cases.
	mustAcct(t, l, 1, 1, 0)
	mustAcct(t, l, 2, 1, 0)
	mustAcct(t, l, 3, 1, AccountDebitsMustNotExceedCredits)
	mustAcct(t, l, 4, 1, AccountCreditsMustNotExceedDebits)
	mustAcct(t, l, 5, 2, 0)
	for id := uint64(10); id < 23; id++ {
		mustAcct(t, l, id, 1, 0)
	}
	existing := tr(100, 1, 2, u(5))
	existing.UserData128, existing.UserData64, existing.UserData32 = U128{1, 2}, 3, 4
	existing.Code, existing.Flags, existing.Timeout = 7, TransferPending, 60
	mustCreate(t, l, existing)

	mod := func(base Transfer, f func(*Transfer)) Transfer { f(&base); return base }
	ok := func(id uint64) Transfer { return tr(id, 1, 2, u(1)) }
	// Setup transfers run before the case and must succeed.
	cases := []struct {
		name  string
		setup []Transfer
		x     Transfer
		want  CreateTransferResult
	}{
		{"created", nil, ok(1000), TransferCreated},
		{"created with zero amount (TigerBeetle >= 0.16)", nil, tr(1001, 1, 2, U128Zero), TransferCreated},
		{"timestamp_must_be_zero", nil, mod(ok(1002), func(x *Transfer) { x.Timestamp = 1 }), TransferTimestampMustBeZero},
		{"reserved_flag linked", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 0 }), TransferReservedFlag},
		{"reserved_flag balancing_debit", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 4 }), TransferReservedFlag},
		{"reserved_flag balancing_credit", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 5 }), TransferReservedFlag},
		{"reserved_flag closing_debit", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 6 }), TransferReservedFlag},
		{"reserved_flag closing_credit", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 7 }), TransferReservedFlag},
		{"reserved_flag imported", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 8 }), TransferReservedFlag},
		{"reserved_flag top bit", nil, mod(ok(1003), func(x *Transfer) { x.Flags = 1 << 15 }), TransferReservedFlag},
		{"id_must_not_be_zero", nil, mod(ok(1), func(x *Transfer) { x.ID = U128Zero }), TransferIDMustNotBeZero},
		{"id_must_not_be_int_max", nil, mod(ok(1), func(x *Transfer) { x.ID = U128Max }), TransferIDMustNotBeIntMax},
		{"exists_with_different_flags", nil, mod(existing, func(x *Transfer) { x.Flags = 0 }), TransferExistsWithDifferentFlags},
		{"exists_with_different_flags beats pending_id", nil, mod(existing, func(x *Transfer) { x.Flags = 0; x.PendingID = u(9) }), TransferExistsWithDifferentFlags},
		{"exists_with_different_pending_id", nil, mod(existing, func(x *Transfer) { x.PendingID = u(9) }), TransferExistsWithDifferentPendingID},
		{"exists_with_different_timeout", nil, mod(existing, func(x *Transfer) { x.Timeout++ }), TransferExistsWithDifferentTimeout},
		{"exists_with_different_debit_account_id", nil, mod(existing, func(x *Transfer) { x.DebitAccountID = u(9) }), TransferExistsWithDifferentDebitAccountID},
		{"exists_with_different_credit_account_id", nil, mod(existing, func(x *Transfer) { x.CreditAccountID = u(9) }), TransferExistsWithDifferentCreditAccountID},
		{"exists_with_different_amount", nil, mod(existing, func(x *Transfer) { x.Amount = u(6) }), TransferExistsWithDifferentAmount},
		{"exists_with_different_user_data_128", nil, mod(existing, func(x *Transfer) { x.UserData128 = U128{1, 3} }), TransferExistsWithDifferentUserData128},
		{"exists_with_different_user_data_64", nil, mod(existing, func(x *Transfer) { x.UserData64++ }), TransferExistsWithDifferentUserData64},
		{"exists_with_different_user_data_32", nil, mod(existing, func(x *Transfer) { x.UserData32++ }), TransferExistsWithDifferentUserData32},
		{"exists_with_different_ledger", nil, mod(existing, func(x *Transfer) { x.Ledger++ }), TransferExistsWithDifferentLedger},
		{"exists_with_different_code", nil, mod(existing, func(x *Transfer) { x.Code++ }), TransferExistsWithDifferentCode},
		{"multi-field differ: first in order wins", nil, mod(existing, func(x *Transfer) { x.Code++; x.Amount = u(6); x.Timeout++ }), TransferExistsWithDifferentTimeout},
		{"exists", nil, existing, TransferExists},
		{"id_already_failed", []Transfer{tr(1010, 99, 2, u(1))}, tr(1010, 1, 2, u(1)), TransferIDAlreadyFailed},
		{"id_already_failed beats debit zero", []Transfer{tr(1011, 99, 2, u(1))}, mod(ok(1011), func(x *Transfer) { x.DebitAccountID = U128Zero }), TransferIDAlreadyFailed},
		{"debit_account_id_must_not_be_zero", nil, mod(ok(1020), func(x *Transfer) { x.DebitAccountID = U128Zero }), TransferDebitAccountIDMustNotBeZero},
		{"debit_account_id_must_not_be_int_max", nil, mod(ok(1021), func(x *Transfer) { x.DebitAccountID = U128Max }), TransferDebitAccountIDMustNotBeIntMax},
		{"credit_account_id_must_not_be_zero", nil, mod(ok(1022), func(x *Transfer) { x.CreditAccountID = U128Zero }), TransferCreditAccountIDMustNotBeZero},
		{"credit_account_id_must_not_be_int_max", nil, mod(ok(1023), func(x *Transfer) { x.CreditAccountID = U128Max }), TransferCreditAccountIDMustNotBeIntMax},
		{"accounts_must_be_different", nil, tr(1024, 1, 1, u(1)), TransferAccountsMustBeDifferent},
		{"pending_id_must_be_zero", nil, mod(ok(1025), func(x *Transfer) { x.PendingID = u(1) }), TransferPendingIDMustBeZero},
		{"timeout_reserved_for_pending_transfer", nil, mod(ok(1026), func(x *Transfer) { x.Timeout = 1 }), TransferTimeoutReservedForPendingTransfer},
		{"pending with timeout is fine", nil, mod(ok(1027), func(x *Transfer) { x.Flags = TransferPending; x.Timeout = 1 }), TransferCreated},
		{"ledger_must_not_be_zero", nil, mod(ok(1028), func(x *Transfer) { x.Ledger = 0 }), TransferLedgerMustNotBeZero},
		{"code_must_not_be_zero", nil, mod(ok(1029), func(x *Transfer) { x.Code = 0 }), TransferCodeMustNotBeZero},
		{"ledger zero beats account not found", nil, mod(tr(1030, 98, 2, u(1)), func(x *Transfer) { x.Ledger = 0 }), TransferLedgerMustNotBeZero},
		{"debit_account_not_found", nil, tr(1031, 98, 2, u(1)), TransferDebitAccountNotFound},
		{"credit_account_not_found", nil, tr(1032, 1, 98, u(1)), TransferCreditAccountNotFound},
		{"debit not found beats credit not found", nil, tr(1033, 97, 98, u(1)), TransferDebitAccountNotFound},
		{"accounts_must_have_the_same_ledger", nil, tr(1034, 1, 5, u(1)), TransferAccountsMustHaveTheSameLedger},
		{"transfer_must_have_the_same_ledger_as_accounts", nil, mod(ok(1035), func(x *Transfer) { x.Ledger = 2 }), TransferTransferMustHaveTheSameLedgerAsAccounts},
		{"exceeds_credits", nil, tr(1036, 3, 2, u(1)), TransferExceedsCredits},
		{"exceeds_debits", nil, tr(1037, 1, 4, u(1)), TransferExceedsDebits},
		{"exceeds_credits beats exceeds_debits", nil, tr(1038, 3, 4, u(1)), TransferExceedsCredits},
		{"limit: zero amount on empty limited account is fine", nil, tr(1039, 3, 4, U128Zero), TransferCreated},
		{"overflows_debits_pending", []Transfer{
			mod(tr(1040, 10, 11, U128Max), func(x *Transfer) { x.Flags = TransferPending }),
		}, mod(tr(1041, 10, 12, u(1)), func(x *Transfer) { x.Flags = TransferPending }), TransferOverflowsDebitsPending},
		{"overflows_credits_pending", []Transfer{
			mod(tr(1042, 13, 14, U128Max), func(x *Transfer) { x.Flags = TransferPending }),
		}, mod(tr(1043, 15, 14, u(1)), func(x *Transfer) { x.Flags = TransferPending }), TransferOverflowsCreditsPending},
		{"overflows_debits_posted", []Transfer{tr(1044, 16, 17, U128Max)}, tr(1045, 16, 18, u(1)), TransferOverflowsDebitsPosted},
		{"overflows_credits_posted", []Transfer{tr(1047, 20, 21, U128Max)}, tr(1046, 22, 21, u(1)), TransferOverflowsCreditsPosted},
	}
	for _, c := range cases {
		for _, s := range c.setup {
			if r := create(t, l, s); r != TransferCreated && !(strings.HasPrefix(c.name, "id_already_failed") && r == TransferDebitAccountNotFound) {
				t.Fatalf("%s: setup %+v: %q", c.name, s, r)
			}
		}
		got, err := l.CreateTransfer(ctx, c.x)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	// Refused transfers are not stored.
	for _, id := range []uint64{1002, 1003, 1020, 1024, 1031, 1036, 1041, 1043, 1045, 1046} {
		if _, found, _ := l.LookupTransfer(ctx, u(id)); found {
			t.Errorf("transfer %d stored despite refusal", id)
		}
	}
}

// overflows_debits and overflows_credits are reached only when pending+posted overflows while
// neither alone does, with a non-pending transfer.
func TestOverflowsSumOfPendingAndPosted(t *testing.T) {
	for _, side := range []string{"debit", "credit"} {
		l, _ := newTL(t)
		mustAccts(t, l, 1, 2, 3, 4)
		half := p2(127)
		pend := Transfer{ID: u(1), DebitAccountID: u(1), CreditAccountID: u(2), Amount: half, Ledger: 1, Code: 1, Flags: TransferPending}
		post := tr(2, 1, 2, mustSub(half, u(1)))
		want := TransferOverflowsDebits
		other := tr(3, 1, 3, u(1))
		if side == "credit" {
			pend.DebitAccountID, pend.CreditAccountID = u(3), u(1)
			post.DebitAccountID, post.CreditAccountID = u(4), u(1)
			other = tr(3, 4, 1, u(1))
			want = TransferOverflowsCredits
		}
		mustCreate(t, l, pend)
		mustCreate(t, l, post)
		if got := create(t, l, other); got != want {
			t.Errorf("%s: got %q want %q", side, got, want)
		}
		// A zero amount still fits at the limit.
		other.Amount = U128Zero
		other.ID = u(4)
		if got := create(t, l, other); got != TransferCreated {
			t.Errorf("%s: zero amount: %q", side, got)
		}
	}
}

func TestOverflowsTimeout(t *testing.T) {
	ctx := context.Background()
	// Two accounts take c and c+1, so the first transfer gets MaxInt64-1e9.
	clk := &testClock{ns: math.MaxInt64 - 1_000_000_000 - 2}
	l, err := Open(ctx, t.TempDir()+"/l.db", WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	mustAccts(t, l, 1, 2)
	p := tr(1, 1, 2, u(1))
	p.Flags = TransferPending
	p.Timeout = 2
	if got := create(t, l, p); got != TransferOverflowsTimeout {
		t.Fatalf("got %q", got)
	}
	// The failed attempt took no timestamp, so timestamp + 1 s is exactly MaxInt64: it fits.
	p.ID, p.Timeout = u(2), 1
	if got := create(t, l, p); got != TransferCreated {
		t.Fatalf("got %q", got)
	}
	if got := create(t, l, tr(3, 1, 2, u(1))); err != nil || got != TransferCreated {
		t.Fatalf("plain transfer at the end of time: %q", got)
	}
}

func TestClockBefore1970(t *testing.T) {
	ctx := context.Background()
	for _, ns := range []int64{0, -5} {
		clk := &testClock{ns: ns}
		l, err := Open(ctx, t.TempDir()+"/l.db", WithClock(clk.now))
		if err != nil {
			t.Fatal(err)
		}
		if r, err := l.CreateAccount(ctx, goodAccount()); err == nil {
			t.Errorf("clock %d: account created (%q)", ns, r)
		}
		l.Close()
	}
}

func TestNextTimestampMaxInt64(t *testing.T) {
	ctx := context.Background()
	clk := &testClock{ns: math.MaxInt64}
	l, err := Open(ctx, t.TempDir()+"/l.db", WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a := goodAccount()
	if r, err := l.CreateAccount(ctx, a); err != nil || r != AccountCreated {
		t.Fatal(r, err)
	}
	// The next row would need MaxInt64+1.
	a.ID = u(2)
	if r, err := l.CreateAccount(ctx, a); err == nil {
		t.Fatalf("created past the end of time: %q", r)
	}
}

func TestDecodeRangeChecks(t *testing.T) {
	if _, err := checkedUint32(-1); err == nil {
		t.Error("-1")
	}
	if _, err := checkedUint32(1 << 32); err == nil {
		t.Error("2^32")
	}
	if v, err := checkedUint32(1<<32 - 1); err != nil || v != 1<<32-1 {
		t.Error(v, err)
	}
	if _, err := checkedUint16(1 << 16); err == nil {
		t.Error("2^16")
	}
	if _, err := checkedUint16(-1); err == nil {
		t.Error("-1")
	}
	if _, err := checkedUint64(-1); err == nil {
		t.Error("-1")
	}
	if v, err := checkedUint64(math.MaxInt64); err != nil || v != math.MaxInt64 {
		t.Error(v, err)
	}
}

// A retry after a transient failure stays failed.
func TestRetryAfterTransientFailure(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	mustAccts(t, l, 1, 2)
	mustAcct(t, l, 3, 1, AccountDebitsMustNotExceedCredits)
	mustAcct(t, l, 4, 1, AccountCreditsMustNotExceedDebits)

	x := tr(50, 3, 2, u(40))
	if got := create(t, l, x); got != TransferExceedsCredits {
		t.Fatalf("got %q", got)
	}
	mustCreate(t, l, tr(51, 1, 3, u(100))) // fund account 3
	before := bal(t, l, 3)
	if got := create(t, l, x); got != TransferIDAlreadyFailed {
		t.Fatalf("retry: got %q", got)
	}
	if after := bal(t, l, 3); after != before {
		t.Errorf("balance changed: %+v -> %+v", before, after)
	}
	if _, found, _ := l.LookupTransfer(ctx, x.ID); found {
		t.Error("failed transfer is stored")
	}
	// With a different id it works, so the funds really were there.
	x.ID = u(52)
	mustCreate(t, l, x)

	// exceeds_debits.
	y := tr(60, 1, 4, u(10))
	if got := create(t, l, y); got != TransferExceedsDebits {
		t.Fatalf("got %q", got)
	}
	mustCreate(t, l, tr(61, 4, 1, u(10)))
	if got := create(t, l, y); got != TransferIDAlreadyFailed {
		t.Fatalf("retry: got %q", got)
	}

	// Account not found, then the account appears.
	z := tr(70, 99, 2, u(1))
	if got := create(t, l, z); got != TransferDebitAccountNotFound {
		t.Fatalf("got %q", got)
	}
	mustAcct(t, l, 99, 1, 0)
	if got := create(t, l, z); got != TransferIDAlreadyFailed {
		t.Fatalf("retry: got %q", got)
	}
	z2 := tr(71, 1, 98, u(1))
	if got := create(t, l, z2); got != TransferCreditAccountNotFound {
		t.Fatalf("got %q", got)
	}
	mustAcct(t, l, 98, 1, 0)
	if got := create(t, l, z2); got != TransferIDAlreadyFailed {
		t.Fatalf("retry: got %q", got)
	}

	// A permanent failure is not recorded: corrected, the same id succeeds.
	w := tr(80, 1, 2, u(1))
	w.Code = 0
	if got := create(t, l, w); got != TransferCodeMustNotBeZero {
		t.Fatalf("got %q", got)
	}
	w.Code = 1
	mustCreate(t, l, w)

	var n int
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM failed_transfers`).Scan(&n); err != nil || n != 4 {
		t.Errorf("failed_transfers rows: %d %v", n, err)
	}
}

// Amounts at the 64-bit and 128-bit edges.
func TestOverflowEdges(t *testing.T) {
	ctx := context.Background()
	edges := map[string]U128{
		"2^63-1": u(1<<63 - 1), "2^63": p2(63), "2^64-1": u(^uint64(0)), "2^64": p2(64), "2^128-1": U128Max,
	}
	for name, x := range edges {
		t.Run(name, func(t *testing.T) {
			l, _ := newTL(t)
			mustAccts(t, l, 1, 2, 3, 4, 5)
			// Credits of 2 reach exactly 2^128-1 in two steps.
			mustCreate(t, l, tr(1, 1, 2, x))
			rest := mustSub(U128Max, x)
			mustCreate(t, l, tr(2, 3, 2, rest))
			b := bal(t, l, 2)
			if b.CreditsPosted != U128Max || !b.DebitsPosted.IsZero() {
				t.Fatalf("balance %+v", b)
			}
			// Account 1 has been debited exactly x so far.
			b1 := bal(t, l, 1)
			if b1.DebitsPosted != x {
				t.Fatalf("debits %v want %v", b1.DebitsPosted, x)
			}
			// One more credit overflows; the first failing check in TigerBeetle's order is
			// credits_posted (the debit account 4 is empty).
			if got := create(t, l, tr(3, 4, 2, u(1))); got != TransferOverflowsCreditsPosted {
				t.Errorf("credit side: %q", got)
			}
			// Same on the debit side: account 1 gets to Max, then 1 more.
			mustCreate(t, l, tr(4, 1, 5, rest))
			if got := create(t, l, tr(5, 1, 4, u(1))); got != TransferOverflowsDebitsPosted {
				t.Errorf("debit side: %q", got)
			}
			if b := bal(t, l, 1); b.DebitsPosted != U128Max {
				t.Errorf("debits %v", b.DebitsPosted)
			}
			// Stored amounts round-trip exactly.
			got, found, err := l.LookupTransfer(ctx, u(1))
			if err != nil || !found || got.Amount != x {
				t.Errorf("lookup: %+v %v %v", got, found, err)
			}
			// A stored transfer is not changed by the refused ones.
			if _, found, _ := l.LookupTransfer(ctx, u(3)); found {
				t.Error("refused transfer stored")
			}
		})
	}
}

func TestPendingBalancesAndExpiry(t *testing.T) {
	l, clk := newTL(t)
	mustAccts(t, l, 1, 2)
	mustAcct(t, l, 3, 1, AccountDebitsMustNotExceedCredits)
	mustCreate(t, l, tr(1, 1, 3, u(100)))
	p := tr(2, 3, 2, u(70))
	p.Flags, p.Timeout = TransferPending, 5
	mustCreate(t, l, p)
	if b := bal(t, l, 3); b.DebitsPending != u(70) || b.CreditsPosted != u(100) || !b.DebitsPosted.IsZero() {
		t.Fatalf("balance %+v", b)
	}
	if b := bal(t, l, 2); b.CreditsPending != u(70) || !b.CreditsPosted.IsZero() {
		t.Fatalf("balance %+v", b)
	}
	// Pending debits count against the limit.
	if got := create(t, l, tr(3, 3, 2, u(31))); got != TransferExceedsCredits {
		t.Fatalf("got %q", got)
	}
	if got := create(t, l, tr(4, 3, 2, u(30))); got != TransferCreated {
		t.Fatalf("got %q", got)
	}
	// Just before expiry the pending amount still counts; at Timestamp+Timeout it is gone.
	got, _, _ := l.LookupTransfer(context.Background(), u(2))
	clk.ns = int64(got.Timestamp) + 5_000_000_000 - 1
	if b := bal(t, l, 3); b.DebitsPending != u(70) {
		t.Fatalf("before expiry %+v", b)
	}
	clk.ns++
	if b := bal(t, l, 3); !b.DebitsPending.IsZero() || b.DebitsPosted != u(30) {
		t.Fatalf("after expiry %+v", b)
	}
	// Expired amounts no longer count against the limit.
	if got := create(t, l, tr(5, 3, 2, u(70))); got != TransferCreated {
		t.Fatalf("got %q", got)
	}
}

func TestTransfersShareTimestampOrder(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t) // a clock that never moves
	mustAccts(t, l, 1, 2)
	var prev uint64
	for i := uint64(1); i <= 5; i++ {
		mustCreate(t, l, tr(i, 1, 2, u(1)))
		got, _, _ := l.LookupTransfer(ctx, u(i))
		if got.Timestamp <= prev {
			t.Fatalf("transfer %d ts %d <= %d", i, got.Timestamp, prev)
		}
		prev = got.Timestamp
	}
	a, _, _ := l.LookupAccount(ctx, u(2))
	if a.Timestamp >= prev {
		t.Error("account after transfers")
	}
	mustAcct(t, l, 3, 1, 0)
	a, _, _ = l.LookupAccount(ctx, u(3))
	if a.Timestamp <= prev {
		t.Errorf("account ts %d not above transfer ts %d", a.Timestamp, prev)
	}
}

func TestCreateTransferConcurrent(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	mustAccts(t, l, 1, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[CreateTransferResult]int{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.CreateTransfer(ctx, tr(1, 1, 2, u(5)))
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			counts[r]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counts[TransferCreated] != 1 || counts[TransferExists] != 7 {
		t.Errorf("%v", counts)
	}
	if b := bal(t, l, 1); b.DebitsPosted != u(5) {
		t.Errorf("%+v", b)
	}
}

// After a seeded random sequence the books balance per ledger and limits hold.
func TestDoubleEntryInvariant(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	flags := []AccountFlags{0, 0, AccountDebitsMustNotExceedCredits, AccountCreditsMustNotExceedDebits, 0, AccountDebitsMustNotExceedCredits, 0, AccountCreditsMustNotExceedDebits}
	ledgers := []uint32{1, 1, 1, 1, 2, 2, 2, 2}
	for i := range flags {
		mustAcct(t, l, uint64(i+1), ledgers[i], flags[i])
	}
	rng := rand.New(rand.NewSource(7))
	created := map[CreateTransferResult]int{}
	for n := 0; n < 2000; n++ {
		x := Transfer{
			ID:              u(uint64(n + 1)),
			DebitAccountID:  u(uint64(rng.Intn(8) + 1)),
			CreditAccountID: u(uint64(rng.Intn(8) + 1)),
			Amount:          u(uint64(rng.Intn(50))),
			Ledger:          uint32(rng.Intn(2) + 1),
			Code:            1,
		}
		if rng.Intn(4) == 0 {
			x.Flags = TransferPending
			x.Timeout = uint32(rng.Intn(3)) * 100000
		}
		created[create(t, l, x)]++
	}
	if created[TransferCreated] < 200 || created[TransferExceedsCredits] == 0 || created[TransferExceedsDebits] == 0 {
		t.Errorf("weak sequence: %v", created)
	}
	debits := map[uint32]*big.Int{1: new(big.Int), 2: new(big.Int)}
	credits := map[uint32]*big.Int{1: new(big.Int), 2: new(big.Int)}
	pd, pc := new(big.Int), new(big.Int)
	for i := range flags {
		b, err := l.Balance(ctx, u(uint64(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		debits[ledgers[i]].Add(debits[ledgers[i]], toBig(b.DebitsPosted))
		credits[ledgers[i]].Add(credits[ledgers[i]], toBig(b.CreditsPosted))
		pd.Add(pd, toBig(b.DebitsPending))
		pc.Add(pc, toBig(b.CreditsPending))
		d := new(big.Int).Add(toBig(b.DebitsPending), toBig(b.DebitsPosted))
		c := new(big.Int).Add(toBig(b.CreditsPending), toBig(b.CreditsPosted))
		if flags[i] == AccountDebitsMustNotExceedCredits && d.Cmp(toBig(b.CreditsPosted)) > 0 {
			t.Errorf("account %d: debits %v exceed credits %v", i+1, d, b.CreditsPosted)
		}
		if flags[i] == AccountCreditsMustNotExceedDebits && c.Cmp(toBig(b.DebitsPosted)) > 0 {
			t.Errorf("account %d: credits %v exceed debits %v", i+1, c, b.DebitsPosted)
		}
	}
	for _, lg := range []uint32{1, 2} {
		if debits[lg].Cmp(credits[lg]) != 0 {
			t.Errorf("ledger %d: debits %v != credits %v", lg, debits[lg], credits[lg])
		}
	}
	if pd.Cmp(pc) != 0 {
		t.Errorf("pending debits %v != credits %v", pd, pc)
	}
}

// Seeded property: results equal an independent model (big.Int balances, TigerBeetle's check
// order written out again), and stored transfers and balances match.
func TestCreateTransferProperty(t *testing.T) {
	ctx := context.Background()
	l, _ := newTL(t)
	m := newTModel()
	flags := []AccountFlags{0, AccountDebitsMustNotExceedCredits, AccountCreditsMustNotExceedDebits, 0, 0, AccountDebitsMustNotExceedCredits}
	ledgers := []uint32{1, 1, 1, 2, 1, 2}
	for i := range flags {
		mustAcct(t, l, uint64(i+1), ledgers[i], flags[i])
		m.accts[uint64(i+1)] = &mAcct{ledger: ledgers[i], flags: flags[i], dp: new(big.Int), dq: new(big.Int), cp: new(big.Int), cq: new(big.Int)}
	}
	rng := rand.New(rand.NewSource(99))
	run(t, l, m, rng, 4000)
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

func run(t *testing.T, l *Ledger, m *tModel, rng *rand.Rand, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		x := genTransfer(rng)
		want := m.create(x)
		got, err := l.CreateTransfer(context.Background(), x)
		if err != nil || got != want {
			t.Fatalf("%d: %+v got %q %v want %q", i, x, got, err, want)
		}
	}
}

// genTransfer is one random single-phase transfer of the property tests: ids 1 to 120 (now and then 0, the max or
// 77), accounts 1 to 6 (likewise), edge amounts, and now and then a bad timestamp, flag, timeout or pending id.
func genTransfer(rng *rand.Rand) Transfer {
	edge := []U128{U128Zero, u(1), u(2), u(1<<63 - 1), p2(63), u(^uint64(0)), p2(64), mustSub(U128Max, u(1)), U128Max, mustSub(p2(127), u(1)), p2(127)}
	amount := func() U128 {
		if rng.Intn(3) == 0 {
			return edge[rng.Intn(len(edge))]
		}
		return u(uint64(rng.Intn(40)))
	}
	pick := func(n int) U128 {
		switch rng.Intn(25) {
		case 0:
			return U128Zero
		case 1:
			return U128Max
		case 2:
			return u(77)
		}
		return u(uint64(rng.Intn(n) + 1))
	}
	x := Transfer{
		ID:              u(uint64(rng.Intn(120) + 1)),
		DebitAccountID:  pick(6),
		CreditAccountID: pick(6),
		Amount:          amount(),
		Ledger:          uint32(rng.Intn(3)),
		Code:            uint16(rng.Intn(2)),
		UserData64:      uint64(rng.Intn(2)),
		Timeout:         uint32(rng.Intn(2)) * 1000,
	}
	if rng.Intn(25) == 0 {
		x.ID = pick(1)
	}
	if rng.Intn(3) == 0 {
		x.Ledger = 1
	}
	if rng.Intn(3) == 0 {
		x.Code = 1
	}
	switch rng.Intn(6) {
	case 0:
		x.Flags = TransferPending
	case 1:
		x.Flags = TransferFlags(rng.Intn(512)) &^ (TransferPostPendingTransfer | TransferVoidPendingTransfer)
	}
	if rng.Intn(30) == 0 {
		x.Timestamp = 1
	}
	if rng.Intn(30) == 0 {
		x.PendingID = u(3)
	}
	return x
}

type mAcct struct {
	ledger         uint32
	flags          AccountFlags
	dp, dq, cp, cq *big.Int // debits pending/posted, credits pending/posted
}

type tModel struct {
	accts  map[uint64]*mAcct
	stored map[U128]Transfer
	failed map[U128]bool
}

func newTModel() *tModel {
	return &tModel{accts: map[uint64]*mAcct{}, stored: map[U128]Transfer{}, failed: map[U128]bool{}}
}

func over(a, b *big.Int) bool { return new(big.Int).Add(a, b).Cmp(bigMax) > 0 }

// create is the model of single-phase creation (flags pending or none; post/void are never
// generated).
func (m *tModel) create(x Transfer) CreateTransferResult {
	fail := func(r CreateTransferResult, transient bool) CreateTransferResult {
		if transient {
			m.failed[x.ID] = true
		}
		return r
	}
	if x.Timestamp != 0 {
		return TransferTimestampMustBeZero
	}
	if x.Flags&^(TransferPending) != 0 {
		return TransferReservedFlag
	}
	if x.ID.IsZero() {
		return TransferIDMustNotBeZero
	}
	if x.ID.IsMax() {
		return TransferIDMustNotBeIntMax
	}
	if e, ok := m.stored[x.ID]; ok {
		switch {
		case e.Flags != x.Flags:
			return TransferExistsWithDifferentFlags
		case e.PendingID != x.PendingID:
			return TransferExistsWithDifferentPendingID
		case e.Timeout != x.Timeout:
			return TransferExistsWithDifferentTimeout
		case e.DebitAccountID != x.DebitAccountID:
			return TransferExistsWithDifferentDebitAccountID
		case e.CreditAccountID != x.CreditAccountID:
			return TransferExistsWithDifferentCreditAccountID
		case e.Amount != x.Amount:
			return TransferExistsWithDifferentAmount
		case e.UserData128 != x.UserData128:
			return TransferExistsWithDifferentUserData128
		case e.UserData64 != x.UserData64:
			return TransferExistsWithDifferentUserData64
		case e.UserData32 != x.UserData32:
			return TransferExistsWithDifferentUserData32
		case e.Ledger != x.Ledger:
			return TransferExistsWithDifferentLedger
		case e.Code != x.Code:
			return TransferExistsWithDifferentCode
		}
		return TransferExists
	}
	if m.failed[x.ID] {
		return TransferIDAlreadyFailed
	}
	switch {
	case x.DebitAccountID.IsZero():
		return TransferDebitAccountIDMustNotBeZero
	case x.DebitAccountID.IsMax():
		return TransferDebitAccountIDMustNotBeIntMax
	case x.CreditAccountID.IsZero():
		return TransferCreditAccountIDMustNotBeZero
	case x.CreditAccountID.IsMax():
		return TransferCreditAccountIDMustNotBeIntMax
	case x.CreditAccountID == x.DebitAccountID:
		return TransferAccountsMustBeDifferent
	case !x.PendingID.IsZero():
		return TransferPendingIDMustBeZero
	case x.Flags&TransferPending == 0 && x.Timeout != 0:
		return TransferTimeoutReservedForPendingTransfer
	case x.Ledger == 0:
		return TransferLedgerMustNotBeZero
	case x.Code == 0:
		return TransferCodeMustNotBeZero
	}
	dr, ok := m.accts[x.DebitAccountID.Lo]
	if !ok || x.DebitAccountID.Hi != 0 {
		return fail(TransferDebitAccountNotFound, true)
	}
	cr, ok := m.accts[x.CreditAccountID.Lo]
	if !ok || x.CreditAccountID.Hi != 0 {
		return fail(TransferCreditAccountNotFound, true)
	}
	if dr.ledger != cr.ledger {
		return TransferAccountsMustHaveTheSameLedger
	}
	if x.Ledger != dr.ledger {
		return TransferTransferMustHaveTheSameLedgerAsAccounts
	}
	amt := toBig(x.Amount)
	pending := x.Flags&TransferPending != 0
	if pending {
		if over(amt, dr.dp) {
			return TransferOverflowsDebitsPending
		}
		if over(amt, cr.cp) {
			return TransferOverflowsCreditsPending
		}
	}
	if over(amt, dr.dq) {
		return TransferOverflowsDebitsPosted
	}
	if over(amt, cr.cq) {
		return TransferOverflowsCreditsPosted
	}
	if over(amt, new(big.Int).Add(dr.dp, dr.dq)) {
		return TransferOverflowsDebits
	}
	if over(amt, new(big.Int).Add(cr.cp, cr.cq)) {
		return TransferOverflowsCredits
	}
	// Timeouts never expire in this test (fixed clock, whole seconds).
	if dr.flags&AccountDebitsMustNotExceedCredits != 0 &&
		new(big.Int).Add(new(big.Int).Add(dr.dp, dr.dq), amt).Cmp(dr.cq) > 0 {
		return fail(TransferExceedsCredits, true)
	}
	if cr.flags&AccountCreditsMustNotExceedDebits != 0 &&
		new(big.Int).Add(new(big.Int).Add(cr.cp, cr.cq), amt).Cmp(cr.dq) > 0 {
		return fail(TransferExceedsDebits, true)
	}
	if pending {
		dr.dp.Add(dr.dp, amt)
		cr.cp.Add(cr.cp, amt)
	} else {
		dr.dq.Add(dr.dq, amt)
		cr.cq.Add(cr.cq, amt)
	}
	m.stored[x.ID] = x
	return TransferCreated
}

// transferResultNames lists every CreateTransferResult constant declared in types.go (parsed from
// the source, so a code added there is covered without editing a list here).
func transferResultNames(t testing.TB) map[CreateTransferResult]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[CreateTransferResult]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			v := sp.(*ast.ValueSpec)
			id, ok := v.Type.(*ast.Ident)
			if !ok || id.Name != "CreateTransferResult" {
				continue
			}
			lit := v.Values[0].(*ast.BasicLit)
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			out[CreateTransferResult(s)] = v.Names[0].Name
		}
	}
	if len(out) < 50 {
		t.Fatalf("parsed only %d result codes", len(out))
	}
	return out
}

// isTransient is pinned to TigerBeetle's CreateTransferStatus.transient() (tigerbeetle.zig:322),
// restricted to our subset, over every result code. The closed-account codes are not reachable.
func TestIsTransientPinned(t *testing.T) {
	want := map[CreateTransferResult]bool{
		TransferDebitAccountNotFound: true, TransferCreditAccountNotFound: true,
		TransferPendingTransferNotFound: true,
		TransferExceedsCredits:          true, TransferExceedsDebits: true,
	}
	names := transferResultNames(t)
	for r := range want {
		if _, ok := names[r]; !ok {
			t.Errorf("%q is not a declared result", r)
		}
	}
	for r, name := range names {
		if isTransient(r) != want[r] {
			t.Errorf("isTransient(%s = %q) = %v, want %v", name, r, isTransient(r), want[r])
		}
	}
}

// A post/void request is judged by the id checks first: exists and id_already_failed come
// before the post/void checks.
func TestTwoPhaseAfterIDChecks(t *testing.T) {
	l, _ := newTL(t)
	mustAccts(t, l, 1, 2)
	mustCreate(t, l, tr(1, 1, 2, u(1)))
	if got := create(t, l, tr(9, 99, 2, u(1))); got != TransferDebitAccountNotFound {
		t.Fatal(got)
	}
	for _, f := range []TransferFlags{TransferPostPendingTransfer, TransferVoidPendingTransfer} {
		x := tr(1, 1, 2, u(1))
		x.Flags = f
		if r, err := l.CreateTransfer(context.Background(), x); err != nil || r != TransferExistsWithDifferentFlags {
			t.Errorf("existing id, flags %d: %q %v", f, r, err)
		}
		y := tr(9, 1, 2, u(1))
		y.Flags = f
		if r, err := l.CreateTransfer(context.Background(), y); err != nil || r != TransferIDAlreadyFailed {
			t.Errorf("failed id, flags %d: %q %v", f, r, err)
		}
	}
}

// Balance judges expiry against ledger time, never a clock that stepped back, or a stepped-back clock would bring an expired pending transfer
// back to life.
func TestBalanceClockStepBack(t *testing.T) {
	ctx := context.Background()
	t.Run("probe A: expired pending stays expired, limit respected", func(t *testing.T) {
		l, clk := newTL(t)
		mustAccts(t, l, 1, 2)
		mustAcct(t, l, 3, 1, AccountDebitsMustNotExceedCredits)
		mustCreate(t, l, tr(1, 1, 3, u(100)))
		p := tr(2, 3, 2, u(70))
		p.Flags, p.Timeout = TransferPending, 5
		mustCreate(t, l, p)
		clk.advance(6 * time.Second)
		mustCreate(t, l, tr(3, 3, 2, u(100)))
		clk.advance(-10 * time.Second)
		b := bal(t, l, 3)
		if !b.DebitsPending.IsZero() || b.DebitsPosted != u(100) || b.CreditsPosted != u(100) {
			t.Fatalf("balance %+v", b)
		}
	})
	t.Run("probe B: no overflow after step back", func(t *testing.T) {
		l, clk := newTL(t)
		mustAccts(t, l, 1, 2, 3)
		p := tr(1, 1, 2, U128Max)
		p.Flags, p.Timeout = TransferPending, 5
		mustCreate(t, l, p)
		clk.advance(6 * time.Second)
		q := tr(2, 1, 3, U128Max)
		q.Flags, q.Timeout = TransferPending, 5
		mustCreate(t, l, q)
		clk.advance(-10 * time.Second)
		b, err := l.Balance(ctx, u(1))
		if err != nil || b.DebitsPending != U128Max {
			t.Fatalf("%+v %v", b, err)
		}
	})
	t.Run("clock before 1970", func(t *testing.T) {
		l, clk := newTL(t)
		mustAccts(t, l, 1, 2)
		p := tr(1, 1, 2, u(7))
		p.Flags, p.Timeout = TransferPending, 5
		mustCreate(t, l, p)
		clk.ns = -5_000_000_000
		b := bal(t, l, 1)
		if b.DebitsPending != u(7) {
			t.Fatalf("%+v", b)
		}
		clk.ns = math.MinInt64
		if b := bal(t, l, 1); b.DebitsPending != u(7) {
			t.Fatalf("%+v", b)
		}
	})
}
