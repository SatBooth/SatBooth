package ledger

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ledger-wide properties on a long seeded random run: accounts with random limit flags on two
// ledgers, single-phase, pending, post and void transfers, clock steps past timeouts, exact
// replays and altered replays of earlier requests. Every operation, including the Balance reads
// (which can record expiries), goes through env.do into an operation log. Checked:
//
//  1. double entry per ledger, pending and posted separately (checkBooks);
//  2. no limited account is over its limit (checkBooks);
//  3. every pending transfer is resolved at most once, and never both resolved and expired
//     (checkResolutions, after every step, and in the log);
//  4. replaying the whole log, with the same clock values, on a fresh database gives the same
//     result for every operation and the same rows in every table: the ledger is deterministic.
//
// Everything is drawn from one rand.Rand in slice order (no map iteration), so a seed reproduces
// the run. Balances are read every checkEvery steps (each read is a write transaction).

type opKind int

const (
	opAccount opKind = iota
	opTransfer
	opBalance
)

type op struct {
	kind  opKind
	clock int64 // set before the operation
	acct  Account
	tr    Transfer
	id    U128
}

func timeDur(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }

func (c *testClock) set(ns int64) {
	c.mu.Lock()
	c.ns = ns
	c.mu.Unlock()
}

// exec runs one logged operation and returns its result as a string (result code, error text or
// balance).
func exec(l *Ledger, clk *testClock, o op) (string, Balance) {
	clk.set(o.clock)
	ctx := context.Background()
	switch o.kind {
	case opAccount:
		r, err := l.CreateAccount(ctx, o.acct)
		return fmt.Sprintf("account %q %v", r, err), Balance{}
	case opTransfer:
		r, err := l.CreateTransfer(ctx, o.tr)
		return fmt.Sprintf("transfer %q %v", r, err), Balance{}
	default:
		b, err := l.Balance(ctx, o.id)
		return fmt.Sprintf("balance %+v %v", b, err), b
	}
}

type pAcct struct {
	id     uint64
	ledger uint32
	flags  AccountFlags
}

type env struct {
	t     *testing.T
	l     *Ledger
	clk   *testClock
	log   []op
	res   []string
	resol map[uint64]int // pending id -> resolutions the ledger reported as created
}

func (e *env) do(o op) (string, Balance) {
	e.t.Helper()
	o.clock = e.clk.now().UnixNano()
	r, b := exec(e.l, e.clk, o)
	e.log = append(e.log, o)
	e.res = append(e.res, r)
	if !strings.HasSuffix(r, " <nil>") { // every operation here must succeed as a call
		e.t.Fatalf("op %d %+v: %s", len(e.log)-1, o, r)
	}
	if o.kind == opTransfer && strings.HasPrefix(r, `transfer "created"`) && o.tr.Flags&(TransferPostPendingTransfer|TransferVoidPendingTransfer) != 0 {
		e.resol[o.tr.PendingID.Lo]++
		if e.resol[o.tr.PendingID.Lo] > 1 {
			e.t.Fatalf("pending transfer %d resolved twice", o.tr.PendingID.Lo)
		}
	}
	return r, b
}

