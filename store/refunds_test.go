package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// refundApproval returns a 296-character approval whose key id, refund id and order id are the given ones (the approval
// layout: the domain, the network, the key id, the refund id, the order id, the amount, the expiry, a 64-byte
// signature). The signature is not verified here: the store only ties the columns to the bytes.
func refundApproval(keyID, id, orderID string, amount, expires int64) string {
	return hex.EncodeToString([]byte("satbooth/refund-approval/v1")) + "03" + keyID + id + orderID +
		fmt.Sprintf("%016x%016x", uint64(amount), uint64(expires)) + strings.Repeat("5a", 64)
}

// timed returns r claimed at claimed and expiring at expires, with the approval carrying that expiry.
func timed(r Refund, claimed, expires time.Time) Refund {
	r.ClaimedAt, r.ExpiresAt = claimed, expires
	r.Approval = refundApproval(r.KeyID, r.ID, r.OrderID, r.AmountMsat, ts(expires))
	return r
}

// refundOf returns a valid refund of order o, whose ids are made of the hex digit d.
func refundOf(o Order, d byte, amount int64) Refund {
	id, key := strings.Repeat(string(d), 32), strings.Repeat("c", 16)
	return Refund{ID: id, OrderID: o.ID, KeyID: key, Approval: refundApproval(key, id, o.ID, amount, 1_800_086_400),
		Invoice: "lnbcrt" + strings.Repeat("q", 40) + string(d), AmountMsat: amount, ClaimedAt: t0, ExpiresAt: t0.Add(24 * time.Hour)}
}

func mkRefund(t *testing.T, s *Store, o Order, d byte, amount int64) Refund {
	t.Helper()
	r := refundOf(o, d, amount)
	if err := s.AddRefund(context.Background(), r); err != nil {
		t.Fatalf("AddRefund: %v", err)
	}
	return r
}

func (c *sqlChecks) agreeRefundInvoice(t testing.TB, s string) {
	t.Helper()
	if got, want := c.eval(t, checkRefundInvoice("?1"), s), validRefundInvoice(s); got != want {
		t.Fatalf("refund invoice(%q): Go says %v, SQL says %v", s, want, got)
	}
}

func (c *sqlChecks) agreeRefundAmount(t testing.TB, n int64) {
	t.Helper()
	if got, want := c.eval(t, checkRefundAmount("?1"), n), validRefundMsat(n); got != want {
		t.Fatalf("refund amount %d: Go says %v, SQL says %v", n, want, got)
	}
}

func (c *sqlChecks) agreeRefundPair(t testing.TB, outcome, reason string) {
	t.Helper()
	var ok sql.NullBool
	if err := c.db.QueryRow(`SELECT `+checkRefundPair("?1", "?2"), outcome, reason).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if got, want := ok.Valid && ok.Bool, validRefundOutcomeReason(outcome, reason); got != want {
		t.Fatalf("pair (%q, %q): Go says %v, SQL says %v", outcome, reason, want, got)
	}
}

// agreeRefundTies: refundTies and checkRefundTies agree about r.
func (c *sqlChecks) agreeRefundTies(t testing.TB, r Refund) {
	t.Helper()
	var ok sql.NullBool
	if err := c.db.QueryRow(`SELECT `+checkRefundTies+` FROM (SELECT ?1 AS id, ?2 AS order_id, ?3 AS key_id, ?4 AS approval, ?5 AS amount_msat, ?6 AS expires_at)`,
		r.ID, r.OrderID, r.KeyID, r.Approval, r.AmountMsat, ts(r.ExpiresAt)).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if got, want := ok.Valid && ok.Bool, refundTies(r); got != want {
		t.Fatalf("ties of %+v: Go says %v, SQL says %v", r, want, got)
	}
}

