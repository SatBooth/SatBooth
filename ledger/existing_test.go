package ledger

import (
	"context"
	"errors"
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

// rawVersion reads the ledger's schema version with no pragmas.
func rawVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// OpenExisting opens a ledger at this build's schema version with the same settings as Open, and refuses a missing
// file (creating nothing), a schema behind this build's or ahead of it, and a file that is not a ledger; it never
// migrates.
func TestOpenExisting(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.db")
	if l, err := OpenExisting(ctx, missing); err == nil {
		l.Close()
		t.Fatal("a missing file was opened")
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(missing + sfx); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OpenExisting created %q: %v", "missing.db"+sfx, err)
		}
	}

	l, path := freshLedger(t)
	a := strings.Repeat("0f", 16)
	nonce, pn := strings.Repeat("1e", 16), strings.Repeat("2e", 16)
	if err := l.SetPair(ctx, PairStamp{ID: a, Gen: 3, Nonce: nonce, Pending: 4, PendingNonce: pn}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	e, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := e.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q %v", mode, err)
	}
	if p, err := e.Pair(ctx); err != nil || p != (PairStamp{ID: a, Gen: 3, Nonce: nonce, Pending: 4, PendingNonce: pn}) {
		t.Fatalf("pair %+v %v", p, err)
	}
	e.Close()

	// A schema behind this build's: refused, and left as it was.
	behind := filepath.Join(t.TempDir(), "ledger.db")
	b, err := Open(ctx, behind)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	rawExec(t, behind, `DROP TABLE pair`, `UPDATE schema_version SET version = 3`)
	if l, err := OpenExisting(ctx, behind); !errors.Is(err, ErrSchemaBehind) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("behind: %v", err)
	}
	if v := rawVersion(t, behind); v != 3 {
		t.Fatalf("the refused open changed the version to %d", v)
	}
	if !strings.Contains(ErrSchemaBehind.Error(), "start SatBooth once to upgrade it") {
		t.Fatalf("ErrSchemaBehind %q", ErrSchemaBehind)
	}

	// Ahead of this build's: refused with the newer-than-build text.
	rawExec(t, path, `UPDATE schema_version SET version = 99`)
	if l, err := OpenExisting(ctx, path); !errors.Is(err, ErrSchemaNewer) || !strings.Contains(err.Error(), "newer than this build") {
		if l != nil {
			l.Close()
		}
		t.Fatalf("ahead: %v", err)
	}
	if v := rawVersion(t, path); v != 99 {
		t.Fatalf("the refused open changed the version to %d", v)
	}

	// A SQLite file that is not a ledger: refused, and no version table is made.
	other := filepath.Join(t.TempDir(), "other.db")
	rawExec(t, other, `CREATE TABLE x (a INTEGER)`)
	if l, err := OpenExisting(ctx, other); err == nil {
		l.Close()
		t.Fatal("a file that is not a ledger was opened")
	}
	db, err := openRawForTest(other)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'schema_version'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the refused open made a version table: %d %v", n, err)
	}
}

// VacuumInto writes a copy (rollback journal mode in the header, bytes 18 and 19 both 1) to a new file, through a
// bound parameter (a quote in the path is fine). QuickCheck passes it, refuses a damaged or missing file, and changes
// nothing in the copy. Open accepts the copy (and turns it to WAL), with the rows in it.
func TestVacuumIntoAndQuickCheck(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	if res, err := l.CreateAccount(ctx, goodAccount()); err != nil || res != AccountCreated {
		t.Fatalf("create account: %v %v", res, err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "it's a copy.db")
	if err := l.VacuumInto(ctx, out); err != nil {
		t.Fatal(err)
	}
	if err := l.VacuumInto(ctx, out); err == nil {
		t.Fatal("VacuumInto overwrote an existing file")
	}
	if err := QuickCheck(ctx, out); err != nil {
		t.Fatal(err)
	}
	// A relative path is refused: SQLite would read one that starts with "file:" as a URI.
	t.Chdir(dir)
	for _, rel := range []string{"rel.db", "file:rel.db", "file:./x/rel.db"} {
		if err := l.VacuumInto(ctx, rel); err == nil {
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
	if _, ok, err := c.LookupAccount(ctx, goodAccount().ID); err != nil || !ok {
		t.Fatalf("the copy lost the account: %v %v", ok, err)
	}
}