// checkBooks checks properties 1 and 2 on balances read for all accounts.
func checkBooks(t testing.TB, accts []pAcct, bals []Balance) {
	t.Helper()
	dq, cq := map[uint32]*big.Int{}, map[uint32]*big.Int{}
	dp, cp := map[uint32]*big.Int{}, map[uint32]*big.Int{}
	for _, m := range []map[uint32]*big.Int{dq, cq, dp, cp} {
		for _, a := range accts {
			m[a.ledger] = new(big.Int)
		}
	}
	for i, a := range accts {
		b := bals[i]
		dq[a.ledger].Add(dq[a.ledger], toBig(b.DebitsPosted))
		cq[a.ledger].Add(cq[a.ledger], toBig(b.CreditsPosted))
		dp[a.ledger].Add(dp[a.ledger], toBig(b.DebitsPending))
		cp[a.ledger].Add(cp[a.ledger], toBig(b.CreditsPending))
		d := new(big.Int).Add(toBig(b.DebitsPending), toBig(b.DebitsPosted))
		c := new(big.Int).Add(toBig(b.CreditsPending), toBig(b.CreditsPosted))
		if a.flags&AccountDebitsMustNotExceedCredits != 0 && d.Cmp(toBig(b.CreditsPosted)) > 0 {
			t.Fatalf("account %d: debits %v exceed credits %v", a.id, d, b.CreditsPosted)
		}
		if a.flags&AccountCreditsMustNotExceedDebits != 0 && c.Cmp(toBig(b.DebitsPosted)) > 0 {
			t.Fatalf("account %d: credits %v exceed debits %v", a.id, c, b.DebitsPosted)
		}
	}
	for lg := range dq {
		if dq[lg].Cmp(cq[lg]) != 0 || dp[lg].Cmp(cp[lg]) != 0 {
			t.Fatalf("ledger %d: posted %v vs %v, pending %v vs %v", lg, dq[lg], cq[lg], dp[lg], cp[lg])
		}
	}
}

