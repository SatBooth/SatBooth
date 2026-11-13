package store

import (
	"database/sql"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/rates"
)

// sqlChecks evaluates the schema's CHECK expressions on a value, in a scratch in-memory database.
type sqlChecks struct{ db *sql.DB }

func newSQLChecks(t testing.TB) *sqlChecks {
	db, err := sql.Open("sqlite3", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return &sqlChecks{db}
}

func (c *sqlChecks) eval(t testing.TB, expr string, v any) bool {
	var ok sql.NullBool
	if err := c.db.QueryRow(`SELECT `+expr, v).Scan(&ok); err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return ok.Valid && ok.Bool
}

// agree fails if the Go validators and the SQL checks disagree about s.
func (c *sqlChecks) agree(t testing.TB, s string) {
	t.Helper()
	for _, x := range []struct {
		name string
		goOK bool
		expr string
	}{
		{"name", validName(s), checkName("?1")},
		{"endpoint", ValidEndpoint(s), checkName("?1")},
		{"hex32", validLowerHex(s, 32), checkLower("?1", 32)},
		{"hex64", validLowerHex(s, 64), checkLower("?1", 64)},
		{"bolt11", validBolt11(s), checkBolt11("?1")},
		{"decimal", validDecimal(s), checkDecimal("?1")},
		{"currency", rates.ValidCurrency(s), checkCurrency("?1")},
		{"subaddress", ValidSubaddress(s), checkSubaddress("?1")},
		{"hex16", validLowerHex(s, 16), checkLower("?1", 16)},
		{"hex296", validLowerHex(s, refundApprovalHex), checkLower("?1", refundApprovalHex)},
		{"refund invoice", validRefundInvoice(s), checkRefundInvoice("?1")},
	} {
		if got := c.eval(t, x.expr, s); got != x.goOK {
			t.Fatalf("%s(%q): Go says %v, SQL says %v", x.name, s, x.goOK, got)
		}
	}
}

// agreeQuote fails if rates' decimal grammar and currency rule, which the store calls, disagree with their SQL checks
// about s.
func (c *sqlChecks) agreeQuote(t testing.TB, s string) {
	t.Helper()
	if got, want := c.eval(t, checkDecimal("?1"), s), validDecimal(s); got != want {
		t.Fatalf("decimal(%q): Go says %v, SQL says %v", s, want, got)
	}
	if got, want := c.eval(t, checkCurrency("?1"), s), rates.ValidCurrency(s); got != want {
		t.Fatalf("currency(%q): Go says %v, SQL says %v", s, want, got)
	}
}

// agreeAlert fails if validAlert and the CHECKs of alerts.message, alerts.fields and alerts.dropped disagree.
func (c *sqlChecks) agreeAlert(t testing.TB, message, fields string, dropped int64) {
	t.Helper()
	if dropped < -1<<40 || dropped > 1<<40 { // int on every target; the bound is far inside either side
		return
	}
	sqlOK := c.eval(t, checkAlertMessage("?1"), message) && c.eval(t, checkAlertFields("?1"), fields) &&
		c.eval(t, checkAlertDropped("?1"), dropped)
	if goOK := validAlert(Alert{Message: message, Fields: fields, Dropped: int(dropped)}) == nil; goOK != sqlOK {
		t.Fatalf("alert(%q, %q, %d): Go says %v, SQL says %v", message, fields, dropped, goOK, sqlOK)
	}
}

// agreeFirstSeen fails if validTXO's first-seen rule and first_seen's CHECK disagree about a time of n seconds. The
// zero time is left out (it means now, and is never stored), and so is a time beyond 2^40 seconds of the epoch.
func (c *sqlChecks) agreeFirstSeen(t testing.TB, n int64) {
	t.Helper()
	at := time.Unix(n, 0)
	if n < -1<<40 || n > 1<<40 || at.IsZero() {
		return
	}
	x := TXO{TxID: strings.Repeat("a", 64), Address: "bcrt1" + strings.Repeat("q", 20), AmountSat: 1, FirstSeen: at}
	if goOK, sqlOK := validTXO(x) == nil, c.eval(t, checkTime("?1"), n); goOK != sqlOK {
		t.Fatalf("first-seen %d: Go says %v, SQL says %v", n, goOK, sqlOK)
	}
}

// agreePaymentFirstSeen is agreeFirstSeen for validPayment, which refuses the zero time too (CommitMoneroPass stores
// the caller's first-seen time, ruling S3): the zero time is refused by Go and never reaches SQL.
func (c *sqlChecks) agreePaymentFirstSeen(t testing.TB, n int64) {
	t.Helper()
	if n < -1<<40 || n > 1<<40 {
		return
	}
	p := Payment{TxID: strings.Repeat("a", 64), Address: sub(1), AmountPiconero: 1, FirstSeen: time.Unix(n, 0)}
	if goOK, sqlOK := validPayment(p) == nil, c.eval(t, checkTime("?1"), n); goOK != sqlOK {
		t.Fatalf("payment first-seen %d: Go says %v, SQL says %v", n, goOK, sqlOK)
	}
	if validPayment(Payment{TxID: p.TxID, Address: p.Address, AmountPiconero: 1}) == nil {
		t.Fatal("a payment without a first-seen time is valid")
	}
}

func TestValidatorsAgreeWithSQL(t *testing.T) {
	c := newSQLChecks(t)
	rng := rand.New(rand.NewSource(3))
	// Every byte in every position of values at and around each length boundary.
	for _, n := range []int{1, 2, 3, 4, 19, 20, 21, 31, 32, 33, 38, 39, 40, 63, 64, 65, 4095, 4096, 4097} {
		base := strings.Repeat("a", n)
		for b := 0; b < 256; b++ {
			for _, pos := range []int{0, n / 2, n - 1} {
				v := []byte(base)
				v[pos] = byte(b)
				c.agree(t, string(v))
			}
		}
		c.agree(t, base)
	}
	c.agree(t, "")
	// The decimal and currency boundaries: digit and letter bases, the point at every place that matters for the
	// integer (1 to 20) and fraction (1 to 18) digit counts, every byte in three positions.
	for _, n := range []int{1, 2, 3, 4, 19, 20, 21, 22, 38, 39, 40, 41} {
		var bases []string
		bases = append(bases, strings.Repeat("1", n), strings.Repeat("U", n))
		for _, k := range []int{0, 1, 2, n - 21, n - 20, n - 19, n - 18, n / 2, n - 3, n - 2, n - 1} {
			if k >= 0 && k < n {
				bases = append(bases, strings.Repeat("1", k)+"."+strings.Repeat("2", n-k-1))
			}
		}
		for _, base := range bases {
			c.agree(t, base)
			for b := 0; b < 256; b++ {
				for _, pos := range []int{0, len(base) / 2, len(base) - 1} {
					v := []byte(base)
					v[pos] = byte(b)
					c.agreeQuote(t, string(v))
				}
			}
		}
	}
	for _, s := range []string{"BTC", "BTc", "USD", "usd", "1.", ".1", "1.1.1", "0", "0.0", "00.00"} {
		c.agree(t, s)
	}
	for _, s := range []string{"\x00", "a\x00", "a\x00!", "café", "\xff", "a\xffb", "ａ", "‮", "a\n", "A-Z_0.9", strings.Repeat("0", 64)} {
		c.agree(t, s)
	}
	// Random strings over an alphabet heavy in the allowed and the nearly-allowed characters.
	alphabet := []byte("abcdefABCDEFTUZ0189._-/ :\x00\x7f\x80!~`@[]")
	for i := 0; i < 5000; i++ {
		n := rng.Intn(70)
		v := make([]byte, n)
		for j := range v {
			v[j] = alphabet[rng.Intn(len(alphabet))]
		}
		c.agree(t, string(v))
	}
	// First-seen bounds, Go and SQL.
	for _, n := range []int64{-1 << 40, -2, -1, 0, 1, 2, 1_800_000_000, 1 << 40} {
		c.agreeFirstSeen(t, n)
		c.agreePaymentFirstSeen(t, n)
	}
	// Amount bounds, Go and SQL. The piconero bound is 2^53 − 1 (accepted), 2^53 refused.
	for _, n := range []int64{-1, 0, 1, 2, 1<<53 - 2, 1<<53 - 1, 1 << 53, 1<<53 + 1, 1<<63 - 1, -1 << 63} {
		if got, want := c.eval(t, checkAmount("?1"), n), validAmount(n); got != want {
			t.Errorf("amount %d: SQL %v Go %v", n, got, want)
		}
		c.agreePiconero(t, n)
	}
	if !validPiconero(9007199254740991) || validPiconero(9007199254740992) || MaxPiconero != 9007199254740991 {
		t.Fatal("the piconero bound is not 2^53 - 1")
	}
	// Subaddresses: lengths 94, 95 and 96 behind each subaddress prefix (8, B, 7), each primary-address prefix (4, 5,
	// 9, A) and others; every byte at the first, second, middle and last positions; the four letters base58 leaves
	// out (0, O, I, l) at every position of a valid one.
	for _, pre := range []string{"8", "B", "7", "4", "5", "9", "A", "1", "b", "x", "0", "O", "I", "l"} {
		for _, n := range []int{94, 95, 96} {
			base := pre + strings.Repeat("u", n-1)
			c.agreeSubaddr(t, base)
			for b := 0; b < 256; b++ {
				for _, pos := range []int{0, 1, n / 2, n - 1} {
					v := []byte(base)
					v[pos] = byte(b)
					c.agreeSubaddr(t, string(v))
				}
			}
		}
	}
	valid := sub(12345)
	for pos := range len(valid) {
		for _, b := range []byte("0OIl") {
			v := []byte(valid)
			v[pos] = b
			if ValidSubaddress(string(v)) {
				t.Fatalf("%q accepted with %q at %d", v, b, pos)
			}
			c.agreeSubaddr(t, string(v))
		}
	}
	for _, b := range []byte(b58) {
		v := []byte(valid)
		v[50] = b
		if !ValidSubaddress(string(v)) {
			t.Fatalf("base58 byte %q refused", b)
		}
	}
	// Monero mark shapes.
	for _, kind := range []string{MarkMempool, MarkMined, MarkDead, "", "other", "Mined"} {
		for _, h := range []int64{-1 << 63, -1, 0, 1, 2, 1 << 40, 1<<63 - 1} {
			for _, n := range []int64{-1 << 63, -1, 0, 1, MaxMoneroConfirmations - 1, MaxMoneroConfirmations,
				MaxMoneroConfirmations + 1, 1<<63 - 1} {
				c.agreeMoneroMark(t, kind, h, n)
			}
		}
	}
}

// alertSeeds cross every edge of the alert checks: the message at 5, 6, 1,024 and 1,025 bytes, a non-ASCII byte, DEL,
// "ALERT" without the colon; fields {}, [], "{" alone, {}x, nesting (deep too), 4,096 and 4,097 bytes; dropped -1,
// 0, 2^20 and 2^20+1.
func alertSeeds() (messages, fields []string, dropped []int64) {
	messages = []string{"ALERT", "ALERT:", "ALERT: x", "ALERT: " + strings.Repeat("x", maxAlertMessage-7),
		"ALERT: " + strings.Repeat("x", maxAlertMessage-6), "ALERT: caf\u00e9", "ALERT: \x7f", "ALERT: \x00", "alert: x", " ALERT: x", ""}
	deep := `{"a":` + strings.Repeat("[", 1100) + strings.Repeat("]", 1100) + `}`
	pad := func(n int) string { return `{"a":"` + strings.Repeat("b", n-8) + `"}` }
	fields = []string{"{}", "[]", "{", "{}x", "{} ", " {}", `{"a":1}`, `{"a":"b"}`, `{"a":{}}`, `{"a":[]}`, `{"a":[1]}`,
		`{"a":"[x"}`, `{"a":"{x"}`, `{"a":"}"}`, `{"a":1,"a":2}`, `{"a":1e999}`, `{"a":"\ud800"}`, `{"a":"\u0000"}`,
		`{a:1}`, `{"a":1,}`, `{'a':1}`, `{"a":0x1}`, `{"a":NaN}`, deep, pad(maxAlertFields), pad(maxAlertFields + 1), ""}
	dropped = []int64{-1, 0, 1, maxAlertDropped, maxAlertDropped + 1}
	return
}

func TestAlertValidatorAgreesWithSQL(t *testing.T) {
	c := newSQLChecks(t)
	ms, fs, ds := alertSeeds()
	for _, m := range ms {
		for _, f := range fs {
			for _, d := range ds {
				c.agreeAlert(t, m, f, d)
			}
		}
	}
	for _, f := range []string{`{"a":{}}`, `{"a":[]}`, `{"a":"[x"}`, `{"a":"{x"}`} {
		if validAlert(Alert{Message: "ALERT: x", Fields: f}) == nil {
			t.Errorf("nested or bracketed fields %s accepted", f)
		}
	}
}

func FuzzValidators(f *testing.F) {
	for _, s := range []string{sub(0), sub(7)[1:], strings.Repeat("u", 94), strings.Repeat("0", 94), "1.5", "21.50", strings.Repeat("9", 20) + "." + strings.Repeat("9", 18), "1.", ".5", "USD", "BTC", "usd", "", "a", "acct-1", strings.Repeat("a", 32), strings.Repeat("a", 64), strings.Repeat("a", 65), "a b", "a\x00!", "\xff", "café", "A", "ln1pvj", "bc1" + strings.Repeat("q", 11), "bcrt1" + strings.Repeat("0", 85), "tb1q\x00" + strings.Repeat("q", 12)} {
		f.Add(s, int64(1), uint8(1))
		f.Add(s, int64(0), uint8(3))
	}
	// Every refund reason and an unknown one, so the fuzzer mutates real reasons against both outcomes.
	for _, r := range []string{RefundReasonSettled, RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown,
		RefundReasonNotSent, RefundReasonFailed, RefundReasonVoided, RefundReasonRecovered, RefundReasonCancelled,
		RefundReasonLedgerPaid, "other"} {
		f.Add(r, int64(1), uint8(1))
	}
	ms, fs, _ := alertSeeds()
	for i, m := range ms {
		f.Add(m, int64(i), uint8(0))
	}
	for _, fl := range fs {
		f.Add(fl, int64(maxAlertDropped), uint8(1))
	}
	c := newSQLChecks(f)
	f.Fuzz(func(t *testing.T, s string, height int64, k uint8) {
		c.agree(t, s)
		// Alerts: s as the message (behind the prefix too) and as the fields (inside braces too), height as dropped.
		c.agreeAlert(t, s, "{}", height)
		c.agreeAlert(t, "ALERT:"+s, "{}", 0)
		c.agreeAlert(t, "ALERT: x", s, height)
		c.agreeAlert(t, "ALERT: x", "{"+s+"}", 0)
		// Refund invoices: s on its own and behind the "ln" prefix, so the fuzzer reaches the charset and length checks.
		c.agree(t, "ln"+s)
		c.agree(t, "l"+s)
		c.agreeRefundAmount(t, height)
		c.agreeRefundPair(t, s, s)
		c.agreeRefundPair(t, RefundOutcomeFailed, s)
		c.agreeRefundPair(t, s, RefundReasonFailed)
		c.agreeRefundPair(t, RefundOutcomeSettled, s)
		// Addresses: s on its own and behind each valid prefix, so the fuzzer reaches the charset and length checks.
		for _, pre := range []string{"", "bc1", "tb1", "bcrt1"} {
			c.agreeAddr(t, pre+s)
		}
		// The mark shape: kind, height and hash from the fuzzed values (the string is also tried as kind and as hash).
		kind := []string{MarkMempool, MarkMined, MarkDead, s}[k%4]
		c.agreeMark(t, kind, height, s)
		c.agreeMark(t, kind, height, strings.Repeat("a", 64))
		c.agreeMark(t, kind, height, "")
		c.agreeFirstSeen(t, height)
		c.agreePaymentFirstSeen(t, height)
		// Monero: s as a subaddress on its own and behind each subaddress prefix, the mark shape and the piconero bound.
		for _, pre := range []string{"", "8", "B", "7"} {
			c.agreeSubaddr(t, pre+s)
		}
		c.agreeMoneroMark(t, kind, height, height)
		c.agreeMoneroMark(t, kind, 1, height)
		c.agreeMoneroMark(t, kind, height, 0)
		c.agreePiconero(t, height)
		// Event ids are always 32 lowercase hex, whatever the type and order strings.
		if id := EventID(s, s, 1); !validLowerHex(id, 32) {
			t.Fatalf("EventID(%q) = %q", s, id)
		}
		if validName(s) && !validBolt11(s) {
			t.Fatalf("%q is a valid name but not a valid bolt11 charset", s)
		}
		// A provider name is a valid name, and the decimal and currency rules hold behind and in front of a digit.
		if validProvider(s) && !validName(s) {
			t.Fatalf("%q is a valid provider but not a valid name", s)
		}
		c.agreeQuote(t, "1"+s)
		c.agreeQuote(t, s+"1")
	})
}
