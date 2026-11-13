package status

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"

	_ "github.com/ncruces/go-sqlite3/driver"
)

var (
	_ Lightning = (*lnpay.Service)(nil)
	_ Onchain   = (*onchain.Service)(nil)
	_ Monero    = (*monero.Service)(nil)
)

type stubLn struct {
	st  lnpay.Status
	err error
}

func (s stubLn) Status(context.Context, string) (lnpay.Status, error) { return s.st, s.err }
func (stubLn) Pay(context.Context, string) (store.Invoice, error)     { return store.Invoice{}, nil }
func (stubLn) ValidateAmount(int64) error                             { return nil }

type stubOc struct {
	rail onchain.Rail
	err  error
}

func (s stubOc) Rail(context.Context, store.Order) (onchain.Rail, error) { return s.rail, s.err }
func (stubOc) Address(context.Context, string) (store.Address, error)    { return store.Address{}, nil }

type stubXm struct {
	view rail.View
	err  error
}

func (s stubXm) Rail(context.Context, store.Order) (rail.View, error) { return s.view, s.err }
func (stubXm) Address(context.Context, string) (store.Subaddress, error) {
	return store.Subaddress{}, nil
}

var t0 = time.Unix(1_800_000_000, 0)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

var refSeq int

func mkOrder(t *testing.T, st *store.Store, msat, sat int64) store.Order {
	t.Helper()
	return mkOrder3(t, st, msat, sat, 0)
}

