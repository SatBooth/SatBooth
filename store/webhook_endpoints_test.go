package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hexOf(d byte) string { return strings.Repeat(string(d), 32) }

// mkEvents adds n order.paid events, the i-th at t0+(i+1) minutes, and returns the event ids.
func mkEvents(t *testing.T, s *Store, clk *testClock, n int) []string {
	t.Helper()
	mins := make([]int, n)
	for i := range mins {
		mins[i] = i + 1
	}
	return mkEventsAt(t, s, clk, mins)
}

// mkEventsAt adds one order.paid event per entry of mins, created at t0 plus that many minutes (equal entries share
// a second), and returns the event ids.
func mkEventsAt(t *testing.T, s *Store, clk *testClock, mins []int) []string {
	t.Helper()
	var ids []string
	for i, m := range mins {
		o := mkOrder(t, s, fmt.Sprintf("ev%d", i), 1000, t0.Add(48*time.Hour))
		clk.set(t0.Add(time.Duration(m) * time.Minute))
		if ok, err := s.AddEvent(context.Background(), Event{Type: EventPaid, OrderID: o.ID}); err != nil || !ok {
			t.Fatalf("AddEvent: %v %v", ok, err)
		}
		ids = append(ids, EventID(EventPaid, o.ID, 1))
	}
	return ids
}

func pendingIDs(t *testing.T, s *Store, endpoint string, since time.Time) []PendingEvent {
	t.Helper()
	return pendingAdmit(t, s, endpoint, since, time.Time{})
}

func pendingAdmit(t *testing.T, s *Store, endpoint string, since, admit time.Time) []PendingEvent {
	t.Helper()
	p, err := s.UndeliveredEvents(context.Background(), endpoint, since, admit)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Migration 9 keeps the attempts of a version-8 database as the attempts of main, and gives main the first-pass time of
// its oldest event.
func TestMigrationNineMovesAttemptsToMain(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e1, e2, e3 := hexOf('1'), hexOf('2'), hexOf('3')
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:8]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (8)`)
	for i, d := range []string{"a", "b", "c"} {
		o := strings.Repeat(d, 32)
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO orders VALUES ('%s', '%s', 'a', 'r%d', 1000, 0, 0, 5, 6)`, o, strings.Repeat(string(rune('d'+i)), 32), i),
			fmt.Sprintf(`INSERT INTO events VALUES ('%s', 'order.paid', '%s', 1, %d)`, []string{e1, e2, e3}[i], o, 1000+i*1000))
	}
	stmts = append(stmts,
		fmt.Sprintf(`INSERT INTO webhook_attempts VALUES ('%s', 1, 1100, 200, 1)`, e1),
		fmt.Sprintf(`INSERT INTO webhook_attempts VALUES ('%s', 1, 2100, 500, 0)`, e2),
		fmt.Sprintf(`INSERT INTO webhook_attempts VALUES ('%s', 2, 2200, 0, 0)`, e2))
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(ctx, path, WithClock(func() time.Time { return t0 }))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	wantRows := []string{fmt.Sprintf("main|%s|1|1100|200|1", e1), fmt.Sprintf("main|%s|1|2100|500|0", e2), fmt.Sprintf("main|%s|2|2200|0|0", e2)}
	if got := rawAttemptRows(t, s.db, `SELECT endpoint, event_id, seq, at, status, ok FROM webhook_attempts`); strings.Join(got, ";") != strings.Join(wantRows, ";") {
		t.Fatalf("migrated rows %v, want %v", got, wantRows)
	}
	a, err := s.AttemptsFor(ctx, MainEndpoint, e2)
	want := []Attempt{{Endpoint: MainEndpoint, EventID: e2, At: fromTS(2100), Status: 500}, {Endpoint: MainEndpoint, EventID: e2, At: fromTS(2200)}}
	if err != nil || len(a) != 2 || a[0] != want[0] || a[1] != want[1] {
		t.Fatalf("attempts of e2: %+v %v", a, err)
	}
	p := pendingIDs(t, s, MainEndpoint, time.Unix(0, 0))
	if len(p) != 2 || p[0].ID != e2 || p[0].Failures != 2 || !p[0].LastAt.Equal(fromTS(2200)) ||
		p[1].ID != e3 || p[1].Failures != 0 || !p[1].LastAt.IsZero() {
		t.Fatalf("pending of main: %+v", p)
	}
	p = pendingIDs(t, s, "other", time.Unix(0, 0))
	if len(p) != 3 || p[0].ID != e1 || p[1].ID != e2 || p[2].ID != e3 || p[0].Failures != 0 || p[1].Failures != 0 || p[2].Failures != 0 || !p[1].LastAt.IsZero() {
		t.Fatalf("pending of other: %+v", p)
	}
	if got, err := s.RegisterEndpoint(ctx, MainEndpoint); err != nil || !got.Equal(fromTS(1000)) {
		t.Fatalf("first pass of main: %v %v", got, err)
	}
	if got, err := s.RegisterEndpoint(ctx, "other"); err != nil || !got.Equal(t0) {
		t.Fatalf("first pass of other: %v %v", got, err)
	}
	// The guards are in place on the rebuilt table.
	for _, q := range []string{`UPDATE webhook_attempts SET ok = 1`, `DELETE FROM webhook_attempts`} {
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("%q succeeded", q)
		}
	}
	if got := triggersOf(t, s.db, "webhook_attempts"); got != "webhook_attempts_known_event webhook_attempts_no_delete webhook_attempts_no_replace webhook_attempts_no_update" {
		t.Fatalf("triggers %q", got)
	}
	var idx string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'webhook_attempts_ok' AND tbl_name = 'webhook_attempts'`).Scan(&idx); err != nil ||
		idx != `CREATE INDEX webhook_attempts_ok ON webhook_attempts (endpoint, event_id) WHERE ok = 1` {
		t.Fatalf("index: %q %v", idx, err)
	}
	// The migrated schema is the schema of a fresh store.
	fresh, err := Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if schemaText(t, s.db) != schemaText(t, fresh.db) {
		t.Fatal("the migrated schema differs from a fresh store's")
	}
}

// With no events at migration 9, main has no row; its first call records the clock.
func TestMigrationNineWithoutEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:8]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (8)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(ctx, path, WithClock(func() time.Time { return t0.Add(time.Hour) }))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM webhook_endpoints`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows: %d %v", n, err)
	}
	if got, err := s.RegisterEndpoint(ctx, MainEndpoint); err != nil || !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("first pass: %v %v", got, err)
	}
}

