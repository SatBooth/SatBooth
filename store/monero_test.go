package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// pay is a payment of pico piconero by the transaction txid(d) to sub(i).
func pay(d byte, i int, pico int64) Payment {
	return Payment{TxID: txid(d), Address: sub(i), AmountPiconero: pico}
}

// moneroScan records ps at height h and fails the test on an error.
func moneroScan(t *testing.T, s *Store, h int64, ps ...Payment) []Payment {
	t.Helper()
	added, err := s.RecordMoneroScan(bg, ps, MoneroCursor{Height: h, WalletHeight: h})
	if err != nil {
		t.Fatal(err)
	}
	return added
}

// moneroRows is the number of stored payments and cursor rows.
func moneroRows(t *testing.T, s *Store) (payments, cursors int) {
	t.Helper()
	return countRows(t, s, "monero_payments"), countRows(t, s, "monero_cursor")
}

func TestValidSubaddressBoundaries(t *testing.T) {
	ok := sub(1)
	for _, c := range []struct {
		s  string
		ok bool
	}{
		{ok, true}, {"B" + ok[1:], true}, {"7" + ok[1:], true},
		{"4" + ok[1:], false}, {"5" + ok[1:], false}, {"9" + ok[1:], false}, {"A" + ok[1:], false}, {"b" + ok[1:], false},
		{ok[:94], false}, {ok + "u", false}, {"", false}, {"8", false},
		{ok[:50] + "0" + ok[51:], false}, {ok[:50] + "O" + ok[51:], false}, {ok[:50] + "I" + ok[51:], false}, {ok[:50] + "l" + ok[51:], false},
		{ok[:50] + "\x00" + ok[51:], false}, {ok[:50] + " " + ok[51:], false}, {ok[:94] + "\xff", false},
		{"8" + strings.Repeat("z", 94), true}, {"8" + strings.Repeat("1", 94), true}, {"8" + strings.Repeat("9", 94), true},
		{"8" + strings.Repeat("Z", 94), true}, {"8" + strings.Repeat("H", 93) + "J", true},
	} {
		if got := ValidSubaddress(c.s); got != c.ok {
			t.Errorf("ValidSubaddress(%q) = %v, want %v", c.s, got, c.ok)
		}
	}
	if MaxSubaddressIdx != 2147483647 {
		t.Fatalf("MaxSubaddressIdx = %d", MaxSubaddressIdx)
	}
}