// The Go validators and the SQL checks of the refund tables say the same, at every boundary: the invoice's length
// (1, 2, 2047, 2048, 2049), its prefix and every byte at the places that matter; the amount bounds and the whole-sat
// rule; every outcome and reason pair; the ties between the columns and the approval's bytes, for a change of every
// character of every tied field.
func TestRefundValidatorsAgreeWithSQL(t *testing.T) {
	c := newSQLChecks(t)
	rng := rand.New(rand.NewSource(9))
	for _, n := range []int{1, 2, 3, 4, 63, 64, 2046, 2047, 2048, 2049, 2050} {
		bases := []string{strings.Repeat("q", n), "ln" + strings.Repeat("q", max(n-2, 0)), "l" + strings.Repeat("n", max(n-1, 0))}
		for _, base := range bases {
			if len(base) > n+1 {
				base = base[:n]
			}
			c.agreeRefundInvoice(t, base)
			for b := 0; b < 256; b++ {
				if len(base) > 64 && !strings.ContainsRune("\x00 +/09AFLNZ_afglnqz~\x7f\x80\xff", rune(b)) {
					continue // long values: the bytes that matter (the others are the same class), to keep the race run short
				}
				for _, pos := range []int{0, 1, 2, len(base) / 2, len(base) - 1} {
					if pos < 0 || pos >= len(base) {
						continue
					}
					v := []byte(base)
					v[pos] = byte(b)
					c.agreeRefundInvoice(t, string(v))
				}
			}
		}
	}
	for _, s := range []string{"", "ln", "LN1", "lN1", "ln\x00", "ln1\x00q", "lnbc1", "ln bc", "ln-1", "ln_1", "lnbc1" + "ｑ", "lné"} {
		c.agreeRefundInvoice(t, s)
	}
	alphabet := []byte("lnqpzry9x8gf2tvdw0s3jn54khce6mua7lLN1-_ \x00\x7f\x80~")
	for i := 0; i < 5000; i++ {
		v := make([]byte, rng.Intn(12))
		for j := range v {
			v[j] = alphabet[rng.Intn(len(alphabet))]
		}
		c.agreeRefundInvoice(t, string(v))
		c.agreeRefundInvoice(t, "ln"+string(v))
	}
	for _, n := range []int64{-1 << 63, -1000, -1, 0, 1, 999, 1000, 1001, 1999, 2000, 1<<53 - 1000, 1<<53 - 992, 1 << 53, 1<<53 + 1,
		9007199254741000, 1<<63 - 1} {
		c.agreeRefundAmount(t, n)
	}
	// The largest multiple of 1000 that is at most 2^53 is accepted; the next is refused.
	if !validRefundMsat(9007199254740000) || validRefundMsat(9007199254741000) || validRefundMsat(0) || !validRefundMsat(1000) {
		t.Fatal("refund amount bounds")
	}
	for i := 0; i < 2000; i++ {
		c.agreeRefundAmount(t, rng.Int63n(1<<54)-1<<40)
	}
	outcomes := []string{"", RefundOutcomeSettled, RefundOutcomeFailed, "Settled", "other", "settled\x00"}
	reasons := []string{"", RefundReasonSettled, RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown,
		RefundReasonNotSent, RefundReasonFailed, RefundReasonVoided, RefundReasonRecovered, RefundReasonCancelled, RefundReasonLedgerPaid, "Failed", "failed ", "unknown"}
	for _, o := range outcomes {
		for _, r := range reasons {
			c.agreeRefundPair(t, o, r)
		}
	}
	o := Order{ID: strings.Repeat("a", 32)}
	base := refundOf(o, 'b', 1000)
	if !refundTies(base) {
		t.Fatal("the base refund does not tie")
	}
	c.agreeRefundTies(t, base)
	for _, field := range []string{"id", "order", "key", "approval"} {
		for pos := 0; pos < 296; pos++ {
			m := base
			ap := []byte(m.Approval)
			switch field {
			case "id":
				b := []byte(m.ID)
				if pos >= len(b) {
					continue
				}
				b[pos] ^= 1 // 'b' ^ 1 = 'c'
				m.ID = string(b)
			case "order":
				b := []byte(m.OrderID)
				if pos >= len(b) {
					continue
				}
				b[pos] ^= 1
				m.OrderID = string(b)
			case "key":
				b := []byte(m.KeyID)
				if pos >= len(b) {
					continue
				}
				b[pos] ^= 1
				m.KeyID = string(b)
			default:
				ap[pos] ^= 1
				m.Approval = string(ap)
			}
			c.agreeRefundTies(t, m)
		}
	}
	// The signed fields the row repeats: a change of the amount or the expiry column, or of any character of the amount,
	// expiry, network or domain bytes in the approval, breaks the ties; so does a network byte above 03.
	for _, f := range []func(*Refund){
		func(r *Refund) { r.AmountMsat += 1000 }, func(r *Refund) { r.AmountMsat = 0 }, func(r *Refund) { r.AmountMsat = -1000 },
		func(r *Refund) { r.ExpiresAt = r.ExpiresAt.Add(time.Second) }, func(r *Refund) { r.ExpiresAt = time.Unix(0, 0) },
	} {
		m := base
		f(&m)
		if refundTies(m) {
			t.Fatalf("a changed column still ties: %+v", m)
		}
		c.agreeRefundTies(t, m)
	}
	for _, net := range []string{"00", "01", "02", "03", "04", "0a", "0f", "10", "ff", "30", "zz", "0", ""} {
		m := base
		m.Approval = base.Approval[:54] + net + base.Approval[56:]
		if want := len(net) == 2 && net <= "03"; refundTies(m) != want {
			t.Fatalf("network byte %q: ties %v", net, !want)
		}
		c.agreeRefundTies(t, m)
	}
	m := base
	m.Approval = base.Approval[:150] + "0" + base.Approval[151:] // inside the amount bytes
	if refundTies(m) {
		t.Fatal("a change in the amount bytes did not break the ties")
	}
	c.agreeRefundTies(t, m)
	c.agreeRefundTies(t, Refund{})
	c.agreeRefundTies(t, Refund{Approval: base.Approval[:295]})
}

func TestRefundRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	clk.set(t0.Add(5 * time.Minute))
	r := refundOf(o, 'a', 3000)
	r.ClaimedAt = time.Time{}
	if err := s.AddRefund(ctx, r); err != nil { // claimed time stamped from the clock
		t.Fatal(err)
	}
	r.ClaimedAt = t0.Add(5 * time.Minute)
	got, ok, err := s.RefundByID(ctx, r.ID)
	if err != nil || !ok || got != r {
		t.Fatalf("RefundByID: %+v %v %v, want %+v", got, ok, err, r)
	}
	if _, ok, err := s.RefundByID(ctx, strings.Repeat("9", 32)); ok || err != nil {
		t.Fatalf("unknown refund: %v %v", ok, err)
	}
	if _, _, err := s.RefundByID(ctx, "XYZ"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	st, ok, err := s.RefundStateByID(ctx, r.ID)
	if err != nil || !ok || st.Refund != r || st.Sent || st.Resolved || !st.SentAt.IsZero() || st.Result != (RefundResult{}) {
		t.Fatalf("claimed state: %+v %v %v", st, ok, err)
	}
	if err := s.AddRefundSend(ctx, r.ID, t0.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, _, _ = s.RefundStateByID(ctx, r.ID)
	if !st.Sent || !st.SentAt.Equal(t0.Add(6*time.Minute)) || st.Resolved {
		t.Fatalf("sent state: %+v", st)
	}
	res := RefundResult{RefundID: r.ID, Outcome: RefundOutcomeSettled, Reason: RefundReasonSettled, At: t0.Add(7 * time.Minute)}
	if err := s.AddRefundResult(ctx, res); err != nil {
		t.Fatal(err)
	}
	st, _, _ = s.RefundStateByID(ctx, r.ID)
	if !st.Sent || !st.Resolved || st.Result != res || st.Refund != r {
		t.Fatalf("settled state: %+v", st)
	}
}

func TestAddRefundRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	r := mkRefund(t, s, o, 'a', 1000)
	// A repeated id, or a repeated invoice (one refund per invoice, ever).
	if err := s.AddRefund(ctx, r); !errors.Is(err, ErrRefundExists) {
		t.Fatalf("same refund: %v", err)
	}
	other := refundOf(o, 'b', 1000)
	other.Invoice = r.Invoice
	if err := s.AddRefund(ctx, other); !errors.Is(err, ErrRefundInvoiceUsed) || errors.Is(err, ErrRefundExists) {
		t.Fatalf("same invoice: %v", err)
	}
	sameID := refundOf(o, 'a', 1000)
	sameID.Invoice = "lnbcrt" + strings.Repeat("q", 40) + "z"
	if err := s.AddRefund(ctx, sameID); !errors.Is(err, ErrRefundExists) || errors.Is(err, ErrRefundInvoiceUsed) {
		t.Fatalf("same id, another invoice: %v", err)
	}
	if err := s.AddRefund(ctx, r); !errors.Is(err, ErrRefundExists) { // the same id and invoice: the id is named
		t.Fatalf("same id and invoice: %v", err)
	}
	if err := s.AddRefund(ctx, refundOf(Order{ID: strings.Repeat("7", 32)}, 'b', 1000)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	ok := refundOf(o, 'c', 1000)
	mod := func(f func(*Refund)) Refund { x := ok; f(&x); return x }
	for name, x := range map[string]Refund{
		"id upper":          mod(func(x *Refund) { x.ID = strings.ToUpper(x.ID) }),
		"id short":          mod(func(x *Refund) { x.ID = x.ID[1:] }),
		"order short":       mod(func(x *Refund) { x.OrderID = x.OrderID[1:] }),
		"key short":         mod(func(x *Refund) { x.KeyID = x.KeyID[1:] }),
		"approval short":    mod(func(x *Refund) { x.Approval = x.Approval[2:] }),
		"approval long":     mod(func(x *Refund) { x.Approval += "00" }),
		"approval upper":    mod(func(x *Refund) { x.Approval = strings.ToUpper(x.Approval) }),
		"approval not hex":  mod(func(x *Refund) { x.Approval = x.Approval[:295] + "g" }),
		"id not in it":      mod(func(x *Refund) { x.ID = strings.Repeat("d", 32) }),
		"order not in it":   mod(func(x *Refund) { x.OrderID = strings.Repeat("d", 32) }),
		"key not in it":     mod(func(x *Refund) { x.KeyID = strings.Repeat("d", 16) }),
		"invoice empty":     mod(func(x *Refund) { x.Invoice = "" }),
		"invoice no ln":     mod(func(x *Refund) { x.Invoice = "bc1qqq" }),
		"invoice upper":     mod(func(x *Refund) { x.Invoice = "LNBC1QQ" }),
		"invoice uppercase": mod(func(x *Refund) { x.Invoice = "lnBC1qq" }),
		"invoice space":     mod(func(x *Refund) { x.Invoice = "lnbc1 qq" }),
		"invoice 2049":      mod(func(x *Refund) { x.Invoice = "ln" + strings.Repeat("q", 2047) }),
		"invoice non-ascii": mod(func(x *Refund) { x.Invoice = "lnbc1é" }),
		"amount 0":          mod(func(x *Refund) { x.AmountMsat = 0 }),
		"amount negative":   mod(func(x *Refund) { x.AmountMsat = -1000 }),
		"amount 1500":       mod(func(x *Refund) { x.AmountMsat = 1500 }),
		"amount above 2^53": mod(func(x *Refund) { x.AmountMsat = 9007199254741000 }),
		"claimed 1970":      mod(func(x *Refund) { x.ClaimedAt = time.Unix(0, 0) }),
		"expiry = claimed":  mod(func(x *Refund) { *x = timed(*x, x.ClaimedAt, x.ClaimedAt) }),
		"expiry before":     mod(func(x *Refund) { *x = timed(*x, x.ClaimedAt, x.ClaimedAt.Add(-time.Second)) }),
	} {
		if err := s.AddRefund(ctx, x); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The boundaries that are allowed: the largest amount, the longest invoice, a one-second expiry.
	big := refundOf(o, 'd', 9007199254740000)
	big.Invoice = "ln" + strings.Repeat("q", 2046)
	big = timed(big, big.ClaimedAt, big.ClaimedAt.Add(time.Second))
	if err := s.AddRefund(ctx, big); err != nil {
		t.Fatalf("boundary refund: %v", err)
	}
	if open, _ := s.OpenRefunds(ctx); len(open) != 2 {
		t.Fatalf("%d refunds, want 2", len(open))
	}
}

func TestRefundSendAndResultRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	late := t0.Add(48 * time.Hour) // after every approval expiry used here
	unknown := strings.Repeat("9", 32)
	if err := s.AddRefundSend(ctx, unknown, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("send, unknown refund: %v", err)
	}
	if err := s.AddRefundResult(ctx, RefundResult{RefundID: unknown, Outcome: RefundOutcomeFailed, Reason: RefundReasonKeyRevoked, At: late}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result, unknown refund: %v", err)
	}
	a := mkRefund(t, s, o, 'a', 1000)
	if err := s.AddRefundSend(ctx, "bad", t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	if err := s.AddRefundSend(ctx, a.ID, time.Unix(0, 0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("send at 1970: %v", err)
	}
	if err := s.AddRefundSend(ctx, a.ID, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRefundSend(ctx, a.ID, t0.Add(time.Second)); !errors.Is(err, ErrSendExists) {
		t.Fatalf("second send: %v", err)
	}
	if st, _, _ := s.RefundStateByID(ctx, a.ID); !st.SentAt.Equal(t0) {
		t.Fatalf("the first marker was changed: %v", st.SentAt)
	}
	// A sent refund takes the reasons that need a send, not those that need none.
	for _, reason := range []string{RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown} {
		if err := s.AddRefundResult(ctx, RefundResult{RefundID: a.ID, Outcome: RefundOutcomeFailed, Reason: reason, At: late}); !errors.Is(err, ErrInvalid) {
			t.Errorf("sent, %s: %v", reason, err)
		}
	}
	if err := s.AddRefundResult(ctx, RefundResult{RefundID: a.ID, Outcome: RefundOutcomeFailed, Reason: RefundReasonFailed, At: late}); err != nil {
		t.Fatal(err)
	}
	for _, res := range []RefundResult{
		{RefundID: a.ID, Outcome: RefundOutcomeFailed, Reason: RefundReasonFailed, At: late},
		{RefundID: a.ID, Outcome: RefundOutcomeSettled, Reason: RefundReasonSettled, At: late},
	} {
		if err := s.AddRefundResult(ctx, res); !errors.Is(err, ErrResultExists) {
			t.Errorf("second result %+v: %v", res, err)
		}
	}
	if st, _, _ := s.RefundStateByID(ctx, a.ID); st.Result.Reason != RefundReasonFailed {
		t.Fatalf("the first result was changed: %+v", st.Result)
	}
	// A send after a result is refused, and writes nothing.
	if err := s.AddRefundSend(ctx, a.ID, t0); !errors.Is(err, ErrSendExists) {
		t.Fatalf("send after result (marker exists): %v", err)
	}
	b := mkRefund(t, s, o, 'b', 1000)
	if err := s.AddRefundResult(ctx, RefundResult{RefundID: b.ID, Outcome: RefundOutcomeFailed, Reason: RefundReasonExpiredUnsent, At: late}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRefundSend(ctx, b.ID, t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("send after result: %v", err)
	}
	if st, _, _ := s.RefundStateByID(ctx, b.ID); st.Sent {
		t.Fatal("a send was written after the result")
	}
	// An unsent refund takes the reasons that need none, and recovered; not those that need a send.
	for i, c := range []struct {
		reason string
		ok     bool
	}{
		{RefundReasonSettled, false}, {RefundReasonNotSent, false}, {RefundReasonFailed, false}, {RefundReasonVoided, false},
		{RefundReasonExpiredUnsent, true}, {RefundReasonKeyRevoked, true}, {RefundReasonInvoiceKnown, true}, {RefundReasonRecovered, true},
	} {
		x := mkRefund(t, s, o, "cdef0123"[i], 1000)
		outcome := RefundOutcomeFailed
		if c.reason == RefundReasonSettled {
			outcome = RefundOutcomeSettled
		}
		err := s.AddRefundResult(ctx, RefundResult{RefundID: x.ID, Outcome: outcome, Reason: c.reason, At: late})
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("unsent, %s: %v", c.reason, err)
		}
	}
	u := mkRefund(t, s, o, '4', 1000)
	for name, res := range map[string]RefundResult{
		"settled with failure reason": {Outcome: RefundOutcomeSettled, Reason: RefundReasonFailed, At: late},
		"failed with settled":         {Outcome: RefundOutcomeFailed, Reason: RefundReasonSettled, At: late},
		"unknown outcome":             {Outcome: "pending", Reason: RefundReasonFailed, At: late},
		"unknown reason":              {Outcome: RefundOutcomeFailed, Reason: "unknown", At: late},
		"time 1970":                   {Outcome: RefundOutcomeFailed, Reason: RefundReasonKeyRevoked, At: time.Unix(0, 0)},
		"bad id":                      {RefundID: "bad", Outcome: RefundOutcomeFailed, Reason: RefundReasonKeyRevoked, At: late},
	} {
		if res.RefundID == "" {
			res.RefundID = u.ID
		}
		if err := s.AddRefundResult(ctx, res); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The CHECKs and triggers on a raw connection (an operator's sqlite3 shell: no foreign keys, no pragmas), each refused
// alone: every case starts from a refund that inserts, changes one thing, and must fail with its guard's own text.
func TestRefundSchemaRefuses(t *testing.T) {
	s, path, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	o2 := mkOrder(t, s, "r2", 10_000, t0.Add(time.Hour))
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	const chk, immutableText = "CHECK constraint failed: ", "store rows are immutable"
	// The text of a failed CHECK carries its expression: the tie, the invoice and the amount checks are told apart by it.
	const tie, inv, amt = "id = substr(approval, 73, 32)", "substr(invoice, 1, 2) = 'ln'", "amount_msat BETWEEN 1 AND"
	insert := func(r Refund) error {
		_, err := raw.Exec(`INSERT INTO refunds (id, order_id, invoice, amount_msat, key_id, approval, claimed_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.OrderID, r.Invoice, r.AmountMsat, r.KeyID, r.Approval, ts(r.ClaimedAt), ts(r.ExpiresAt))
		return err
	}
	if err := insert(refundOf(o, 'a', 1000)); err != nil {
		t.Fatalf("the base refund is refused: %v", err)
	}
	// Each case gets a digit of its own, so that only the changed thing can be the cause.
	digits := "bcdef0123456789"
	next := 0
	for _, c := range []struct {
		name string
		mod  func(*Refund)
		want string
	}{
		{"id upper-case", func(r *Refund) { r.ID = strings.ToUpper(r.ID) }, chk},
		{"id 31", func(r *Refund) { r.ID = r.ID[1:] }, chk},
		{"order id 31", func(r *Refund) { r.OrderID = r.OrderID[1:] }, "unknown order"}, // the trigger shadows the CHECK
		{"key id 15", func(r *Refund) { r.KeyID = r.KeyID[1:] }, chk},
		{"key id not hex", func(r *Refund) { r.KeyID = strings.Repeat("g", 16) }, chk},
		{"approval 294", func(r *Refund) { r.Approval = r.Approval[2:] }, chk},
		{"approval 298", func(r *Refund) { r.Approval += "00" }, chk},
		{"approval upper-case", func(r *Refund) { r.Approval = strings.ToUpper(r.Approval) }, chk},
		// The ties to the signed bytes: each case changes one thing, and the CHECK that names the offsets refuses it.
		{"id differs from the approval's", func(r *Refund) { r.ID = strings.Repeat("e", 32) }, tie},
		{"order id differs from the approval's (a real order)", func(r *Refund) { r.OrderID = o2.ID }, tie},
		{"key id differs from the approval's", func(r *Refund) { r.KeyID = strings.Repeat("e", 16) }, tie},
		{"amount differs from the approval's", func(r *Refund) { r.AmountMsat = 2000 }, tie},
		{"expiry differs from the approval's", func(r *Refund) { r.ExpiresAt = r.ExpiresAt.Add(time.Second) }, tie},
		{"domain differs", func(r *Refund) { r.Approval = "74" + r.Approval[2:] }, tie},
		{"domain version differs", func(r *Refund) { r.Approval = r.Approval[:52] + "32" + r.Approval[54:] }, tie},
		{"network byte 04", func(r *Refund) { r.Approval = r.Approval[:54] + "04" + r.Approval[56:] }, tie},
		{"network byte ff", func(r *Refund) { r.Approval = r.Approval[:54] + "ff" + r.Approval[56:] }, tie},
		{"unknown order", func(r *Refund) {
			r.OrderID = strings.Repeat("8", 32)
			*r = timed(*r, r.ClaimedAt, r.ExpiresAt)
		}, "unknown order"},
		{"invoice empty", func(r *Refund) { r.Invoice = "" }, inv},
		{"invoice without ln", func(r *Refund) { r.Invoice = "bc" + r.Invoice }, inv},
		{"invoice upper-case", func(r *Refund) { r.Invoice = strings.ToUpper(r.Invoice) }, inv},
		{"invoice with a space", func(r *Refund) { r.Invoice += " q" }, inv},
		{"invoice with a NUL", func(r *Refund) { r.Invoice += "\x00q" }, inv},
		{"invoice non-ASCII", func(r *Refund) { r.Invoice += "é" }, inv},
		{"invoice 2049", func(r *Refund) { r.Invoice = "ln" + strings.Repeat("q", 2047) }, inv},
		{"invoice already refunded", func(r *Refund) { r.Invoice = refundOf(o, 'a', 1000).Invoice }, immutableText},
		{"id already used", func(r *Refund) {
			r.ID = strings.Repeat("a", 32)
			*r = timed(*r, r.ClaimedAt, r.ExpiresAt)
		}, immutableText},
		{"amount 0", func(r *Refund) { r.AmountMsat = 0; *r = timed(*r, r.ClaimedAt, r.ExpiresAt) }, amt},
		{"amount negative", func(r *Refund) { r.AmountMsat = -1000; *r = timed(*r, r.ClaimedAt, r.ExpiresAt) }, amt},
		{"amount 1001", func(r *Refund) { r.AmountMsat = 1001; *r = timed(*r, r.ClaimedAt, r.ExpiresAt) }, amt},
		{"amount above 2^53", func(r *Refund) { r.AmountMsat = 9007199254741000; *r = timed(*r, r.ClaimedAt, r.ExpiresAt) }, amt},
		{"claimed at 0", func(r *Refund) { *r = timed(*r, time.Unix(0, 0), time.Unix(10, 0)) }, "claimed_at > 0"},
		{"expires = claimed", func(r *Refund) { *r = timed(*r, r.ClaimedAt, r.ClaimedAt) }, "expires_at > claimed_at"},
		{"expires before claimed", func(r *Refund) { *r = timed(*r, r.ClaimedAt, r.ClaimedAt.Add(-time.Second)) }, "expires_at > claimed_at"},
	} {
		d := digits[next%len(digits)]
		next++
		r := refundOf(o, d, 1000)
		c.mod(&r)
		if err := insert(r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// A number as the invoice: STRICT turns it into the text "12345", which the CHECK refuses.
	if _, err := raw.Exec(`INSERT INTO refunds (id, order_id, invoice, amount_msat, key_id, approval, claimed_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, strings.Repeat("0", 32), o.ID, 12345, 1000, strings.Repeat("c", 16),
		timed(Refund{ID: strings.Repeat("0", 32), OrderID: o.ID, KeyID: strings.Repeat("c", 16), AmountMsat: 1000}, t0, t0.Add(time.Hour)).Approval,
		ts(t0), ts(t0.Add(time.Hour))); err == nil || !strings.Contains(err.Error(), inv) {
		t.Errorf("a number as the invoice: %v, want %q", err, inv)
	}
	// An upsert on the invoice (or the id) is refused by the replace guard, whatever its action, and changes nothing.
	base := refundOf(o, 'a', 1000)
	dup := refundOf(o, 'f', 1000)
	dup.Invoice = base.Invoice
	for _, action := range []string{`DO NOTHING`, `DO UPDATE SET amount_msat = excluded.amount_msat`} {
		_, err := raw.Exec(`INSERT INTO refunds (id, order_id, invoice, amount_msat, key_id, approval, claimed_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(invoice) `+action, dup.ID, dup.OrderID, dup.Invoice, dup.AmountMsat, dup.KeyID, dup.Approval, ts(dup.ClaimedAt), ts(dup.ExpiresAt))
		if err == nil || !strings.Contains(err.Error(), immutableText) {
			t.Errorf("upsert on the invoice, %s: %v", action, err)
		}
		_, err = raw.Exec(`INSERT INTO refunds (id, order_id, invoice, amount_msat, key_id, approval, claimed_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) `+action, base.ID, base.OrderID, base.Invoice, base.AmountMsat, base.KeyID, base.Approval, ts(base.ClaimedAt), ts(base.ExpiresAt))
		if err == nil || !strings.Contains(err.Error(), immutableText) {
			t.Errorf("upsert on the id, %s: %v", action, err)
		}
	}
	// A type error: STRICT refuses text for the amount.
	if _, err := raw.Exec(`INSERT INTO refunds VALUES (?, ?, ?, 'x', ?, ?, 5, 6)`, strings.Repeat("0", 32), o.ID, "lnbcx", strings.Repeat("c", 16),
		refundApproval(strings.Repeat("c", 16), strings.Repeat("0", 32), o.ID, 1000, 6)); err == nil {
		t.Error("a text amount was accepted")
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM refunds`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d refunds after the refused inserts, want 1 (%v)", n, err)
	}

	// The send marker and the result: the parent, the state machine, the checks. T is the claim time of the refunds.
	T := ts(t0)
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	if err := insert(refundOf(o, 'b', 1000)); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) error { _, err := raw.Exec(q, args...); return err }
	send := func(id string, at int64) error {
		return exec(`INSERT INTO refund_sends (refund_id, sent_at) VALUES (?, ?)`, id, at)
	}
	result := func(id, outcome, reason string, at int64) error {
		return exec(`INSERT INTO refund_results (refund_id, outcome, reason, resolved_at) VALUES (?, ?, ?, ?)`, id, outcome, reason, at)
	}
	want := func(name string, err error, text string) {
		t.Helper()
		// SQLite fires the triggers of one event newest first: a row that breaks two rules is refused by either ("|").
		hit := false
		for _, w := range strings.Split(text, "|") {
			hit = hit || (err != nil && strings.Contains(err.Error(), w))
		}
		if !hit {
			t.Errorf("%s: %v, want %q", name, err, text)
		}
	}
	want("send of an unknown refund", send(strings.Repeat("7", 32), T+5), "unknown refund")
	want("send with a bad id", send(strings.ToUpper(a), T+5), "unknown refund")
	want("result of an unknown refund", result(strings.Repeat("7", 32), "failed", "key_revoked", T+5), "unknown refund")
	// The time triggers fire before the CHECKs, so a send at 0 or -1 is refused by the window trigger.
	const window = "send outside the approval window"
	want("send at 0", send(a, 0), window)
	want("send at -1", send(a, -1), window)
	// Results without a send.
	want("settled with no send", result(b, "settled", "settled", T+5), "result needs a send")
	for _, reason := range []string{"not_sent", "failed", "voided"} {
		want(reason+" with no send", result(b, "failed", reason, T+5), "result needs a send")
	}
	// Sent: the reasons that need no send are refused.
	if err := send(a, T+5); err != nil {
		t.Fatal(err)
	}
	want("second send", send(a, T+6), immutableText)
	// The CHECKs of the result, past the triggers (a is sent, so the send triggers stay quiet for these reasons).
	want("result outcome unknown", result(a, "pending", "failed", T+5), chk)
	want("result reason unknown", result(a, "failed", "unknown", T+5), chk)
	want("settled/failed", result(a, "settled", "failed", T+5), chk)
	want("failed/settled", result(a, "failed", "settled", T+5), chk)
	want("failed upper-case", result(a, "Failed", "failed", T+5), chk)
	want("result at 0", result(a, "failed", "failed", 0), "result before the")
	for _, reason := range []string{"expired_unsent", "key_revoked", "invoice_known"} {
		want(reason+" after a send", result(a, "failed", reason, T+5), "result needs no send|expired_unsent before the approval expired")
	}
	// Resolved: no send after a result, no second result.
	if err := result(b, "failed", "invoice_known", T+5); err != nil {
		t.Fatal(err)
	}
	want("send after a result", send(b, T+6), "refund already resolved")
	want("second result", result(b, "failed", "key_revoked", T+6), immutableText)
	if err := result(a, "settled", "settled", T+6); err != nil {
		t.Fatal(err)
	}
	want("result after a result", result(a, "failed", "failed", T+7), immutableText)
	// Immutability of the three tables: update, delete, replace.
	for _, q := range []string{
		`UPDATE refunds SET claimed_at = claimed_at`, `DELETE FROM refunds`, `REPLACE INTO refunds SELECT * FROM refunds`,
		`UPDATE refund_sends SET sent_at = sent_at`, `DELETE FROM refund_sends`, `REPLACE INTO refund_sends SELECT * FROM refund_sends`,
		`UPDATE refund_results SET reason = reason`, `DELETE FROM refund_results`, `REPLACE INTO refund_results SELECT * FROM refund_results`,
	} {
		text := immutableText
		if q == `REPLACE INTO refund_sends SELECT * FROM refund_sends` {
			text = "refund already resolved" // the state trigger fires before the replace guard; either refuses
		}
		want(q, exec(q), text)
	}
	for tb, rows := range map[string]int{"refunds": 2, "refund_sends": 1, "refund_results": 2} {
		if err := raw.QueryRow(`SELECT count(*) FROM ` + tb).Scan(&n); err != nil || n != rows {
			t.Errorf("%s: %d rows, want %d (%v)", tb, n, rows, err)
		}
	}
}

// Every outcome and reason against both send states, on the raw connection, equals what the Go side allows.
func TestRefundResultMatrixMatchesGo(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	n := 0
	late := t0.Add(48 * time.Hour) // after the expiry, long after the send
	for _, outcome := range []string{RefundOutcomeSettled, RefundOutcomeFailed, "other"} {
		for _, reason := range []string{RefundReasonSettled, RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown,
			RefundReasonNotSent, RefundReasonFailed, RefundReasonVoided, RefundReasonRecovered, RefundReasonCancelled,
			RefundReasonLedgerPaid, "other"} {
			for _, sent := range []bool{false, true} {
				// One refund per case through the raw connection and the Go side: two refunds with the same case.
				var ids [2]string
				for k := range ids {
					r := refundOf(o, "0123456789abcdef"[n%16], 1000)
					r.ID = fmt.Sprintf("%016x%016x", n, k)
					r.Invoice = fmt.Sprintf("ln%d", n*2+k)
					r = timed(r, r.ClaimedAt, r.ExpiresAt)
					if err := s.AddRefund(ctx, r); err != nil {
						t.Fatal(err)
					}
					if sent {
						if err := s.AddRefundSend(ctx, r.ID, t0); err != nil {
							t.Fatal(err)
						}
					}
					ids[k] = r.ID
				}
				n++
				_, rawErr := raw.Exec(`INSERT INTO refund_results (refund_id, outcome, reason, resolved_at) VALUES (?, ?, ?, ?)`, ids[0], outcome, reason, ts(late))
				goErr := s.AddRefundResult(ctx, RefundResult{RefundID: ids[1], Outcome: outcome, Reason: reason, At: late})
				allowed := validRefundOutcomeReason(outcome, reason) && !(refundNeedsSend(reason) && !sent) && !(refundNeedsNoSend(reason) && sent)
				if (rawErr == nil) != allowed || (goErr == nil) != allowed {
					t.Errorf("(%s, %s, sent %v): SQL %v, Go %v, want allowed %v", outcome, reason, sent, rawErr, goErr, allowed)
				}
			}
		}
	}
}

func TestRefundReadsAndFigures(t *testing.T) {
	ctx := context.Background()
	s, path, clk := freshStore(t)
	o := mkOrder(t, s, "r1", 100_000, t0.Add(time.Hour))
	other := mkOrder(t, s, "r2", 100_000, t0.Add(time.Hour))
	if st, op, err := s.RefundFigures(ctx, o.ID); err != nil || st != 0 || op != 0 {
		t.Fatalf("empty figures: %d %d %v", st, op, err)
	}
	// Claimed at t0, +1 s, +2 s, +3 s, +3 s (ties by id), the last on another order.
	add := func(order Order, d byte, amount int64, at time.Duration) Refund {
		r := refundOf(order, d, amount)
		r = timed(r, t0.Add(at), t0.Add(at+time.Hour))
		if err := s.AddRefund(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	settled := add(o, 'a', 1000, 0)   // sent, settled
	failed := add(o, 'b', 2000, 1e9)  // sent, failed
	claimed := add(o, 'c', 4000, 2e9) // open, not sent
	sent := add(o, 'e', 8000, 3e9)    // open, sent
	foreign := add(other, 'd', 16000, 3e9)
	for _, r := range []Refund{settled, failed, sent} {
		if err := s.AddRefundSend(ctx, r.ID, t0.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for r, res := range map[Refund]RefundResult{
		settled: {Outcome: RefundOutcomeSettled, Reason: RefundReasonSettled},
		failed:  {Outcome: RefundOutcomeFailed, Reason: RefundReasonFailed},
	} {
		res.RefundID, res.At = r.ID, t0.Add(20*time.Second)
		if err := s.AddRefundResult(ctx, res); err != nil {
			t.Fatal(err)
		}
	}
	if st, op, err := s.RefundFigures(ctx, o.ID); err != nil || st != 1000 || op != 12000 {
		t.Fatalf("figures: settled %d open %d %v, want 1000 and 12000 (failed counts in neither)", st, op, err)
	}
	if st, op, err := s.RefundFigures(ctx, other.ID); err != nil || st != 0 || op != 16000 {
		t.Fatalf("other figures: %d %d %v", st, op, err)
	}
	if _, _, err := s.RefundFigures(ctx, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad order id: %v", err)
	}
	open, err := s.OpenRefunds(ctx)
	if err != nil || len(open) != 3 || open[0].Refund != claimed || open[1].Refund != foreign || open[2].Refund != sent {
		t.Fatalf("OpenRefunds: %+v %v", open, err)
	}
	if open[0].Sent || open[0].Resolved || open[1].Sent || !open[2].Sent || !open[2].SentAt.Equal(t0.Add(10*time.Second)) || open[2].Resolved {
		t.Fatalf("open states: %+v", open)
	}
	// The daily-cap sum: settled and open count, failed does not; the bound is inclusive and whole seconds.
	for _, c := range []struct {
		d    time.Duration
		want int64
	}{{0, 1000 + 4000 + 8000 + 16000}, {1e9, 4000 + 8000 + 16000}, {2e9 + 1e6, 4000 + 8000 + 16000}, {3e9, 8000 + 16000}, {4e9, 0}} {
		if got, err := s.RefundMsatClaimedSince(ctx, t0.Add(c.d)); err != nil || got != c.want {
			t.Fatalf("RefundMsatClaimedSince(+%v) = %d, %v; want %d", c.d, got, err, c.want)
		}
	}
	// Everything survives a reopen: the same reads on the same file.
	s.Close()
	s, err = Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, op, err := s.RefundFigures(ctx, o.ID); err != nil || st != 1000 || op != 12000 {
		t.Fatalf("figures after a reopen: %d %d %v", st, op, err)
	}
	if open, err := s.OpenRefunds(ctx); err != nil || len(open) != 3 || open[2].Refund != sent || !open[2].Sent {
		t.Fatalf("OpenRefunds after a reopen: %+v %v", open, err)
	}
	st, ok, err := s.RefundStateByID(ctx, failed.ID)
	if err != nil || !ok || !st.Resolved || st.Result.Outcome != RefundOutcomeFailed || !st.Sent {
		t.Fatalf("failed state: %+v %v %v", st, ok, err)
	}
	if _, ok, err := s.RefundStateByID(ctx, strings.Repeat("7", 32)); ok || err != nil {
		t.Fatalf("unknown state: %v %v", ok, err)
	}
	if _, _, err := s.RefundStateByID(ctx, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestInvoiceIssued(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 1000, t0.Add(time.Hour))
	inv := Invoice{OrderID: o.ID, PaymentHash: hash('a'), Bolt11: "lnbcrt10n1PMixedCaseInvoice", AmountMsat: 1000, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
	if err := s.AddInvoice(ctx, inv); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]bool{
		inv.Bolt11: true, strings.ToLower(inv.Bolt11): true, strings.ToUpper(inv.Bolt11): true,
		inv.Bolt11 + "q": false, inv.Bolt11[:len(inv.Bolt11)-1]: false, "lnbcrt": false,
	} {
		if got, err := s.InvoiceIssued(ctx, in); err != nil || got != want {
			t.Errorf("InvoiceIssued(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ln bc", strings.Repeat("a", 4097), "lnbc\x00"} {
		if _, err := s.InvoiceIssued(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("InvoiceIssued(%q): %v", bad, err)
		}
	}
	// The lookup uses the index.
	var plan strings.Builder
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT 1 FROM invoices WHERE lower(bolt11) = lower(?)`, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if !strings.Contains(plan.String(), "invoices_bolt11_lower") {
		t.Fatalf("query plan %q does not use invoices_bolt11_lower", plan.String())
	}
}

// A database at version 5 gains the refund tables and the index at Open, and keeps its rows.
func TestMigrationSixAddsRefunds(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/s.db"
	s, err := Open(ctx, path, WithClock(func() time.Time { return t0 }))
	if err != nil {
		t.Fatal(err)
	}
	o := mkOrder(t, s, "v5", 1000, t0.Add(time.Hour))
	for _, q := range append(dropMigrationsFourToEight()[:len(dropMigrationTen())+len(dropMigrationNine())+8], `UPDATE store_schema_version SET version = 5`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if _, err := s.OrderByID(ctx, o.ID); err != nil {
		t.Fatalf("the order did not survive: %v", err)
	}
	mkRefund(t, s, o, 'a', 1000)
	fresh, _, _ := freshStore(t)
	if got, want := schemaText(t, s.db), schemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\nvs\n%s", got, want)
	}
}

// The time order of a refund, in SQL and in Go: a send is written from the claim up to (not including) the approval's
// expiry; a result is not before the claim or the send; a void waits RefundVoidWait (11 minutes) after the send;
// expired_unsent follows the expiry. Each case is run on the raw connection and through the store, which must agree, at
// the boundary and one second either side.
func TestRefundTimeOrderRefused(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	T := ts(t0)        // the claim
	E := T + 24*3600   // the approval's expiry
	const sentAt = 100 // the send, where a case has one, in seconds after the claim
	n := 0
	newRefund := func() Refund {
		n++
		r := refundOf(o, 'a', 1000)
		r.ID = fmt.Sprintf("%032x", n)
		r.Invoice = fmt.Sprintf("ln%d", n)
		return timed(r, t0, t0.Add(24*time.Hour))
	}
	type step struct {
		name   string
		send   int64  // 0: no send marker is written first (the time under test is the result's)
		reason string // "": the case tests the send at sendTry
		at     int64  // the time under test (the send's, or the result's)
		want   string // "": allowed; else the trigger text that refuses it
	}
	const (
		win, claim, before, void, exp = "send outside the approval window", "result before the claim", "result before the send",
			"void before its wait", "expired_unsent before the approval expired"
	)
	for _, c := range []step{
		{"send one second before the claim", 0, "", T - 1, win},
		{"send at the claim", 0, "", T, ""},
		{"send one second before the expiry", 0, "", E - 1, ""},
		{"send at the expiry", 0, "", E, win},
		{"send after the expiry", 0, "", E + 1, win},
		{"failed before the send", T + sentAt, RefundReasonFailed, T + sentAt - 1, before},
		{"failed at the send", T + sentAt, RefundReasonFailed, T + sentAt, ""},
		{"settled before the send", T + sentAt, RefundReasonSettled, T + sentAt - 1, before},
		{"settled at the send", T + sentAt, RefundReasonSettled, T + sentAt, ""},
		{"not_sent before the send", T + sentAt, RefundReasonNotSent, T + sentAt - 1, before},
		{"not_sent at the send", T + sentAt, RefundReasonNotSent, T + sentAt, ""},
		{"voided 600 s after the send", T + sentAt, RefundReasonVoided, T + sentAt + 600, void},
		{"voided 659 s after the send", T + sentAt, RefundReasonVoided, T + sentAt + 659, void},
		{"voided 660 s after the send", T + sentAt, RefundReasonVoided, T + sentAt + 660, ""},
		{"voided before the send", T + sentAt, RefundReasonVoided, T + sentAt - 1, before + "|" + void},
		{"recovered before the send", T + sentAt, RefundReasonRecovered, T + sentAt - 1, before},
		{"recovered at the send", T + sentAt, RefundReasonRecovered, T + sentAt, ""},
		{"key_revoked before the claim", 0, RefundReasonKeyRevoked, T - 1, claim},
		{"key_revoked at the claim", 0, RefundReasonKeyRevoked, T, ""},
		{"invoice_known before the claim", 0, RefundReasonInvoiceKnown, T - 1, claim},
		{"invoice_known at the claim", 0, RefundReasonInvoiceKnown, T, ""},
		{"recovered before the claim", 0, RefundReasonRecovered, T - 1, claim},
		{"cancelled before the claim", 0, RefundReasonCancelled, T - 1, claim},
		{"cancelled at the claim", 0, RefundReasonCancelled, T, ""},
		{"ledger_paid before the claim", 0, RefundReasonLedgerPaid, T - 1, claim},
		{"ledger_paid at the claim", 0, RefundReasonLedgerPaid, T, ""},
		{"expired_unsent before the claim", 0, RefundReasonExpiredUnsent, T - 1, claim + "|" + exp},
		{"expired_unsent one second before the expiry", 0, RefundReasonExpiredUnsent, E - 1, exp},
		{"expired_unsent at the expiry", 0, RefundReasonExpiredUnsent, E, ""},
		{"expired_unsent after the expiry", 0, RefundReasonExpiredUnsent, E + 1, ""},
	} {
		rawR, goR := newRefund(), newRefund()
		for _, r := range []Refund{rawR, goR} {
			if err := s.AddRefund(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		outcome := RefundOutcomeFailed
		if c.reason == RefundReasonSettled || c.reason == RefundReasonLedgerPaid {
			outcome = RefundOutcomeSettled
		}
		var rawErr, goErr error
		if c.reason == "" { // the send is the thing under test
			_, rawErr = raw.Exec(`INSERT INTO refund_sends (refund_id, sent_at) VALUES (?, ?)`, rawR.ID, c.at)
			goErr = s.AddRefundSend(ctx, goR.ID, time.Unix(c.at, 0))
		} else {
			if c.send != 0 {
				if _, err := raw.Exec(`INSERT INTO refund_sends (refund_id, sent_at) VALUES (?, ?)`, rawR.ID, c.send); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if err := s.AddRefundSend(ctx, goR.ID, time.Unix(c.send, 0)); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
			}
			_, rawErr = raw.Exec(`INSERT INTO refund_results (refund_id, outcome, reason, resolved_at) VALUES (?, ?, ?, ?)`, rawR.ID, outcome, c.reason, c.at)
			goErr = s.AddRefundResult(ctx, RefundResult{RefundID: goR.ID, Outcome: outcome, Reason: c.reason, At: time.Unix(c.at, 0)})
		}
		if c.want == "" {
			if rawErr != nil || goErr != nil {
				t.Errorf("%s: SQL %v, Go %v; want both to accept", c.name, rawErr, goErr)
			}
			continue
		}
		// SQLite fires the triggers of one event newest first, so a time that breaks two rules is refused by either.
		hit := false
		for _, w := range strings.Split(c.want, "|") {
			hit = hit || (rawErr != nil && strings.Contains(rawErr.Error(), w))
		}
		if !hit {
			t.Errorf("%s: SQL %v, want %q", c.name, rawErr, c.want)
		}
		if !errors.Is(goErr, ErrInvalid) {
			t.Errorf("%s: Go %v, want ErrInvalid", c.name, goErr)
		}
	}
}

// TestRefundResultPairs: of every outcome and the ten reasons (and one unknown) with and without a send marker, exactly
// the allowed combinations are stored, by AddRefundResult and by a raw INSERT on a second connection alike. The table
// is written out here, not derived from the store's own lists. A cancelled or ledger_paid result never has a send.
func TestRefundResultPairs(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	o := mkOrder(t, s, "r1", 10_000, t0.Add(time.Hour))
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	type pair struct{ outcome, reason string }
	// Allowed pairs: whether they need a send (true), need none (false), or take either (recovered).
	const either = 2
	allowed := map[pair]int{
		{"settled", "settled"}: 1, {"settled", "ledger_paid"}: 0,
		{"failed", "expired_unsent"}: 0, {"failed", "key_revoked"}: 0, {"failed", "invoice_known"}: 0, {"failed", "cancelled"}: 0,
		{"failed", "not_sent"}: 1, {"failed", "failed"}: 1, {"failed", "voided"}: 1, {"failed", "recovered"}: either,
	}
	reasons := []string{RefundReasonSettled, RefundReasonExpiredUnsent, RefundReasonKeyRevoked, RefundReasonInvoiceKnown,
		RefundReasonNotSent, RefundReasonFailed, RefundReasonVoided, RefundReasonRecovered, RefundReasonCancelled,
		RefundReasonLedgerPaid, "other"}
	if RefundReasonCancelled != "cancelled" || RefundReasonLedgerPaid != "ledger_paid" {
		t.Fatal("the new reason constants changed")
	}
	late := t0.Add(48 * time.Hour)
	n := 0
	for _, outcome := range []string{RefundOutcomeSettled, RefundOutcomeFailed} {
		for _, reason := range reasons {
			for _, sent := range []bool{false, true} {
				var ids [2]string
				for k := range ids {
					r := refundOf(o, "0123456789abcdef"[n%16], 1000)
					r.ID = fmt.Sprintf("%016x%016x", n, k)
					r.Invoice = fmt.Sprintf("ln%d", n*2+k)
					r = timed(r, r.ClaimedAt, r.ExpiresAt)
					if err := s.AddRefund(ctx, r); err != nil {
						t.Fatal(err)
					}
					if sent {
						if err := s.AddRefundSend(ctx, r.ID, t0); err != nil {
							t.Fatal(err)
						}
					}
					ids[k] = r.ID
				}
				n++
				need, ok := allowed[pair{outcome, reason}]
				want := ok && (need == either || (need == 1) == sent)
				_, rawErr := raw.Exec(`INSERT INTO refund_results (refund_id, outcome, reason, resolved_at) VALUES (?, ?, ?, ?)`, ids[0], outcome, reason, ts(late))
				goErr := s.AddRefundResult(ctx, RefundResult{RefundID: ids[1], Outcome: outcome, Reason: reason, At: late})
				if (rawErr == nil) != want || (goErr == nil) != want {
					t.Errorf("(%s, %s, sent %v): SQL %v, Go %v, want stored %v", outcome, reason, sent, rawErr, goErr, want)
				}
				if sent && (pair{outcome, reason} == pair{"failed", "cancelled"} || pair{outcome, reason} == pair{"settled", "ledger_paid"}) {
					if !errors.Is(goErr, ErrInvalid) {
						t.Errorf("(%s, %s) with a send: Go %v, want ErrInvalid", outcome, reason, goErr)
					}
					if rawErr == nil || !strings.Contains(rawErr.Error(), "result needs no send") {
						t.Errorf("(%s, %s) with a send: raw insert %v, want the needs-no-send abort", outcome, reason, rawErr)
					}
				}
			}
		}
	}
}