// On a raw connection, the new tables refuse updates, deletes, repeats, orphans and bad names with their own guards.
func TestWebhookTablesOnRawConnection(t *testing.T) {
	ctx := context.Background()
	s, path, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 1)
	if err := s.AddAttempt(ctx, Attempt{Endpoint: MainEndpoint, EventID: ids[0], Status: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterEndpoint(ctx, MainEndpoint); err != nil {
		t.Fatal(err)
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	const immutableText = "store rows are immutable"
	const chk = "CHECK constraint failed: "
	ev := ids[0]
	cases := []struct{ name, q, want string }{
		{"attempt update", `UPDATE webhook_attempts SET ok = 1`, immutableText},
		{"attempt delete", `DELETE FROM webhook_attempts`, immutableText},
		{"attempt repeat", `INSERT INTO webhook_attempts SELECT * FROM webhook_attempts`, immutableText},
		{"attempt replace", `INSERT OR REPLACE INTO webhook_attempts SELECT * FROM webhook_attempts`, immutableText},
		{"attempt unknown event", `INSERT INTO webhook_attempts VALUES ('main', '` + hexOf('9') + `', 1, 5, 200, 1)`, "unknown event"},
		{"attempt no endpoint", `INSERT INTO webhook_attempts (event_id, seq, at, status, ok) VALUES ('` + ev + `', 9, 5, 200, 1)`, "NOT NULL constraint failed: webhook_attempts.endpoint"},
		{"attempt empty endpoint", `INSERT INTO webhook_attempts VALUES ('', '` + ev + `', 9, 5, 200, 1)`, chk + "typeof(endpoint) = 'text'"},
		{"attempt 65 endpoint", `INSERT INTO webhook_attempts VALUES ('` + strings.Repeat("a", 65) + `', '` + ev + `', 9, 5, 200, 1)`, chk + "typeof(endpoint) = 'text'"},
		{"attempt space endpoint", `INSERT INTO webhook_attempts VALUES ('a b', '` + ev + `', 9, 5, 200, 1)`, chk + "typeof(endpoint) = 'text'"},
		{"attempt slash endpoint", `INSERT INTO webhook_attempts VALUES ('a/b', '` + ev + `', 9, 5, 200, 1)`, chk + "typeof(endpoint) = 'text'"},
		{"endpoint update", `UPDATE webhook_endpoints SET first_pass = 9`, immutableText},
		{"endpoint delete", `DELETE FROM webhook_endpoints`, immutableText},
		{"endpoint repeat", `INSERT INTO webhook_endpoints VALUES ('main', 9)`, immutableText},
		{"endpoint replace", `INSERT OR REPLACE INTO webhook_endpoints VALUES ('main', 9)`, immutableText},
		{"endpoint no name", `INSERT INTO webhook_endpoints (first_pass) VALUES (9)`, "NOT NULL constraint failed"},
		{"endpoint empty", `INSERT INTO webhook_endpoints VALUES ('', 9)`, chk + "typeof(name) = 'text'"},
		{"endpoint 65", `INSERT INTO webhook_endpoints VALUES ('` + strings.Repeat("a", 65) + `', 9)`, chk + "typeof(name) = 'text'"},
		{"endpoint space", `INSERT INTO webhook_endpoints VALUES ('a b', 9)`, chk + "typeof(name) = 'text'"},
		{"endpoint slash", `INSERT INTO webhook_endpoints VALUES ('a/b', 9)`, chk + "typeof(name) = 'text'"},
		{"endpoint time 0", `INSERT INTO webhook_endpoints VALUES ('zero', 0)`, chk + "first_pass > 0"},
	}
	for _, c := range cases {
		if _, err := raw.Exec(c.q); err == nil {
			t.Errorf("%s succeeded", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	var n int
	if err := raw.QueryRow(`SELECT (SELECT count(*) FROM webhook_attempts) * 10 + (SELECT count(*) FROM webhook_endpoints)`).Scan(&n); err != nil || n != 11 {
		t.Fatalf("rows after the refusals: %d %v", n, err)
	}
	// Another endpoint's attempt with the same event and seq is not a repeat.
	if _, err := raw.Exec(`INSERT INTO webhook_attempts VALUES ('other', '` + ev + `', 1, 5, 200, 1)`); err != nil {
		t.Fatalf("same event and seq for another endpoint: %v", err)
	}
}

func TestEndpointsAttemptIndependently(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 2)
	add := func(ep, id string, ok bool, at time.Time) {
		t.Helper()
		if err := s.AddAttempt(ctx, Attempt{Endpoint: ep, EventID: id, At: at, Status: 500, OK: ok}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", ids[0], false, t0.Add(10*time.Minute))
	add("b", ids[0], false, t0.Add(11*time.Minute))
	add("a", ids[0], false, t0.Add(12*time.Minute))
	add("b", ids[0], true, t0.Add(13*time.Minute))
	// seq is per endpoint: a has 1, 2; b has 1, 2.
	for _, ep := range []string{"a", "b"} {
		var seqs string
		if err := s.db.QueryRow(`SELECT group_concat(seq) FROM (SELECT seq FROM webhook_attempts WHERE endpoint = ? ORDER BY seq)`, ep).Scan(&seqs); err != nil || seqs != "1,2" {
			t.Fatalf("seqs of %s: %q %v", ep, seqs, err)
		}
	}
	if got, _ := s.AttemptsFor(ctx, "a", ids[0]); len(got) != 2 || got[0].OK || got[1].OK || got[0].Endpoint != "a" {
		t.Fatalf("attempts of a: %+v", got)
	}
	if got, _ := s.AttemptsFor(ctx, "b", ids[0]); len(got) != 2 || got[0].OK || !got[1].OK {
		t.Fatalf("attempts of b: %+v", got)
	}
	if p := pendingIDs(t, s, "a", t0); len(p) != 2 || p[0].ID != ids[0] || p[0].Failures != 2 {
		t.Fatalf("pending of a: %+v", p)
	}
	if p := pendingIDs(t, s, "b", t0); len(p) != 1 || p[0].ID != ids[1] {
		t.Fatalf("pending of b: %+v", p)
	}
	// since bounds each endpoint's list.
	since := t0.Add(2 * time.Minute)
	if p := pendingIDs(t, s, "a", since); len(p) != 1 || p[0].ID != ids[1] {
		t.Fatalf("pending of a since: %+v", p)
	}
	if p := pendingIDs(t, s, "b", since); len(p) != 1 || p[0].ID != ids[1] {
		t.Fatalf("pending of b since: %+v", p)
	}
	if p := pendingIDs(t, s, "c", since.Add(time.Second)); len(p) != 0 {
		t.Fatalf("pending of c past both: %+v", p)
	}
}

func TestRegisterEndpoint(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	clk.set(t0.Add(time.Minute))
	got, err := s.RegisterEndpoint(ctx, "one")
	if err != nil || !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first call: %v %v", got, err)
	}
	clk.set(t0.Add(time.Hour))
	if got, err = s.RegisterEndpoint(ctx, "one"); err != nil || !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("later call: %v %v", got, err)
	}
	if got, err = s.RegisterEndpoint(ctx, "two"); err != nil || !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("second name: %v %v", got, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM webhook_endpoints`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows: %d %v", n, err)
	}
}

func TestRegisterEndpointConcurrent(t *testing.T) {
	ctx := context.Background()
	var tick atomic.Int64
	s, err := Open(ctx, filepath.Join(t.TempDir(), "s.db"), WithClock(func() time.Time {
		return t0.Add(time.Duration(tick.Add(1)) * time.Second)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const workers = 16
	var wg sync.WaitGroup
	got := make([]time.Time, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = s.RegisterEndpoint(ctx, "race")
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || !got[i].Equal(got[0]) {
			t.Fatalf("call %d: %v %v, call 0: %v", i, got[i], errs[i], got[0])
		}
	}
}

func TestEndpointNameIsInvalid(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 1)
	rowCount := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM webhook_endpoints) * 1000 + (SELECT count(*) FROM webhook_attempts)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := rowCount()
	for _, name := range []string{"", strings.Repeat("a", 65), "a b", "a/b", "é", "a\x00b", "a\n"} {
		if _, err := s.RegisterEndpoint(ctx, name); !errors.Is(err, ErrInvalid) {
			t.Errorf("RegisterEndpoint(%q): %v", name, err)
		}
		if err := s.AddAttempt(ctx, Attempt{Endpoint: name, EventID: ids[0], Status: 200}); !errors.Is(err, ErrInvalid) {
			t.Errorf("AddAttempt(%q): %v", name, err)
		}
		if _, err := s.AttemptsFor(ctx, name, ids[0]); !errors.Is(err, ErrInvalid) {
			t.Errorf("AttemptsFor(%q): %v", name, err)
		}
		if _, err := s.UndeliveredEvents(ctx, name, t0, time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("UndeliveredEvents(%q): %v", name, err)
		}
	}
	if after := rowCount(); after != before {
		t.Errorf("rows %d after the invalid names, were %d", after, before)
	}
	for _, name := range []string{"a", strings.Repeat("A", 64), "a.b_c-D9"} {
		if _, err := s.RegisterEndpoint(ctx, name); err != nil {
			t.Errorf("RegisterEndpoint(%q): %v", name, err)
		}
	}
}

// Random interleavings of attempts over three endpoints and five events (two pairs of events share a second): each
// endpoint's listing equals a model, ordered by creation time and then id, with the latest attempt time the maximum.
func TestUndeliveredEventsModel(t *testing.T) {
	ctx := context.Background()
	eps := []string{"main", "alpha", "b.c-1"}
	mins := []int{1, 1, 2, 4, 4}
	for seed := int64(1); seed <= 6; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s, _, clk := freshStore(t)
		ids := mkEventsAt(t, s, clk, mins)
		type key struct{ ep, ev int }
		type state struct {
			fails     int
			last      time.Time
			delivered bool
		}
		model := map[key]*state{}
		check := func() {
			t.Helper()
			for e, ep := range eps {
				sinceMin := rng.Intn(6) // events are at minutes 1, 2 and 4
				since := t0.Add(time.Duration(sinceMin) * time.Minute)
				admitMin := rng.Intn(7) - 1 // before every event, between them, and after all of them
				admit := t0.Add(time.Duration(admitMin) * time.Minute)
				var want []PendingEvent
				for v := range ids {
					st := model[key{e, v}]
					if st == nil {
						st = &state{}
					}
					if mins[v] < sinceMin || st.delivered || (mins[v] < admitMin && st.fails == 0) {
						continue
					}
					want = append(want, PendingEvent{Event: Event{ID: ids[v], CreatedAt: t0.Add(time.Duration(mins[v]) * time.Minute)},
						Failures: st.fails, LastAt: st.last})
				}
				sort.Slice(want, func(i, j int) bool {
					if !want[i].CreatedAt.Equal(want[j].CreatedAt) {
						return want[i].CreatedAt.Before(want[j].CreatedAt)
					}
					return want[i].ID < want[j].ID
				})
				got := pendingAdmit(t, s, ep, since, admit)
				if len(got) != len(want) {
					t.Fatalf("seed %d %s since %d admit %d: %d listed, want %d", seed, ep, sinceMin, admitMin, len(got), len(want))
				}
				for i := range got {
					if got[i].ID != want[i].ID || got[i].Failures != want[i].Failures || !got[i].LastAt.Equal(want[i].LastAt) {
						t.Fatalf("seed %d %s since %d admit %d: [%d] %+v, want %+v", seed, ep, sinceMin, admitMin, i, got[i], want[i])
					}
				}
			}
		}
		for n := 0; n < 60; n++ {
			e, v := rng.Intn(3), rng.Intn(5)
			at := t0.Add(time.Hour + time.Duration(rng.Intn(1000))*time.Second)
			ok := rng.Intn(4) == 0
			if err := s.AddAttempt(ctx, Attempt{Endpoint: eps[e], EventID: ids[v], At: at, Status: 500, OK: ok}); err != nil {
				t.Fatal(err)
			}
			st := model[key{e, v}]
			if st == nil {
				st = &state{}
				model[key{e, v}] = st
			}
			st.fails++
			if at.After(st.last) {
				st.last = at
			}
			st.delivered = st.delivered || ok
			if n%5 == 4 {
				check()
			}
		}
		check()
	}
}

// admit decides which events enter the list; an event that already has an attempt for the endpoint stays, whatever
// admit is, until it is delivered. An attempt for another endpoint does not count.
func TestUndeliveredEventsAdmit(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEventsAt(t, s, clk, []int{1, 2, 3, 4})
	attempt := func(ep string, id string, ok bool) {
		t.Helper()
		if err := s.AddAttempt(ctx, Attempt{Endpoint: ep, EventID: id, At: t0.Add(time.Hour), Status: 500, OK: ok}); err != nil {
			t.Fatal(err)
		}
	}
	attempt("a", ids[0], false) // tried by a, below admit: stays
	attempt("b", ids[1], false) // tried only by b: not listed for a
	attempt("a", ids[2], true)  // delivered: never listed
	admit := t0.Add(4 * time.Minute)
	got := pendingAdmit(t, s, "a", t0, admit)
	if len(got) != 2 || got[0].ID != ids[0] || got[0].Failures != 1 || got[1].ID != ids[3] {
		t.Fatalf("listed for a: %+v", got)
	}
	// since still bounds an attempted event.
	if got := pendingAdmit(t, s, "a", t0.Add(2*time.Minute), admit); len(got) != 1 || got[0].ID != ids[3] {
		t.Fatalf("listed for a since minute 2: %+v", got)
	}
	// An admit at or before since lists everything from since.
	if got := pendingAdmit(t, s, "a", t0, t0); len(got) != 3 {
		t.Fatalf("admit at since: %+v", got)
	}
	if got := pendingAdmit(t, s, "c", t0, admit); len(got) != 1 || got[0].ID != ids[3] {
		t.Fatalf("listed for c: %+v", got)
	}
}
