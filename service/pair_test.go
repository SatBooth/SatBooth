package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/store"
)

// memPair holds both stamps in memory and records every write, so a test sees the exact order of the writes.
// failAt > 0 makes that write (1-based) apply and then fail, as a crash right after its commit would.
type memPair struct {
	s        store.PairStamp
	l        ledger.PairStamp
	writes   []string
	failAt   int
	nWrites  int
	readFail bool
}

var errCrash = errors.New("crash")

func (m *memPair) wrote(w string) error {
	m.writes = append(m.writes, w)
	m.nWrites++
	if m.nWrites == m.failAt {
		return errCrash
	}
	return nil
}

type memStore struct{ m *memPair }

func (f memStore) Pair(context.Context) (store.PairStamp, error) {
	if f.m.readFail {
		return store.PairStamp{}, errors.New("read")
	}
	return f.m.s, nil
}

func (f memStore) SetPair(_ context.Context, s store.PairStamp) error {
	f.m.s = s
	return f.m.wrote(sw(s.ID, s.Gen, s.Nonce))
}

type memLedger struct{ m *memPair }

func (f memLedger) Pair(context.Context) (ledger.PairStamp, error) { return f.m.l, nil }

func (f memLedger) SetPair(_ context.Context, l ledger.PairStamp) error {
	f.m.l = l
	return f.m.wrote(lw(l.ID, l.Gen, l.Nonce, l.Pending, l.PendingNonce))
}

// sw and lw spell a store write and a ledger write; "-" stands for an empty id or nonce.
func sw(id string, gen int64, nonce string) string {
	return fmt.Sprintf("S %s %d %s", dash(id), gen, dash(nonce))
}