// The bound, on a raw connection: 2^53 − 1 piconero is stored, 2^53 is refused by the CHECK, on orders
// and on monero_payments alike; in Go too.
func TestPiconeroBoundInSQL(t *testing.T) {
	s, path, _ := freshStore(t)
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	const max, over = int64(9007199254740991), int64(9007199254740992)
	if MaxPiconero != max || !validPiconero(max) || validPiconero(over) || validPiconero(0) {
		t.Fatal("the Go bound is not 2^53 - 1")
	}
	ins := `INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES (?, ?, 'a', ?, 0, 0, ?, 5, 6)`
	if _, err := raw.Exec(ins, strings.Repeat("a", 32), strings.Repeat("b", 32), "over", over); err == nil {
		t.Fatal("orders accepted 2^53 piconero")
	}
	if _, err := raw.Exec(ins, strings.Repeat("a", 32), strings.Repeat("b", 32), "max", max); err != nil {
		t.Fatalf("orders refused 2^53 - 1 piconero: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO monero_subaddresses VALUES (?, ?, 1, 5)`, sub(1), strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	pins := `INSERT INTO monero_payments VALUES (?, ?, ?, 0, 5, 0)`
	if _, err := raw.Exec(pins, txid('a'), sub(1), over); err == nil {
		t.Fatal("monero_payments accepted 2^53 piconero")
	}
	if _, err := raw.Exec(pins, txid('a'), sub(1), max); err != nil {
		t.Fatalf("monero_payments refused 2^53 - 1 piconero: %v", err)
	}
	if p, err := s.Payment(bg, PaymentKey{TxID: txid('a'), Address: sub(1)}); err != nil || p.AmountPiconero != max {
		t.Fatalf("read back %+v %v", p, err)
	}
	if o, err := s.OrderByID(bg, strings.Repeat("a", 32)); err != nil || o.AmountPiconero != max {
		t.Fatalf("order read back %+v %v", o, err)
	}
	// Through the store: the bound order and payment, one above refused before anything is written.
	o := mkMoneroOrder(t, s, "max2", max, t0.Add(time.Hour))
	mkSub(t, s, o, 2)
	if _, err := s.RecordMoneroScan(bg, []Payment{pay('b', 2, over)}, MoneroCursor{Height: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RecordMoneroScan 2^53: %v", err)
	}
	if added := moneroScan(t, s, 1, pay('b', 2, max)); len(added) != 1 || added[0].AmountPiconero != max {
		t.Fatalf("RecordMoneroScan 2^53 - 1: %+v", added)
	}
}

func TestAddSubaddress(t *testing.T) {
	s, _, clk := freshStore(t)
	o1 := mkMoneroOrder(t, s, "r1", 1000, t0.Add(time.Hour))
	o2 := mkMoneroOrder(t, s, "r2", 1000, t0.Add(time.Hour))
	if _, ok, err := s.MaxSubaddressIndex(bg); ok || err != nil {
		t.Fatalf("empty max: %v %v", ok, err)
	}
	clk.set(t0.Add(time.Second))
	a1 := mkSub(t, s, o1, 1)
	got, err := s.SubaddressForOrder(bg, o1.ID)
	if err != nil || got.Address != a1.Address || got.Index != 1 || got.OrderID != o1.ID || !got.CreatedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("SubaddressForOrder: %+v %v", got, err)
	}
	for _, id := range []string{o2.ID, "nope", strings.ToUpper(o1.ID)} {
		if _, err := s.SubaddressForOrder(bg, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("no subaddress %q: %v", id, err)
		}
	}
	// A second subaddress for the order; the same subaddress for another order.
	if err := s.AddSubaddress(bg, Subaddress{OrderID: o1.ID, Address: sub(5), Index: 5}); !errors.Is(err, ErrSubaddressTaken) {
		t.Fatalf("second for order: %v", err)
	}
	if err := s.AddSubaddress(bg, Subaddress{OrderID: o2.ID, Address: a1.Address, Index: 5}); !errors.Is(err, ErrSubaddressTaken) ||
		err.Error() != "store: the order has a subaddress, or the subaddress is already stored" {
		t.Fatalf("same subaddress: %v", err)
	}
	if err := s.AddSubaddress(bg, Subaddress{OrderID: strings.Repeat("e", 32), Address: sub(7), Index: 7}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	// Equal to the maximum is ErrIndexNotAbove; index 0 (the primary address), negative, above 2^31 − 1 and a
	// malformed subaddress are ErrInvalid but not ErrIndexNotAbove.
	if err := s.AddSubaddress(bg, Subaddress{OrderID: o2.ID, Address: sub(8), Index: 1}); !errors.Is(err, ErrInvalid) || !errors.Is(err, ErrIndexNotAbove) {
		t.Fatalf("index = max: %v", err)
	}
	for name, a := range map[string]Subaddress{
		"index 0":       {OrderID: o2.ID, Address: sub(8), Index: 0},
		"index -1":      {OrderID: o2.ID, Address: sub(8), Index: -1},
		"index 2^31":    {OrderID: o2.ID, Address: sub(8), Index: 1 << 31},
		"bad address":   {OrderID: o2.ID, Address: "bad", Index: 2},
		"primary":       {OrderID: o2.ID, Address: "4" + sub(8)[1:], Index: 2},
		"bad order id":  {OrderID: "x", Address: sub(8), Index: 2},
		"on-chain addr": {OrderID: o2.ID, Address: addr(8), Index: 2},
	} {
		if err := s.AddSubaddress(bg, a); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIndexNotAbove) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.AddSubaddress(bg, Subaddress{OrderID: o2.ID, Address: sub(9), Index: MaxSubaddressIdx}); err != nil {
		t.Fatalf("max index: %v", err)
	}
	if idx, ok, err := s.MaxSubaddressIndex(bg); idx != MaxSubaddressIdx || !ok || err != nil {
		t.Fatalf("max: %d %v %v", idx, ok, err)
	}
	// An order without a Monero amount takes no subaddress, in Go and by the trigger.
	ln := mkBothOrder(t, s, "ln", 1000, 500, t0.Add(time.Hour))
	if err := s.AddSubaddress(bg, Subaddress{OrderID: ln.ID, Address: sub(10), Index: MaxSubaddressIdx}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIndexNotAbove) {
		t.Fatalf("order without Monero: %v", err)
	}
	// The triggers, on a raw connection: a fresh store so the indexes are small.
	s2, path2, _ := freshStore(t)
	p1 := mkMoneroOrder(t, s2, "p1", 1000, t0.Add(time.Hour))
	p2 := mkMoneroOrder(t, s2, "p2", 1000, t0.Add(time.Hour))
	lnOnly := mkOrder(t, s2, "lnonly", 1000, t0.Add(time.Hour))
	raw2, err := openRawForTest(path2)
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	if _, err := raw2.Exec(`INSERT INTO monero_subaddresses VALUES (?, ?, 0, 5)`, sub(1), p1.ID); err == nil {
		t.Error("SQL accepted index 0 on an empty table")
	}
	if _, err := raw2.Exec(`INSERT INTO monero_subaddresses VALUES (?, ?, 1, 5)`, sub(1), lnOnly.ID); err == nil {
		t.Error("trigger accepted a subaddress for an order without a Monero amount")
	}
	if _, err := raw2.Exec(`INSERT INTO monero_subaddresses VALUES (?, ?, 1, 5)`, sub(1), p1.ID); err != nil {
		t.Fatalf("index 1: %v", err)
	}
	if _, err := raw2.Exec(`INSERT INTO monero_subaddresses VALUES (?, ?, 1, 5)`, sub(2), p2.ID); err == nil {
		t.Error("trigger accepted an index equal to the maximum")
	}
	if _, err := raw2.Exec(`INSERT INTO monero_subaddresses VALUES (?, ?, 3, 5)`, sub(2), p2.ID); err != nil {
		t.Errorf("trigger refused index max+2: %v", err)
	}
	m, err := s.SubaddressesByAddress(bg, []string{sub(1), sub(9), sub(77), "bad"})
	if err != nil || len(m) != 2 || m[sub(9)].OrderID != o2.ID || m[sub(9)].Index != MaxSubaddressIdx || m[sub(1)].OrderID != o1.ID {
		t.Fatalf("SubaddressesByAddress: %+v %v", m, err)
	}
	if m, err := s.SubaddressesByAddress(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("empty: %+v %v", m, err)
	}
	// More than one batch.
	var many []string
	for i := 0; i < 1100; i++ {
		many = append(many, sub(1000+i))
	}
	many = append(many, sub(1))
	if m, err := s.SubaddressesByAddress(bg, many); err != nil || len(m) != 1 {
		t.Fatalf("batches: %d %v", len(m), err)
	}
}

func TestUnusedSubaddressRun(t *testing.T) {
	s, _, _ := freshStore(t)
	if n, err := s.UnusedSubaddressRun(bg); n != 0 || err != nil {
		t.Fatalf("none: %d %v", n, err)
	}
	for i := 1; i <= 5; i++ {
		mkSub(t, s, mkMoneroOrder(t, s, "r"+itoa(int64(i)), 1000, t0.Add(time.Hour)), int64(i))
	}
	if n, err := s.UnusedSubaddressRun(bg); n != 5 || err != nil {
		t.Fatalf("1..5, none paid: %d %v", n, err)
	}
	moneroScan(t, s, 1, pay('a', 2, 1000))
	if n, err := s.UnusedSubaddressRun(bg); n != 3 || err != nil {
		t.Fatalf("payment at 2: %d %v", n, err)
	}
	moneroScan(t, s, 1, pay('b', 1, 1000)) // a payment at a lower index does not change it
	if n, _ := s.UnusedSubaddressRun(bg); n != 3 {
		t.Fatalf("payment at 1: %d", n)
	}
	moneroScan(t, s, 1, pay('c', 5, 1000))
	if n, _ := s.UnusedSubaddressRun(bg); n != 0 {
		t.Fatalf("payment at 5: %d", n)
	}
}

func TestRecordMoneroScan(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkMoneroOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 1)
	o2 := mkMoneroOrder(t, s, "r2", 1000, t0.Add(time.Hour))
	mkSub(t, s, o2, 2)
	if _, ok, err := s.MoneroCursor(bg); ok || err != nil {
		t.Fatalf("cursor before: %v %v", ok, err)
	}
	// The caller's FirstSeen, FirstSeenHeight and Confirmed are not read: the clock and the cursor stamp the rows.
	clk.set(t0.Add(time.Minute))
	first := []Payment{
		{TxID: txid('a'), Address: sub(1), AmountPiconero: 10, UnlockTime: 0, FirstSeen: t0.Add(-time.Hour), FirstSeenHeight: 3, Confirmed: true},
		{TxID: txid('a'), Address: sub(2), AmountPiconero: 11, UnlockTime: 1500},
		{TxID: txid('b'), Address: sub(1), AmountPiconero: 12},
	}
	added, err := s.RecordMoneroScan(bg, first, MoneroCursor{Height: 100, WalletHeight: 100})
	if err != nil || len(added) != 3 {
		t.Fatalf("first scan: %+v %v", added, err)
	}
	for _, p := range added {
		if !p.FirstSeen.Equal(t0.Add(time.Minute)) || p.FirstSeenHeight != 100 || p.Confirmed {
			t.Fatalf("added %+v", p)
		}
	}
	if p, err := s.Payment(bg, PaymentKey{TxID: txid('a'), Address: sub(2)}); err != nil || p.UnlockTime != 1500 || p.AmountPiconero != 11 ||
		p.FirstSeenHeight != 100 || !p.FirstSeen.Equal(t0.Add(time.Minute)) || p.FirstSeen.Location() != time.UTC {
		t.Fatalf("stored %+v %v", p, err)
	}
	// Replay: nothing added, the rows keep their first-seen time and height, no cursor row at the same height.
	clk.set(t0.Add(2 * time.Minute))
	if added := moneroScan(t, s, 100, first...); len(added) != 0 {
		t.Fatalf("replay added %+v", added)
	}
	if n, c := moneroRows(t, s); n != 3 || c != 1 {
		t.Fatalf("after the replay: %d payments, %d cursors", n, c)
	}
	// A new payment at a later height: one added with that height, one cursor row; the old rows keep theirs.
	second := append(first, pay('c', 2, 7))
	added = moneroScan(t, s, 105, second...)
	if len(added) != 1 || added[0].TxID != txid('c') || added[0].FirstSeenHeight != 105 || !added[0].FirstSeen.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("second scan: %+v", added)
	}
	if p, _ := s.Payment(bg, PaymentKey{TxID: txid('a'), Address: sub(1)}); p.FirstSeenHeight != 100 || !p.FirstSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("old payment changed: %+v", p)
	}
	if _, c := moneroRows(t, s); c != 2 {
		t.Fatalf("cursor rows %d", c)
	}
	// Quiet wallet: same height, nothing new, no row. A new height: one row, even with nothing new; a lower one too.
	moneroScan(t, s, 105)
	if _, c := moneroRows(t, s); c != 2 {
		t.Fatalf("a quiet scan added a row: %d", c)
	}
	for i, h := range []int64{106, 104, 0} {
		moneroScan(t, s, h)
		if c, ok, err := s.MoneroCursor(bg); err != nil || !ok || c.Height != h || !c.At.Equal(t0.Add(2*time.Minute)) {
			t.Fatalf("cursor at %d: %+v %v %v", h, c, ok, err)
		}
		if _, n := moneroRows(t, s); n != 3+i {
			t.Fatalf("height %d: %d cursor rows", h, n)
		}
	}
	// One transaction: an unknown subaddress, or a stored payment with another amount or unlock time, leaves nothing,
	// not even the cursor.
	n0, c0 := moneroRows(t, s)
	for name, c := range map[string]struct {
		ps   []Payment
		want error
		text string
	}{
		"unknown subaddress": {[]Payment{pay('d', 1, 5), pay('d', 42, 5)}, ErrNotFound, "store: not found"},
		// A changed payment is ErrPaymentChanged (an ErrInvalid) naming the txid and the subaddress index only.
		"other amount": {[]Payment{pay('d', 1, 5), {TxID: txid('a'), Address: sub(1), AmountPiconero: 11}}, ErrPaymentChanged,
			"store: invalid argument: a stored payment arrived with another amount or unlock time (txid " + txid('a') + ", subaddress index 1)"},
		"other unlock time": {[]Payment{pay('d', 1, 5), {TxID: txid('a'), Address: sub(2), AmountPiconero: 11, UnlockTime: 1501}}, ErrPaymentChanged,
			"store: invalid argument: a stored payment arrived with another amount or unlock time (txid " + txid('a') + ", subaddress index 2)"},
	} {
		_, err := s.RecordMoneroScan(bg, c.ps, MoneroCursor{Height: 999})
		if !errors.Is(err, c.want) || err.Error() != c.text {
			t.Errorf("%s: %v", name, err)
		}
		if c.want == ErrPaymentChanged && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: not an ErrInvalid: %v", name, err)
		}
		if c.want != ErrPaymentChanged && errors.Is(err, ErrPaymentChanged) {
			t.Errorf("%s: an ErrPaymentChanged: %v", name, err)
		}
		if n, cc := moneroRows(t, s); n != n0 || cc != c0 {
			t.Errorf("%s: written: %d payments, %d cursors", name, n, cc)
		}
	}
	// Refusals before the write.
	if _, err := s.RecordMoneroScan(bg, nil, MoneroCursor{Height: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative height: %v", err)
	}
	for name, p := range map[string]Payment{
		"short txid":     {TxID: "ab", Address: sub(1), AmountPiconero: 1},
		"upper txid":     {TxID: strings.Repeat("A", 64), Address: sub(1), AmountPiconero: 1},
		"amount 0":       {TxID: txid('e'), Address: sub(1)},
		"amount -1":      {TxID: txid('e'), Address: sub(1), AmountPiconero: -1},
		"amount 2^53":    {TxID: txid('e'), Address: sub(1), AmountPiconero: 1 << 53},
		"bad subaddress": {TxID: txid('e'), Address: "bad", AmountPiconero: 1},
		"primary":        {TxID: txid('e'), Address: "4" + sub(1)[1:], AmountPiconero: 1},
		"unlock -1":      {TxID: txid('e'), Address: sub(1), AmountPiconero: 1, UnlockTime: -1},
	} {
		if _, err := s.RecordMoneroScan(bg, []Payment{p}, MoneroCursor{Height: 1}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrPaymentChanged) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n, c := moneroRows(t, s); n != n0 || c != c0 {
		t.Fatalf("refusals wrote: %d payments, %d cursors", n, c)
	}
	// A payment listed twice in one scan is stored once.
	if added := moneroScan(t, s, 106, pay('f', 1, 3), pay('f', 1, 3)); len(added) != 1 {
		t.Fatalf("duplicate in one scan: %+v", added)
	}
	if _, err := s.Payment(bg, PaymentKey{TxID: txid('9'), Address: sub(1)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown payment: %v", err)
	}
	if l, err := s.PaymentsForOrder(bg, o.ID); err != nil || len(l) != 3 || l[0].TxID != txid('a') || l[1].TxID != txid('b') || l[2].TxID != txid('f') {
		t.Fatalf("PaymentsForOrder: %+v %v", l, err)
	}
	if l, err := s.PaymentsForOrder(bg, o2.ID); err != nil || len(l) != 2 || l[0].TxID != txid('a') || l[1].TxID != txid('c') {
		t.Fatalf("PaymentsForOrder o2: %+v %v", l, err)
	}
}

// Ruling 8-A2: the cursor carries the wallet's own height beside where the next scan starts. It is read back, a scan
// that changes only the wallet height adds a row (a wallet catching up below the cursor), one that changes neither
// adds none, and a wallet height below 0 or above the height is refused before the write; every boundary is crossed.
func TestMoneroCursorWalletHeight(t *testing.T) {
	s, _, _ := freshStore(t)
	scan := func(h, wh int64) error {
		_, err := s.RecordMoneroScan(bg, nil, MoneroCursor{Height: h, WalletHeight: wh})
		return err
	}
	for _, c := range []struct {
		h, wh int64
		rows  int
	}{{100, 100, 1}, {100, 100, 1}, {100, 95, 2}, {100, 95, 2}, {100, 99, 3}, {100, 100, 4}, {101, 101, 5}, {101, 0, 6}} {
		if err := scan(c.h, c.wh); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		cur, ok, err := s.MoneroCursor(bg)
		if err != nil || !ok || cur.Height != c.h || cur.WalletHeight != c.wh {
			t.Fatalf("%+v: cursor %+v %v %v", c, cur, ok, err)
		}
		if _, n := moneroRows(t, s); n != c.rows {
			t.Fatalf("%+v: %d cursor rows", c, n)
		}
	}
	_, n0 := moneroRows(t, s)
	for _, c := range []struct{ h, wh int64 }{{101, 102}, {101, -1}, {0, 1}, {5, 1 << 62}, {5, -1 << 62}} {
		if err := scan(c.h, c.wh); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if _, n := moneroRows(t, s); n != n0 {
		t.Fatalf("a refused cursor was written: %d rows, was %d", n, n0)
	}
	// The positive edges: 0 and equal to the height.
	for _, c := range []struct{ h, wh int64 }{{0, 0}, {7, 7}, {7, 0}} {
		if err := scan(c.h, c.wh); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// Ruling R6: a new payment's FirstSeenHeight is the cursor's WalletHeight (the wallet's own height, against which the
// lock rule is judged), not its scan position; an old row keeps its height.
func TestFirstSeenHeightIsTheWalletHeight(t *testing.T) {
	s, _, _ := freshStore(t)
	o := mkMoneroOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 1)
	added, err := s.RecordMoneroScan(bg, []Payment{pay('a', 1, 10)}, MoneroCursor{Height: 200, WalletHeight: 190})
	if err != nil || len(added) != 1 || added[0].FirstSeenHeight != 190 {
		t.Fatalf("added %+v %v: want first seen at the wallet's height 190", added, err)
	}
	if _, err := s.RecordMoneroScan(bg, []Payment{pay('a', 1, 10)}, MoneroCursor{Height: 200, WalletHeight: 195}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Payment(bg, PaymentKey{TxID: txid('a'), Address: sub(1)}); err != nil || p.FirstSeenHeight != 190 {
		t.Fatalf("stored %+v %v: want 190 kept", p, err)
	}
}

// RecordMoneroScan reopens a settled payment whose current mark is dead only when the scan lists it Confirmed (mined):
// a dropped transaction re-relayed to the pool is not reopened.
func TestRecordMoneroScanReopensADeadSettledPayment(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkMoneroOrder(t, s, "reopen", 1000, exp)
	mkSub(t, s, o, 1)
	p := func(d byte, confirmed bool) Payment { x := pay(d, 1, 5); x.Confirmed = confirmed; return x }
	moneroScan(t, s, 1, p('a', false), p('b', false), p('c', false))
	for d, k := range map[byte]string{'a': MarkDead, 'b': MarkMempool, 'c': MarkMined} {
		m := MoneroMark{TxID: txid(d), Address: sub(1), Kind: k}
		if k == MarkMined {
			m.Height = 1
		}
		if err := s.AddMoneroMark(bg, m); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMoneroSettled(bg, PaymentKey{TxID: txid(d), Address: sub(1)}, 6); err != nil {
			t.Fatal(err)
		}
	}
	edge := exp.Add(WatchWindow)
	clk.set(edge)
	watched := func() string {
		t.Helper()
		w, err := s.WatchedPayments(bg, edge)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, x := range w {
			if x.Confirmed {
				t.Errorf("%s returned with Confirmed", x.TxID[:1])
			}
			ids = append(ids, x.TxID[:1])
		}
		return strings.Join(ids, "")
	}
	open := func() bool {
		t.Helper()
		b, err := s.HasOpenMoneroOrder(bg, edge)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if watched() != "" || open() {
		t.Fatal("setup: something watched")
	}
	moneroScan(t, s, 2, p('a', false), p('b', true), p('c', true)) // a in the pool again; b and c not dead
	if watched() != "" || open() {
		t.Fatalf("reopened without a dead mark listed mined: %q", watched())
	}
	moneroScan(t, s, 3, p('a', true))
	if watched() != "a" || !open() {
		t.Fatalf("not reopened: %q", watched())
	}
	moneroScan(t, s, 3, p('a', true)) // again: no settled fact left, nothing to do
	if watched() != "a" {
		t.Fatalf("replay: %q", watched())
	}
	if _, err := s.AddMoneroSettled(bg, PaymentKey{TxID: txid('a'), Address: sub(1)}, 9); err != nil {
		t.Fatal(err)
	}
	if watched() != "" {
		t.Fatalf("settled again: %q", watched())
	}
	if l, _ := s.PaymentsForOrder(bg, o.ID); len(l) != 3 || l[0].Confirmed {
		t.Fatalf("rows %+v", l)
	}
}

// A settled fact may be deleted only while its payment's current mark is dead (the rule of migration 3), on any
// connection; updates and replaces stay refused.
func TestMoneroSettledDeleteOnlyWhenDead(t *testing.T) {
	s, path, _ := freshStore(t)
	o := mkMoneroOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 1)
	keys := map[string]byte{"dead": 'a', "mempool": 'b', "mined": 'c', "unmarked": 'd', "dead then mempool": 'e'}
	for _, d := range keys {
		moneroScan(t, s, 1, pay(d, 1, 5))
	}
	for name, kinds := range map[string][]string{"dead": {MarkDead}, "mempool": {MarkMempool}, "mined": {MarkMined},
		"dead then mempool": {MarkDead, MarkMempool}} {
		for _, k := range kinds {
			m := MoneroMark{TxID: txid(keys[name]), Address: sub(1), Kind: k}
			if k == MarkMined {
				m.Height = 1
			}
			if err := s.AddMoneroMark(bg, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range keys {
		if _, err := s.AddMoneroSettled(bg, PaymentKey{TxID: txid(d), Address: sub(1)}, 6); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for name, d := range keys {
		_, err := raw.Exec(`DELETE FROM monero_settled WHERE txid = ?`, txid(d))
		if (err == nil) != (name == "dead") {
			t.Errorf("%s: delete %v", name, err)
		}
	}
	for name, q := range map[string]string{
		"update":  `UPDATE monero_settled SET height = 8`,
		"replace": `INSERT OR REPLACE INTO monero_settled VALUES ('` + txid('b') + `', '` + sub(1) + `', 7, 5)`,
	} {
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("SQL accepted %s", name)
		}
	}
}

func TestMoneroMarks(t *testing.T) {
	s, path, clk := freshStore(t)
	o := mkMoneroOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 1)
	var keys []PaymentKey
	for i := 0; i < 3; i++ {
		clk.set(t0.Add(time.Duration(i) * time.Minute))
		moneroScan(t, s, int64(i), pay(byte('a'+i), 1, 100))
		keys = append(keys, PaymentKey{TxID: txid(byte('a' + i)), Address: sub(1)})
	}
	clk.set(t0.Add(time.Hour))
	must := func(m MoneroMark) {
		t.Helper()
		if err := s.AddMoneroMark(bg, m); err != nil {
			t.Fatal(err)
		}
	}
	must(MoneroMark{TxID: keys[1].TxID, Address: keys[1].Address, Kind: MarkMempool, Seq: 9, At: t0}) // Seq and At are ignored
	must(MoneroMark{TxID: keys[2].TxID, Address: keys[2].Address, Kind: MarkMempool})
	must(MoneroMark{TxID: keys[2].TxID, Address: keys[2].Address, Kind: MarkMined, Height: 5})
	must(MoneroMark{TxID: keys[2].TxID, Address: keys[2].Address, Kind: MarkDead})
	cm, err := s.CurrentMoneroMarks(bg, keys)
	if err != nil || len(cm) != 2 {
		t.Fatalf("CurrentMoneroMarks: %+v %v", cm, err)
	}
	if m := cm[keys[1]]; m.Seq != 1 || m.Kind != MarkMempool || !m.At.Equal(t0.Add(time.Hour)) || m.Address != sub(1) {
		t.Errorf("payment 1: %+v", m)
	}
	if m := cm[keys[2]]; m.Seq != 3 || m.Kind != MarkDead || m.Height != 0 {
		t.Errorf("payment 2: %+v", m)
	}
	if m, ok, err := s.CurrentMoneroMark(bg, keys[0]); ok || err != nil {
		t.Errorf("none: %+v %v %v", m, ok, err)
	}
	must(MoneroMark{TxID: keys[1].TxID, Address: keys[1].Address, Kind: MarkMined, Height: 9, Confirmations: 4})
	if m, _, _ := s.CurrentMoneroMark(bg, keys[1]); m.Seq != 2 || m.Height != 9 || m.Kind != MarkMined || m.Confirmations != 4 {
		t.Errorf("mined: %+v", m)
	}
	must(MoneroMark{TxID: keys[1].TxID, Address: keys[1].Address, Kind: MarkMined, Height: 9, Confirmations: MaxMoneroConfirmations})
	if cm, _ := s.CurrentMoneroMarks(bg, keys[1:2]); cm[keys[1]].Confirmations != MaxMoneroConfirmations || cm[keys[1]].Seq != 3 {
		t.Errorf("mined at the bound: %+v", cm[keys[1]])
	}
	for name, m := range map[string]MoneroMark{
		"mined no height": {TxID: keys[0].TxID, Address: sub(1), Kind: MarkMined},
		"mempool height":  {TxID: keys[0].TxID, Address: sub(1), Kind: MarkMempool, Height: 1},
		"dead height":     {TxID: keys[0].TxID, Address: sub(1), Kind: MarkDead, Height: 1},
		"bad kind":        {TxID: keys[0].TxID, Address: sub(1), Kind: "x"},
		"mined conf -1":   {TxID: keys[0].TxID, Address: sub(1), Kind: MarkMined, Height: 1, Confirmations: -1},
		"mined conf big":  {TxID: keys[0].TxID, Address: sub(1), Kind: MarkMined, Height: 1, Confirmations: MaxMoneroConfirmations + 1},
		"mempool conf 1":  {TxID: keys[0].TxID, Address: sub(1), Kind: MarkMempool, Confirmations: 1},
		"dead conf 1":     {TxID: keys[0].TxID, Address: sub(1), Kind: MarkDead, Confirmations: 1},
		"bad txid":        {TxID: "ab", Address: sub(1), Kind: MarkDead},
		"bad subaddress":  {TxID: keys[0].TxID, Address: "bad", Kind: MarkDead},
	} {
		if err := s.AddMoneroMark(bg, m); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.AddMoneroMark(bg, MoneroMark{TxID: txid('9'), Address: sub(1), Kind: MarkDead}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown payment: %v", err)
	}
	if err := s.AddMoneroMark(bg, MoneroMark{TxID: keys[0].TxID, Address: sub(2), Kind: MarkDead}); !errors.Is(err, ErrNotFound) {
		t.Errorf("known txid, other subaddress: %v", err)
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for name, q := range map[string]string{
		"mined no height": `INSERT INTO monero_marks VALUES (?, ?, 1, 'mined', 0, 5, 0)`,
		"mempool height":  `INSERT INTO monero_marks VALUES (?, ?, 1, 'mempool', 3, 5, 0)`,
		"bad kind":        `INSERT INTO monero_marks VALUES (?, ?, 1, 'zzz', 0, 5, 0)`,
		"negative height": `INSERT INTO monero_marks VALUES (?, ?, 1, 'mined', -1, 5, 0)`,
		"mined conf -1":   `INSERT INTO monero_marks VALUES (?, ?, 1, 'mined', 1, 5, -1)`,
		"mined conf big":  `INSERT INTO monero_marks VALUES (?, ?, 1, 'mined', 1, 5, 500000001)`,
		"mempool conf 1":  `INSERT INTO monero_marks VALUES (?, ?, 1, 'mempool', 0, 5, 1)`,
		"dead conf 1":     `INSERT INTO monero_marks VALUES (?, ?, 1, 'dead', 0, 5, 1)`,
	} {
		if _, err := raw.Exec(q, keys[0].TxID, sub(1)); err == nil {
			t.Errorf("SQL accepted %s", name)
		}
	}
	if _, err := raw.Exec(`INSERT INTO monero_marks VALUES (?, ?, 1, 'dead', 0, 5, 0)`, txid('9'), sub(1)); err == nil {
		t.Error("SQL accepted a mark of an unknown payment")
	}
	if _, err := raw.Exec(`UPDATE monero_marks SET kind = 'dead'`); err == nil {
		t.Error("SQL accepted an update")
	}
}

// AddMoneroSettled records the fact once; WatchedPayments leaves it out from then on, in any process.
func TestAddMoneroSettled(t *testing.T) {
	s, _, _ := freshStore(t)
	o := mkMoneroOrder(t, s, "settled", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 1)
	o2 := mkMoneroOrder(t, s, "settled2", 1000, t0.Add(time.Hour))
	mkSub(t, s, o2, 2)
	moneroScan(t, s, 1, pay('a', 1, 5), pay('a', 2, 6))
	k := PaymentKey{TxID: txid('a'), Address: sub(1)}
	if added, err := s.AddMoneroSettled(bg, k, 7); !added || err != nil {
		t.Fatalf("first: %v %v", added, err)
	}
	if added, err := s.AddMoneroSettled(bg, k, 9); added || err != nil {
		t.Fatalf("replay: %v %v", added, err)
	}
	var h int64
	if err := s.db.QueryRow(`SELECT height FROM monero_settled`).Scan(&h); err != nil || h != 7 {
		t.Fatalf("height %d %v", h, err)
	}
	w, err := s.WatchedPayments(bg, t0)
	if err != nil || len(w) != 1 || w[0].Address != sub(2) {
		t.Fatalf("WatchedPayments after AddMoneroSettled: %+v %v", w, err)
	}
	if _, err := s.AddMoneroSettled(bg, PaymentKey{TxID: txid('b'), Address: sub(1)}, 7); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown payment: %v", err)
	}
	for name, c := range map[string]struct {
		k PaymentKey
		h int64
	}{"bad txid": {PaymentKey{TxID: "ab", Address: sub(2)}, 7}, "bad subaddress": {PaymentKey{TxID: txid('a'), Address: "x"}, 7},
		"height 0": {PaymentKey{TxID: txid('a'), Address: sub(2)}, 0}} {
		if _, err := s.AddMoneroSettled(bg, c.k, c.h); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, q := range map[string]string{
		"unknown payment": `INSERT INTO monero_settled VALUES ('` + txid('9') + `', '` + sub(1) + `', 7, 5)`,
		"height 0":        `INSERT INTO monero_settled VALUES ('` + txid('a') + `', '` + sub(2) + `', 0, 5)`,
		"update":          `UPDATE monero_settled SET height = 8`,
		"delete":          `DELETE FROM monero_settled`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("SQL accepted %s", name)
		}
	}
}

func TestMoneroWatchWindowBoundaries(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkMoneroOrder(t, s, "r", 1000, exp)
	mkSub(t, s, o, 1)
	clk.set(exp) // first seen at the expiry: late, so only the window arm applies
	moneroScan(t, s, 1, pay('a', 1, 5))
	edge := exp.Add(WatchWindow)
	clk.set(edge)
	for _, c := range []struct {
		now time.Time
		in  bool
	}{{edge.Add(-time.Second), true}, {edge, false}, {edge.Add(time.Second), false}, {exp, true}} {
		w, err := s.WatchedPayments(bg, c.now)
		if err != nil || (len(w) == 1) != c.in {
			t.Errorf("WatchedPayments(%v): %d %v, want in=%v", c.now, len(w), err, c.in)
		}
		p, err := s.MoneroOrdersPastExpiry(bg, c.now)
		wantIn := c.in && !c.now.Before(exp)
		if err != nil || (len(p) == 1) != wantIn {
			t.Errorf("MoneroOrdersPastExpiry(%v): %d %v, want in=%v", c.now, len(p), err, wantIn)
		}
	}
	if p, _ := s.MoneroOrdersPastExpiry(bg, exp.Add(-time.Second)); len(p) != 0 {
		t.Error("listed before expiry")
	}
	clk.set(t0)
	now := exp.Add(time.Hour)
	for _, typ := range []string{EventPaid, EventExpired, EventUnderpaid} {
		o2 := mkMoneroOrder(t, s, "r-"+typ, 1000, exp)
		if _, err := s.AddEvent(bg, Event{Type: typ, OrderID: o2.ID}); err != nil {
			t.Fatal(err)
		}
		p, _ := s.MoneroOrdersPastExpiry(bg, now)
		for _, x := range p {
			if x.ID == o2.ID {
				t.Errorf("%s order listed", typ)
			}
		}
	}
	// Orders without a Monero amount are never listed.
	ln := mkBothOrder(t, s, "ln", 1000, 500, exp)
	p, _ := s.MoneroOrdersPastExpiry(bg, now)
	for _, x := range p {
		if x.ID == ln.ID {
			t.Error("an order without Monero listed")
		}
	}
	if len(p) != 1 || p[0].ID != o.ID {
		t.Errorf("past expiry: %+v", p)
	}
}

// An on-time payment still unconfirmed at the window close stays watched until it is settled, whatever its mark;
// one first seen at the expiry, or a settled one, is not.
func TestWatchedPaymentsPastTheWindow(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkMoneroOrder(t, s, "stuck", 1000, exp)
	mkSub(t, s, o, 1)
	other := mkMoneroOrder(t, s, "other", 1000, exp.Add(time.Hour))
	mkSub(t, s, other, 2)
	mark := func(d byte, kinds ...string) {
		t.Helper()
		for _, k := range kinds {
			m := MoneroMark{TxID: txid(d), Address: sub(1), Kind: k}
			if k == MarkMined {
				m.Height = 1
			}
			if err := s.AddMoneroMark(bg, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	settle := func(d byte) {
		t.Helper()
		if _, err := s.AddMoneroSettled(bg, PaymentKey{TxID: txid(d), Address: sub(1)}, 6); err != nil {
			t.Fatal(err)
		}
	}
	clk.set(exp.Add(-time.Second)) // the last second before the expiry: on time
	moneroScan(t, s, 1, pay('a', 1, 5), pay('b', 1, 5), pay('c', 1, 5), pay('d', 1, 5), pay('e', 1, 5), pay('f', 1, 5))
	mark('b', MarkMempool)
	mark('c', MarkDead, MarkMempool)
	mark('d', MarkMempool, MarkDead)
	mark('e', MarkMined)
	mark('f', MarkMined)
	settle('f')
	clk.set(exp)
	moneroScan(t, s, 2, pay('8', 1, 5), pay('9', 2, 5)) // 8 is o's (late), 9 is other's (on time)
	listed := func(now time.Time) string {
		t.Helper()
		w, err := s.WatchedPayments(bg, now)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, x := range w {
			ids = append(ids, x.TxID[:1])
		}
		return strings.Join(ids, "")
	}
	edge := exp.Add(WatchWindow)
	if got := listed(edge.Add(-time.Second)); got != "abcde89" {
		t.Errorf("one second before the window end: %q", got)
	}
	if got := listed(edge); got != "abcde9" {
		t.Errorf("at the window end: %q", got)
	}
	if got := listed(edge.Add(2 * time.Hour)); got != "abcde9" {
		t.Errorf("past both windows: %q", got)
	}
	settle('d')
	settle('e')
	if got := listed(edge); got != "abc9" {
		t.Errorf("after d and e are settled: %q", got)
	}
}

func TestHasOpenMoneroOrder(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	edge := exp.Add(WatchWindow)
	check := func(name string, now time.Time, want bool) {
		t.Helper()
		if got, err := s.HasOpenMoneroOrder(bg, now); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	check("empty", t0, false)
	mkBothOrder(t, s, "ln", 1000, 500, exp)
	check("Lightning and on-chain only", t0, false)
	o := mkMoneroOrder(t, s, "r", 1000, exp)
	mkSub(t, s, o, 1)
	check("before expiry", t0, true)
	check("at expiry", exp, true)
	check("last second of the window", edge.Add(-time.Second), true)
	check("window end", edge, false)
	if _, err := s.AddEvent(bg, Event{Type: EventLate, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	check("late is not final", exp, true)
	if _, err := s.AddEvent(bg, Event{Type: EventExpired, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	check("final", exp, false)
	// Past the window: a stuck payment keeps it open until it is settled, whatever the events.
	clk.set(exp) // first seen at the expiry: late, never stuck
	moneroScan(t, s, 1, pay('7', 1, 5))
	check("a late payment, at the window end", edge, false)
	clk.set(exp.Add(-time.Second))
	moneroScan(t, s, 1, pay('a', 1, 5))
	check("a stuck payment, at the window end", edge, true)
	check("a stuck payment, a month later", edge.Add(30*24*time.Hour), true)
	if err := s.AddMoneroMark(bg, MoneroMark{TxID: txid('a'), Address: sub(1), Kind: MarkDead}); err != nil {
		t.Fatal(err)
	}
	check("dead, not yet settled", edge, true)
	if _, err := s.AddMoneroSettled(bg, PaymentKey{TxID: txid('a'), Address: sub(1)}, 8); err != nil {
		t.Fatal(err)
	}
	check("settled", edge, false)
	check("settled, inside the window with a final event", exp, false)
}

func TestMoneroOrdersLeftWindow(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkMoneroOrder(t, s, "r", 1000, exp)
	done := mkMoneroOrder(t, s, "done", 1000, exp)
	if _, err := s.AddEvent(bg, Event{Type: EventUnderpaid, OrderID: done.ID}); err != nil {
		t.Fatal(err)
	}
	mkBothOrder(t, s, "ln", 1000, 500, exp) // no Monero: never listed
	edge := exp.Add(WatchWindow)
	for _, c := range []struct {
		since, now time.Time
		in         bool
	}{
		{edge.Add(-time.Second), edge, true},
		{edge.Add(-time.Hour), edge.Add(time.Hour), true},
		{edge.Add(-2 * time.Second), edge.Add(-time.Second), false},
		{edge, edge.Add(time.Second), false},
		{edge.Add(time.Second), edge, false},
	} {
		l, err := s.MoneroOrdersLeftWindow(bg, c.since, c.now)
		if err != nil || (len(l) == 1 && l[0].ID == o.ID) != c.in || len(l) > 1 {
			t.Errorf("(%v, %v]: %+v %v, want in=%v", c.since.Sub(edge), c.now.Sub(edge), l, err, c.in)
		}
	}
}

func TestMoneroPaymentsLeftUnobserved(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkMoneroOrder(t, s, "r", 1000, exp)
	mkSub(t, s, o, 1)
	if _, err := s.AddEvent(bg, Event{Type: EventExpired, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	clk.set(exp.Add(-time.Second))
	moneroScan(t, s, 1, pay('c', 1, 5)) // on time: stuck, still polled past the window
	clk.set(exp)
	moneroScan(t, s, 1, pay('a', 1, 5), pay('b', 1, 5)) // late
	if err := s.AddMoneroMark(bg, MoneroMark{TxID: txid('b'), Address: sub(1), Kind: MarkMempool}); err != nil {
		t.Fatal(err)
	}
	edge := exp.Add(WatchWindow)
	for _, c := range []struct {
		since, now time.Time
		in         bool
	}{
		{edge.Add(-time.Second), edge, true},
		{edge.Add(-time.Hour), edge.Add(time.Hour), true},
		{edge.Add(-2 * time.Second), edge.Add(-time.Second), false},
		{edge, edge.Add(time.Second), false},
		{edge.Add(time.Second), edge, false},
	} {
		l, err := s.MoneroPaymentsLeftUnobserved(bg, c.since, c.now)
		want := UnobservedPayment{PaymentKey: PaymentKey{TxID: txid('a'), Address: sub(1)}, OrderID: o.ID}
		if err != nil || (len(l) == 1 && l[0] == want) != c.in || len(l) > 1 {
			t.Errorf("(%v, %v]: %+v %v, want in=%v", c.since.Sub(edge), c.now.Sub(edge), l, err, c.in)
		}
	}
}

func TestMoneroOrdersAwaitingPaidAgain(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkMoneroOrder(t, s, "r", 1000, exp)
	noMonero := mkBothOrder(t, s, "ln", 1000, 500, exp)
	now := exp.Add(time.Hour)
	list := func(at time.Time) []Order {
		t.Helper()
		l, err := s.MoneroOrdersAwaitingPaidAgain(bg, at)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if len(list(now)) != 0 {
		t.Fatal("listed with no reversal")
	}
	for _, id := range []string{o.ID, noMonero.ID} {
		if _, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: id, Seq: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if l := list(now); len(l) != 1 || l[0].ID != o.ID {
		t.Fatalf("after reverted: %+v", l)
	}
	if len(list(exp.Add(WatchWindow))) != 0 || len(list(exp.Add(WatchWindow-time.Second))) != 1 {
		t.Fatal("window edge")
	}
	if _, err := s.AddEvent(bg, Event{Type: EventPaidAgain, OrderID: o.ID, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if len(list(now)) != 0 {
		t.Fatal("listed after paid_again")
	}
}

// The Monero watch-window queries start from orders_expires, as the on-chain ones do.
func TestMoneroWatchQueriesUseTheExpiryIndex(t *testing.T) {
	s, _, _ := freshStore(t)
	lo := t0.Unix()
	for name, q := range map[string]string{
		"MoneroOrdersPastExpiry":        moneroPastExpirySQL,
		"MoneroOrdersAwaitingPaidAgain": moneroAwaitingPaidAgainSQL,
		"WatchedPayments":               watchedPaymentsSQL,
		"HasOpenMoneroOrder":            openMoneroSQL,
		"MoneroPaymentsLeftUnobserved":  moneroLeftUnobservedSQL,
	} {
		args := []any{lo}
		if name == "MoneroOrdersPastExpiry" || name == "MoneroPaymentsLeftUnobserved" {
			args = []any{lo + 1, lo}
		}
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "INDEX orders_expires (expires_at") {
			t.Errorf("%s plan does not use orders_expires:\n%s", name, joined)
		}
	}
}

// CurrentMoneroMarks and MoneroSettledKeys read in batches and agree with CurrentMoneroMark and with WatchedPayments
// key by key, on random payments: several subaddresses of one transaction, keys asked twice, unknown keys, a
// subaddress of a known txid that has no payment, and more txids than one batch of sqlChunk.
func TestMoneroBatchReadsAgree(t *testing.T) {
	s, _, _ := freshStore(t)
	rng := rand.New(rand.NewSource(37))
	for i := 1; i <= 4; i++ {
		mkSub(t, s, mkMoneroOrder(t, s, "r"+itoa(int64(i)), 1000, t0.Add(time.Hour)), int64(i))
	}
	hexTx := func(i int) string {
		h := sha256.Sum256([]byte("monero" + itoa(int64(i))))
		return hex.EncodeToString(h[:])
	}
	var all []PaymentKey
	var ps []Payment
	for i := 0; i < 700; i++ {
		for _, a := range rng.Perm(4)[:1+rng.Intn(3)] {
			p := Payment{TxID: hexTx(i), Address: sub(1 + a), AmountPiconero: 1}
			ps = append(ps, p)
			all = append(all, PaymentKey{TxID: p.TxID, Address: p.Address})
		}
	}
	moneroScan(t, s, 1, ps...)
	kinds := []MoneroMark{{Kind: MarkMempool}, {Kind: MarkDead}, {Kind: MarkMined, Height: 7}}
	settled := map[PaymentKey]bool{}
	for _, k := range all {
		for n := rng.Intn(4); n > 0; n-- {
			m := kinds[rng.Intn(len(kinds))]
			m.TxID, m.Address = k.TxID, k.Address
			if err := s.AddMoneroMark(bg, m); err != nil {
				t.Fatal(err)
			}
		}
		if rng.Intn(2) == 0 {
			if _, err := s.AddMoneroSettled(bg, k, 1); err != nil {
				t.Fatal(err)
			}
			settled[k] = true
		}
	}
	w, err := s.WatchedPayments(bg, t0)
	if err != nil || len(w) != len(all)-len(settled) {
		t.Fatalf("%d watched, want %d (%v)", len(w), len(all)-len(settled), err)
	}
	for _, p := range w {
		if settled[PaymentKey{TxID: p.TxID, Address: p.Address}] {
			t.Fatal("a settled payment is watched")
		}
	}
	for round := 0; round < 20; round++ {
		var keys []PaymentKey
		for n := rng.Intn(len(all) + 1); n > 0; n-- {
			switch r := rng.Intn(10); {
			case r == 0:
				keys = append(keys, PaymentKey{TxID: hexTx(10_000 + rng.Intn(5)), Address: sub(1)})
			case r == 1:
				k := all[rng.Intn(len(all))]
				keys = append(keys, PaymentKey{TxID: k.TxID, Address: sub(99)})
			default:
				keys = append(keys, all[rng.Intn(len(all))])
			}
		}
		marks, err := s.CurrentMoneroMarks(bg, keys)
		if err != nil {
			t.Fatal(err)
		}
		gotSettled, err := s.MoneroSettledKeys(bg, keys)
		if err != nil {
			t.Fatal(err)
		}
		wantMarks, wantSettled := map[PaymentKey]MoneroMark{}, map[PaymentKey]bool{}
		for _, k := range keys {
			m, ok, err := s.CurrentMoneroMark(bg, k)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				wantMarks[k] = m
			}
			if settled[k] {
				wantSettled[k] = true
			}
		}
		if len(marks) != len(wantMarks) || len(gotSettled) != len(wantSettled) {
			t.Fatalf("round %d: %d marks (want %d), %d settled (want %d)", round, len(marks), len(wantMarks), len(gotSettled), len(wantSettled))
		}
		for k, m := range wantMarks {
			if marks[k] != m {
				t.Fatalf("round %d: %+v, want %+v", round, marks[k], m)
			}
		}
		for k := range wantSettled {
			if !gotSettled[k] {
				t.Fatalf("round %d: %s not named settled", round, k.TxID[:8])
			}
		}
	}
	if m, err := s.CurrentMoneroMarks(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("no keys: %+v %v", m, err)
	}
	if m, err := s.MoneroSettledKeys(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("no keys: %+v %v", m, err)
	}
}

// PaymentsByTxIDs reads in batches of sqlChunk: 0, 1 and 501 txids, unknown ones left out, every subaddress of a
// txid, a txid asked twice read once, and the result oldest first across the batches.
func TestPaymentsByTxIDs(t *testing.T) {
	s, _, clk := freshStore(t)
	for i := 1; i <= 2; i++ {
		mkSub(t, s, mkMoneroOrder(t, s, "r"+itoa(int64(i)), 1000, t0.Add(time.Hour)), int64(i))
	}
	hexTx := func(i int) string {
		h := sha256.Sum256([]byte("tx " + itoa(int64(i))))
		return hex.EncodeToString(h[:])
	}
	// The 501st txid first, with two subaddresses: oldest first puts it first though it is read in the second batch.
	clk.set(t0)
	moneroScan(t, s, 1, Payment{TxID: hexTx(500), Address: sub(1), AmountPiconero: 9}, Payment{TxID: hexTx(500), Address: sub(2), AmountPiconero: 8})
	clk.set(t0.Add(time.Minute))
	var first []Payment
	for i := 0; i < 500; i++ {
		first = append(first, Payment{TxID: hexTx(i), Address: sub(1), AmountPiconero: int64(1 + i)})
	}
	moneroScan(t, s, 2, first...)
	if l, err := s.PaymentsByTxIDs(bg, nil); err != nil || len(l) != 0 {
		t.Fatalf("none: %+v %v", l, err)
	}
	if l, err := s.PaymentsByTxIDs(bg, []string{hexTx(7)}); err != nil || len(l) != 1 || l[0].AmountPiconero != 8 || l[0].FirstSeenHeight != 2 ||
		!l[0].FirstSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("one: %+v %v", l, err)
	}
	var ids []string
	for i := 0; i < 501; i++ {
		ids = append(ids, hexTx(i))
	}
	l, err := s.PaymentsByTxIDs(bg, ids)
	if err != nil || len(l) != 502 {
		t.Fatalf("501 txids: %d rows %v", len(l), err)
	}
	if l[0].TxID != hexTx(500) || l[0].Address != sub(1) || l[1].Address != sub(2) || !l[0].FirstSeen.Equal(t0) {
		t.Fatalf("not oldest first: %+v %+v", l[0], l[1])
	}
	for i := 3; i < len(l); i++ {
		p, c := l[i-1], l[i]
		if c.FirstSeen.Before(p.FirstSeen) || (c.FirstSeen.Equal(p.FirstSeen) && c.TxID < p.TxID) {
			t.Fatalf("row %d out of order", i)
		}
	}
	mixed := []string{hexTx(500), strings.Repeat("0", 64), hexTx(3), hexTx(500), "not hex"}
	if l, err := s.PaymentsByTxIDs(bg, mixed); err != nil || len(l) != 3 || l[2].TxID != hexTx(3) {
		t.Fatalf("unknown and repeated: %+v %v", l, err)
	}
}

// A restart, and a copy of the live files: every Monero row and rule is there, and the idempotent writes stay
// idempotent.
func TestMoneroRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: t0}
	s, err := Open(bg, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	o := mkMoneroOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 3)
	p := Payment{TxID: txid('a'), Address: sub(3), AmountPiconero: 900, UnlockTime: 12}
	moneroScan(t, s, 7, p)
	pk := PaymentKey{TxID: p.TxID, Address: p.Address}
	if err := s.AddMoneroMark(bg, MoneroMark{TxID: pk.TxID, Address: pk.Address, Kind: MarkMined, Height: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(bg, Event{Type: EventPaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	check := func(s *Store) {
		t.Helper()
		if a, err := s.SubaddressForOrder(bg, o.ID); err != nil || a.Index != 3 || a.Address != sub(3) {
			t.Fatalf("subaddress: %+v %v", a, err)
		}
		if l, _ := s.PaymentsForOrder(bg, o.ID); len(l) != 1 || l[0].AmountPiconero != 900 || l[0].UnlockTime != 12 || l[0].FirstSeenHeight != 7 {
			t.Fatalf("payments %+v", l)
		}
		if c, ok, _ := s.MoneroCursor(bg); !ok || c.Height != 7 {
			t.Fatalf("cursor %+v", c)
		}
		if m, ok, _ := s.CurrentMoneroMark(bg, pk); !ok || m.Kind != MarkMined || m.Height != 7 {
			t.Fatalf("mark %+v", m)
		}
		if added := moneroScan(t, s, 7, p); len(added) != 0 {
			t.Fatalf("replay after restart: %+v", added)
		}
		if ok, err := s.AddEvent(bg, Event{Type: EventExpired, OrderID: o.ID}); ok || err != nil {
			t.Fatalf("expired after paid, after restart: %v %v", ok, err)
		}
		if idx, ok, _ := s.MaxSubaddressIndex(bg); !ok || idx != 3 {
			t.Fatalf("max %d", idx)
		}
		if err := s.AddSubaddress(bg, Subaddress{OrderID: mkMoneroOrder(t, s, "r"+itoa(time.Now().UnixNano()), 1, t0.Add(time.Hour)).ID,
			Address: sub(2), Index: 2}); !errors.Is(err, ErrIndexNotAbove) {
			t.Fatalf("index below the stored maximum after restart: %v", err)
		}
	}
	s2, err := Open(bg, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	check(s2)
	dst := filepath.Join(t.TempDir(), "copy.db")
	copyFile(t, path, dst)
	copyFile(t, path+"-wal", dst+"-wal")
	c, err := Open(bg, dst, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check(c)
	s2.Close()
}

// Each Monero guard holds on its own (the mutation check): a guard that another one shadows on the
// usual paths (a CHECK behind a parent-must-exist trigger, a trigger behind a foreign key or a unique constraint) is
// tested with the overlapping triggers dropped, on a raw connection with foreign keys off, in a scratch store. Each
// bad row must be refused with the named guard's own error.
func TestMoneroGuardsStandAlone(t *testing.T) {
	upper := strings.Repeat("B", 64)
	bad := sub(1)[:94] + "l"
	for _, c := range []struct {
		name, drop, q, want string
	}{
		{"subaddress order_id CHECK", "monero_subaddresses_known_order monero_subaddresses_order_monero",
			`INSERT INTO monero_subaddresses VALUES ('` + sub(5) + `', '` + strings.Repeat("A", 32) + `', 5, 5)`, "CHECK constraint failed"},
		{"subaddress index 0 CHECK", "monero_subaddresses_index_increases",
			`INSERT INTO monero_subaddresses SELECT '` + sub(5) + `', id, 0, 5 FROM orders WHERE merchant_ref = 'free'`, "CHECK constraint failed"},
		{"subaddress index -1 CHECK", "monero_subaddresses_index_increases",
			`INSERT INTO monero_subaddresses SELECT '` + sub(5) + `', id, -1, 5 FROM orders WHERE merchant_ref = 'free'`, "CHECK constraint failed"},
		{"subaddress known_order", "monero_subaddresses_order_monero",
			`INSERT INTO monero_subaddresses VALUES ('` + sub(5) + `', '` + strings.Repeat("f", 32) + `', 5, 5)`, "unknown order"},
		{"subaddress index_increases", "",
			`INSERT INTO monero_subaddresses SELECT '` + sub(2) + `', id, 2, 5 FROM orders WHERE merchant_ref = 'free'`, "subaddress index must exceed"},
		{"subaddress replace guard", "monero_subaddresses_index_increases",
			`INSERT OR REPLACE INTO monero_subaddresses SELECT * FROM monero_subaddresses WHERE idx = 1`, "store rows are immutable"},
		{"payment address CHECK", "monero_payments_known_subaddress",
			`INSERT INTO monero_payments VALUES ('` + txid('d') + `', '` + bad + `', 1, 0, 5, 0)`, "CHECK constraint failed"},
		{"payment known_subaddress", "",
			`INSERT INTO monero_payments VALUES ('` + txid('d') + `', '` + sub(9) + `', 1, 0, 5, 0)`, "unknown subaddress"},
		{"mark txid CHECK", "monero_marks_known_payment",
			`INSERT INTO monero_marks VALUES ('` + upper + `', '` + sub(1) + `', 1, 'dead', 0, 5, 0)`, "CHECK constraint failed"},
		{"mark address CHECK", "monero_marks_known_payment",
			`INSERT INTO monero_marks VALUES ('` + txid('b') + `', '` + bad + `', 1, 'dead', 0, 5, 0)`, "CHECK constraint failed"},
		{"settled txid CHECK", "monero_settled_known_payment",
			`INSERT INTO monero_settled VALUES ('` + upper + `', '` + sub(1) + `', 6, 5)`, "CHECK constraint failed"},
		{"settled address CHECK", "monero_settled_known_payment",
			`INSERT INTO monero_settled VALUES ('` + txid('b') + `', '` + bad + `', 6, 5)`, "CHECK constraint failed"},
		{"pending order_id CHECK", "monero_pending_settle_known_order",
			`INSERT INTO monero_pending_settle VALUES ('` + strings.Repeat("A", 32) + `', 5)`, "CHECK constraint failed"},
		{"pending at CHECK", "",
			`INSERT INTO monero_pending_settle SELECT id, 0 FROM orders WHERE merchant_ref = 'free'`, "CHECK constraint failed"},
		{"pending known_order", "",
			`INSERT INTO monero_pending_settle VALUES ('` + strings.Repeat("f", 32) + `', 5)`, "unknown order"},
		{"pending replace guard", "",
			`INSERT OR REPLACE INTO monero_pending_settle SELECT order_id, 6 FROM monero_pending_settle`, "store rows are immutable"},
		{"pending update guard", "",
			`UPDATE monero_pending_settle SET at = 6`, "store rows are immutable"},
		{"quote rate order_id CHECK", "order_quote_rates_known_quote",
			`INSERT INTO order_quote_rates VALUES ('` + strings.Repeat("A", 32) + `', 'BTC', '1', '[{"provider":"kraken","rate":"1","at":5}]')`, "CHECK constraint failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, path, _ := freshStore(t)
			// The chain the cases hang on: Monero orders with subaddresses 1 and 3, one without one ("free"), and a
			// payment to subaddress 1.
			one := mkMoneroOrder(t, s, "one", 1000, t0.Add(time.Hour))
			mkSub(t, s, one, 1)
			mkSub(t, s, mkMoneroOrder(t, s, "three", 1000, t0.Add(time.Hour)), 3)
			mkMoneroOrder(t, s, "free", 1000, t0.Add(time.Hour))
			moneroScan(t, s, 1, pay('b', 1, 5))
			// and order "one" pending settle.
			if _, err := s.CommitMoneroPass(bg, nil, MoneroCursor{Height: 1, WalletHeight: 1}, nil, []string{one.ID}); err != nil {
				t.Fatal(err)
			}
			raw, err := openRawForTest(path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			raw.SetMaxOpenConns(1) // the pragma below is per connection
			if _, err := raw.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
				t.Fatal(err)
			}
			var fk int
			if err := raw.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 0 {
				t.Fatalf("foreign keys %d %v", fk, err)
			}
			for _, tr := range strings.Fields(c.drop) {
				if _, err := raw.Exec(`DROP TRIGGER ` + tr); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string]int{}
			for _, tb := range []string{"monero_subaddresses", "monero_payments", "monero_marks", "monero_settled", "monero_pending_settle", "order_quote_rates"} {
				before[tb] = countRows(t, s, tb)
			}
			_, err = raw.Exec(c.q)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want an error with %q", err, c.want)
			}
			for tb, n := range before {
				if got := countRows(t, s, tb); got != n {
					t.Fatalf("%s: %d rows, want %d", tb, got, n)
				}
			}
		})
	}
}

// Ruling 8-A6: CommitMoneroPass writes one pass's scan (new payments), cursor and marks in one transaction. A mark may
// be for a payment added by the same commit (the payments go first). Any refusal (a bad mark, a mark for a payment
// that is neither stored nor in the scan, a payment listed with other details, a bad cursor) writes nothing at all;
// a quiet commit (no new payment, the same cursor, no mark) adds no row.
func TestCommitMoneroPass(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkMoneroOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkSub(t, s, o, 1)
	moneroScan(t, s, 10, pay('a', 1, 100))
	ka, kb := PaymentKey{TxID: txid('a'), Address: sub(1)}, PaymentKey{TxID: txid('b'), Address: sub(1)}
	rows := func() [5]int {
		p, c := moneroRows(t, s)
		return [5]int{p, c, countRows(t, s, "monero_marks"), countRows(t, s, "monero_settled"), countRows(t, s, "monero_pending_settle")}
	}

	clk.set(t0.Add(time.Minute))
	// The caller's first-seen time is stored (ruling S3: discovery's clock), not the commit's.
	seen := func(p Payment) Payment { p.FirstSeen = t0.Add(time.Second); return p }
	added, err := s.CommitMoneroPass(bg, []Payment{seen(pay('a', 1, 100)), seen(pay('b', 1, 50))}, MoneroCursor{Height: 12, WalletHeight: 11},
		[]MoneroMark{{TxID: ka.TxID, Address: ka.Address, Kind: MarkMined, Height: 11, Confirmations: 1},
			{TxID: kb.TxID, Address: kb.Address, Kind: MarkMempool}}, []string{o.ID})
	if err != nil || len(added) != 1 || added[0].TxID != kb.TxID || added[0].FirstSeenHeight != 11 ||
		!added[0].FirstSeen.Equal(t0.Add(time.Second)) {
		t.Fatalf("commit: %+v %v", added, err)
	}
	if p, err := s.Payment(bg, kb); err != nil || !p.FirstSeen.Equal(t0.Add(time.Second)) {
		t.Fatalf("stored first seen: %+v %v, want the caller's %v", p, err, t0.Add(time.Second))
	}
	if c, _, _ := s.MoneroCursor(bg); c.Height != 12 || c.WalletHeight != 11 {
		t.Fatalf("cursor %+v", c)
	}
	marks, err := s.CurrentMoneroMarks(bg, []PaymentKey{ka, kb})
	if err != nil || marks[ka].Kind != MarkMined || marks[ka].Confirmations != 1 || marks[kb].Kind != MarkMempool {
		t.Fatalf("marks %+v %v", marks, err)
	}
	want := rows()

	if l, err := s.MoneroPendingSettle(bg); err != nil || len(l) != 1 || l[0].ID != o.ID {
		t.Fatalf("pending settle after the commit: %+v %v", l, err)
	}
	other := mkMoneroOrder(t, s, "other", 1000, t0.Add(time.Hour))
	for name, c := range map[string]struct {
		ps      []Payment
		cursor  MoneroCursor
		marks   []MoneroMark
		pending []string
		is      error
	}{
		"bad mark": {[]Payment{seen(pay('c', 1, 7))}, MoneroCursor{Height: 13, WalletHeight: 13},
			[]MoneroMark{{TxID: kb.TxID, Address: kb.Address, Kind: MarkMempool, Confirmations: 1}}, []string{other.ID}, ErrInvalid},
		"mark of an unknown payment": {[]Payment{seen(pay('c', 1, 7))}, MoneroCursor{Height: 13, WalletHeight: 13},
			[]MoneroMark{{TxID: txid('d'), Address: sub(1), Kind: MarkDead}}, []string{other.ID}, ErrNotFound},
		"payment changed": {[]Payment{seen(pay('c', 1, 7)), seen(pay('a', 1, 101))}, MoneroCursor{Height: 13, WalletHeight: 13},
			[]MoneroMark{{TxID: ka.TxID, Address: ka.Address, Kind: MarkDead}}, []string{other.ID}, ErrPaymentChanged},
		"bad cursor": {[]Payment{seen(pay('c', 1, 7))}, MoneroCursor{Height: 13, WalletHeight: 14},
			[]MoneroMark{{TxID: ka.TxID, Address: ka.Address, Kind: MarkDead}}, []string{other.ID}, ErrInvalid},
		"no first seen": {[]Payment{pay('c', 1, 7)}, MoneroCursor{Height: 13, WalletHeight: 13}, nil, []string{other.ID}, ErrInvalid},
		"first seen at 1970": {[]Payment{{TxID: txid('c'), Address: sub(1), AmountPiconero: 7, FirstSeen: time.Unix(0, 0)}},
			MoneroCursor{Height: 13, WalletHeight: 13}, nil, []string{other.ID}, ErrInvalid},
		"pending unknown order": {[]Payment{seen(pay('c', 1, 7))}, MoneroCursor{Height: 13, WalletHeight: 13}, nil,
			[]string{other.ID, strings.Repeat("f", 32)}, ErrNotFound},
		"pending malformed order": {[]Payment{seen(pay('c', 1, 7))}, MoneroCursor{Height: 13, WalletHeight: 13}, nil,
			[]string{other.ID, strings.Repeat("F", 32)}, ErrInvalid},
	} {
		if _, err := s.CommitMoneroPass(bg, c.ps, c.cursor, c.marks, c.pending); !errors.Is(err, c.is) {
			t.Errorf("%s: %v, want %v", name, err, c.is)
		}
		if got := rows(); got != want {
			t.Errorf("%s wrote: rows %v, want %v", name, got, want)
		}
	}
	if _, err := s.CommitMoneroPass(bg, []Payment{seen(pay('a', 1, 100))}, MoneroCursor{Height: 12, WalletHeight: 11}, nil, []string{o.ID}); err != nil {
		t.Fatal(err)
	}
	if got := rows(); got != want {
		t.Errorf("a quiet commit wrote: rows %v, want %v", got, want)
	}
	if _, err := s.CommitMoneroPass(bg, nil, MoneroCursor{Height: 12, WalletHeight: 11},
		[]MoneroMark{{TxID: kb.TxID, Address: kb.Address, Kind: MarkMined, Height: 12, Confirmations: 0}}, nil); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := s.CurrentMoneroMark(bg, kb); m.Kind != MarkMined || m.Seq != 2 || countRows(t, s, "monero_cursor") != want[1] {
		t.Errorf("a marks-only commit: %+v, cursor rows %d", m, countRows(t, s, "monero_cursor"))
	}
}

// Ruling S4: CommitMoneroPass records the orders it is given as pending settle (once each, whatever their window or
// events), MoneroPendingSettle lists them oldest expiry first, ClearMoneroPendingSettle deletes the given ones (an id
// without a row is no error, a malformed one is ErrInvalid and deletes nothing), and a pending order keeps the Monero
// watcher required (HasOpenMoneroOrder) after every window has closed.
func TestMoneroPendingSettle(t *testing.T) {
	s, _, _ := freshStore(t)
	late := mkMoneroOrder(t, s, "late", 1000, t0.Add(2*time.Hour))
	early := mkMoneroOrder(t, s, "early", 1000, t0.Add(time.Hour))
	idle := mkMoneroOrder(t, s, "idle", 1000, t0.Add(time.Hour))
	ids := func() []string {
		t.Helper()
		l, err := s.MoneroPendingSettle(bg)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, o := range l {
			out = append(out, o.ID)
		}
		return out
	}
	commit := func(pending ...string) {
		t.Helper()
		if _, err := s.CommitMoneroPass(bg, nil, MoneroCursor{Height: 5, WalletHeight: 5}, nil, pending); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(); len(got) != 0 {
		t.Fatalf("pending before any commit: %v", got)
	}
	commit(late.ID, early.ID, late.ID)
	commit(early.ID)
	if got := ids(); !slices.Equal(got, []string{early.ID, late.ID}) {
		t.Fatalf("pending %v, want early then late", got)
	}
	far := t0.Add(10 * time.Hour).Add(WatchWindow)
	if open, err := s.HasOpenMoneroOrder(bg, far); err != nil || !open {
		t.Fatalf("a pending order past every window: open %v %v", open, err)
	}
	if err := s.ClearMoneroPendingSettle(bg, []string{early.ID, strings.Repeat("F", 32)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed id: %v", err)
	}
	if got := ids(); len(got) != 2 {
		t.Fatalf("a refused clear deleted: %v", got)
	}
	if err := s.ClearMoneroPendingSettle(bg, []string{early.ID, idle.ID}); err != nil {
		t.Fatal(err)
	}
	if got := ids(); !slices.Equal(got, []string{late.ID}) {
		t.Fatalf("pending after the clear %v, want late", got)
	}
	commit(early.ID) // pending again after a clear
	if err := s.ClearMoneroPendingSettle(bg, []string{early.ID, late.ID}); err != nil {
		t.Fatal(err)
	}
	if got := ids(); len(got) != 0 {
		t.Fatalf("pending after clearing all: %v", got)
	}
	if open, err := s.HasOpenMoneroOrder(bg, far); err != nil || open {
		t.Fatalf("nothing pending past every window: open %v %v", open, err)
	}
	if err := s.ClearMoneroPendingSettle(bg, nil); err != nil {
		t.Fatal(err)
	}
}
