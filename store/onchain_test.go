package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var bg = context.Background()

func addr(i int) string { return "bcrt1q" + strings.Repeat("x", 20) + itoa(int64(i)) }

func txid(d byte) string { return strings.Repeat(string(d), 64) }

// mkChainOrder makes an on-chain-only order (AmountSat set, AmountMsat 0).
func mkChainOrder(t *testing.T, s *Store, ref string, sat int64, expires time.Time) Order {
	t.Helper()
	o, created, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: ref, AmountSat: sat, ExpiresAt: expires})
	if err != nil || !created {
		t.Fatalf("CreateOrder(%q): %v %v", ref, created, err)
	}
	return o
}

func mkAddr(t *testing.T, s *Store, o Order, idx int64) Address {
	t.Helper()
	a := Address{OrderID: o.ID, Address: addr(int(idx)), Index: idx}
	if err := s.AddAddress(bg, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestValidAddressBoundaries(t *testing.T) {
	pad := func(prefix string, n int) string { return prefix + strings.Repeat("q", n-len(prefix)) }
	for _, c := range []struct {
		s  string
		ok bool
	}{
		{pad("bc1", 13), false}, {pad("bc1", 14), true}, {pad("bc1", 90), true}, {pad("bc1", 91), false},
		{pad("tb1", 14), true}, {pad("bcrt1", 14), true}, {pad("bcrt1", 90), true},
		{pad("bc1", 13) + "x", true},
		{pad("bt1", 20), false}, {pad("bcr1", 20), false}, {"BC1" + strings.Repeat("q", 20), false},
		{pad("bc1", 20)[:19] + "Q", false},
		{pad("bc1", 20)[:10] + "\x00" + pad("bc1", 20)[11:], false},
		{pad("bc1", 20)[:10] + "\xc3\xa9" + pad("bc1", 20)[12:], false},
		{"", false}, {pad("bc1", 20) + " ", false}, {pad("bc1", 20)[:10] + "-" + pad("bc1", 20)[11:], false},
	} {
		if got := ValidAddress(c.s); got != c.ok {
			t.Errorf("ValidAddress(%q) = %v, want %v", c.s, got, c.ok)
		}
	}
}

// addressProbeBytes is the representative set substituted at every position: the bytes the address
// grammar treats specially (prefix characters, the separator 1, digits, both cases of letters), the
// bytes on either side of each allowed range ('/', ':', '@', '[', backtick, '{'), other separators,
// and the class boundaries of ASCII and UTF-8. FuzzValidators covers all 256 bytes.
var addressProbeBytes = []byte("bcrtq01p9azAZBCRT/:@[`{-_ .!~" + "\x00\x1f\x20\x7e\x7f\x80\xc3\xa9\xff")

// The Go and SQL address validators agree on a representative byte in every position at the boundary
// lengths.
func TestAddressValidatorAgreesWithSQL(t *testing.T) {
	c := newSQLChecks(t)
	for _, prefix := range []string{"bc1", "tb1", "bcrt1", "xx1"} {
		for _, n := range []int{13, 14, 15, 89, 90, 91} {
			base := prefix + strings.Repeat("q", max(n-len(prefix), 0))
			for _, b := range addressProbeBytes {
				for pos := range len(base) {
					v := []byte(base)
					v[pos] = b
					c.agreeAddr(t, string(v))
				}
			}
			c.agreeAddr(t, base)
		}
	}
}

func TestMarkValidatorsAgreeWithSQL(t *testing.T) {
	c := newSQLChecks(t)
	rng := rand.New(rand.NewSource(11))
	hashes := []string{"", strings.Repeat("a", 64), strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("a", 65)}
	for _, kind := range []string{MarkMempool, MarkMined, MarkDead, "", "other"} {
		for _, h := range []int64{-1, 0, 1, 2, 1 << 40} {
			for _, bh := range hashes {
				c.agreeMark(t, kind, h, bh)
			}
		}
	}
	for i := 0; i < 2000; i++ {
		c.agreeMark(t, []string{MarkMempool, MarkMined, MarkDead, "x"}[rng.Intn(4)], int64(rng.Intn(4))-1, hashes[rng.Intn(len(hashes))])
	}
}

func TestAmountRules(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	i := 0
	try := func(msat, sat, pico int64) error {
		i++
		_, _, err := s.CreateOrder(bg, Order{AccountID: "a", MerchantRef: "r" + itoa(int64(i)), AmountMsat: msat, AmountSat: sat, AmountPiconero: pico, ExpiresAt: exp})
		return err
	}
	for _, c := range []struct {
		msat, sat, pico int64
		ok              bool
	}{
		{0, 0, 0, false}, {1000, 0, 0, true}, {0, 1, 0, true}, {1000, 1, 0, true}, {0, MaxSat, 0, true}, {0, MaxSat + 1, 0, false},
		{0, -1, 0, false}, {-1, 1, 0, false}, {maxAmount + 1, 0, 0, false},
		{0, 0, 1, true}, {0, 0, 1<<53 - 1, true}, {0, 0, 1 << 53, false}, {0, 0, -1, false}, {1000, 1, 5, true},
		{1000, 0, 1 << 53, false}, {0, 0, 1<<63 - 1, false}, {0, 0, -1 << 63, false},
	} {
		err := try(c.msat, c.sat, c.pico)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("(%d,%d,%d): err %v, want ok=%v", c.msat, c.sat, c.pico, err, c.ok)
		}
	}
	if err := try(0, 0, 0); err == nil || err.Error() != "store: invalid argument: an order needs a Lightning, an on-chain or a Monero amount" {
		t.Errorf("(0,0,0): %v", err)
	}
	// SQL refuses all zero and too large.
	if _, err := s.db.Exec(`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES (?, ?, 'a', 'zz', 0, 0, 0, 5, 6)`, strings.Repeat("a", 32), strings.Repeat("b", 32)); err == nil {
		t.Error("SQL accepted (0,0,0)")
	}
	if _, err := s.db.Exec(`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES (?, ?, 'a', 'zz', 0, ?, 0, 5, 6)`, strings.Repeat("a", 32), strings.Repeat("b", 32), MaxSat+1); err == nil {
		t.Error("SQL accepted MaxSat+1")
	}
	// Idempotency compares both amounts.
	o := mkChainOrder(t, s, "same", 500, exp)
	again, created, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: "same", AmountSat: 500, ExpiresAt: exp})
	if err != nil || created || again != o {
		t.Fatalf("again: %+v %v %v", again, created, err)
	}
	if _, _, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: "same", AmountSat: 501, ExpiresAt: exp}); !errors.Is(err, ErrConflict) {
		t.Fatalf("different sat: %v", err)
	}
	if _, _, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: "same", AmountMsat: 1000, AmountSat: 500, ExpiresAt: exp}); !errors.Is(err, ErrConflict) {
		t.Fatalf("different msat: %v", err)
	}
	// And the Monero amount: added to an order without one, or another one.
	if _, _, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: "same", AmountSat: 500, AmountPiconero: 1, ExpiresAt: exp}); !errors.Is(err, ErrConflict) {
		t.Fatalf("piconero added: %v", err)
	}
	xmr := mkMoneroOrder(t, s, "xmr", 7000, exp)
	if again, created, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: "xmr", AmountPiconero: 7000, ExpiresAt: exp}); err != nil || created || again != xmr {
		t.Fatalf("Monero repeat: %+v %v %v", again, created, err)
	}
	for _, pico := range []int64{6999, 7001} {
		if _, _, err := s.CreateOrder(bg, Order{AccountID: "acct-1", MerchantRef: "xmr", AmountPiconero: pico, ExpiresAt: exp}); !errors.Is(err, ErrConflict) {
			t.Fatalf("other piconero %d: %v", pico, err)
		}
	}
	if got, err := s.OrderByID(bg, xmr.ID); err != nil || got.AmountPiconero != 7000 || got.AmountMsat != 0 || got.AmountSat != 0 {
		t.Fatalf("Monero order read back: %+v %v", got, err)
	}
	// An invoice for an on-chain-only order is refused, in Go and by the trigger.
	err = s.AddInvoice(bg, Invoice{OrderID: o.ID, PaymentHash: hash('a'), Bolt11: "ln", AmountMsat: 1000, ExpiresAt: t0.Add(time.Minute)})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("invoice for on-chain order: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO invoices VALUES (?, ?, 'ln', 1000, 5, 6)`, hash('b'), o.ID); err == nil {
		t.Error("trigger accepted an invoice for an on-chain-only order")
	}
}