func mkOrder3(t *testing.T, st *store.Store, msat, sat, pico int64) store.Order {
	t.Helper()
	refSeq++
	o, _, err := st.CreateOrder(context.Background(), store.Order{AccountID: "a", MerchantRef: "r" + strconv.Itoa(refSeq),
		AmountMsat: msat, AmountSat: sat, AmountPiconero: pico, ExpiresAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPrecedenceTable(t *testing.T) {
	st := newStore(t)
	early, late := t0.Add(-2*time.Hour), t0.Add(-1*time.Hour)
	earliest, latest := t0.Add(-3*time.Hour), t0.Add(-30*time.Minute)

	lns := map[string]*lnpay.Status{
		"nil":     nil,
		"new":     {State: lnpay.StateNew},
		"paid":    {State: lnpay.StatePaid, PaidAt: late},
		"expired": {State: lnpay.StateExpired},
	}
	// The rail states as Rail derives them; the flags follow the state. Past the expiry Rail gives expired and
	// underpaid only with their events (onchain's TestRailFinalStatesFollowTheEvents), so these rows stand for an
	// order whose order.expired or order.underpaid exists; derive itself is unchanged by that rule.
	ocs := map[string]*onchain.Rail{
		"nil":       nil,
		"waiting":   {State: onchain.RailWaiting},
		"seen":      {State: onchain.RailSeen, ReceivedSat: 1000},
		"paid":      {State: onchain.RailPaid, Paid: true, PaidAt: early, ConfirmedSat: 1000},
		"overpaid":  {State: onchain.RailOverpaid, Paid: true, Overpaid: true, PaidAt: early},
		"underpaid": {State: onchain.RailUnderpaid, ReceivedSat: 5},
		"late":      {State: onchain.RailLate, LateSat: 1000},
		"expired":   {State: onchain.RailExpired},
	}
	// The Monero rail's views: paid is the earliest of all the paid times, overpaid the latest.
	xms := map[string]*rail.View{
		"nil":       nil,
		"waiting":   {State: rail.Waiting},
		"seen":      {State: rail.Seen, Received: 1000},
		"paid":      {State: rail.Paid, Paid: true, PaidAt: earliest, Received: 1000, Confirmed: 1000},
		"overpaid":  {State: rail.Overpaid, Paid: true, Overpaid: true, PaidAt: latest, Received: 2000, Confirmed: 2000},
		"underpaid": {State: rail.Underpaid, Received: 5},
		"late":      {State: rail.Late, Late: 1000},
		"expired":   {State: rail.Expired},
	}
	// One order per set of rails.
	orders := map[[3]bool]store.Order{}
	for _, ln := range []bool{false, true} {
		for _, oc := range []bool{false, true} {
			for _, xm := range []bool{false, true} {
				if !ln && !oc && !xm {
					continue // an order needs one amount
				}
				var msat, sat, pico int64
				if ln {
					msat = 1000
				}
				if oc {
					sat = 1000
				}
				if xm {
					pico = 1000
				}
				orders[[3]bool{ln, oc, xm}] = mkOrder3(t, st, msat, sat, pico)
			}
		}
	}
	rows := 0
	for lk, ln := range lns {
		for ok, oc := range ocs {
			for xk, xm := range xms {
				if ln == nil && oc == nil && xm == nil {
					continue
				}
				for _, after := range []bool{false, true} {
					now := t0.Add(-time.Minute)
					if after {
						now = t0.Add(time.Minute)
					}
					// Expected state by the precedence rules, written out independently of derive.
					lnPaid := lk == "paid"
					ocPaid := ok == "paid" || ok == "overpaid"
					xmPaid := xk == "paid" || xk == "overpaid"
					var paidAts []time.Time
					if lnPaid {
						paidAts = append(paidAts, late)
					}
					if ocPaid {
						paidAts = append(paidAts, early)
					}
					if xmPaid {
						paidAts = append(paidAts, xm.PaidAt)
					}
					lnDone := lk == "nil" || lk == "expired" // settle's lnDone
					var want string
					var wantAt time.Time
					switch {
					case len(paidAts) > 0:
						want, wantAt = StatePaid, paidAts[0]
						for _, at := range paidAts[1:] {
							wantAt = minTime(wantAt, at)
						}
					case ok == "late" || xk == "late":
						want = StateLate
					case after && lnDone && (ok == "underpaid" || xk == "underpaid"):
						want = StateUnderpaid
					case after && lnDone && (ok == "nil" || ok == "expired") && (xk == "nil" || xk == "expired"):
						want = StateExpired
					default:
						want = StateNew
					}
					r := &Resolver{St: st, Now: func() time.Time { return now }}
					if ln != nil {
						r.Ln = stubLn{st: *ln}
					} else {
						r.Ln = stubLn{err: errors.New("must not be called")}
					}
					if oc != nil {
						r.Oc = stubOc{rail: *oc}
					}
					if xm != nil {
						r.Xm = stubXm{view: *xm}
					}
					ord := orders[[3]bool{ln != nil, oc != nil, xm != nil}]
					what := "ln=" + lk + " oc=" + ok + " xm=" + xk + " after=" + strconv.FormatBool(after)
					got, err := r.ByID(context.Background(), ord.ID)
					if err != nil {
						t.Fatalf("%s: %v", what, err)
					}
					if got.State != want || !got.PaidAt.Equal(wantAt) {
						t.Errorf("%s: state %s paidAt %v, want %s %v", what, got.State, got.PaidAt, want, wantAt)
					}
					if (got.Lightning == nil) != (ln == nil) || (got.Onchain == nil) != (oc == nil) || (got.Monero == nil) != (xm == nil) {
						t.Errorf("%s: rail pointers wrong", what)
					}
					if xm != nil && *got.Monero != *xm {
						t.Errorf("%s: Monero view %+v, want %+v", what, *got.Monero, *xm)
					}
					if byTok, err := r.ByToken(context.Background(), ord.Token); err != nil || byTok.State != want {
						t.Errorf("ByToken %s: %v %v", what, byTok.State, err)
					}
					rows++
				}
			}
		}
	}
	if rows != 2*(4*8*8-1) {
		t.Fatalf("%d rows", rows)
	}
}

// Paid on any rail wins, at the earliest PaidAt of the paid rails, whichever rail that is.
func TestEarliestPaidAtOfThreeRails(t *testing.T) {
	st := newStore(t)
	o := mkOrder3(t, st, 1000, 1000, 1000)
	a, b, c := t0.Add(-3*time.Hour), t0.Add(-2*time.Hour), t0.Add(-time.Hour)
	for _, ts := range [][3]time.Time{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}, {a, a, a}} {
		r := &Resolver{St: st, Now: func() time.Time { return t0 },
			Ln: stubLn{st: lnpay.Status{State: lnpay.StatePaid, PaidAt: ts[0]}},
			Oc: stubOc{rail: onchain.Rail{State: onchain.RailPaid, Paid: true, PaidAt: ts[1]}},
			Xm: stubXm{view: rail.View{State: rail.Paid, Paid: true, PaidAt: ts[2]}}}
		got, err := r.ByID(context.Background(), o.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != StatePaid || !got.PaidAt.Equal(a) {
			t.Errorf("%v: got %s %v, want paid %v", ts, got.State, got.PaidAt, a)
		}
	}
	// Monero alone paid, the others not: paid at Monero's time.
	r := &Resolver{St: st, Now: func() time.Time { return t0 }, Ln: stubLn{st: lnpay.Status{State: lnpay.StateNew}},
		Oc: stubOc{rail: onchain.Rail{State: onchain.RailSeen, ReceivedSat: 5}}, Xm: stubXm{view: rail.View{State: rail.Paid, Paid: true, PaidAt: b}}}
	if got, err := r.ByID(context.Background(), o.ID); err != nil || got.State != StatePaid || !got.PaidAt.Equal(b) {
		t.Errorf("Monero paid: %s %v %v", got.State, got.PaidAt, err)
	}
}

func TestBothPaidEarlierWins(t *testing.T) {
	st := newStore(t)
	o := mkOrder(t, st, 1000, 1000)
	for _, c := range []struct{ ln, oc time.Time }{
		{t0.Add(-time.Hour), t0.Add(-2 * time.Hour)},
		{t0.Add(-2 * time.Hour), t0.Add(-time.Hour)},
		{t0.Add(-time.Hour), t0.Add(-time.Hour)},
	} {
		r := &Resolver{St: st, Now: func() time.Time { return t0 },
			Ln: stubLn{st: lnpay.Status{State: lnpay.StatePaid, PaidAt: c.ln}},
			Oc: stubOc{rail: onchain.Rail{State: onchain.RailPaid, Paid: true, PaidAt: c.oc}}}
		got, err := r.ByID(context.Background(), o.ID)
		if err != nil {
			t.Fatal(err)
		}
		if want := minTime(c.ln, c.oc); got.State != StatePaid || !got.PaidAt.Equal(want) {
			t.Errorf("got %s %v, want paid %v", got.State, got.PaidAt, want)
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func TestUnavailableAndHasOnchain(t *testing.T) {
	st := newStore(t)
	both := mkOrder(t, st, 1000, 1000)
	onlyLn := mkOrder(t, st, 1000, 0)
	r := &Resolver{St: st, Ln: stubLn{st: lnpay.Status{State: lnpay.StateNew}}}
	if r.HasOnchain() {
		t.Error("HasOnchain true with Oc nil")
	}
	if (&Resolver{}).HasOnchain() {
		t.Error("zero Resolver has on-chain")
	}
	if _, err := r.ByID(context.Background(), both.ID); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrOnchainUnavailable) ||
		errors.Is(err, ErrMoneroUnavailable) {
		t.Errorf("err = %v, want ErrOnchainUnavailable", err)
	}
	if got, err := r.ByID(context.Background(), onlyLn.ID); err != nil || got.State != StateNew || got.Onchain != nil || got.Monero != nil {
		t.Errorf("Lightning-only order: %+v %v", got, err)
	}
	r.Oc = stubOc{}
	if !r.HasOnchain() {
		t.Error("HasOnchain false with Oc set")
	}
}

// A Monero amount without the Monero rail is ErrMoneroUnavailable, which is an ErrUnavailable; the on-chain check
// comes first.
func TestUnavailableAndHasMonero(t *testing.T) {
	st := newStore(t)
	xm := mkOrder3(t, st, 0, 0, 1000)
	lnXm := mkOrder3(t, st, 1000, 0, 1000)
	all := mkOrder3(t, st, 1000, 1000, 1000)
	r := &Resolver{St: st, Ln: stubLn{st: lnpay.Status{State: lnpay.StateNew}}, Oc: stubOc{}}
	if r.HasMonero() || (&Resolver{}).HasMonero() {
		t.Error("HasMonero true with Xm nil")
	}
	for _, o := range []store.Order{xm, lnXm, all} {
		if _, err := r.ByID(context.Background(), o.ID); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrMoneroUnavailable) ||
			errors.Is(err, ErrOnchainUnavailable) {
			t.Errorf("err = %v, want ErrMoneroUnavailable", err)
		}
		if _, err := r.ByToken(context.Background(), o.Token); !errors.Is(err, ErrMoneroUnavailable) {
			t.Errorf("ByToken err = %v, want ErrMoneroUnavailable", err)
		}
	}
	// Both rails missing: the on-chain one is named.
	r.Oc = nil
	if _, err := r.ByID(context.Background(), all.ID); !errors.Is(err, ErrOnchainUnavailable) {
		t.Errorf("both missing: %v", err)
	}
	r.Oc, r.Xm = stubOc{}, stubXm{view: rail.View{State: rail.Waiting}}
	if !r.HasMonero() {
		t.Error("HasMonero false with Xm set")
	}
	if got, err := r.ByID(context.Background(), xm.ID); err != nil || got.State != StateNew || got.Monero == nil || got.Lightning != nil || got.Onchain != nil {
		t.Errorf("Monero-only order: %+v %v", got, err)
	}
	// An order without a Monero amount never asks the Monero rail.
	r.Xm = stubXm{err: errors.New("must not be called")}
	if got, err := r.ByID(context.Background(), mkOrder(t, st, 1000, 1000).ID); err != nil || got.Monero != nil {
		t.Errorf("no Monero amount: %+v %v", got, err)
	}
	for err, want := range map[error]string{
		ErrOnchainUnavailable: "status: the on-chain rail is not configured",
		ErrMoneroUnavailable:  "status: the Monero rail is not configured",
	} {
		if err.Error() != want {
			t.Errorf("%q, want %q", err, want)
		}
	}
}

func TestErrorsPass(t *testing.T) {
	st := newStore(t)
	o := mkOrder(t, st, 1000, 1000)
	boom := errors.New("boom")
	r := &Resolver{St: st, Ln: stubLn{err: boom}, Oc: stubOc{}}
	if _, err := r.ByID(context.Background(), o.ID); !errors.Is(err, boom) {
		t.Errorf("ln err = %v", err)
	}
	r = &Resolver{St: st, Ln: stubLn{}, Oc: stubOc{err: boom}}
	if _, err := r.ByID(context.Background(), o.ID); !errors.Is(err, boom) {
		t.Errorf("oc err = %v", err)
	}
	r = &Resolver{St: st, Ln: stubLn{}, Oc: stubOc{}, Xm: stubXm{err: boom}}
	if _, err := r.ByID(context.Background(), mkOrder3(t, st, 1000, 1000, 1000).ID); !errors.Is(err, boom) {
		t.Errorf("xm err = %v", err)
	}
	if _, err := r.ByID(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing order: %v", err)
	}
	if _, err := r.ByToken(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing token: %v", err)
	}
}

func TestExpiryBoundaryAndLateBeatsUnderpaid(t *testing.T) {
	st := newStore(t)
	o := mkOrder(t, st, 1000, 1000)
	ctx := context.Background()
	// now == ExpiresAt is already expired (the store's boundary: ExpiresAt <= now).
	r := &Resolver{St: st, Now: func() time.Time { return o.ExpiresAt },
		Ln: stubLn{st: lnpay.Status{State: lnpay.StateExpired}},
		Oc: stubOc{rail: onchain.Rail{State: onchain.RailExpired}}}
	if got, err := r.ByID(ctx, o.ID); err != nil || got.State != StateExpired {
		t.Errorf("now == ExpiresAt: %q %v, want expired", got.State, err)
	}
	// One second before is not.
	r.Now = func() time.Time { return o.ExpiresAt.Add(-time.Second) }
	if got, _ := r.ByID(ctx, o.ID); got.State != StateNew {
		t.Errorf("before expiry: %q, want new", got.State)
	}
	// LateSat > 0 beats an underpaid state.
	r.Now = func() time.Time { return o.ExpiresAt.Add(time.Hour) }
	r.Oc = stubOc{rail: onchain.Rail{State: onchain.RailUnderpaid, LateSat: 5}}
	if got, _ := r.ByID(ctx, o.ID); got.State != StateLate {
		t.Errorf("late+underpaid: %q, want late", got.State)
	}
	// Late on the Monero rail beats an on-chain underpaid too, and expired needs every rail expired.
	x := mkOrder3(t, st, 0, 1000, 1000)
	r.Ln = stubLn{err: errors.New("must not be called")}
	r.Xm = stubXm{view: rail.View{State: rail.Late, Late: 5}}
	if got, _ := r.ByID(ctx, x.ID); got.State != StateLate {
		t.Errorf("Monero late + on-chain underpaid: %q, want late", got.State)
	}
	r.Oc, r.Xm = stubOc{rail: onchain.Rail{State: onchain.RailExpired}}, stubXm{view: rail.View{State: rail.Seen, Received: 5}}
	if got, _ := r.ByID(ctx, x.ID); got.State != StateNew {
		t.Errorf("on-chain expired, Monero seen: %q, want new", got.State)
	}
	r.Xm = stubXm{view: rail.View{State: rail.Expired}}
	if got, _ := r.ByID(ctx, x.ID); got.State != StateExpired {
		t.Errorf("both expired: %q, want expired", got.State)
	}
}

// A mixed order past its expiry with a partial on-chain payment reads underpaid only when settle can emit
// order.underpaid, that is when Lightning is done (onchain.LightningDone): while its invoice may still be paid it reads
// new, as the events do.
func TestUnderpaidWaitsForLightning(t *testing.T) {
	st := newStore(t)
	o := mkOrder(t, st, 1000, 1000)
	ctx := context.Background()
	after := o.ExpiresAt.Add(time.Minute)
	under := stubOc{rail: onchain.Rail{State: onchain.RailUnderpaid, ReceivedSat: 5}}
	for _, c := range []struct {
		ln   lnpay.State
		want string
	}{
		{lnpay.StateNew, StateNew},
		{lnpay.StateExpired, StateUnderpaid},
		{lnpay.StatePaid, StatePaid},
	} {
		r := &Resolver{St: st, Now: func() time.Time { return after }, Ln: stubLn{st: lnpay.Status{State: c.ln}}, Oc: under}
		got, err := r.ByID(ctx, o.ID)
		if err != nil || got.State != c.want {
			t.Errorf("lightning %s: %q %v, want %q", c.ln, got.State, err, c.want)
		}
		if onchain.LightningDone(o, lnpay.Status{State: c.ln}) != (c.ln == lnpay.StateExpired) {
			t.Errorf("LightningDone(%s)", c.ln)
		}
	}
	// An on-chain-only order has no Lightning side to wait for.
	oc := mkOrder(t, st, 0, 1000)
	r := &Resolver{St: st, Now: func() time.Time { return after }, Ln: stubLn{err: errors.New("must not be called")}, Oc: under}
	if got, err := r.ByID(ctx, oc.ID); err != nil || got.State != StateUnderpaid || !onchain.LightningDone(oc, lnpay.Status{}) {
		t.Errorf("on-chain only: %q %v", got.State, err)
	}
}

// testQuote is a valid quote for a priced order with a Lightning and a Monero amount: the XMR rate first, as given.
func testQuote() store.Quote {
	return store.Quote{Price: "12.50", Currency: "EUR", MarginBps: 150, QuotedAt: t0,
		Rates: []store.AssetRate{
			{Asset: "XMR", Rate: "150.25", Sources: []store.QuoteSource{{Provider: "kraken", Rate: "150.25", At: t0}}},
			{Asset: "BTC", Rate: "70000.5",
				Sources: []store.QuoteSource{{Provider: "kraken", Rate: "70000", At: t0}, {Provider: "yadio", Rate: "70001", At: t0}}}}}
}

// A priced order's resolved Order carries its quote, by id and by token; an explicit order's Quote is nil.
func TestResolveCarriesTheQuote(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	q, _, err := st.CreateQuotedOrder(ctx, store.Order{AccountID: "a", MerchantRef: "priced", AmountMsat: 18_000, AmountPiconero: 84_000_000_000,
		ExpiresAt: t0}, testQuote())
	if err != nil {
		t.Fatal(err)
	}
	ex := mkOrder(t, st, 1000, 0)
	r := &Resolver{St: st, Ln: stubLn{st: lnpay.Status{State: lnpay.StateNew}}, Xm: stubXm{view: rail.View{State: rail.Waiting}}}
	for name, get := range map[string]func(store.Order) (Order, error){
		"id":    func(o store.Order) (Order, error) { return r.ByID(ctx, o.ID) },
		"token": func(o store.Order) (Order, error) { return r.ByToken(ctx, o.Token) },
	} {
		so, err := get(q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := testQuote()
		want.OrderID = q.ID
		// The store returns the rates sorted by asset: BTC, then XMR.
		if so.Quote == nil || so.Quote.OrderID != q.ID || so.Quote.Price != want.Price || so.Quote.Currency != want.Currency ||
			len(so.Quote.Rates) != 2 || so.Quote.MarginBps != want.MarginBps || !so.Quote.QuotedAt.Equal(t0) ||
			so.Quote.Rates[0].Asset != "BTC" || so.Quote.Rates[0].Rate != want.Rates[1].Rate ||
			len(so.Quote.Rates[0].Sources) != 2 || so.Quote.Rates[0].Sources[1].Provider != "yadio" ||
			so.Quote.Rates[1].Asset != "XMR" || so.Quote.Rates[1].Rate != want.Rates[0].Rate || len(so.Quote.Rates[1].Sources) != 1 {
			t.Errorf("%s: quoted order's Quote = %+v", name, so.Quote)
		}
		so, err = get(ex)
		if err != nil || so.Quote != nil {
			t.Errorf("%s: explicit order's Quote = %+v (%v)", name, so.Quote, err)
		}
	}
}

// A failed quote read is returned, as the rails' errors are: never a priced order without its price.
func TestQuoteErrorPasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	o := mkOrder(t, st, 1000, 0)
	raw, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE order_quotes`); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{St: st, Ln: stubLn{}}
	if _, err := r.ByID(ctx, o.ID); err == nil {
		t.Error("ByID with the quote table gone: no error")
	}
	if _, err := r.ByToken(ctx, o.Token); err == nil {
		t.Error("ByToken with the quote table gone: no error")
	}
}
