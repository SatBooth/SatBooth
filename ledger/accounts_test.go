package ledger

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func goodAccount() Account {
	return Account{ID: U128From64(1), UserData128: U128{7, 8}, UserData64: 9, UserData32: 10, Ledger: 1, Code: 2}
}

// TestCreateAccountResults has one case per result code, in create_accounts.md's order.
func TestCreateAccountResults(t *testing.T) {
	ctx := context.Background()
	mod := func(f func(*Account)) Account { a := goodAccount(); f(&a); return a }
	existing := goodAccount()
	existing.ID = U128From64(100)
	existing.Flags = AccountDebitsMustNotExceedCredits

	cases := []struct {
		name string
		acct Account
		want CreateAccountResult
	}{
		{"created", goodAccount(), AccountCreated},
		{"timestamp_must_be_zero", mod(func(a *Account) { a.Timestamp = 5 }), AccountTimestampMustBeZero},
		{"reserved_flag linked", mod(func(a *Account) { a.Flags = 1 << 0 }), AccountReservedFlag},
		{"reserved_flag history", mod(func(a *Account) { a.Flags = 1 << 3 }), AccountReservedFlag},
		{"reserved_flag imported", mod(func(a *Account) { a.Flags = 1 << 4 }), AccountReservedFlag},
		{"reserved_flag closed", mod(func(a *Account) { a.Flags = 1 << 5 }), AccountReservedFlag},
		{"reserved_flag top bit", mod(func(a *Account) { a.Flags = 1 << 15 }), AccountReservedFlag},
		{"id_must_not_be_zero", mod(func(a *Account) { a.ID = U128Zero }), AccountIDMustNotBeZero},
		{"id_must_not_be_int_max", mod(func(a *Account) { a.ID = U128Max }), AccountIDMustNotBeIntMax},
		{"exists_with_different_flags", mod(func(a *Account) { *a = existing; a.Flags = AccountCreditsMustNotExceedDebits }), AccountExistsWithDifferentFlags},
		{"multi-field differ: flags and ledger", mod(func(a *Account) { *a = existing; a.Flags = AccountCreditsMustNotExceedDebits; a.Ledger++ }), AccountExistsWithDifferentFlags},
		{"multi-field differ: ledger and code", mod(func(a *Account) { *a = existing; a.Ledger++; a.Code++ }), AccountExistsWithDifferentLedger},
		{"exists_with_different_user_data_128", mod(func(a *Account) { *a = existing; a.UserData128 = U128{1, 1} }), AccountExistsWithDifferentUserData128},
		{"exists_with_different_user_data_64", mod(func(a *Account) { *a = existing; a.UserData64++ }), AccountExistsWithDifferentUserData64},
		{"exists_with_different_user_data_32", mod(func(a *Account) { *a = existing; a.UserData32++ }), AccountExistsWithDifferentUserData32},
		{"exists_with_different_ledger", mod(func(a *Account) { *a = existing; a.Ledger++ }), AccountExistsWithDifferentLedger},
		{"exists_with_different_code", mod(func(a *Account) { *a = existing; a.Code++ }), AccountExistsWithDifferentCode},
		{"exists", existing, AccountExists},
		{"flags_are_mutually_exclusive", mod(func(a *Account) {
			a.ID = U128From64(2)
			a.Flags = AccountDebitsMustNotExceedCredits | AccountCreditsMustNotExceedDebits
		}), AccountFlagsAreMutuallyExclusive},
		{"ledger_must_not_be_zero", mod(func(a *Account) { a.ID = U128From64(3); a.Ledger = 0 }), AccountLedgerMustNotBeZero},
		{"code_must_not_be_zero", mod(func(a *Account) { a.ID = U128From64(4); a.Code = 0 }), AccountCodeMustNotBeZero},
	}
	l, _ := freshLedger(t)
	if r, err := l.CreateAccount(ctx, existing); err != nil || r != AccountCreated {
		t.Fatalf("seed: %v %v", r, err)
	}
	for _, c := range cases {
		got, err := l.CreateAccount(ctx, c.acct)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	// A refused account must not have been stored.
	for _, id := range []uint64{2, 3, 4} {
		if _, ok, _ := l.LookupAccount(ctx, U128From64(id)); ok {
			t.Errorf("account %d stored despite refusal", id)
		}
	}
}

// Check order: an earlier failing check wins over a later one.
func TestCreateAccountCheckOrder(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	a := Account{ID: U128Zero, Timestamp: 1, Flags: 1, Ledger: 0, Code: 0}
	if r, _ := l.CreateAccount(ctx, a); r != AccountTimestampMustBeZero {
		t.Errorf("got %q", r)
	}
	a.Timestamp = 0
	if r, _ := l.CreateAccount(ctx, a); r != AccountReservedFlag {
		t.Errorf("got %q", r)
	}
	a.Flags = 0
	if r, _ := l.CreateAccount(ctx, a); r != AccountIDMustNotBeZero {
		t.Errorf("got %q", r)
	}
	a.ID = U128From64(1)
	a.Flags = AccountDebitsMustNotExceedCredits | AccountCreditsMustNotExceedDebits
	if r, _ := l.CreateAccount(ctx, a); r != AccountFlagsAreMutuallyExclusive {
		t.Errorf("got %q", r)
	}
	a.Flags = 0
	if r, _ := l.CreateAccount(ctx, a); r != AccountLedgerMustNotBeZero {
		t.Errorf("got %q", r)
	}
	// An id at the 64-bit edge works and round-trips with user data at the edges.
	b := Account{ID: U128{^uint64(0), 0}, UserData128: U128Max, UserData64: ^uint64(0), UserData32: ^uint32(0), Ledger: ^uint32(0), Code: ^uint16(0), Flags: AccountCreditsMustNotExceedDebits}
	if r, err := l.CreateAccount(ctx, b); err != nil || r != AccountCreated {
		t.Fatalf("%q %v", r, err)
	}
	got, ok, err := l.LookupAccount(ctx, b.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	b.Timestamp = got.Timestamp
	if got != b || got.Timestamp == 0 {
		t.Errorf("round trip: %+v != %+v", got, b)
	}
}

func TestCreateAccountIdempotent(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	a := goodAccount()
	if r, err := l.CreateAccount(ctx, a); err != nil || r != AccountCreated {
		t.Fatal(r, err)
	}
	first, ok, err := l.LookupAccount(ctx, a.ID)
	if err != nil || !ok || first.Timestamp == 0 {
		t.Fatal(first, ok, err)
	}
	if r, err := l.CreateAccount(ctx, a); err != nil || r != AccountExists {
		t.Fatal(r, err)
	}
	second, _, _ := l.LookupAccount(ctx, a.ID)
	if second != first {
		t.Errorf("changed: %+v -> %+v", first, second)
	}
	if _, ok, err := l.LookupAccount(ctx, U128From64(99)); ok || err != nil {
		t.Errorf("missing account: %v %v", ok, err)
	}
}

func TestAccountTimestampsIncrease(t *testing.T) {
	ctx := context.Background()
	// A clock that steps backwards, then stands still.
	times := []int64{1000, 500, 500, 500, 2000, 1}
	var i int
	clock := func() time.Time {
		v := times[i%len(times)]
		i++
		return time.Unix(0, v)
	}
	path := t.TempDir() + "/l.db"
	l, err := Open(ctx, path, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var prev uint64
	for n := 1; n <= 12; n++ {
		a := goodAccount()
		a.ID = U128From64(uint64(n))
		if r, err := l.CreateAccount(ctx, a); err != nil || r != AccountCreated {
			t.Fatal(r, err)
		}
		got, _, _ := l.LookupAccount(ctx, a.ID)
		if got.Timestamp <= prev {
			t.Fatalf("account %d: timestamp %d not above %d", n, got.Timestamp, prev)
		}
		prev = got.Timestamp
	}
}

// Timestamps are strictly increasing across accounts and transfers together.
func TestAccountTimestampAboveTransfers(t *testing.T) {
	ctx := context.Background()
	l, err := Open(ctx, t.TempDir()+"/l.db", WithClock(func() time.Time { return time.Unix(0, 1000) }))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, n := range []uint64{1, 2} {
		a := goodAccount()
		a.ID = U128From64(n)
		if r, err := l.CreateAccount(ctx, a); err != nil || r != AccountCreated {
			t.Fatal(r, err)
		}
	}
	// A transfer far in the future, inserted directly.
	id77 := U128From64(77).Bytes()
	d, c, amt, z, ud := U128From64(1).Bytes(), U128From64(2).Bytes(), U128From64(5).Bytes(), U128Zero.Bytes(), make([]byte, 8)
	// Bytes() returns an array, so slices of the arrays are bound.
	if _, err := l.db.ExecContext(ctx, `INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id,
		user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 1, 0, 50000)`,
		id77[:], d[:], c[:], amt[:], z[:], z[:], ud); err != nil {
		t.Fatal(err)
	}
	a := goodAccount()
	a.ID = U128From64(3)
	if r, err := l.CreateAccount(ctx, a); err != nil || r != AccountCreated {
		t.Fatal(r, err)
	}
	got, _, _ := l.LookupAccount(ctx, a.ID)
	if got.Timestamp != 50001 {
		t.Errorf("timestamp %d, want 50001", got.Timestamp)
	}
}

// Concurrent creators of the same id: exactly one created, the rest exists.
func TestCreateAccountConcurrent(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	var wg sync.WaitGroup
	res := make([]CreateAccountResult, 8)
	for i := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.CreateAccount(ctx, goodAccount())
			if err != nil {
				t.Error(err)
			}
			res[i] = r
		}()
	}
	wg.Wait()
	created := 0
	for _, r := range res {
		switch r {
		case AccountCreated:
			created++
		case AccountExists:
		default:
			t.Errorf("unexpected %q", r)
		}
	}
	if created != 1 {
		t.Errorf("created %d times", created)
	}
}

// Seeded property: for random accounts, the result equals an independent model, and what is
// stored round-trips.
func TestCreateAccountProperty(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	rng := rand.New(rand.NewSource(42))
	edge64 := []uint64{0, 1, 1<<63 - 1, 1 << 63, ^uint64(0)}
	r64 := func() uint64 {
		if rng.Intn(2) == 0 {
			return edge64[rng.Intn(len(edge64))]
		}
		return rng.Uint64()
	}
	model := map[U128]Account{}
	for n := 0; n < 3000; n++ {
		a := Account{
			ID:          U128{r64() % 3, r64() % 4},
			UserData128: U128{r64() % 2, r64() % 2},
			UserData64:  r64() % 2,
			UserData32:  uint32(rng.Intn(2)),
			Ledger:      uint32(rng.Intn(3)),
			Code:        uint16(rng.Intn(3)),
			Flags:       AccountFlags(rng.Intn(64)),
		}
		if rng.Intn(20) == 0 {
			a.Timestamp = r64()
		}
		if rng.Intn(20) == 0 {
			a.ID = U128Max
		}
		want := modelCreate(model, a)
		got, err := l.CreateAccount(ctx, a)
		if err != nil || got != want {
			t.Fatalf("%d: %+v got %q %v want %q", n, a, got, err, want)
		}
	}
	for id, a := range model {
		got, ok, err := l.LookupAccount(ctx, id)
		a.Timestamp = got.Timestamp
		if err != nil || !ok || got != a || got.Timestamp == 0 {
			t.Fatalf("lookup %v: %+v vs %+v", id, got, a)
		}
	}
}

func modelCreate(m map[U128]Account, a Account) CreateAccountResult {
	if a.Timestamp != 0 {
		return AccountTimestampMustBeZero
	}
	if a.Flags&^6 != 0 {
		return AccountReservedFlag
	}
	if a.ID == U128Zero {
		return AccountIDMustNotBeZero
	}
	if a.ID == U128Max {
		return AccountIDMustNotBeIntMax
	}
	if e, ok := m[a.ID]; ok {
		switch {
		case e.Flags != a.Flags:
			return AccountExistsWithDifferentFlags
		case e.UserData128 != a.UserData128:
			return AccountExistsWithDifferentUserData128
		case e.UserData64 != a.UserData64:
			return AccountExistsWithDifferentUserData64
		case e.UserData32 != a.UserData32:
			return AccountExistsWithDifferentUserData32
		case e.Ledger != a.Ledger:
			return AccountExistsWithDifferentLedger
		case e.Code != a.Code:
			return AccountExistsWithDifferentCode
		}
		return AccountExists
	}
	if a.Flags == 6 {
		return AccountFlagsAreMutuallyExclusive
	}
	if a.Ledger == 0 {
		return AccountLedgerMustNotBeZero
	}
	if a.Code == 0 {
		return AccountCodeMustNotBeZero
	}
	m[a.ID] = a
	return AccountCreated
}