func lw(id string, gen int64, nonce string, pending int64, pn string) string {
	return fmt.Sprintf("L %s %d %s %d %s", dash(id), gen, dash(nonce), pending, dash(pn))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pairRand and pairRand2 are the first and the second 16 bytes the deterministic reader below gives, in hex: the
// first stamp's id and nonce, or an advance's nonce and nothing else.
var (
	pairRand  = strings.Repeat("11", 16)
	pairRand2 = strings.Repeat("22", 16)
)

func randReader() io.Reader {
	var b []byte
	for i := 1; i <= 4; i++ {
		b = append(b, bytes.Repeat([]byte{byte(0x11 * i)}, 16)...)
	}
	return bytes.NewReader(b)
}

// Every row of the pair check's table, and the states it refuses: for each, the writes in their order, or the refusal
// with no write.
func TestPairStates(t *testing.T) {
	x, y, r1, r2 := strings.Repeat("ab", 16), strings.Repeat("cd", 16), pairRand, pairRand2
	a, b, c := strings.Repeat("a0", 16), strings.Repeat("b0", 16), strings.Repeat("c0", 16)
	type S = store.PairStamp
	type L = ledger.PairStamp
	ss := func(id string, gen int64, nonce string) S { return S{ID: id, Gen: gen, Nonce: nonce} }
	ll := func(id string, gen int64, nonce string, pending int64, pn string) L {
		return L{ID: id, Gen: gen, Nonce: nonce, Pending: pending, PendingNonce: pn}
	}
	for _, tc := range []struct {
		name   string
		s      S
		l      L
		writes []string
		want   error
	}{
		{"first stamp", ss("", 0, ""), ll("", 0, "", 0, ""), []string{lw(r1, 0, "", 0, ""), sw(r1, 0, ""), lw(r1, 0, "", 1, r2), sw(r1, 1, r2), lw(r1, 1, r2, 0, "")}, nil},
		{"finish the first stamp", ss("", 0, ""), L{ID: x}, []string{sw(x, 0, ""), lw(x, 0, "", 1, r1), sw(x, 1, r1), lw(x, 1, r1, 0, "")}, nil},
		{"advance", ss(x, 4, a), L{ID: x, Gen: 4, Nonce: a}, []string{lw(x, 4, a, 5, r1), sw(x, 5, r1), lw(x, 5, r1, 0, "")}, nil},
		{"advance from 0", ss(x, 0, ""), L{ID: x}, []string{lw(x, 0, "", 1, r1), sw(x, 1, r1), lw(x, 1, r1, 0, "")}, nil},
		{"commit, advance", ss(x, 5, b), ll(x, 4, a, 5, b), []string{lw(x, 5, b, 0, ""), lw(x, 5, b, 6, r1), sw(x, 6, r1), lw(x, 6, r1, 0, "")}, nil},
		{"write the store, commit, advance", ss(x, 4, a), ll(x, 4, a, 5, b), []string{sw(x, 5, b), lw(x, 5, b, 0, ""), lw(x, 5, b, 6, r1), sw(x, 6, r1), lw(x, 6, r1, 0, "")}, nil},
		{"commit from 0, advance", ss(x, 1, b), ll(x, 0, "", 1, b), []string{lw(x, 1, b, 0, ""), lw(x, 1, b, 2, r1), sw(x, 2, r1), lw(x, 2, r1, 0, "")}, nil},
		{"write the store from 0, commit, advance", ss(x, 0, ""), ll(x, 0, "", 1, b), []string{sw(x, 1, b), lw(x, 1, b, 0, ""), lw(x, 1, b, 2, r1), sw(x, 2, r1), lw(x, 2, r1, 0, "")}, nil},

		{"ids differ", ss(y, 4, a), L{ID: x, Gen: 4, Nonce: a}, nil, ErrPairDifferent},
		{"ids differ, generations differ", ss(y, 3, a), L{ID: x, Gen: 4, Nonce: a}, nil, ErrPairDifferent},
		{"store id, ledger id NULL", ss(x, 0, ""), ll("", 0, "", 0, ""), nil, ErrPairDifferent},
		{"fresh store", ss("", 0, ""), L{ID: x, Gen: 3, Nonce: a}, nil, ErrPairDifferent},
		{"fresh store at ledger generation 1", ss("", 0, ""), L{ID: x, Gen: 1, Nonce: a}, nil, ErrPairDifferent},
		{"fresh ledger", ss(x, 3, a), ll("", 0, "", 0, ""), nil, ErrPairDifferent},
		{"finish with a pending value", ss("", 0, ""), ll(x, 0, "", 1, b), nil, ErrPairDifferent},
		{"finish with a store generation", ss("", 1, a), L{ID: x}, nil, ErrPairDifferent},
		{"finish with a store nonce", ss("", 0, a), L{ID: x}, nil, ErrPairDifferent},
		{"finish with a ledger nonce", ss("", 0, ""), L{ID: x, Nonce: a}, nil, ErrPairDifferent},
		{"first stamp with a nonce", ss("", 0, a), L{Nonce: a}, nil, ErrPairDifferent},
		{"both ids NULL, generations 1", ss("", 1, a), L{Gen: 1, Nonce: a}, nil, ErrPairDifferent},
		{"both ids NULL, pending", ss("", 0, ""), L{Pending: 1, PendingNonce: b}, nil, ErrPairDifferent},
		{"both ids NULL, pending G+1, store G+1", ss("", 1, b), L{Pending: 1, PendingNonce: b}, nil, ErrPairDifferent},
		{"generations 2 apart, store older", ss(x, 2, a), L{ID: x, Gen: 4, Nonce: b}, nil, ErrPairOlderStore},
		{"ledger ahead, P NULL", ss(x, 3, a), L{ID: x, Gen: 4, Nonce: b}, nil, ErrPairOlderStore},
		{"ledger ahead, P set", ss(x, 3, a), ll(x, 4, b, 5, c), nil, ErrPairOlderStore},
		{"generations 2 apart, ledger older", ss(x, 6, a), L{ID: x, Gen: 4, Nonce: b}, nil, ErrPairOlderLedger},
		{"store ahead, P NULL", ss(x, 5, a), L{ID: x, Gen: 4, Nonce: b}, nil, ErrPairOlderLedger},
		{"store ahead, P set lower", ss(x, 6, a), ll(x, 4, b, 5, c), nil, ErrPairOlderLedger},
		{"store and P 2 ahead", ss(x, 6, c), ll(x, 4, b, 6, c), nil, ErrPairOlderLedger},
		{"P set but not G+1", ss(x, 4, a), ll(x, 4, a, 7, b), nil, ErrPairDifferent},
		{"P equal to G", ss(x, 4, a), ll(x, 4, a, 4, b), nil, ErrPairDifferent},

		// The nonce: two timelines of one pair reach the same generations with other nonces.
		{"equal generations, nonces differ", ss(x, 4, b), L{ID: x, Gen: 4, Nonce: a}, nil, ErrPairDifferent},
		{"store written, another nonce", ss(x, 5, c), ll(x, 4, a, 5, b), nil, ErrPairOlderLedger},
		{"ledger marked, store of another nonce", ss(x, 4, c), ll(x, 4, a, 5, b), nil, ErrPairDifferent},
		{"store nonce empty above 0", ss(x, 4, ""), L{ID: x, Gen: 4, Nonce: a}, nil, ErrPairDifferent},
		{"ledger nonce empty above 0", ss(x, 4, a), L{ID: x, Gen: 4}, nil, ErrPairDifferent},
		{"both nonces empty above 0", ss(x, 4, ""), L{ID: x, Gen: 4}, nil, ErrPairDifferent},
		{"both nonces set at 0", ss(x, 0, a), L{ID: x, Nonce: a}, nil, ErrPairDifferent},
		{"store written, store nonce empty", ss(x, 5, ""), ll(x, 4, a, 5, ""), nil, ErrPairOlderLedger},
		{"store written, ledger nonce empty above 0", ss(x, 5, b), ll(x, 4, "", 5, b), nil, ErrPairOlderLedger},
		{"ledger marked, nonces empty above 0", ss(x, 4, ""), ll(x, 4, "", 5, b), nil, ErrPairDifferent},
		{"pending without its nonce", ss(x, 4, a), ll(x, 4, a, 5, ""), nil, ErrPairDifferent},
		{"pending nonce without pending", ss(x, 4, a), ll(x, 4, a, 0, b), nil, ErrPairDifferent},
	} {
		m := &memPair{s: tc.s, l: tc.l}
		err := checkPair(context.Background(), memStore{m}, memLedger{m}, randReader())
		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
			continue
		}
		if !slices.Equal(m.writes, tc.writes) {
			t.Errorf("%s: writes %q, want %q", tc.name, m.writes, tc.writes)
		}
		if tc.want == nil && !completed(m) {
			t.Errorf("%s: ends at S %+v, L %+v", tc.name, m.s, m.l)
		}
	}
}