func TestAddAddress(t *testing.T) {
	s, path, _ := freshStore(t)
	o1 := mkChainOrder(t, s, "r1", 1000, t0.Add(time.Hour))
	o2 := mkChainOrder(t, s, "r2", 1000, t0.Add(time.Hour))
	o3 := mkChainOrder(t, s, "r3", 1000, t0.Add(time.Hour))
	if _, ok, err := s.MaxAddressIndex(bg); ok || err != nil {
		t.Fatalf("empty max: %v %v", ok, err)
	}
	a1 := mkAddr(t, s, o1, 0)
	got, err := s.AddressForOrder(bg, o1.ID)
	if err != nil || got.Address != a1.Address || got.Index != 0 || got.OrderID != o1.ID || !got.CreatedAt.Equal(t0) {
		t.Fatalf("AddressForOrder: %+v %v", got, err)
	}
	if _, err := s.AddressForOrder(bg, o2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no address: %v", err)
	}
	// A second address for the order; the same address for another order.
	if err := s.AddAddress(bg, Address{OrderID: o1.ID, Address: addr(5), Index: 5}); !errors.Is(err, ErrAddressTaken) {
		t.Fatalf("second for order: %v", err)
	}
	if err := s.AddAddress(bg, Address{OrderID: o2.ID, Address: a1.Address, Index: 5}); !errors.Is(err, ErrAddressTaken) {
		t.Fatalf("same address: %v", err)
	}
	if err := s.AddAddress(bg, Address{OrderID: strings.Repeat("e", 32), Address: addr(7), Index: 7}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	// Index equal to the maximum, or below, is ErrIndexNotAbove (an ErrInvalid); one above is accepted. The other
	// ErrInvalid causes are not ErrIndexNotAbove: the rail sets behind on that sentinel alone.
	if err := s.AddAddress(bg, Address{OrderID: o2.ID, Address: addr(8), Index: 0}); !errors.Is(err, ErrInvalid) || !errors.Is(err, ErrIndexNotAbove) {
		t.Fatalf("index = max: %v", err)
	}
	if err := s.AddAddress(bg, Address{OrderID: o2.ID, Address: addr(8), Index: -1}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIndexNotAbove) {
		t.Fatalf("index -1: %v", err)
	}
	if err := s.AddAddress(bg, Address{OrderID: o2.ID, Address: "bad", Index: 1}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIndexNotAbove) {
		t.Fatalf("bad address: %v", err)
	}
	if err := s.AddAddress(bg, Address{OrderID: o2.ID, Address: addr(9), Index: 1 << 31}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIndexNotAbove) {
		t.Fatalf("index too large: %v", err)
	}
	if err := s.AddAddress(bg, Address{OrderID: o2.ID, Address: addr(9), Index: 1}); err != nil {
		t.Fatal(err)
	}
	if idx, ok, err := s.MaxAddressIndex(bg); idx != 1 || !ok || err != nil {
		t.Fatalf("max: %d %v %v", idx, ok, err)
	}
	// The trigger, on a raw connection.
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO addresses VALUES (?, ?, 1, 5)`, addr(10), o3.ID); err == nil {
		t.Error("trigger accepted an index equal to the maximum")
	}
	if _, err := raw.Exec(`INSERT INTO addresses VALUES (?, ?, 0, 5)`, addr(10), o3.ID); err == nil {
		t.Error("trigger accepted an index below the maximum")
	}
	if _, err := raw.Exec(`INSERT INTO addresses VALUES (?, ?, 2, 5)`, addr(10), o3.ID); err != nil {
		t.Errorf("trigger refused index max+1: %v", err)
	}
	m, err := s.AddressesByAddress(bg, []string{addr(0), addr(9), addr(77)})
	if err != nil || len(m) != 2 || m[addr(9)].OrderID != o2.ID || m[addr(9)].Index != 1 {
		t.Fatalf("AddressesByAddress: %+v %v", m, err)
	}
	if m, err := s.AddressesByAddress(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("empty: %+v %v", m, err)
	}
}

func TestUnusedRun(t *testing.T) {
	s, _, _ := freshStore(t)
	if n, err := s.UnusedRun(bg); n != 0 || err != nil {
		t.Fatalf("none: %d %v", n, err)
	}
	var orders []Order
	for i := 0; i < 5; i++ {
		o := mkChainOrder(t, s, "r"+itoa(int64(i)), 1000, t0.Add(time.Hour))
		orders = append(orders, o)
		mkAddr(t, s, o, int64(i))
	}
	if n, err := s.UnusedRun(bg); n != 5 || err != nil {
		t.Fatalf("none paid: %d %v", n, err)
	}
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('a'), Vout: 0, Address: addr(2), AmountSat: 1000}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UnusedRun(bg); n != 2 || err != nil {
		t.Fatalf("payment at 2: %d %v", n, err)
	}
	// A payment at a lower index does not change it.
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('b'), Vout: 0, Address: addr(0), AmountSat: 1000}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnusedRun(bg); n != 2 {
		t.Fatalf("payment at 0: %d", n)
	}
}

func scanTXOs(n int, off int) []TXO {
	var out []TXO
	for i := 0; i < n; i++ {
		out = append(out, TXO{TxID: txid("0123456789abcdef"[(i+off)%16]), Vout: uint32(i + off), Address: addr(0), AmountSat: int64(1000 + i)})
	}
	return out
}

func TestRecordScan(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	if _, ok, err := s.Cursor(bg); ok || err != nil {
		t.Fatalf("cursor before: %v %v", ok, err)
	}
	first := scanTXOs(10, 0)
	// A caller FirstSeen before now is stored as given (an inherited time); zero is now.
	first[0].FirstSeen = t0.Add(-time.Hour)
	clk.set(t0.Add(time.Minute))
	added, err := s.RecordScan(bg, first, Cursor{BlockHash: txid('1'), Height: 100})
	if err != nil || len(added) != 10 || !added[0].FirstSeen.Equal(t0.Add(-time.Hour)) || !added[1].FirstSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first scan: %d %v", len(added), err)
	}
	if x, err := s.TXO(bg, TXOKey{TxID: first[0].TxID, Vout: first[0].Vout}); err != nil || !x.FirstSeen.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("the caller's FirstSeen is not stored: %+v %v", x, err)
	}
	clk.set(t0.Add(2 * time.Minute))
	second := append(scanTXOs(10, 0), TXO{TxID: txid('f'), Vout: 99, Address: addr(0), AmountSat: 7})
	added, err = s.RecordScan(bg, second, Cursor{BlockHash: txid('1'), Height: 100})
	if err != nil || len(added) != 1 || added[0].Vout != 99 || !added[0].FirstSeen.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("second scan: %+v %v", added, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_cursor`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cursor rows %d, want 2", n)
	}
	old, err := s.TXO(bg, TXOKey{TxID: first[1].TxID, Vout: first[1].Vout})
	if err != nil || !old.FirstSeen.Equal(t0.Add(time.Minute)) || old.AmountSat != first[1].AmountSat {
		t.Fatalf("old FirstSeen changed: %+v %v", old, err)
	}
	if _, err := s.TXO(bg, TXOKey{TxID: txid('9'), Vout: 5}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TXO unknown: %v", err)
	}
	// Quiet node: same hash, nothing new, no row. New hash: one row.
	if added, err := s.RecordScan(bg, second, Cursor{BlockHash: txid('1'), Height: 100}); err != nil || len(added) != 0 {
		t.Fatalf("third: %v %v", added, err)
	}
	if added, err := s.RecordScan(bg, nil, Cursor{BlockHash: txid('1'), Height: 100}); err != nil || len(added) != 0 {
		t.Fatalf("nil: %v %v", added, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_cursor`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("quiet scans added rows: %d", n)
	}
	if _, err := s.RecordScan(bg, nil, Cursor{BlockHash: txid('2'), Height: 101}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_cursor`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("new hash: %d rows", n)
	}
	c, ok, err := s.Cursor(bg)
	if err != nil || !ok || c.BlockHash != txid('2') || c.Height != 101 || !c.At.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("cursor: %+v %v %v", c, ok, err)
	}
	// Same hash, another tip height (the lastblock lags the tip and stays at the genesis on a short chain): one row,
	// so the stored tip height is never stale.
	if _, err := s.RecordScan(bg, nil, Cursor{BlockHash: txid('2'), Height: 102}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := s.Cursor(bg); c.BlockHash != txid('2') || c.Height != 102 {
		t.Fatalf("height not updated: %+v", c)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_cursor`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("new height: %d rows %v", n, err)
	}
	// One transaction: an unknown address leaves nothing, not even the cursor.
	bad := []TXO{{TxID: txid('c'), Vout: 0, Address: addr(0), AmountSat: 5}, {TxID: txid('c'), Vout: 1, Address: addr(42), AmountSat: 5}}
	if _, err := s.RecordScan(bg, bad, Cursor{BlockHash: txid('3'), Height: 102}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown address: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_cursor`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var m int
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_txos`).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if n != 4 || m != 11 {
		t.Fatalf("after failed scan: %d cursor rows, %d txos", n, m)
	}
	// Refusals.
	if _, err := s.RecordScan(bg, nil, Cursor{Height: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty hash: %v", err)
	}
	if _, err := s.RecordScan(bg, nil, Cursor{BlockHash: "xyz", Height: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad hash: %v", err)
	}
	if _, err := s.RecordScan(bg, nil, Cursor{BlockHash: txid('2'), Height: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative height: %v", err)
	}
	for _, x := range []TXO{
		{TxID: "ab", Address: addr(0), AmountSat: 1}, {TxID: txid('d'), Address: addr(0), AmountSat: 0},
		{TxID: txid('d'), Address: addr(0), AmountSat: MaxSat + 1}, {TxID: txid('d'), Address: "bad", AmountSat: 1},
	} {
		if _, err := s.RecordScan(bg, []TXO{x}, Cursor{BlockHash: txid('2'), Height: 1}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", x, err)
		}
	}
	// Max vout and max amount round-trip.
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('e'), Vout: 4294967295, Address: addr(0), AmountSat: MaxSat}}, Cursor{BlockHash: txid('2'), Height: 101}); err != nil {
		t.Fatal(err)
	}
	if x, err := s.TXO(bg, TXOKey{TxID: txid('e'), Vout: 4294967295}); err != nil || x.AmountSat != MaxSat {
		t.Fatalf("max vout: %+v %v", x, err)
	}
}

func TestTXOsForOrderAndMarks(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	var keys []TXOKey
	for i := 0; i < 3; i++ {
		clk.set(t0.Add(time.Duration(i) * time.Minute))
		x := TXO{TxID: txid(byte('a' + i)), Vout: uint32(i), Address: addr(0), AmountSat: 100}
		if _, err := s.RecordScan(bg, []TXO{x}, Cursor{BlockHash: txid(byte('1' + i)), Height: int64(i)}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, TXOKey{TxID: x.TxID, Vout: x.Vout})
	}
	list, err := s.TXOsForOrder(bg, o.ID)
	if err != nil || len(list) != 3 || list[0].TxID != txid('a') || list[2].TxID != txid('c') {
		t.Fatalf("TXOsForOrder: %+v %v", list, err)
	}
	// Marks: output 0 none, output 1 one, output 2 three.
	clk.set(t0.Add(time.Hour))
	mustMark := func(m Mark) {
		t.Helper()
		if err := s.AddMark(bg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustMark(Mark{TxID: keys[1].TxID, Vout: keys[1].Vout, Kind: MarkMempool})
	mustMark(Mark{TxID: keys[2].TxID, Vout: keys[2].Vout, Kind: MarkMempool})
	mustMark(Mark{TxID: keys[2].TxID, Vout: keys[2].Vout, Kind: MarkMined, Height: 5, BlockHash: txid('7')})
	mustMark(Mark{TxID: keys[2].TxID, Vout: keys[2].Vout, Kind: MarkDead})
	cm, err := s.CurrentMarks(bg, keys)
	if err != nil || len(cm) != 2 {
		t.Fatalf("CurrentMarks: %+v %v", cm, err)
	}
	if _, has := cm[keys[0]]; has {
		t.Error("output 0 has a mark")
	}
	if m := cm[keys[1]]; m.Seq != 1 || m.Kind != MarkMempool || !m.At.Equal(t0.Add(time.Hour)) {
		t.Errorf("output 1: %+v", m)
	}
	if m := cm[keys[2]]; m.Seq != 3 || m.Kind != MarkDead {
		t.Errorf("output 2: %+v", m)
	}
	if m, ok, err := s.CurrentMark(bg, keys[0]); ok || err != nil {
		t.Errorf("CurrentMark none: %+v %v %v", m, ok, err)
	}
	if m, ok, err := s.CurrentMark(bg, keys[2]); !ok || err != nil || m.Seq != 3 {
		t.Errorf("CurrentMark: %+v %v %v", m, ok, err)
	}
	// Mined mark details round trip.
	mustMark(Mark{TxID: keys[1].TxID, Vout: keys[1].Vout, Kind: MarkMined, Height: 9, BlockHash: txid('8')})
	if m, _, _ := s.CurrentMark(bg, keys[1]); m.Seq != 2 || m.Height != 9 || m.BlockHash != txid('8') {
		t.Errorf("mined: %+v", m)
	}
	// Refusals, in Go and in SQL.
	for name, m := range map[string]Mark{
		"mined no height": {TxID: keys[0].TxID, Vout: 0, Kind: MarkMined, BlockHash: txid('7')},
		"mined no hash":   {TxID: keys[0].TxID, Vout: 0, Kind: MarkMined, Height: 1},
		"mempool height":  {TxID: keys[0].TxID, Vout: 0, Kind: MarkMempool, Height: 1},
		"dead hash":       {TxID: keys[0].TxID, Vout: 0, Kind: MarkDead, BlockHash: txid('7')},
		"bad kind":        {TxID: keys[0].TxID, Vout: 0, Kind: "x"},
		"bad txid":        {TxID: "ab", Vout: 0, Kind: MarkDead},
	} {
		if err := s.AddMark(bg, m); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.AddMark(bg, Mark{TxID: txid('9'), Vout: 0, Kind: MarkDead}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown output: %v", err)
	}
	for name, q := range map[string]string{
		"mined no height": `INSERT INTO onchain_marks VALUES (?, 0, 1, 'mined', 0, '` + txid('7') + `', 5)`,
		"mined no hash":   `INSERT INTO onchain_marks VALUES (?, 0, 1, 'mined', 3, '', 5)`,
		"mempool height":  `INSERT INTO onchain_marks VALUES (?, 0, 1, 'mempool', 3, '', 5)`,
		"unknown output":  `INSERT INTO onchain_marks VALUES ('` + txid('9') + `', 0, 1, 'dead', 0, '', 5)`,
		"bad kind":        `INSERT INTO onchain_marks VALUES (?, 0, 1, 'zzz', 0, '', 5)`,
	} {
		if _, err := s.db.Exec(q, keys[0].TxID); err == nil {
			t.Errorf("SQL accepted %s", name)
		}
	}
}

// AddSettled records the fact that an output is settled for good, once; WatchedTXOs leaves it out from then on,
// in any process. The fact is insert-only and needs a stored output.
func TestAddSettled(t *testing.T) {
	s, _, _ := freshStore(t)
	o := mkChainOrder(t, s, "settled", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	xs := []TXO{{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 5}, {TxID: txid('a'), Vout: 1, Address: addr(0), AmountSat: 6}}
	if _, err := s.RecordScan(bg, xs, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	k := TXOKey{TxID: txid('a'), Vout: 0}
	if added, err := s.AddSettled(bg, k, 7); !added || err != nil {
		t.Fatalf("first: %v %v", added, err)
	}
	if added, err := s.AddSettled(bg, k, 9); added || err != nil {
		t.Fatalf("replay: %v %v", added, err)
	}
	w, err := s.WatchedTXOs(bg, t0)
	if err != nil || len(w) != 1 || w[0].Vout != 1 {
		t.Fatalf("WatchedTXOs after AddSettled: %+v %v", w, err)
	}
	if _, err := s.AddSettled(bg, TXOKey{TxID: txid('b')}, 7); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown output: %v", err)
	}
	for name, c := range map[string]struct {
		k TXOKey
		h int64
	}{"bad txid": {TXOKey{TxID: "ab"}, 7}, "height 0": {TXOKey{TxID: txid('a'), Vout: 1}, 0}} {
		if _, err := s.AddSettled(bg, c.k, c.h); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, q := range map[string]string{
		"unknown output": `INSERT INTO onchain_settled VALUES ('` + txid('9') + `', 0, 7, 5)`,
		"height 0":       `INSERT INTO onchain_settled VALUES ('` + txid('a') + `', 1, 0, 5)`,
		"update":         `UPDATE onchain_settled SET height = 8`,
		"delete":         `DELETE FROM onchain_settled`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("SQL accepted %s", name)
		}
	}
}

func TestWatchWindowBoundaries(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkChainOrder(t, s, "r", 1000, exp)
	mkAddr(t, s, o, 0)
	// First seen at the expiry (late), so only the window arm of WatchedTXOs applies here; the stuck arm (an on-time
	// output, kept past the window until settled) is TestWatchedTXOsPastTheWindow's.
	clk.set(exp)
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 5}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	edge := exp.Add(WatchWindow)
	clk.set(edge)
	for _, c := range []struct {
		now time.Time
		in  bool
	}{{edge.Add(-time.Second), true}, {edge, false}, {edge.Add(time.Second), false}, {exp, true}} {
		w, err := s.WatchedTXOs(bg, c.now)
		if err != nil || (len(w) == 1) != c.in {
			t.Errorf("WatchedTXOs(%v): %d %v, want in=%v", c.now, len(w), err, c.in)
		}
		p, err := s.OnchainOrdersPastExpiry(bg, c.now)
		wantIn := c.in && !c.now.Before(exp)
		if err != nil || (len(p) == 1) != wantIn {
			t.Errorf("OnchainOrdersPastExpiry(%v): %d %v, want in=%v", c.now, len(p), err, wantIn)
		}
	}
	// Before expiry: not listed.
	if p, _ := s.OnchainOrdersPastExpiry(bg, exp.Add(-time.Second)); len(p) != 0 {
		t.Error("listed before expiry")
	}
	// A final event removes it from OnchainOrdersPastExpiry.
	clk.set(t0)
	now := exp.Add(time.Hour)
	for _, typ := range []string{EventPaid, EventExpired, EventUnderpaid} {
		o2 := mkChainOrder(t, s, "r-"+typ, 1000, exp)
		if _, err := s.AddEvent(bg, Event{Type: typ, OrderID: o2.ID}); err != nil {
			t.Fatal(err)
		}
		p, _ := s.OnchainOrdersPastExpiry(bg, now)
		for _, x := range p {
			if x.ID == o2.ID {
				t.Errorf("%s order listed", typ)
			}
		}
	}
	// A Lightning-only order is never listed.
	ln := mkOrder(t, s, "ln", 1000, exp)
	for _, x := range func() []Order { p, _ := s.OnchainOrdersPastExpiry(bg, now); return p }() {
		if x.ID == ln.ID {
			t.Error("lightning-only order listed")
		}
	}
}

func TestOrdersToExpireLeavesOnchainOut(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	ln := mkOrder(t, s, "ln", 1000, exp)
	mkChainOrder(t, s, "chain", 1000, exp)
	both, _, err := s.CreateOrder(bg, Order{AccountID: "a", MerchantRef: "both", AmountMsat: 1000, AmountSat: 10, ExpiresAt: exp})
	if err != nil {
		t.Fatal(err)
	}
	_ = both
	got, err := s.OrdersToExpire(bg, exp.Add(time.Second))
	if err != nil || len(got) != 1 || got[0].ID != ln.ID {
		t.Fatalf("OrdersToExpire: %+v %v", got, err)
	}
}

func TestClosedInvoicesWithoutPaid(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(20 * time.Minute)
	o1 := mkOrder(t, s, "r1", 1000, t0.Add(time.Hour))
	o2 := mkOrder(t, s, "r2", 1000, t0.Add(time.Hour))
	o3 := mkOrder(t, s, "r3", 1000, t0.Add(time.Hour))
	i1, i2, i3 := mkInvoice(t, s, o1, 'a', exp), mkInvoice(t, s, o2, 'b', exp), mkInvoice(t, s, o3, 'c', exp)
	clk.set(exp.Add(time.Minute))
	if _, err := s.CloseInvoice(bg, i3.PaymentHash, exp, CloseLookup); err != nil { // before since
		t.Fatal(err)
	}
	clk.set(exp.Add(10 * time.Minute))
	if _, err := s.CloseInvoice(bg, i2.PaymentHash, exp.Add(5*time.Minute), CloseLookup); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseInvoice(bg, i1.PaymentHash, exp.Add(2*time.Minute), CloseLookup); err != nil {
		t.Fatal(err)
	}
	since := exp.Add(time.Minute)
	got, err := s.ClosedInvoicesWithoutPaid(bg, since)
	if err != nil || len(got) != 2 || got[0].PaymentHash != i1.PaymentHash || got[1].PaymentHash != i2.PaymentHash {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := s.AddEvent(bg, Event{Type: EventPaid, OrderID: o1.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ClosedInvoicesWithoutPaid(bg, since)
	if len(got) != 1 || got[0].PaymentHash != i2.PaymentHash {
		t.Fatalf("after paid: %+v", got)
	}
	// An expired event is not a paid event.
	if _, err := s.AddEvent(bg, Event{Type: EventExpired, OrderID: o2.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ClosedInvoicesWithoutPaid(bg, since); len(got) != 1 {
		t.Fatalf("expired event hid it: %+v", got)
	}
	if got, _ = s.ClosedInvoicesWithoutPaid(bg, exp.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("since after all: %+v", got)
	}
}

func TestEventSequences(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	clk.set(t0.Add(time.Minute))
	add := func(typ string, seq int) (bool, error) {
		return s.AddEvent(bg, Event{Type: typ, OrderID: o.ID, Seq: seq})
	}
	if ok, err := add(EventReverted, 1); !ok || err != nil {
		t.Fatalf("reverted 1: %v %v", ok, err)
	}
	if ok, err := add(EventReverted, 1); ok || err != nil {
		t.Fatalf("reverted 1 again: %v %v", ok, err)
	}
	if _, err := add(EventReverted, 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reverted 3 before 2: %v", err)
	}
	if ok, err := add(EventReverted, 2); !ok || err != nil {
		t.Fatalf("reverted 2: %v %v", ok, err)
	}
	if ok, err := add(EventReverted, 0); ok || err != nil { // seq 0 means 1, which exists
		t.Fatalf("reverted 0: %v %v", ok, err)
	}
	// One order per final type: a second final event is never added (TestFinalEventOnce).
	for i, typ := range []string{EventPaid, EventExpired, EventUnderpaid, EventLate} {
		on := o
		if typ == EventExpired || typ == EventUnderpaid {
			on = mkChainOrder(t, s, "final-"+itoa(int64(i)), 1000, t0.Add(time.Hour))
		}
		add := func(typ string, seq int) (bool, error) {
			return s.AddEvent(bg, Event{Type: typ, OrderID: on.ID, Seq: seq})
		}
		if _, err := add(typ, 2); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s seq 2: %v", typ, err)
		}
		if _, err := add(typ, -1); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s seq -1: %v", typ, err)
		}
		if ok, err := add(typ, 1); !ok || err != nil {
			t.Errorf("%s seq 1: %v %v", typ, ok, err)
		}
		if ok, err := add(typ, 0); ok || err != nil {
			t.Errorf("%s seq 0 again: %v %v", typ, ok, err)
		}
	}
	if _, err := add(EventPaidAgain, 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("paid_again 3 without reverted 3: %v", err)
	}
	if ok, err := add(EventPaidAgain, 0); !ok || err != nil { // seq 0 means 1
		t.Fatalf("paid_again seq 0: %v %v", ok, err)
	}
	if ok, err := add(EventPaidAgain, 1); ok || err != nil {
		t.Fatalf("paid_again 1 again: %v %v", ok, err)
	}
	if ok, err := add(EventPaidAgain, 2); !ok || err != nil {
		t.Fatalf("paid_again 2: %v %v", ok, err)
	}
	// A fresh order: paid_again before any reverted.
	o2 := mkChainOrder(t, s, "r2", 1000, t0.Add(time.Hour))
	if _, err := s.AddEvent(bg, Event{Type: EventPaidAgain, OrderID: o2.ID, Seq: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("paid_again before reverted: %v", err)
	}
	if _, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: o2.ID, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AddEvent(bg, Event{Type: EventPaidAgain, OrderID: o2.ID, Seq: 1}); !ok || err != nil {
		t.Fatalf("paid_again after reverted: %v %v", ok, err)
	}
	if ok, err := s.AddEvent(bg, Event{Type: EventPaidAgain, OrderID: o2.ID, Seq: 1}); ok || err != nil {
		t.Fatalf("paid_again again: %v %v", ok, err)
	}
	// Wrong id.
	if _, err := s.AddEvent(bg, Event{ID: EventID(EventReverted, o.ID, 1), Type: EventReverted, OrderID: o.ID, Seq: 2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("id for another seq: %v", err)
	}
	// Seq reaches the pending list.
	pend, err := s.UndeliveredEvents(bg, MainEndpoint, t0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range pend {
		if p.OrderID == o.ID {
			seen[p.Type] += p.Seq
			if p.ID != EventID(p.Type, p.OrderID, p.Seq) {
				t.Errorf("pending id %s for %s seq %d", p.ID, p.Type, p.Seq)
			}
		}
	}
	if seen[EventReverted] != 3 || seen[EventPaidAgain] != 3 || seen[EventPaid] != 1 {
		t.Fatalf("seqs %v", seen)
	}
}

func TestEventIDDerivation(t *testing.T) {
	oid := strings.Repeat("ab", 16)
	sum := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:])[:32] }
	if got, want := EventID("order.paid", oid, 1), sum("satbooth/event/v1order.paid"+oid); got != want {
		t.Fatalf("seq 1 = %s, want %s", got, want)
	}
	if got, want := EventID("order.reverted", oid, 2), sum("satbooth/event/v1order.reverted"+oid+"/2"); got != want {
		t.Fatalf("seq 2 = %s, want %s", got, want)
	}
	if got, want := EventID("order.reverted", oid, 12), sum("satbooth/event/v1order.reverted"+oid+"/12"); got != want {
		t.Fatalf("seq 12 = %s, want %s", got, want)
	}
	// Pinned values, computed with shasum -a 256 over the described input.
	fixed := strings.Repeat("ab", 16)
	if got := EventID("order.paid", fixed, 1); got != "1563ec674292cfba08b069afc3fe4aff" {
		t.Fatalf("pinned seq 1 = %s", got)
	}
	if got := EventID("order.reverted", fixed, 2); got != "3f33435cb08ab966cab70c368b59e124" {
		t.Fatalf("pinned seq 2 = %s", got)
	}
	if EventID("order.paid", oid, 0) != EventID("order.paid", oid, 1) || EventID("order.paid", oid, -3) != EventID("order.paid", oid, 1) {
		t.Fatal("seq <= 0 is not seq 1")
	}
	if EventID("order.paid_again", oid, 1) == EventID("order.paid", oid, 1) {
		t.Fatal("types collide")
	}
	if EventID("order.reverted", oid, 1) == EventID("order.reverted", oid, 2) {
		t.Fatal("seqs collide")
	}
}

func TestOrdersAwaitingPaidAgain(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkChainOrder(t, s, "r", 1000, exp)
	lnOnly := mkOrder(t, s, "ln", 1000, exp)
	now := exp.Add(time.Hour)
	list := func(at time.Time) []Order {
		t.Helper()
		l, err := s.OrdersAwaitingPaidAgain(bg, at)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if len(list(now)) != 0 {
		t.Fatal("listed with no reversal")
	}
	if _, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: o.ID, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: lnOnly.ID, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if l := list(now); len(l) != 1 || l[0].ID != o.ID {
		t.Fatalf("after reverted: %+v", l)
	}
	if len(list(exp.Add(WatchWindow))) != 0 {
		t.Fatal("listed at the window edge")
	}
	if len(list(exp.Add(WatchWindow-time.Second))) != 1 {
		t.Fatal("not listed inside the window")
	}
	if _, err := s.AddEvent(bg, Event{Type: EventPaidAgain, OrderID: o.ID, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if len(list(now)) != 0 {
		t.Fatal("listed after paid_again")
	}
	if _, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: o.ID, Seq: 2}); err != nil {
		t.Fatal(err)
	}
	if len(list(now)) != 1 {
		t.Fatal("not listed after the second reversal")
	}
}

// The plan 5 schema's orders table, as the guard test needs it.
const oldOrdersDDL = `CREATE TABLE orders (
	id TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, account_id TEXT NOT NULL, merchant_ref TEXT NOT NULL UNIQUE,
	amount_msat INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID`

// The plan 7 schema's orders table (amount_sat, no amount_piconero), as the guard test needs it.
const plan7OrdersDDL = `CREATE TABLE orders (
	id TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE, account_id TEXT NOT NULL, merchant_ref TEXT NOT NULL UNIQUE,
	amount_msat INTEGER NOT NULL, amount_sat INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
	CHECK (amount_msat > 0 OR amount_sat > 0)
) STRICT, WITHOUT ROWID`

// A database from before plan 8 (plan 5's orders at version 1, plan 7's at version 4) is refused with ErrOldSchema;
// nothing is migrated.
func TestOldSchemaGuard(t *testing.T) {
	for name, c := range map[string]struct {
		ddl     string
		version int
	}{"plan 5": {oldOrdersDDL, 1}, "plan 7": {plan7OrdersDDL, 4}} {
		path := filepath.Join(t.TempDir(), "old.db")
		raw, err := openRawForTest(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{c.ddl, `CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`,
			`INSERT INTO store_schema_version VALUES (` + itoa(int64(c.version)) + `)`} {
			if _, err := raw.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		raw.Close()
		s, err := Open(bg, path)
		if err == nil {
			s.Close()
			t.Fatalf("%s: old schema opened", name)
		}
		if !errors.Is(err, ErrOldSchema) || err.Error() != "store: the database is from before the Monero tables; this version does not upgrade it; start with a new store and a new ledger (both database files)" {
			t.Fatalf("%s: err = %v", name, err)
		}
		raw, err = openRawForTest(path)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(tableNames(t, raw)); n != 1 {
			t.Errorf("%s: %d tables after the refusal", name, n)
		}
		raw.Close()
	}
	// A fresh file does not trip the guard, and reopening it does not either.
	fresh := filepath.Join(t.TempDir(), "new.db")
	for i := 0; i < 2; i++ {
		s, err := Open(bg, fresh)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s.Close()
	}
}

func TestOnchainRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: t0}
	s, err := Open(bg, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 3)
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('a'), Vout: 1, Address: addr(3), AmountSat: 900}}, Cursor{BlockHash: txid('1'), Height: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMark(bg, Mark{TxID: txid('a'), Vout: 1, Kind: MarkMined, Height: 7, BlockHash: txid('1')}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: o.ID, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	check := func(s *Store) {
		t.Helper()
		if a, err := s.AddressForOrder(bg, o.ID); err != nil || a.Index != 3 {
			t.Fatalf("address: %+v %v", a, err)
		}
		if l, _ := s.TXOsForOrder(bg, o.ID); len(l) != 1 || l[0].AmountSat != 900 {
			t.Fatalf("txos %+v", l)
		}
		if c, ok, _ := s.Cursor(bg); !ok || c.Height != 7 {
			t.Fatalf("cursor %+v", c)
		}
		if m, ok, _ := s.CurrentMark(bg, TXOKey{TxID: txid('a'), Vout: 1}); !ok || m.Kind != MarkMined {
			t.Fatalf("mark %+v", m)
		}
		if ok, err := s.AddEvent(bg, Event{Type: EventReverted, OrderID: o.ID, Seq: 1}); ok || err != nil {
			t.Fatalf("event again: %v %v", ok, err)
		}
		if idx, ok, _ := s.MaxAddressIndex(bg); !ok || idx != 3 {
			t.Fatalf("max %d", idx)
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

func TestRecordScanMismatchAndAddressNeedsOnchain(t *testing.T) {
	s, _, _ := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	o2 := mkChainOrder(t, s, "r2", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	mkAddr(t, s, o2, 1)
	x := TXO{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 500}
	cur := Cursor{BlockHash: txid('1'), Height: 1}
	if _, err := s.RecordScan(bg, []TXO{x}, cur); err != nil {
		t.Fatal(err)
	}
	for name, y := range map[string]TXO{
		"other address": {TxID: x.TxID, Vout: 0, Address: addr(1), AmountSat: 500},
		"other amount":  {TxID: x.TxID, Vout: 0, Address: addr(0), AmountSat: 501},
	} {
		if _, err := s.RecordScan(bg, []TXO{y}, Cursor{BlockHash: txid('2'), Height: 2}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c, _, _ := s.Cursor(bg); c.BlockHash != txid('1') {
		t.Error("a refused scan moved the cursor")
	}
	if _, err := s.RecordScan(bg, []TXO{x}, cur); err != nil {
		t.Errorf("identical redelivery: %v", err)
	}
	// An order with no on-chain amount takes no address, in Go and by the trigger.
	ln := mkOrder(t, s, "ln", 1000, t0.Add(time.Hour))
	if err := s.AddAddress(bg, Address{OrderID: ln.ID, Address: addr(2), Index: 2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("lightning-only order: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO addresses VALUES (?, ?, 2, 5)`, addr(2), ln.ID); err == nil {
		t.Fatal("trigger accepted an address for a lightning-only order")
	}
}

// The watch-window queries start from orders_expires, not a scan of every order.
func TestWatchQueriesUseTheExpiryIndex(t *testing.T) {
	s, _, _ := freshStore(t)
	lo := t0.Unix()
	for name, q := range map[string]string{
		"OnchainOrdersPastExpiry": onchainPastExpirySQL,
		"OrdersAwaitingPaidAgain": awaitingPaidAgainSQL,
		"WatchedTXOs":             watchedTXOsSQL,
		"HasOpenOnchainOrder":     openOnchainSQL,
	} {
		args := []any{lo}
		if name == "OnchainOrdersPastExpiry" {
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
		// WatchedTXOs and HasOpenOnchainOrder have a second arm, the stuck outputs (stuckTXOSQL): it scans onchain_txos
		// once a pass, by design, and the assertion holds for the first arm, the orders inside their window.
		if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "INDEX orders_expires (expires_at") {
			t.Errorf("%s plan does not use orders_expires:\n%s", name, joined)
		}
	}
}

// CurrentMarks reads in batches and agrees with CurrentMark key by key, on random outputs and marks: several
// outputs of one transaction, outputs without a mark, keys asked twice, unknown keys, and more keys than one batch.
func TestCurrentMarksAgreesWithCurrentMark(t *testing.T) {
	s, _, _ := freshStore(t)
	rng := rand.New(rand.NewSource(29))
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	hexTx := func(i int) string {
		h := sha256.Sum256([]byte(itoa(int64(i))))
		return hex.EncodeToString(h[:])
	}
	var all []TXOKey
	var txos []TXO
	for i := 0; i < 700; i++ { // 700 txids: more than one batch of sqlChunk
		for v := 0; v < 1+rng.Intn(3); v++ {
			x := TXO{TxID: hexTx(i), Vout: uint32(v * (1 + rng.Intn(5))), Address: addr(0), AmountSat: 1}
			k := TXOKey{TxID: x.TxID, Vout: x.Vout}
			if len(all) > 0 && all[len(all)-1] == k {
				continue
			}
			txos = append(txos, x)
			all = append(all, k)
		}
	}
	if _, err := s.RecordScan(bg, txos, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	kinds := []Mark{{Kind: MarkMempool}, {Kind: MarkDead}, {Kind: MarkMined, Height: 7, BlockHash: txid('7')}}
	for _, k := range all {
		for n := rng.Intn(4); n > 0; n-- { // 0 to 3 marks
			m := kinds[rng.Intn(len(kinds))]
			m.TxID, m.Vout = k.TxID, k.Vout
			if err := s.AddMark(bg, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	for round := 0; round < 20; round++ {
		var keys []TXOKey
		for n := rng.Intn(len(all) + 1); n > 0; n-- {
			switch r := rng.Intn(10); {
			case r == 0:
				keys = append(keys, TXOKey{TxID: hexTx(10_000 + rng.Intn(5)), Vout: 0}) // unknown
			case r == 1:
				k := all[rng.Intn(len(all))]
				keys = append(keys, TXOKey{TxID: k.TxID, Vout: k.Vout + 100}) // a vout of a known txid that is not stored
			default:
				keys = append(keys, all[rng.Intn(len(all))]) // duplicates included
			}
		}
		got, err := s.CurrentMarks(bg, keys)
		if err != nil {
			t.Fatal(err)
		}
		want := map[TXOKey]Mark{}
		for _, k := range keys {
			m, ok, err := s.CurrentMark(bg, k)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				want[k] = m
			}
		}
		if len(got) != len(want) {
			t.Fatalf("round %d: %d marks, want %d", round, len(got), len(want))
		}
		for k, m := range want {
			if got[k] != m {
				t.Fatalf("round %d %s:%d: %+v, want %+v", round, k.TxID[:8], k.Vout, got[k], m)
			}
		}
	}
	if m, err := s.CurrentMarks(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("no keys: %+v %v", m, err)
	}
}

// SettledKeys reads in batches and names exactly the keys WatchedTXOs leaves out, on
// random outputs: several outputs of one transaction, keys asked twice, unknown keys, a vout of a known txid that is
// not stored, and more txids than one batch of sqlChunk.
func TestSettledKeysAgreesWithWatchedTXOs(t *testing.T) {
	s, _, _ := freshStore(t)
	rng := rand.New(rand.NewSource(31))
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	hexTx := func(i int) string {
		h := sha256.Sum256([]byte("settled" + itoa(int64(i))))
		return hex.EncodeToString(h[:])
	}
	var all []TXOKey
	var txos []TXO
	for i := 0; i < 700; i++ {
		for v := 0; v < 1+rng.Intn(3); v++ {
			x := TXO{TxID: hexTx(i), Vout: uint32(v * (1 + rng.Intn(5))), Address: addr(0), AmountSat: 1}
			k := TXOKey{TxID: x.TxID, Vout: x.Vout}
			if len(all) > 0 && all[len(all)-1] == k {
				continue
			}
			txos = append(txos, x)
			all = append(all, k)
		}
	}
	if _, err := s.RecordScan(bg, txos, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	settled := map[TXOKey]bool{}
	for _, k := range all {
		if rng.Intn(2) == 0 {
			if _, err := s.AddSettled(bg, k, 1); err != nil {
				t.Fatal(err)
			}
			settled[k] = true
		}
	}
	// Inside the order's window every output is watched unless it is settled.
	w, err := s.WatchedTXOs(bg, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != len(all)-len(settled) {
		t.Fatalf("%d watched, want %d", len(w), len(all)-len(settled))
	}
	for _, x := range w {
		if settled[TXOKey{TxID: x.TxID, Vout: x.Vout}] {
			t.Fatalf("a settled output is watched: %s:%d", x.TxID[:8], x.Vout)
		}
	}
	for round := 0; round < 20; round++ {
		var keys []TXOKey
		for n := rng.Intn(len(all) + 1); n > 0; n-- {
			switch r := rng.Intn(10); {
			case r == 0:
				keys = append(keys, TXOKey{TxID: hexTx(10_000 + rng.Intn(5)), Vout: 0}) // unknown
			case r == 1:
				k := all[rng.Intn(len(all))]
				keys = append(keys, TXOKey{TxID: k.TxID, Vout: k.Vout + 100}) // not stored
			default:
				keys = append(keys, all[rng.Intn(len(all))]) // duplicates included
			}
		}
		got, err := s.SettledKeys(bg, keys)
		if err != nil {
			t.Fatal(err)
		}
		want := map[TXOKey]bool{}
		for _, k := range keys {
			if settled[k] {
				want[k] = true
			}
		}
		if len(got) != len(want) {
			t.Fatalf("round %d: %d settled, want %d", round, len(got), len(want))
		}
		for k := range want {
			if !got[k] {
				t.Fatalf("round %d %s:%d: not named settled", round, k.TxID[:8], k.Vout)
			}
		}
	}
	if m, err := s.SettledKeys(bg, nil); err != nil || len(m) != 0 {
		t.Fatalf("no keys: %+v %v", m, err)
	}
}

// OnchainOrdersLeftWindow lists the on-chain orders whose window closed in (since, now] without a final event.
func TestOnchainOrdersLeftWindow(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkChainOrder(t, s, "r", 1000, exp)
	done := mkChainOrder(t, s, "done", 1000, exp)
	if _, err := s.AddEvent(bg, Event{Type: EventUnderpaid, OrderID: done.ID}); err != nil {
		t.Fatal(err)
	}
	mkOrder(t, s, "ln", 1000, exp) // Lightning only: never listed
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
		l, err := s.OnchainOrdersLeftWindow(bg, c.since, c.now)
		if err != nil || (len(l) == 1 && l[0].ID == o.ID) != c.in || len(l) > 1 {
			t.Errorf("(%v, %v]: %+v %v, want in=%v", c.since.Sub(edge), c.now.Sub(edge), l, err, c.in)
		}
	}
}

// HasOpenOnchainOrder is true for an on-chain order inside its watch window (before its expiry too) without a
// final event, and false at the window's end, with a final event, and for a Lightning-only order.
func TestHasOpenOnchainOrder(t *testing.T) {
	s, _, _ := freshStore(t)
	exp := t0.Add(time.Hour)
	edge := exp.Add(WatchWindow)
	check := func(name string, now time.Time, want bool) {
		t.Helper()
		if got, err := s.HasOpenOnchainOrder(bg, now); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	check("empty", t0, false)
	mkOrder(t, s, "ln", 1000, exp)
	check("lightning only", t0, false)
	o := mkChainOrder(t, s, "r", 1000, exp)
	check("before expiry", t0, true)
	check("at expiry", exp, true)
	check("last second of the window", edge.Add(-time.Second), true)
	check("window end", edge, false)
	for i, typ := range []string{EventPaid, EventExpired, EventUnderpaid} {
		o2 := mkChainOrder(t, s, "f"+itoa(int64(i)), 1000, exp)
		if _, err := s.AddEvent(bg, Event{Type: typ, OrderID: o2.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddEvent(bg, Event{Type: EventLate, OrderID: o.ID}); err != nil { // not final
		t.Fatal(err)
	}
	check("late is not final", exp, true)
	if _, err := s.AddEvent(bg, Event{Type: EventExpired, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	check("all final", exp, false)
}

// An on-time output still unconfirmed at the window close stays watched until it is settled: WatchedTXOs at and past
// ExpiresAt + WatchWindow lists the unsettled outputs first seen before the expiry, whatever their mark (none,
// mempool, dead then mempool, mempool then dead, mined: the watcher settles a dead one once its order settled, so a
// crash between its dead mark and its reversal is healed by the next pass), and leaves out one first seen at the
// expiry and a settled one. One second before the window end every unsettled output is listed, as before.
func TestWatchedTXOsPastTheWindow(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkChainOrder(t, s, "stuck", 1000, exp)
	mkAddr(t, s, o, 0)
	other := mkChainOrder(t, s, "other", 1000, exp.Add(time.Hour)) // a later window: still open at o's window end
	mkAddr(t, s, other, 1)
	out := func(d byte) TXO { return TXO{TxID: txid(d), Vout: 0, Address: addr(0), AmountSat: 5} }
	scan := func(xs ...TXO) {
		t.Helper()
		if _, err := s.RecordScan(bg, xs, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
			t.Fatal(err)
		}
	}
	mark := func(d byte, kinds ...string) {
		t.Helper()
		for _, k := range kinds {
			m := Mark{TxID: txid(d), Vout: 0, Kind: k}
			if k == MarkMined {
				m.Height, m.BlockHash = 1, txid('1')
			}
			if err := s.AddMark(bg, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	settle := func(d byte) {
		t.Helper()
		if _, err := s.AddSettled(bg, TXOKey{TxID: txid(d)}, 6); err != nil {
			t.Fatal(err)
		}
	}
	clk.set(exp.Add(-time.Second)) // the last second before the expiry: on time
	scan(out('a'), out('b'), out('c'), out('d'), out('e'), out('f'))
	mark('b', MarkMempool)
	mark('c', MarkDead, MarkMempool)
	mark('d', MarkMempool, MarkDead)
	mark('e', MarkMined)
	mark('f', MarkMined)
	settle('f')
	clk.set(exp)                                                                  // first seen at the expiry: late
	scan(out('8'), TXO{TxID: txid('9'), Vout: 0, Address: addr(1), AmountSat: 5}) // 8 is o's, 9 is other's (on time)
	listed := func(now time.Time) string {
		t.Helper()
		w, err := s.WatchedTXOs(bg, now)
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
	// Ordered by first_seen, then txid: a..e at exp-1s, then 8 and 9 at exp.
	if got := listed(edge.Add(-time.Second)); got != "abcde89" {
		t.Errorf("one second before the window end: %q", got)
	}
	if got := listed(edge); got != "abcde9" {
		t.Errorf("at the window end: %q, want the on-time a..e (stuck) and 9 (inside other's window)", got)
	}
	if got := listed(edge.Add(2 * time.Hour)); got != "abcde9" {
		t.Errorf("past both windows: %q, want the stuck a..e and 9 (on time for other)", got)
	}
	settle('d')
	settle('e')
	if got := listed(edge); got != "abc9" {
		t.Errorf("after d and e are settled: %q", got)
	}
}

// HasOpenOnchainOrder past the window: true while a stuck output (on time, not settled) exists, whatever the order's
// events and its mark; false once it is settled.
func TestHasOpenOnchainOrderPastTheWindow(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	edge := exp.Add(WatchWindow)
	o := mkChainOrder(t, s, "stuck", 1000, exp)
	mkAddr(t, s, o, 0)
	check := func(name string, now time.Time, want bool) {
		t.Helper()
		if got, err := s.HasOpenOnchainOrder(bg, now); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	check("no output, at the window end", edge, false)
	clk.set(exp) // first seen at the expiry: late, never stuck
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('7'), Vout: 0, Address: addr(0), AmountSat: 5}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	check("a late output, at the window end", edge, false)
	clk.set(exp.Add(-time.Second))
	if _, err := s.RecordScan(bg, []TXO{{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 5}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	check("a stuck output, at the window end", edge, true)
	check("a stuck output, a month later", edge.Add(30*24*time.Hour), true)
	if _, err := s.AddEvent(bg, Event{Type: EventUnderpaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	check("after order.underpaid", edge, true)
	check("after order.underpaid, inside the window", exp, true)
	if err := s.AddMark(bg, Mark{TxID: txid('a'), Vout: 0, Kind: MarkDead}); err != nil {
		t.Fatal(err)
	}
	check("dead, not yet settled", edge, true) // its order must still settle once (a reversal, its event)
	if _, err := s.AddSettled(bg, TXOKey{TxID: txid('a')}, 8); err != nil {
		t.Fatal(err)
	}
	check("settled", edge, false)
	check("settled, inside the window with a final event", exp, false)
}

// RecordScan reopens a settled output whose current mark is dead when the scan lists it mined (Confirmed): its
// settled fact is deleted in the scan's transaction, so it is watched again (on time: the stuck set) and the startup
// guard holds. Listed unconfirmed, or with any other current mark, nothing changes; a reopened output listed again is
// a no-op. Confirmed is never stored or returned.
// Confirmed is read by RecordScan only, never stored or returned: the rows it adds come back with Confirmed false,
// and a stored row reads false too.
func TestRecordScanReturnsNoConfirmed(t *testing.T) {
	s, _, _ := freshStore(t)
	o := mkChainOrder(t, s, "confirmed", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	added, err := s.RecordScan(bg, []TXO{{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 5, Confirmed: true}},
		Cursor{BlockHash: txid('1'), Height: 1})
	if err != nil || len(added) != 1 || added[0].Confirmed {
		t.Fatalf("added %+v, %v", added, err)
	}
	if x, err := s.TXO(bg, TXOKey{TxID: txid('a')}); err != nil || x.Confirmed {
		t.Fatalf("stored %+v, %v", x, err)
	}
}

func TestRecordScanReopensADeadSettledOutput(t *testing.T) {
	s, _, clk := freshStore(t)
	exp := t0.Add(time.Hour)
	o := mkChainOrder(t, s, "reopen", 1000, exp)
	mkAddr(t, s, o, 0)
	out := func(d byte, confirmed bool) TXO {
		return TXO{TxID: txid(d), Vout: 0, Address: addr(0), AmountSat: 5, Confirmed: confirmed}
	}
	scan := func(xs ...TXO) {
		t.Helper()
		if _, err := s.RecordScan(bg, xs, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
			t.Fatal(err)
		}
	}
	scan(out('a', false), out('b', false), out('c', false))
	for d, k := range map[byte]string{'a': MarkDead, 'b': MarkMempool, 'c': MarkMined} {
		m := Mark{TxID: txid(d), Vout: 0, Kind: k}
		if k == MarkMined {
			m.Height, m.BlockHash = 1, txid('1')
		}
		if err := s.AddMark(bg, m); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddSettled(bg, TXOKey{TxID: txid(d)}, 6); err != nil {
			t.Fatal(err)
		}
	}
	edge := exp.Add(WatchWindow)
	clk.set(edge)
	watched := func() string {
		t.Helper()
		w, err := s.WatchedTXOs(bg, edge)
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
		b, err := s.HasOpenOnchainOrder(bg, edge)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if watched() != "" || open() {
		t.Fatal("setup: something watched")
	}
	scan(out('a', false), out('b', true), out('c', true)) // a unconfirmed; b and c not dead
	if watched() != "" || open() {
		t.Fatalf("reopened without a dead mark listed mined: %q", watched())
	}
	scan(out('a', true))
	if watched() != "a" || !open() {
		t.Fatalf("not reopened: %q", watched())
	}
	scan(out('a', true)) // again: no settled fact left, nothing to do
	if watched() != "a" {
		t.Fatalf("replay: %q", watched())
	}
	if _, err := s.AddSettled(bg, TXOKey{TxID: txid('a')}, 9); err != nil { // settled again later
		t.Fatal(err)
	}
	if watched() != "" {
		t.Fatalf("settled again: %q", watched())
	}
	if l, _ := s.TXOsForOrder(bg, o.ID); len(l) != 3 || l[0].Confirmed {
		t.Fatalf("rows %+v", l)
	}
}

// rowCounts is the number of stored outputs and cursor rows.
func rowCounts(t *testing.T, s *Store) (txos, cursors int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_txos`).Scan(&txos); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM onchain_cursor`).Scan(&cursors); err != nil {
		t.Fatal(err)
	}
	return txos, cursors
}

// RecordScan stores a caller's FirstSeen that is not after the clock's now, in whole seconds; one after now
// (or before 1970-01-01T00:00:01Z) is ErrInvalid and nothing of the scan is written; zero is now.
func TestRecordScanCallerFirstSeen(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	now := t0.Add(time.Minute)
	clk.set(now)
	cur := Cursor{BlockHash: txid('1'), Height: 1}
	for i, c := range []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"one second before now", now.Add(-time.Second), now.Add(-time.Second)},
		{"equal to now", now, now},
		{"zero", time.Time{}, now},
		{"half a second after now", now.Add(500 * time.Millisecond), now},
		{"the first second of the epoch", time.Unix(1, 0), time.Unix(1, 0).UTC()},
	} {
		x := TXO{TxID: txid('a'), Vout: uint32(i), Address: addr(0), AmountSat: 5, FirstSeen: c.in}
		added, err := s.RecordScan(bg, []TXO{x}, cur)
		if err != nil || len(added) != 1 || !added[0].FirstSeen.Equal(c.want) {
			t.Fatalf("%s: %+v %v", c.name, added, err)
		}
		if got, err := s.TXO(bg, TXOKey{TxID: x.TxID, Vout: x.Vout}); err != nil || !got.FirstSeen.Equal(c.want) {
			t.Fatalf("%s: stored %+v %v", c.name, got, err)
		}
	}
	txos, cursors := rowCounts(t, s)
	for _, c := range []struct {
		name string
		in   time.Time
	}{
		{"one second after now", now.Add(time.Second)},
		{"the epoch", time.Unix(0, 0)},
		{"before the epoch", time.Unix(-5, 0)},
	} {
		// With a valid output and a new cursor in the same scan: one transaction, nothing written.
		l := []TXO{{TxID: txid('b'), Vout: 0, Address: addr(0), AmountSat: 5},
			{TxID: txid('b'), Vout: 1, Address: addr(0), AmountSat: 5, FirstSeen: c.in}}
		if _, err := s.RecordScan(bg, l, Cursor{BlockHash: txid('2'), Height: 2}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n, m := rowCounts(t, s); n != txos || m != cursors {
			t.Fatalf("%s: written: %d outputs, %d cursors", c.name, n, m)
		}
	}
	// A stored output keeps its row: a caller FirstSeen on it is not read, even one after now.
	x := TXO{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 5, FirstSeen: now.Add(time.Hour)}
	if added, err := s.RecordScan(bg, []TXO{x}, cur); err != nil || len(added) != 0 {
		t.Fatalf("stored: %+v %v", added, err)
	}
}

// The whole-second rule on seeded random times around now, sub-second parts included: stored as fromTS(ts(in)) when
// 1 <= ts(in) <= ts(now), else ErrInvalid with nothing written; zero is now.
func TestRecordScanFirstSeenProperty(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	rng := rand.New(rand.NewSource(41))
	for i := 0; i < 400; i++ {
		now := t0.Add(time.Duration(rng.Int63n(int64(time.Hour))))
		clk.set(now)
		var in time.Time
		switch rng.Intn(5) {
		case 0:
		case 1:
			in = time.Unix(rng.Int63n(5)-2, rng.Int63n(1e9)) // around the epoch
		default:
			in = now.Add(time.Duration(rng.Int63n(int64(6*time.Second))) - 3*time.Second)
		}
		x := TXO{TxID: hex.EncodeToString(func() []byte { h := sha256.Sum256([]byte(itoa(int64(i)))); return h[:] }()),
			Address: addr(0), AmountSat: 5, FirstSeen: in}
		before, _ := rowCounts(t, s)
		added, err := s.RecordScan(bg, []TXO{x}, Cursor{BlockHash: txid('1'), Height: 1})
		want, ok := fromTS(now.Unix()), true
		if !in.IsZero() {
			want, ok = fromTS(in.Unix()), in.Unix() >= 1 && in.Unix() <= now.Unix()
		}
		after, _ := rowCounts(t, s)
		switch {
		case ok && (err != nil || len(added) != 1 || !added[0].FirstSeen.Equal(want) || after != before+1):
			t.Fatalf("%d: in %v now %v: %+v %v", i, in, now, added, err)
		case !ok && (!errors.Is(err, ErrInvalid) || after != before):
			t.Fatalf("%d: in %v now %v: want ErrInvalid, got %v (%d rows)", i, in, now, err, after-before)
		}
	}
}

// TXOsByTxIDs reads in batches of sqlChunk: 0, 1 and 501 txids, unknown ones left out, every output of a txid, a
// txid asked twice read once, and the result oldest first across the batches.
func TestTXOsByTxIDs(t *testing.T) {
	s, _, clk := freshStore(t)
	o := mkChainOrder(t, s, "r", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	hexTx := func(i int) string {
		h := sha256.Sum256([]byte("tx " + itoa(int64(i))))
		return hex.EncodeToString(h[:])
	}
	clk.set(t0.Add(time.Minute))
	var first []TXO
	for i := 0; i < 500; i++ {
		first = append(first, TXO{TxID: hexTx(i), Address: addr(0), AmountSat: int64(1 + i)})
	}
	if _, err := s.RecordScan(bg, first, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	// The 501st txid, two outputs, stored earlier than the rest (an inherited time): oldest first puts it first,
	// though it is read in the second batch.
	last := []TXO{{TxID: hexTx(500), Vout: 0, Address: addr(0), AmountSat: 9, FirstSeen: t0},
		{TxID: hexTx(500), Vout: 3, Address: addr(0), AmountSat: 8, FirstSeen: t0}}
	if _, err := s.RecordScan(bg, last, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	if l, err := s.TXOsByTxIDs(bg, nil); err != nil || len(l) != 0 {
		t.Fatalf("none: %+v %v", l, err)
	}
	if l, err := s.TXOsByTxIDs(bg, []string{hexTx(7)}); err != nil || len(l) != 1 || l[0].TxID != hexTx(7) || l[0].AmountSat != 8 ||
		!l[0].FirstSeen.Equal(t0.Add(time.Minute)) {
		t.Fatalf("one: %+v %v", l, err)
	}
	var ids []string
	for i := 0; i < 501; i++ {
		ids = append(ids, hexTx(i))
	}
	l, err := s.TXOsByTxIDs(bg, ids)
	if err != nil || len(l) != 502 {
		t.Fatalf("501 txids: %d rows %v", len(l), err)
	}
	if l[0].TxID != hexTx(500) || l[0].Vout != 0 || l[1].TxID != hexTx(500) || l[1].Vout != 3 || !l[0].FirstSeen.Equal(t0) {
		t.Fatalf("not oldest first: %+v %+v", l[0], l[1])
	}
	for i := 3; i < len(l); i++ {
		p, c := l[i-1], l[i]
		if c.FirstSeen.Before(p.FirstSeen) || (c.FirstSeen.Equal(p.FirstSeen) && c.TxID < p.TxID) {
			t.Fatalf("row %d out of order", i)
		}
	}
	mixed := []string{hexTx(500), strings.Repeat("0", 64), hexTx(3), hexTx(500), "not hex"}
	if l, err := s.TXOsByTxIDs(bg, mixed); err != nil || len(l) != 3 || l[2].TxID != hexTx(3) {
		t.Fatalf("unknown and repeated: %+v %v", l, err)
	}
}