// checkResolutions is property 3 on the stored rows.
func checkResolutions(t testing.TB, l *Ledger) {
	t.Helper()
	var n int
	if err := l.db.QueryRow(`SELECT count(*) FROM (SELECT pending_id FROM transfers WHERE (flags & 12) != 0
		GROUP BY pending_id HAVING count(*) > 1)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d pending transfers resolved more than once, err %v", n, err)
	}
	if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers e JOIN transfers r ON r.pending_id = e.pending_id
		WHERE (r.flags & 12) != 0`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d pending transfers both resolved and expired, err %v", n, err)
	}
	if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers e JOIN transfers p ON p.id = e.pending_id
		WHERE (p.flags & 2) = 0`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d expiry records for transfers that are not pending, err %v", n, err)
	}
}

// dump lists every row of the ledger tables in a fixed order.
func dump(t testing.TB, l *Ledger) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{`SELECT * FROM accounts ORDER BY id`, `SELECT * FROM transfers ORDER BY id`,
		`SELECT * FROM failed_transfers ORDER BY id`, `SELECT * FROM expired_transfers ORDER BY pending_id`} {
		rows, err := l.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%v\n", vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// replay runs the first n logged operations on a fresh database and compares every result and
// all stored rows with the original run's. original is nil when n is the whole log (the rows
// are then compared with e.l).
func (e *env) replay(n int, orig string) {
	e.t.Helper()
	clk := &testClock{ns: clockStart}
	l, err := Open(context.Background(), filepath.Join(e.t.TempDir(), "replay.db"), WithClock(clk.now))
	if err != nil {
		e.t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < n; i++ {
		if got, _ := exec(l, clk, e.log[i]); got != e.res[i] {
			e.t.Fatalf("replay differs at op %d %+v:\n replay: %s\n original: %s", i, e.log[i], got, e.res[i])
		}
	}
	if got := dump(e.t, l); got != orig {
		e.t.Fatalf("replay of %d ops left different rows", n)
	}
}

func TestLedgerProperties(t *testing.T) {
	// Seed 1 is the long run (5000 operations); seed 2 a shorter second shape.
	for _, c := range []struct {
		seed  int64
		steps int
	}{{1, 5000}, {2, 800}} {
		if testing.Short() || raceOn {
			c.steps /= 5 // the race detector makes this several times slower
		}
		t.Run(fmt.Sprint(c.seed), func(t *testing.T) { ledgerProperties(t, c.seed, c.steps, 25) })
	}
}

func ledgerProperties(t *testing.T, seed int64, steps, checkEvery int) {
	rng := rand.New(rand.NewSource(seed))
	clk := &testClock{ns: clockStart}
	l, err := Open(context.Background(), filepath.Join(t.TempDir(), "l.db"), WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	e := &env{t: t, l: l, clk: clk, resol: map[uint64]int{}}

	// Eight accounts on two ledgers; 1 and 5 are unlimited funders, the others get random flags.
	var accts []pAcct
	for i := uint64(1); i <= 8; i++ {
		a := pAcct{id: i, ledger: 1 + uint32(i-1)/4}
		if i != 1 && i != 5 {
			a.flags = []AccountFlags{0, AccountDebitsMustNotExceedCredits, AccountCreditsMustNotExceedDebits}[rng.Intn(3)]
		}
		accts = append(accts, a)
		e.do(op{kind: opAccount, acct: Account{ID: u(a.id), Ledger: a.ledger, Code: 1, Flags: a.flags}})
	}

	balances := func() []Balance {
		out := make([]Balance, len(accts))
		for i, a := range accts {
			_, out[i] = e.do(op{kind: opBalance, id: u(a.id)})
		}
		return out
	}
	// check reads all balances twice: the first pass records the expiries due now (a record
	// moves ledger time by a few ns), so the second is a consistent set of reads.
	check := func() {
		t.Helper()
		balances()
		checkBooks(t, accts, balances())
	}

	edge := []U128{u(0), u(1), u(1<<63 - 1), p2(63), u(^uint64(0)), p2(64), mustSub(U128Max, u(1)), U128Max}
	type pend struct {
		id     uint64
		amount U128
	}
	var pendings []pend
	var singles []uint64 // created ids
	var reqs []Transfer  // every transfer request submitted, in order
	nextID := uint64(1000)
	counts := map[string]int{}

	for step := 1; step <= steps; step++ {
		switch r := rng.Intn(10); {
		case r < 5:
			clk.advance(timeDur(rng.Intn(3000)))
		case r == 5 && rng.Intn(4) == 0:
			clk.advance(timeDur(3000 + rng.Intn(10000))) // past most timeouts
		case r == 6:
			clk.advance(-timeDur(rng.Intn(2000)))
		}
		var o op
		nextID++
		switch r := rng.Intn(100); {
		case r < 45: // a new single-phase or pending transfer
			d := uint64(rng.Intn(8) + 1)
			if rng.Intn(4) == 0 {
				d = []uint64{1, 5}[rng.Intn(2)] // a funder pays
			}
			c := uint64(rng.Intn(4)) + 1 + (d-1)/4*4 // same ledger as d
			if c == d && rng.Intn(6) != 0 {
				c = (d-1)/4*4 + (d-1+uint64(rng.Intn(3))+1)%4 + 1 // another account of the same ledger
			}
			if rng.Intn(15) == 0 {
				c = uint64(rng.Intn(8) + 1)
			}
			x := Transfer{ID: u(nextID), DebitAccountID: u(d), CreditAccountID: u(c), Ledger: accts[d-1].ledger, Code: 1}
			if rng.Intn(12) == 0 {
				x.Ledger = uint32(rng.Intn(2) + 1)
			}
			if rng.Intn(10) == 0 {
				x.Amount = edge[rng.Intn(len(edge))]
			} else {
				x.Amount = u(uint64(rng.Intn(60)))
				if rng.Intn(3) == 0 {
					x.Amount = u(uint64(rng.Intn(400)))
				}
			}
			if rng.Intn(100) < 40 {
				x.Flags = TransferPending
				x.Timeout = []uint32{0, 0, 1, 2, 5, 30, 4294967295}[rng.Intn(7)]
			}
			if rng.Intn(60) == 0 {
				x.Flags |= TransferFlags(1) << uint(3+rng.Intn(3)) // reserved flag bits
			}
			o = op{kind: opTransfer, tr: x}
		case r < 70 && len(pendings)+len(singles) > 0: // post or void
			pid := nextID + 1 // not found
			if len(pendings) > 0 && rng.Intn(20) != 0 {
				pid = pendings[rng.Intn(len(pendings))].id
			} else if len(singles) > 0 {
				pid = singles[rng.Intn(len(singles))]
			}
			x := Transfer{ID: u(nextID), PendingID: u(pid), Flags: TransferPostPendingTransfer}
			if rng.Intn(2) == 0 {
				x.Flags = TransferVoidPendingTransfer
			}
			var pa U128
			for _, p := range pendings {
				if p.id == pid {
					pa = p.amount
				}
			}
			switch rng.Intn(8) {
			case 0:
				x.Amount = U128Max
			case 1:
				x.Amount = edge[rng.Intn(len(edge))]
			case 2, 3:
				x.Amount = pa
			case 4, 5:
				if pa.Hi == 0 && pa.Lo < 1<<20 {
					x.Amount = u(uint64(rng.Intn(int(pa.Lo) + 1)))
				}
			}
			o = op{kind: opTransfer, tr: x}
		case r < 85 && len(reqs) > 0: // an exact replay, or one field changed
			x := reqs[rng.Intn(len(reqs))]
			if rng.Intn(2) == 0 {
				switch rng.Intn(5) {
				case 0:
					if sum, ok := x.Amount.Add(u(1)); ok {
						x.Amount = sum
					}
				case 1:
					x.Flags ^= TransferPending
				case 2:
					x.UserData64++
				case 3:
					x.Timeout++
				case 4:
					x.Code = 2
				}
			}
			o = op{kind: opTransfer, tr: x}
		case r < 90: // an account request: a replay, or the same id with other fields
			a := accts[rng.Intn(len(accts))]
			acc := Account{ID: u(a.id), Ledger: a.ledger, Code: 1, Flags: a.flags}
			switch rng.Intn(3) {
			case 0:
				acc.Flags ^= AccountDebitsMustNotExceedCredits
			case 1:
				acc.Ledger = 3 - a.ledger
			}
			o = op{kind: opAccount, acct: acc}
		default: // a balance read (records expiries)
			o = op{kind: opBalance, id: u(uint64(rng.Intn(8) + 1))}
		}
		res, _ := e.do(o)
		if o.kind == opTransfer {
			reqs = append(reqs, o.tr)
			counts[strings.Fields(res)[1]]++
			if strings.HasPrefix(res, `transfer "created"`) && o.tr.PendingID.IsZero() {
				if o.tr.Flags&TransferPending != 0 {
					pendings = append(pendings, pend{o.tr.ID.Lo, o.tr.Amount})
				} else {
					singles = append(singles, o.tr.ID.Lo)
				}
			}
		}
		checkResolutions(t, l)
		if step%checkEvery == 0 {
			check()
		}
		if step == steps/2 {
			check()
			e.replay(len(e.log), dump(t, l)) // property 4 on the first half
		}
	}
	check()
	e.replay(len(e.log), dump(t, l)) // property 4 on the whole log

	// The run must have exercised what it claims to (the reduced runs are too short for the rare codes).
	for _, want := range []string{`"created"`, `"exceeds_credits"`, `"exceeds_debits"`, `"pending_transfer_expired"`,
		`"pending_transfer_already_posted"`, `"pending_transfer_already_voided"`, `"exists"`, `"id_already_failed"`} {
		if counts[want] == 0 && steps >= 800 {
			t.Errorf("result %s never occurred: %v", want, counts)
		}
	}
	var np, nv, ne int
	for _, q := range []struct {
		q string
		n *int
	}{{`SELECT count(*) FROM transfers WHERE (flags & 4) != 0`, &np}, {`SELECT count(*) FROM transfers WHERE (flags & 8) != 0`, &nv},
		{`SELECT count(*) FROM expired_transfers`, &ne}} {
		if err := l.db.QueryRow(q.q).Scan(q.n); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("results %v; %d posts, %d voids, %d expiries", counts, np, nv, ne)
	if np < steps/100 || nv < steps/150 || ne < 3 {
		t.Errorf("weak run: %d posts, %d voids, %d expiries", np, nv, ne)
	}
}