// completed reports whether m is the state a completed check leaves: equal ids, generations of at least 1 and
// nonces, nothing pending.
func completed(m *memPair) bool {
	return m.s.ID != "" && m.s.ID == m.l.ID && m.s.Gen >= 1 && m.s.Gen == m.l.Gen && m.s.Nonce != "" &&
		m.s.Nonce == m.l.Nonce && m.l.Pending == 0 && m.l.PendingNonce == ""
}

// The whole input space for small values: ids {"", a, b}, generations 0..3, nonces {"", x, y}, pending 0..4 with
// pending nonces {"", x, y}. An oracle written from the five rows of the pair check's table (not from classifyPair)
// says which inputs are accepted; classifyPair must accept exactly those, each finished by checkPair to a completed
// state, and refuse every other with one of the three refusals: ErrPairDifferent when the ids differ (or are empty),
// otherwise by generation.
func TestPairClassifyExhaustive(t *testing.T) {
	ids := []string{"", strings.Repeat("a1", 16), strings.Repeat("b1", 16)}
	nonces := []string{"", strings.Repeat("e1", 16), strings.Repeat("f1", 16)}
	shape := func(gen int64, n string) bool { return (gen == 0) == (n == "") }
	oracle := func(s store.PairStamp, l ledger.PairStamp) pairState {
		noPending := l.Pending == 0 && l.PendingNonce == ""
		fresh := s == store.PairStamp{}
		same := s.ID != "" && s.ID == l.ID
		switch {
		case fresh && l == ledger.PairStamp{}:
			return pairFirstStamp
		case fresh && l.ID != "" && l.Gen == 0 && l.Nonce == "" && noPending:
			return pairFinishFirstStamp
		case same && s.Gen == l.Gen && s.Nonce == l.Nonce && shape(s.Gen, s.Nonce) && noPending:
			return pairEqual
		case same && s.Gen == l.Gen && s.Nonce == l.Nonce && shape(s.Gen, s.Nonce) && l.Pending == l.Gen+1 && l.PendingNonce != "":
			return pairLedgerMarked
		case same && shape(l.Gen, l.Nonce) && l.Pending == l.Gen+1 && l.PendingNonce != "" && s.Gen == l.Pending && s.Nonce == l.PendingNonce:
			return pairStoreWritten
		}
		return pairRefused
	}
	var accepted, refused int
	for _, sid := range ids {
		for sg := int64(0); sg <= 3; sg++ {
			for _, sn := range nonces {
				for _, lid := range ids {
					for lg := int64(0); lg <= 3; lg++ {
						for _, ln := range nonces {
							for lp := int64(0); lp <= 4; lp++ {
								for _, lpn := range nonces {
									s := store.PairStamp{ID: sid, Gen: sg, Nonce: sn}
									l := ledger.PairStamp{ID: lid, Gen: lg, Nonce: ln, Pending: lp, PendingNonce: lpn}
									want := oracle(s, l)
									got, err := classifyPair(s, l)
									if got != want {
										t.Fatalf("S %+v L %+v: state %d, want %d (%v)", s, l, got, want, err)
									}
									if want != pairRefused {
										accepted++
										m := &memPair{s: s, l: l}
										if err != nil || checkPair(context.Background(), memStore{m}, memLedger{m}, randReader()) != nil || !completed(m) {
											t.Fatalf("S %+v L %+v: accepted with %v, ends at S %+v L %+v", s, l, err, m.s, m.l)
										}
										continue
									}
									refused++
									wantErr := ErrPairDifferent
									switch {
									case lid == "" || lid != sid:
									case sg < lg:
										wantErr = ErrPairOlderStore
									case sg > lg:
										wantErr = ErrPairOlderLedger
									}
									if err != wantErr {
										t.Fatalf("S %+v L %+v: refused with %v, want %v", s, l, err, wantErr)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d inputs accepted, %d refused", accepted, refused)
}

// The refusals are three fixed messages that name no path, id or generation.
func TestPairRefusalText(t *testing.T) {
	for _, e := range []error{ErrPairOlderStore, ErrPairOlderLedger, ErrPairDifferent} {
		if strings.ContainsAny(e.Error(), "/0123456789") {
			t.Errorf("%q names a path or a number", e)
		}
	}
	if a, b, c := ErrPairOlderStore.Error(), ErrPairOlderLedger.Error(), ErrPairDifferent.Error(); a == b || a == c || b == c {
		t.Error("two refusals read the same")
	}
}

// A read that fails, or a random source that runs dry, writes nothing and is not one of the refusals: the random
// values a start needs are drawn before its first write.
func TestPairReadAndRandFailures(t *testing.T) {
	m := &memPair{readFail: true}
	if err := checkPair(context.Background(), memStore{m}, memLedger{m}, randReader()); err == nil || isPairRefusal(err) || len(m.writes) != 0 {
		t.Fatalf("read failure: %v, writes %q", err, m.writes)
	}
	x, a, b := strings.Repeat("ab", 16), strings.Repeat("a0", 16), strings.Repeat("b0", 16)
	for _, c := range []struct {
		name string
		in   memPair
		have int // bytes the random source holds: one short of what the start needs
	}{
		{"first stamp", memPair{}, 31},
		{"finish the first stamp", memPair{l: ledger.PairStamp{ID: x}}, 15},
		{"advance", memPair{s: store.PairStamp{ID: x, Gen: 4, Nonce: a}, l: ledger.PairStamp{ID: x, Gen: 4, Nonce: a}}, 15},
		{"recover a marked ledger", memPair{s: store.PairStamp{ID: x, Gen: 4, Nonce: a}, l: ledger.PairStamp{ID: x, Gen: 4, Nonce: a, Pending: 5, PendingNonce: b}}, 15},
		{"recover a written store", memPair{s: store.PairStamp{ID: x, Gen: 5, Nonce: b}, l: ledger.PairStamp{ID: x, Gen: 4, Nonce: a, Pending: 5, PendingNonce: b}}, 15},
	} {
		m := c.in
		if err := checkPair(context.Background(), memStore{&m}, memLedger{&m}, bytes.NewReader(make([]byte, c.have))); err == nil || isPairRefusal(err) || len(m.writes) != 0 {
			t.Fatalf("%s, short random source: %v, writes %q", c.name, err, m.writes)
		}
	}
}

func isPairRefusal(err error) bool {
	return errors.Is(err, ErrPairOlderStore) || errors.Is(err, ErrPairOlderLedger) || errors.Is(err, ErrPairDifferent)
}

// A crash right after each write of a start (the first stamp's two, the advance's three, and the five or four of a
// start that finishes an interrupted advance and advances once more) leaves a state the next start finishes: equal ids,
// equal generations of at least 1 and equal nonces, no pending value. Every start that finishes an interrupted advance
// advances once more, and a crash after the last write of an advance leaves a completed advance, so the next start
// advances again. At every one of those states
// a file from another timeline of the same pair (same id, the generation in play, another nonce), in place of the
// store or of the ledger, is refused.
func TestPairCrashAtEachWrite(t *testing.T) {
	x, a, b := strings.Repeat("ab", 16), strings.Repeat("a0", 16), strings.Repeat("b0", 16)
	z, z2 := strings.Repeat("0e", 16), strings.Repeat("0f", 16) // the other timeline's nonces
	for _, c := range []struct {
		name     string
		start    memPair
		wantGens []int64 // the generation the next start ends at, after a crash following write 1, 2, ...
	}{
		{"first stamp", memPair{}, []int64{1, 1, 2, 2, 2}},
		{"finish the first stamp", memPair{l: ledger.PairStamp{ID: x}}, []int64{1, 2, 2, 2}},
		{"advance", memPair{s: store.PairStamp{ID: x, Gen: 7, Nonce: a}, l: ledger.PairStamp{ID: x, Gen: 7, Nonce: a}}, []int64{9, 9, 9}},
		// A recovering start finishes the recorded advance, then advances once more: the five and the four writes.
		{"recover a marked ledger", memPair{s: store.PairStamp{ID: x, Gen: 7, Nonce: a}, l: ledger.PairStamp{ID: x, Gen: 7, Nonce: a, Pending: 8, PendingNonce: b}}, []int64{9, 9, 10, 10, 10}},
		{"recover a written store", memPair{s: store.PairStamp{ID: x, Gen: 8, Nonce: b}, l: ledger.PairStamp{ID: x, Gen: 7, Nonce: a, Pending: 8, PendingNonce: b}}, []int64{9, 10, 10, 10}},
	} {
		for k := 1; k <= len(c.wantGens); k++ {
			m := c.start
			m.failAt = k
			if err := checkPair(context.Background(), memStore{&m}, memLedger{&m}, randReader()); !errors.Is(err, errCrash) {
				t.Fatalf("%s, crash after write %d: %v", c.name, k, err)
			}
			if len(m.writes) != k {
				t.Fatalf("%s: went on after the crash at write %d: %q", c.name, k, m.writes)
			}
			m.failAt = 0
			m.writes, m.nWrites = nil, 0
			if m.l.ID != "" {
				refuseOtherTimeline(t, fmt.Sprintf("%s, crash after write %d", c.name, k), m, z, z2)
			}
			if err := checkPair(context.Background(), memStore{&m}, memLedger{&m}, randReader()); err != nil {
				t.Fatalf("%s, restart after write %d: %v (S %+v, L %+v)", c.name, k, err, m.s, m.l)
			}
			want := c.wantGens[k-1]
			if !completed(&m) || m.s.Gen != want {
				t.Fatalf("%s, restart after write %d: S %+v, L %+v, want generation %d", c.name, k, m.s, m.l, want)
			}
		}
	}
}

// A start that finds an interrupted advance finishes it with the recorded nonce, then advances once more with a fresh
// one: both generations end at pending + 1, on a nonce that is not the recorded pending nonce, in the writes of the
// finish followed by the three of an advance.
func TestPairRecoveryAdvances(t *testing.T) {
	x, a, b := strings.Repeat("ab", 16), strings.Repeat("a0", 16), strings.Repeat("b0", 16)
	marked := ledger.PairStamp{ID: x, Gen: 7, Nonce: a, Pending: 8, PendingNonce: b}
	for _, c := range []struct {
		name   string
		s      store.PairStamp
		writes []string
	}{
		{"ledger marked", store.PairStamp{ID: x, Gen: 7, Nonce: a}, []string{sw(x, 8, b), lw(x, 8, b, 0, ""),
			lw(x, 8, b, 9, pairRand), sw(x, 9, pairRand), lw(x, 9, pairRand, 0, "")}},
		{"store written", store.PairStamp{ID: x, Gen: 8, Nonce: b}, []string{lw(x, 8, b, 0, ""),
			lw(x, 8, b, 9, pairRand), sw(x, 9, pairRand), lw(x, 9, pairRand, 0, "")}},
	} {
		m := &memPair{s: c.s, l: marked}
		if err := checkPair(context.Background(), memStore{m}, memLedger{m}, randReader()); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !completed(m) || m.s.Gen != 9 || m.s.Nonce == b || m.s.Nonce != pairRand {
			t.Errorf("%s: ends at S %+v, L %+v, want generation 9 and a fresh nonce", c.name, m.s, m.l)
		}
		if !slices.Equal(m.writes, c.writes) {
			t.Errorf("%s: writes %q, want %q", c.name, m.writes, c.writes)
		}
	}
}

// Two copies of one crashed data directory, each started with its own random source, end on different nonces: the
// store of one copy with the ledger of the other is refused. Both crash states are covered.
func TestPairCrashCopiesDiverge(t *testing.T) {
	x, a := strings.Repeat("ab", 16), strings.Repeat("a0", 16)
	for _, crashAfter := range []int{1, 2} { // after the ledger's mark, after the store's write
		crashed := memPair{s: store.PairStamp{ID: x, Gen: 7, Nonce: a}, l: ledger.PairStamp{ID: x, Gen: 7, Nonce: a}, failAt: crashAfter}
		if err := checkPair(context.Background(), memStore{&crashed}, memLedger{&crashed}, randReader()); !errors.Is(err, errCrash) {
			t.Fatalf("crash after write %d: %v", crashAfter, err)
		}
		crashed.failAt, crashed.writes, crashed.nWrites = 0, nil, 0
		copyA, copyB := crashed, crashed
		for i, c := range []*memPair{&copyA, &copyB} {
			src := bytes.NewReader(bytes.Repeat([]byte{byte(0x30 + i)}, 64))
			if err := checkPair(context.Background(), memStore{c}, memLedger{c}, src); err != nil || !completed(c) {
				t.Fatalf("crash %d, copy %d: %v (S %+v, L %+v)", crashAfter, i, err, c.s, c.l)
			}
		}
		if copyA.s.Nonce == copyB.s.Nonce {
			t.Fatalf("crash %d: both copies ended on the nonce %s", crashAfter, copyA.s.Nonce)
		}
		for _, mix := range []memPair{{s: copyA.s, l: copyB.l}, {s: copyB.s, l: copyA.l}} {
			mm := mix
			err := checkPair(context.Background(), memStore{&mm}, memLedger{&mm}, randReader())
			if !errors.Is(err, ErrPairDifferent) || len(mm.writes) != 0 {
				t.Fatalf("crash %d: S %+v with L %+v: %v, writes %q, want %v", crashAfter, mix.s, mix.l, err, mm.writes, ErrPairDifferent)
			}
		}
	}
}

// refuseOtherTimeline puts in place of m's store, then of its ledger, files of another timeline of the same pair: the
// same id, the generations in play (the stamp's own and the pending one), the other timeline's nonces z and z2. Each
// mix must be refused with one of the three refusals and no write.
func refuseOtherTimeline(t *testing.T, name string, m memPair, z, z2 string) {
	t.Helper()
	id := m.l.ID
	gens := []int64{m.s.Gen, m.l.Gen}
	if m.l.Pending != 0 {
		gens = append(gens, m.l.Pending)
	}
	var mixes []memPair
	for _, g := range gens {
		if g == 0 {
			continue // generation 0 has no nonce: no other timeline reaches it (a backup is never at 0)
		}
		mixes = append(mixes,
			memPair{s: store.PairStamp{ID: id, Gen: g, Nonce: z}, l: m.l},
			memPair{s: m.s, l: ledger.PairStamp{ID: id, Gen: g, Nonce: z}},
			memPair{s: m.s, l: ledger.PairStamp{ID: id, Gen: g, Nonce: z, Pending: g + 1, PendingNonce: z2}},
		)
	}
	for _, mix := range mixes {
		mm := mix
		err := checkPair(context.Background(), memStore{&mm}, memLedger{&mm}, randReader())
		if !isPairRefusal(err) || len(mm.writes) != 0 {
			t.Fatalf("%s: S %+v with L %+v from two timelines: %v, writes %q", name, mix.s, mix.l, err, mm.writes)
		}
	}
}

// The real store and ledger: a fresh pair (the state a migrated pre-plan-10 pair is in, store and ledger tests show)
// is stamped and advanced by one checkPair, and advanced again, with a new nonce, by the next.
func TestPairOnRealDatabases(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	lg, err := ledger.Open(ctx, filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	st, err := store.Open(ctx, filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, w := range []struct {
		gen   int64
		nonce string
	}{{1, pairRand2}, {2, pairRand}} {
		if err := checkPair(ctx, st, lg, randReader()); err != nil {
			t.Fatal(err)
		}
		s, err1 := st.Pair(ctx)
		l, err2 := lg.Pair(ctx)
		if err1 != nil || err2 != nil || s != (store.PairStamp{ID: pairRand, Gen: w.gen, Nonce: w.nonce}) ||
			l != (ledger.PairStamp{ID: pairRand, Gen: w.gen, Nonce: w.nonce}) {
			t.Fatalf("S %+v, L %+v (%v %v), want generation %d nonce %s", s, l, err1, err2, w.gen, w.nonce)
		}
	}
}

// countingWallet counts get_info calls: Run makes the first one after the pair check, so 0 means it stopped before
// any hub check, recovery, listener or worker.
type countingWallet struct {
	*fakeWallet
	mu    sync.Mutex
	infos int
}

func (w *countingWallet) GetInfo(ctx context.Context) (*nwc.Info, error) {
	w.mu.Lock()
	w.infos++
	w.mu.Unlock()
	return w.fakeWallet.GetInfo(ctx)
}

// runAgain starts Run on cfg and returns its error when it fails, or stops it once it has started. It also returns
// the logs and the number of get_info calls.
func runAgain(t *testing.T, cfg *Config, extra ...Option) (error, string, int) {
	t.Helper()
	logs := &syncBuf{}
	w := &countingWallet{fakeWallet: newFakeWallet()}
	w.network = cfg.NWC.Network // the hub is on the network the configuration names
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pub, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	prv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, append([]Option{WithWallet(w), WithListeners(pub, prv), WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))}, extra...)...)
	}()
	infos := func() int { w.mu.Lock(); defer w.mu.Unlock(); return w.infos }
	for {
		select {
		case err := <-done:
			pub.Close()
			prv.Close()
			return err, logs.String(), infos()
		case <-time.After(20 * time.Millisecond):
			if strings.Contains(logs.String(), logStarted) {
				cancel()
				return <-done, logs.String(), infos()
			}
		}
	}
}

// pairOf reads both stamps of a stopped service's data directory.
func pairOf(t *testing.T, dataDir string) (store.PairStamp, ledger.PairStamp) {
	t.Helper()
	ctx := context.Background()
	l, err := ledger.Open(ctx, filepath.Join(dataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s, err := store.Open(ctx, filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sp, err := s.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lp, err := l.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return sp, lp
}

// checkCompletedStart: after a completed start both stamps are equal, at generation want (at least 1), with one nonce.
func checkCompletedStart(t *testing.T, dataDir string, want int64) {
	t.Helper()
	s, l := pairOf(t, dataDir)
	if want < 1 || !completed(&memPair{s: s, l: l}) || s.Gen != want {
		t.Fatalf("after a completed start: S %+v, L %+v, want generation %d", s, l, want)
	}
}

// removeDB removes name and its -wal and -shm files, as docs/backup-and-restore.md's restore says.
func removeDB(t *testing.T, dataDir, name string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dataDir, name+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// wantRefused checks a refused start: the reason "pair", the fixed message, and nothing started.
func wantRefused(t *testing.T, err error, logs string, infos int, want error) {
	t.Helper()
	if err == nil || err.Error() != "service: "+want.Error() || reasonOf(t, logs) != "pair" {
		t.Fatalf("err %v, reason %q, want %q\n%s", err, reasonOf(t, logs), want, logs)
	}
	if infos != 0 || strings.Contains(logs, logStarted) {
		t.Fatalf("the refused start went on: %d get_info calls\n%s", infos, logs)
	}
}

// An older store.db copied over a live one (the -wal and -shm files removed, the .db copied, as
// docs/backup-and-restore.md's
// restore does it for both files) is refused: the store is older.
func TestRunRefusesMismatch(t *testing.T) {
	r := startRig(t)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	checkCompletedStart(t, dataDir, 1)
	old := filepath.Join(t.TempDir(), "store.db")
	copyFile(t, filepath.Join(dataDir, "store.db"), old)
	if err, logs, _ := runAgain(t, r.cfg); err != nil {
		t.Fatalf("second start: %v\n%s", err, logs)
	}
	checkCompletedStart(t, dataDir, 2)
	removeDB(t, dataDir, "store.db")
	copyFile(t, old, filepath.Join(dataDir, "store.db"))
	err, logs, infos := runAgain(t, r.cfg)
	wantRefused(t, err, logs, infos, ErrPairOlderStore)
}

// A lost store.db comes back as a fresh file (store.Open creates it); that is not a pair, never a first stamp.
func TestRunRefusesFreshStore(t *testing.T) {
	r := startRig(t)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	checkCompletedStart(t, r.cfg.DataDir, 1)
	removeDB(t, r.cfg.DataDir, "store.db")
	err, logs, infos := runAgain(t, r.cfg)
	wantRefused(t, err, logs, infos, ErrPairDifferent)
}

// Likewise a lost ledger.db.
func TestRunRefusesFreshLedger(t *testing.T) {
	r := startRig(t)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	checkCompletedStart(t, r.cfg.DataDir, 1)
	removeDB(t, r.cfg.DataDir, "ledger.db")
	err, logs, infos := runAgain(t, r.cfg)
	wantRefused(t, err, logs, infos, ErrPairDifferent)
}

// An upgrade: a pre-plan-10 pair holding a paid order (built by taking a running pair back to store version 6 and
// ledger version 3, with raw SQL on the sqlite3 driver the store and ledger register) is migrated, stamped and
// advanced by the next Run, not refused, and its rows survive.
func TestRunUpgradesAPrePlan10Pair(t *testing.T) {
	r := startRig(t)
	o := r.createOrder("upgrade-1")
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	if hashes := r.wallet.settleAll(); len(hashes) != 1 {
		t.Fatalf("want one invoice, got %d", len(hashes))
	}
	eventually(t, "the order to be paid", func() bool { return r.getOrder(o.ID).State == "paid" })
	// The ledger transfer comes before the order.paid event: wait for the event's delivery, so that the store holds an
	// event and its successful attempt for main when the service stops.
	eventually(t, "the order.paid delivery", func() bool {
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(r.cfg.DataDir, "store.db"))
		if err != nil {
			return false
		}
		defer db.Close()
		var n int
		return db.QueryRow(`SELECT count(*) FROM webhook_attempts WHERE endpoint = 'main' AND ok = 1`).Scan(&n) == nil && n > 0
	})
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir
	sp, _ := pairOf(t, dataDir)
	rawExec := func(name string, stmts ...string) {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range stmts {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %s: %v", name, q, err)
			}
		}
	}
	rawCount := func(name, q string) int {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	transfers := rawCount("ledger.db", `SELECT count(*) FROM transfers`)
	if transfers == 0 {
		t.Fatal("the paid order left no transfer in the ledger")
	}
	attemptRows := func() string {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT endpoint, event_id, seq, at, status, ok FROM webhook_attempts ORDER BY endpoint, event_id, seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var ep, ev string
			var seq, at, status, ok int64
			if err := rows.Scan(&ep, &ev, &seq, &at, &status, &ok); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s|%s|%d|%d|%d|%d", ep, ev, seq, at, status, ok))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "\n")
	}
	attemptsBefore := attemptRows()
	if attemptsBefore == "" || !strings.HasPrefix(attemptsBefore, "main|") {
		t.Fatalf("attempts before the forge: %q", attemptsBefore)
	}
	// Migrations 11 (the abandoned table and the invoice index dropped), 10 (the network table dropped) and 9 undone. The second is a copy of the store's test helper
	// dropMigrationNine (store/schema_test.go) and must change with it: the endpoints table dropped, and the attempts
	// table rebuilt as migration 1 made it, text for text (every CHECK, the REFERENCES clause, the three guards, the
	// known-event trigger and the old partial index), keeping main's rows. Without this, the next start would run
	// migration 9 on a migrated table.
	rawExec("store.db", `DROP TABLE webhook_abandoned`, `DROP INDEX invoices_created`, `DROP TABLE store_network`, `DROP TABLE webhook_endpoints`,
		`ALTER TABLE webhook_attempts RENAME TO webhook_attempts_v9`,
		`CREATE TABLE webhook_attempts (
	event_id TEXT NOT NULL REFERENCES events (id) CHECK (typeof(event_id) = 'text' AND length(event_id) = length(CAST(event_id AS BLOB)) AND length(event_id) = 32 AND event_id NOT GLOB '*[^0-9a-f]*'),
	seq INTEGER NOT NULL CHECK (seq >= 1),
	at INTEGER NOT NULL CHECK (at > 0),
	status INTEGER NOT NULL CHECK (status BETWEEN 0 AND 999),
	ok INTEGER NOT NULL CHECK (ok IN (0, 1)),
	PRIMARY KEY (event_id, seq)
) STRICT, WITHOUT ROWID`,
		`INSERT INTO webhook_attempts (event_id, seq, at, status, ok) SELECT event_id, seq, at, status, ok FROM webhook_attempts_v9 WHERE endpoint = 'main'`,
		`DROP TABLE webhook_attempts_v9`,
		`CREATE INDEX webhook_attempts_ok ON webhook_attempts (event_id) WHERE ok = 1`,
		`CREATE TRIGGER webhook_attempts_no_update BEFORE UPDATE ON webhook_attempts
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END`,
		`CREATE TRIGGER webhook_attempts_no_delete BEFORE DELETE ON webhook_attempts
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END`,
		`CREATE TRIGGER webhook_attempts_no_replace BEFORE INSERT ON webhook_attempts
WHEN EXISTS (SELECT 1 FROM webhook_attempts WHERE event_id = NEW.event_id AND seq = NEW.seq)
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END`,
		`CREATE TRIGGER webhook_attempts_known_event BEFORE INSERT ON webhook_attempts
WHEN NOT EXISTS (SELECT 1 FROM events WHERE id = NEW.event_id)
BEGIN SELECT RAISE(ABORT, 'unknown event'); END`)
	rawExec("store.db", `DROP TABLE alert_attempts`, `DROP TABLE alert_hits`, `DROP TABLE alerts`, `DROP TABLE pair`,
		`UPDATE store_schema_version SET version = 6`)
	rawExec("ledger.db", `DROP TABLE pair`, `UPDATE schema_version SET version = 3`)
	if err, logs, _ := runAgain(t, r.cfg); err != nil {
		t.Fatalf("the upgrade start: %v\n%s", err, logs)
	}
	checkCompletedStart(t, dataDir, 1)
	if after := attemptRows(); after != attemptsBefore {
		t.Fatalf("the attempts changed in the upgrade:\n%s\nwant\n%s", after, attemptsBefore)
	}
	if np, _ := pairOf(t, dataDir); np.ID == sp.ID {
		t.Fatal("the upgrade kept the old id: the pair table was not rebuilt")
	}
	if n := rawCount("ledger.db", `SELECT count(*) FROM transfers`); n != transfers {
		t.Fatalf("ledger transfers %d after the upgrade, want %d", n, transfers)
	}
	st, err := store.Open(context.Background(), filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, err := st.OrderByID(context.Background(), o.ID); err != nil || got.MerchantRef != "upgrade-1" {
		t.Fatalf("the order did not survive the upgrade: %+v %v", got, err)
	}
}

// After a restore the generation rewinds, so a backup made on the timeline that was given up has the same id and a
// generation the restored pair reaches again. Each advance writes a fresh nonce into both stamps, so a store from one
// timeline and a ledger from the other are refused as not a pair.
func TestPairForkAfterRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	open := func() (*store.Store, *ledger.Ledger) {
		lg, err := ledger.Open(ctx, filepath.Join(dir, "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(ctx, filepath.Join(dir, "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		return st, lg
	}
	start := func() {
		st, lg := open()
		defer lg.Close()
		defer st.Close()
		if err := checkPair(ctx, st, lg, rand.Reader); err != nil {
			t.Fatalf("checkPair: %v", err)
		}
	}
	backup := func(name string) string {
		to := filepath.Join(t.TempDir(), name)
		if _, err := Backup(ctx, &Config{DataDir: dir}, to, nil); err != nil {
			t.Fatalf("backup %s: %v", name, err)
		}
		return to
	}
	restore := func(from string) {
		for _, n := range []string{"store.db", "ledger.db"} {
			removeDB(t, dir, n)
			copyFile(t, filepath.Join(from, n), filepath.Join(dir, n))
		}
	}
	start() // generation 1
	a := backup("a")
	start() // generation 2, timeline 1
	b := backup("b")
	restore(a) // back to generation 1
	start()    // generation 2 again, timeline 2
	// Only store.db of the newer backup b is copied over the restored pair.
	removeDB(t, dir, "store.db")
	copyFile(t, filepath.Join(b, "store.db"), filepath.Join(dir, "store.db"))
	st, lg := open()
	defer lg.Close()
	defer st.Close()
	if err := checkPair(ctx, st, lg, rand.Reader); !errors.Is(err, ErrPairDifferent) {
		t.Fatalf("a store and a ledger from two timelines: %v, want %v", err, ErrPairDifferent)
	}
}
