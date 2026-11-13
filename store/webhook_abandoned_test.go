package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func pendingAbandoned(t *testing.T, s *Store, endpoint string) []string {
	t.Helper()
	ids, err := s.PendingAbandoned(context.Background(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// A failed attempt marked Final writes the "abandoned, not yet announced" row in the same transaction; no other
// attempt does; a row is per endpoint and is removed by ClearAbandoned, which is a no-op for a row that is not there.
func TestFinalFailedAttemptMarksTheEventForAnnouncement(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 4)
	at := t0.Add(time.Hour)
	for _, a := range []Attempt{
		{Endpoint: "a", EventID: ids[0], At: at, Status: 500},              // failed, not final
		{Endpoint: "a", EventID: ids[1], At: at, Status: 200, OK: true},    // delivered
		{Endpoint: "a", EventID: ids[2], At: at, Status: 500, Final: true}, // abandoned
		{Endpoint: "b", EventID: ids[2], At: at, Status: 0, Final: true},   // abandoned, for another endpoint
		{Endpoint: "a", EventID: ids[3], At: at, Status: 200, OK: true, Final: true},
	} {
		if err := s.AddAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if got := pendingAbandoned(t, s, "a"); len(got) != 1 || got[0] != ids[2] {
		t.Fatalf("a: %v, want only %s", got, ids[2])
	}
	if got := pendingAbandoned(t, s, "b"); len(got) != 1 || got[0] != ids[2] {
		t.Fatalf("b: %v", got)
	}
	if got := pendingAbandoned(t, s, "c"); len(got) != 0 {
		t.Fatalf("c: %v", got)
	}
	if err := s.ClearAbandoned(ctx, "a", ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAbandoned(ctx, "a", ids[2]); err != nil {
		t.Fatalf("clearing a row that is not there: %v", err)
	}
	if got := pendingAbandoned(t, s, "a"); len(got) != 0 {
		t.Fatalf("a after the clear: %v", got)
	}
	if got := pendingAbandoned(t, s, "b"); len(got) != 1 {
		t.Fatalf("the clear for a took b's row: %v", got)
	}
}

// The oldest abandonment is listed first.
func TestPendingAbandonedIsOldestFirst(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 3)
	for i, k := range []int{2, 0, 1} {
		if err := s.AddAttempt(ctx, Attempt{Endpoint: "a", EventID: ids[k], At: t0.Add(time.Duration(i+1) * time.Hour), Status: 500, Final: true}); err != nil {
			t.Fatal(err)
		}
	}
	got := pendingAbandoned(t, s, "a")
	if len(got) != 3 || got[0] != ids[2] || got[1] != ids[0] || got[2] != ids[1] {
		t.Fatalf("%v", got)
	}
}

// A refused attempt leaves no row, and the names are checked.
func TestAbandonedRowRules(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 1)
	if err := s.AddAttempt(ctx, Attempt{Endpoint: "a", EventID: strings.Repeat("0", 32), Status: 500, Final: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown event: %v", err)
	}
	if err := s.AddAttempt(ctx, Attempt{Endpoint: "a", EventID: ids[0], Status: 1000, Final: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	if got := pendingAbandoned(t, s, "a"); len(got) != 0 {
		t.Fatalf("rows after refused attempts: %v", got)
	}
	for _, bad := range []string{"", "a b", strings.Repeat("a", 65)} {
		if _, err := s.PendingAbandoned(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("PendingAbandoned(%q): %v", bad, err)
		}
		if err := s.ClearAbandoned(ctx, bad, ids[0]); !errors.Is(err, ErrInvalid) {
			t.Errorf("ClearAbandoned(%q): %v", bad, err)
		}
	}
	if err := s.ClearAbandoned(ctx, "a", "xyz"); !errors.Is(err, ErrInvalid) {
		t.Errorf("ClearAbandoned bad id: %v", err)
	}
}

// The guards of the table: a row cannot be updated or replaced and needs an event; it can be deleted (that is the
// announcement).
func TestAbandonedTableGuards(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 1)
	if err := s.AddAttempt(ctx, Attempt{Endpoint: "a", EventID: ids[0], At: t0.Add(time.Hour), Status: 500, Final: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE webhook_abandoned SET at = at + 1`); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`INSERT INTO webhook_abandoned (endpoint, event_id, at) VALUES ('a', '%s', 99)`, ids[0])); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("replace: %v", err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`INSERT INTO webhook_abandoned (endpoint, event_id, at) VALUES ('b', '%s', 99)`, strings.Repeat("0", 32))); err == nil || !strings.Contains(err.Error(), "unknown event") {
		t.Fatalf("unknown event: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM webhook_abandoned`); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// dropMigrationEleven returns the statements that take a version-11 database back to version 10: the outbox table and
// the newest-invoice index dropped.
func dropMigrationEleven() []string {
	return []string{`DROP TABLE webhook_abandoned`, `DROP INDEX invoices_created`}
}

// A version-10 database gains the table and the index at Open; its rows stay; the result is a fresh store's schema.
func TestMigrationElevenAddsTheAbandonedTable(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/s.db"
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e1 := hexOf('1')
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:10]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (10)`,
		fmt.Sprintf(`INSERT INTO orders VALUES ('%s', '%s', 'a', 'r', 1000, 0, 0, 5, 6)`, strings.Repeat("a", 32), strings.Repeat("d", 32)),
		fmt.Sprintf(`INSERT INTO events VALUES ('%s', 'order.paid', '%s', 1, 1000)`, e1, strings.Repeat("a", 32)),
		fmt.Sprintf(`INSERT INTO webhook_attempts VALUES ('main', '%s', 1, 1100, 500, 0)`, e1))
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	db.Close()
	s, err := Open(ctx, path, WithClock(func() time.Time { return t0 }))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil || v != 11 || len(migrations) != 11 {
		t.Fatalf("version %d (%d migrations) %v", v, len(migrations), err)
	}
	if got := pendingAbandoned(t, s, "main"); len(got) != 0 {
		t.Fatalf("a migrated store has rows: %v", got)
	}
	if got := rawAttemptRows(t, s.db, `SELECT endpoint, event_id, seq, at, status, ok FROM webhook_attempts`); len(got) != 1 {
		t.Fatalf("attempts %v", got)
	}
	fresh, _, _ := freshStore(t)
	if got, want := schemaText(t, s.db), schemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
	if got := triggersOf(t, s.db, "webhook_abandoned"); got != "webhook_abandoned_known_event webhook_abandoned_no_replace webhook_abandoned_no_update" {
		t.Fatalf("triggers %q", got)
	}
	if err := s.AddAttempt(ctx, Attempt{Endpoint: "main", EventID: e1, At: t0, Status: 500, Final: true}); err != nil {
		t.Fatal(err)
	}
}

// The attempt and its mark are one write: if the mark cannot be written, the attempt is not recorded either, so an
// event is never left with its last attempt recorded and no mark.
func TestMarkAndAttemptAreOneWrite(t *testing.T) {
	ctx := context.Background()
	s, path, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 1)
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER hold_mark BEFORE INSERT ON webhook_abandoned BEGIN SELECT RAISE(ABORT, 'held'); END`); err != nil {
		t.Fatal(err)
	}
	err = s.AddAttempt(ctx, Attempt{Endpoint: "a", EventID: ids[0], At: t0.Add(time.Hour), Status: 500, Final: true})
	var n int
	if qerr := s.db.QueryRow(`SELECT count(*) FROM webhook_attempts`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if err == nil || n != 0 {
		t.Fatalf("the mark failed, AddAttempt returned %v and %d attempt rows stay", err, n)
	}
	if got := pendingAbandoned(t, s, "a"); len(got) != 0 {
		t.Fatalf("marks: %v", got)
	}
}
