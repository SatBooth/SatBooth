package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawExec runs statements on path with no pragmas, as an operator's sqlite3 shell would.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rawVersion reads the store's schema version with no pragmas.
func rawVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// OpenExisting opens a store at this build's schema version with the same settings as Open, and refuses a missing
// file (creating nothing), a schema behind this build's or ahead of it, and a file that is not a store; it never
// migrates.
func TestOpenExisting(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.db")
	if s, err := OpenExisting(ctx, missing); err == nil {
		s.Close()
		t.Fatal("a missing file was opened")
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(missing + sfx); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OpenExisting created %q: %v", "missing.db"+sfx, err)
		}
	}

	s, path, _ := freshStore(t)
	a := strings.Repeat("a1", 16)
	nonce := strings.Repeat("1e", 16)
	if err := s.SetPair(ctx, PairStamp{ID: a, Gen: 3, Nonce: nonce}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	e, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := e.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q %v", mode, err)
	}
	if p, err := e.Pair(ctx); err != nil || p != (PairStamp{ID: a, Gen: 3, Nonce: nonce}) {
		t.Fatalf("pair %+v %v", p, err)
	}
	e.Close()

	// A schema behind this build's: refused, and left as it was.
	behind := filepath.Join(t.TempDir(), "store.db")
	b, err := Open(ctx, behind)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	rawExec(t, behind, `DROP TABLE pair`, `UPDATE store_schema_version SET version = 6`)
	if s, err := OpenExisting(ctx, behind); !errors.Is(err, ErrSchemaBehind) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("behind: %v", err)
	}
	if v := rawVersion(t, behind); v != 6 {
		t.Fatalf("the refused open changed the version to %d", v)
	}
	if !strings.Contains(ErrSchemaBehind.Error(), "start SatBooth once to upgrade it") {
		t.Fatalf("ErrSchemaBehind %q", ErrSchemaBehind)
	}

	// Ahead of this build's: refused with the newer-than-build text.
	rawExec(t, path, `UPDATE store_schema_version SET version = 99`)
	if s, err := OpenExisting(ctx, path); !errors.Is(err, ErrSchemaNewer) || !strings.Contains(err.Error(), "newer than this build") {
		if s != nil {
			s.Close()
		}
		t.Fatalf("ahead: %v", err)
	}
	if v := rawVersion(t, path); v != 99 {
		t.Fatalf("the refused open changed the version to %d", v)
	}

	// A SQLite file that is not a store: refused, and no version table is made.
	other := filepath.Join(t.TempDir(), "other.db")
	rawExec(t, other, `CREATE TABLE x (a INTEGER)`)
	if s, err := OpenExisting(ctx, other); err == nil {
		s.Close()
		t.Fatal("a file that is not a store was opened")
	}
	db, err := openRawForTest(other)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'store_schema_version'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the refused open made a version table: %d %v", n, err)
	}
}

// Migration 6 was edited in place for plan 11 (the reasons cancelled and ledger_paid; no release existed). A database
// whose refund_results was made without them is refused at Open with ErrOldRefundSchema, and nothing is changed.
func TestOpenRefusesOldRefundSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	// The previous migration 6: this one without the two new reasons, in the table's CHECK and the trigger's list.
	old := strings.ReplaceAll(migrations[5], ", 'cancelled'", "")
	old = strings.ReplaceAll(old, ", 'ledger_paid'", "")
	if strings.Contains(old, "cancelled") || strings.Contains(old, "ledger_paid") || old == migrations[5] {
		t.Fatal("setup: the old migration 6 still names a new reason, or is unchanged")
	}
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:5]...)
	stmts = append(stmts, old)
	stmts = append(stmts, migrations[6:]...)
	stmts = append(stmts, fmt.Sprintf(`INSERT INTO store_schema_version (version) VALUES (%d)`, len(migrations)))
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := schemaText(t, db)
	db.Close()
	s, err := Open(ctx, path)
	if err == nil {
		s.Close()
		t.Fatal("the database with the old refund_results opened")
	}
	if !errors.Is(err, ErrOldRefundSchema) || err.Error() != "store: the database is from before the refund void change; this version does not upgrade it; start with a new store and a new ledger (both database files)" {
		t.Fatalf("err = %v, want ErrOldRefundSchema", err)
	}
	db, err = openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if after := schemaText(t, db); after != before {
		t.Fatal("the refusal changed the schema")
	}
}

// VacuumInto writes a copy (rollback journal mode in the header, bytes 18 and 19 both 1) to a new file, through a
// bound parameter (a quote in the path is fine). QuickCheck passes it, refuses a damaged or missing file, and changes
// nothing in the copy. Open accepts the copy (and turns it to WAL), with the rows in it.
func TestVacuumIntoAndQuickCheck(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	o, _, err := s.CreateOrder(ctx, Order{AccountID: "acct1", MerchantRef: "copy-1", AmountMsat: 1000, CreatedAt: clk.now(), ExpiresAt: clk.now().Add(3600e9)})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "it's a copy.db")
	if err := s.VacuumInto(ctx, out); err != nil {
		t.Fatal(err)
	}
	if err := s.VacuumInto(ctx, out); err == nil {
		t.Fatal("VacuumInto overwrote an existing file")
	}
	if err := QuickCheck(ctx, out); err != nil {
		t.Fatal(err)
	}
	// A relative path is refused: SQLite would read one that starts with "file:" as a URI.
	t.Chdir(dir)
	for _, rel := range []string{"rel.db", "file:rel.db", "file:./x/rel.db"} {
		if err := s.VacuumInto(ctx, rel); err == nil {
			t.Fatalf("%q: a relative path was taken", rel)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if b[18] != 1 || b[19] != 1 {
		t.Fatalf("header bytes 18, 19: %d %d, want 1 1 (rollback journal)", b[18], b[19])
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("the copy and the check left %d files, want 1", len(ents))
	}

	damaged := filepath.Join(dir, "damaged.db")
	if err := os.WriteFile(damaged, append(b[:100:100], make([]byte, len(b)-100)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := QuickCheck(ctx, damaged); err == nil {
		t.Fatal("a damaged file passed")
	}
	missing := filepath.Join(dir, "missing.db")
	if err := QuickCheck(ctx, missing); err == nil {
		t.Fatal("a missing file passed")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("QuickCheck created a file: %v", err)
	}

	c, err := Open(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, err := c.OrderByID(ctx, o.ID); err != nil || got.MerchantRef != "copy-1" {
		t.Fatalf("the copy lost the order: %+v %v", got, err)
	}
}
