package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freshLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func id16(b byte) []byte { return bytes.Repeat([]byte{b}, 16) }

func TestOpenPragmas(t *testing.T) {
	l, _ := freshLedger(t)
	want := []struct{ name, val string }{
		{"journal_mode", "wal"},
		{"synchronous", "2"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
		{"fullfsync", "1"},
		{"checkpoint_fullfsync", "1"},
		{"recursive_triggers", "1"},
	}
	for _, w := range want {
		var got string
		if err := l.db.QueryRow("PRAGMA " + w.name).Scan(&got); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if got != w.val {
			t.Errorf("PRAGMA %s = %q, want %q", w.name, got, w.val)
		}
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("ab?#% é日 &=;'\"_.-0")
	names := []string{"a?b#c%d.db", "%3Fx.db", "sp ace.db", "q?_pragma=journal_mode(delete).db"}
	for i := 0; i < 40; i++ {
		n := 1 + rng.Intn(12)
		r := make([]rune, n)
		for j := range r {
			r[j] = alphabet[rng.Intn(len(alphabet))]
		}
		names = append(names, fmt.Sprintf("%d-%s.db", i, string(r)))
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		l, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		l.Close()
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%q: file not created under that name: %v", name, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		ok := false
		for _, n := range names {
			if e.Name() == n || strings.HasPrefix(e.Name(), n+"-") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("unexpected file %q", e.Name())
		}
	}
	if _, err := Open(context.Background(), ""); err == nil {
		t.Error("empty path accepted")
	}
}

func TestWithClockNil(t *testing.T) {
	l, err := Open(context.Background(), filepath.Join(t.TempDir(), "l.db"), WithClock(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.now == nil || l.now().IsZero() {
		t.Error("nil clock not ignored")
	}
}

func TestWritePanicRollsBack(t *testing.T) {
	l, _ := freshLedger(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic not propagated")
			}
		}()
		l.write(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO failed_transfers (id, result) VALUES (?, 'x')`, id16(1)); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := l.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO failed_transfers (id, result) VALUES (?, 'y')`, id16(2))
		return err
	})
	if err != nil {
		t.Fatalf("write after panic: %v", err)
	}
	var n int
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM failed_transfers`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, err %v; want 1 (the panicking write rolled back)", n, err)
	}
}

func exec0(c *sql.Conn, q string) error { _, err := c.ExecContext(context.Background(), q); return err }

// The guards must hold on a connection with none of the ledger's pragmas, like an operator's shell.
func TestGuardsOnRawConnection(t *testing.T) {
	l, path := freshLedger(t)
	seedRows(t, l)
	pending := id16(0x50)
	z := make([]byte, 16)
	if err := insertTransfer(l.db, id16(0x51), z, pending, id16(0), 4, 20); err != nil {
		t.Fatal(err)
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	// The driver turns foreign_keys on by default; switch it off like a plain shell would have it.
	if err := exec0(conn, `PRAGMA foreign_keys=off`); err != nil {
		t.Fatal(err)
	}
	var fk, rt int
	conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
	conn.QueryRowContext(ctx, `PRAGMA recursive_triggers`).Scan(&rt)
	if fk != 0 || rt != 0 {
		t.Fatalf("raw connection has foreign_keys=%d recursive_triggers=%d, want 0, 0", fk, rt)
	}
	exec := func(q string, args ...any) error { _, err := conn.ExecContext(ctx, q, args...); return err }
	const acct = `(id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp) VALUES (?, ?, ?, 0, 1, 7, 0, ?)`
	const xfer = `(id, debit_account_id, credit_account_id, amount, pending_id, user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 1, ?, ?)`
	u64 := make([]byte, 8)
	wantErr(t, exec(`INSERT OR REPLACE INTO accounts`+acct, id16(1), id16(0), u64, 100), "immutable", "replace account by id")
	wantErr(t, exec(`INSERT OR REPLACE INTO accounts`+acct, id16(0x61), id16(0), u64, 1), "immutable", "replace account by timestamp")
	wantErr(t, exec(`REPLACE INTO failed_transfers (id, result) VALUES (?, 'other')`, id16(4)), "immutable", "replace failed_transfers")
	wantErr(t, exec(`INSERT OR REPLACE INTO transfers`+xfer, id16(3), id16(1), id16(2), z, z, id16(0), u64, 0, 101), "immutable", "replace transfer by id")
	wantErr(t, exec(`INSERT OR REPLACE INTO transfers`+xfer, id16(0x62), id16(1), id16(2), z, pending, id16(0), u64, 8, 102), "immutable", "replace resolution")
	wantErr(t, exec(`INSERT INTO transfers`+xfer, id16(0x63), id16(1), id16(2), z, pending, id16(0), u64, 4, 103), "immutable", "second resolution")
	wantErr(t, exec(`UPDATE accounts SET code = 9`), "immutable", "update")
	wantErr(t, exec(`DELETE FROM transfers`), "immutable", "delete")
	// Foreign keys are off on this connection, so the trigger is what refuses a missing account.
	wantErr(t, exec(`INSERT INTO transfers`+xfer, id16(0x64), id16(1), id16(0x99), z, z, id16(0), u64, 0, 104), "unknown account", "missing credit account")
	wantErr(t, exec(`INSERT INTO transfers`+xfer, id16(0x65), id16(0x99), id16(2), z, z, id16(0), u64, 0, 105), "unknown account", "missing debit account")
	var code int
	if err := conn.QueryRowContext(ctx, `SELECT code FROM accounts WHERE id = ?`, id16(1)).Scan(&code); err != nil || code != 1 {
		t.Errorf("account 1 code = %d, err %v; want 1", code, err)
	}
}

func TestSchemaVersion(t *testing.T) {
	l, path := freshLedger(t)
	count := func(l *Ledger) (n, v int) {
		t.Helper()
		if err := l.db.QueryRow(`SELECT count(*), coalesce(max(version),0) FROM schema_version`).Scan(&n, &v); err != nil {
			t.Fatal(err)
		}
		return
	}
	if n, v := count(l); n != 1 || v != len(migrations) {
		t.Fatalf("rows=%d version=%d, want 1, %d", n, v, len(migrations))
	}
	// Running a migration twice would fail (CREATE TABLE on an existing table), so a successful
	// reopen shows the applied migrations are skipped; it must also leave exactly one version row.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	if n, v := count(l2); n != 1 || v != len(migrations) {
		t.Fatalf("after reopen rows=%d version=%d, want 1, %d", n, v, len(migrations))
	}
}

func TestSchemaVersionNewerRefused(t *testing.T) {
	l, path := freshLedger(t)
	if _, err := l.db.Exec(`UPDATE schema_version SET version = 99`); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if l2, err := Open(context.Background(), path); err == nil {
		l2.Close()
		t.Fatal("opened a database from a newer schema")
	}
}

func insertAccount(db *sql.DB, id []byte, ts int) error {
	_, err := db.Exec(`INSERT INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, 0, 1, 1, 0, ?)`, id, id16(0), make([]byte, 8), ts)
	return err
}

func insertTransfer(db *sql.DB, id, amount, pending, ud128 []byte, flags, ts int) error {
	_, err := db.Exec(`INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id,
		user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 1, ?, ?)`,
		id, id16(1), id16(2), amount, pending, ud128, make([]byte, 8), flags, ts)
	return err
}

func seedRows(t *testing.T, l *Ledger) {
	t.Helper()
	if err := insertAccount(l.db, id16(1), 1); err != nil {
		t.Fatal(err)
	}
	if err := insertAccount(l.db, id16(2), 2); err != nil {
		t.Fatal(err)
	}
	if err := insertTransfer(l.db, id16(3), id16(9), make([]byte, 16), id16(0), 0, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`INSERT INTO failed_transfers (id, result) VALUES (?, 'exceeds_credits')`, id16(4)); err != nil {
		t.Fatal(err)
	}
}

func TestInsertOnly(t *testing.T) {
	l, _ := freshLedger(t)
	seedRows(t, l)
	stmts := map[string][2]string{
		"accounts":         {`UPDATE accounts SET code = 2`, `DELETE FROM accounts`},
		"transfers":        {`UPDATE transfers SET flags = 0`, `DELETE FROM transfers`},
		"failed_transfers": {`UPDATE failed_transfers SET result = 'x'`, `DELETE FROM failed_transfers`},
	}
	for table, s := range stmts {
		for _, q := range s {
			_, err := l.db.Exec(q)
			if err == nil || !strings.Contains(err.Error(), "ledger rows are immutable") {
				t.Errorf("%s: %q: err = %v, want 'ledger rows are immutable'", table, q, err)
			}
		}
		var n int
		if err := l.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n == 0 {
			t.Errorf("%s: rows after attempts = %d, err %v", table, n, err)
		}
	}
	// INSERT OR REPLACE deletes the row it conflicts with; that must fire the delete trigger too,
	// whether the conflict is on the primary key or on the unique timestamp.
	for _, c := range []struct {
		name string
		id   []byte
		ts   int
	}{{"same id", id16(1), 100}, {"same timestamp", id16(0x60), 1}} {
		_, err := l.db.Exec(`INSERT OR REPLACE INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, 0, 1, 7, 0, ?)`, c.id, id16(0), make([]byte, 8), c.ts)
		if err == nil || !strings.Contains(err.Error(), "ledger rows are immutable") {
			t.Errorf("INSERT OR REPLACE (%s): err = %v", c.name, err)
		}
	}
	_, err := l.db.Exec(`REPLACE INTO failed_transfers (id, result) VALUES (?, 'other')`, id16(4))
	if err == nil || !strings.Contains(err.Error(), "ledger rows are immutable") {
		t.Errorf("REPLACE INTO failed_transfers: err = %v", err)
	}
	var code int
	if err := l.db.QueryRow(`SELECT code FROM accounts WHERE id = ?`, id16(1)).Scan(&code); err != nil || code != 1 {
		t.Errorf("account 1 code = %d, err %v; want 1", code, err)
	}
}

func wantErr(t *testing.T, err error, substr, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Errorf("%s: err = %v, want one containing %q", what, err, substr)
	}
}

func TestBlobChecks(t *testing.T) {
	l, _ := freshLedger(t)
	seedRows(t, l)
	z16 := make([]byte, 16)
	ts := 10
	next := func() int { ts++; return ts }
	const check = "CHECK constraint failed"
	for _, n := range []int{0, 8, 15, 17, 32} {
		bad := make([]byte, n)
		what := func(f string) string { return fmt.Sprintf("%s of %d bytes", f, n) }
		wantErr(t, insertAccount(l.db, bad, next()), check, what("account id"))
		wantErr(t, insertTransfer(l.db, bad, id16(9), z16, id16(0), 0, next()), check, what("transfer id"))
		wantErr(t, insertTransfer(l.db, id16(0x20), bad, z16, id16(0), 0, next()), check, what("amount"))
		wantErr(t, insertTransfer(l.db, id16(0x21), id16(9), bad, id16(0), 0, next()), check, what("pending_id"))
		wantErr(t, insertTransfer(l.db, id16(0x22), id16(9), z16, bad, 0, next()), check, what("user_data_128"))
		_, err := l.db.Exec(`INSERT INTO failed_transfers (id, result) VALUES (?, 'x')`, bad)
		wantErr(t, err, check, what("failed_transfers id"))
	}
	if err := insertAccount(l.db, id16(0x30), next()); err != nil {
		t.Errorf("valid account rejected: %v", err)
	}
	// An INTEGER where a BLOB is required is refused by STRICT, so money cannot be stored as a number.
	_, err := l.db.Exec(`INSERT INTO failed_transfers (id, result) VALUES (12345, 'x')`)
	wantErr(t, err, "cannot store", "integer id")
	if err := insertTransfer(l.db, id16(0x23), id16(0x77), z16, id16(0), 0, next()); err != nil {
		t.Errorf("valid transfer rejected: %v", err)
	}
	// user_data_64 must be 8 bytes.
	_, err = l.db.Exec(`INSERT INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, 0, 1, 1, 0, ?)`, id16(0x31), id16(0), make([]byte, 16), next())
	wantErr(t, err, check, "user_data_64 of 16 bytes")
	// Range checks, on both tables (timeout only exists on transfers).
	for _, q := range []string{
		"ledger=0", "ledger=4294967296", "code=0", "code=65536", "flags=65536", "flags=-1",
		"user_data_32=4294967296", "user_data_32=-1",
	} {
		set := strings.SplitN(q, "=", 2)
		cols := map[string]string{"ledger": "1", "code": "1", "flags": "0", "user_data_32": "0", "timeout": "0"}
		cols[set[0]] = set[1]
		_, err := l.db.Exec(`INSERT INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, `+cols["user_data_32"]+`, `+cols["ledger"]+`, `+cols["code"]+`, `+cols["flags"]+`, ?)`,
			id16(0x40), id16(0), make([]byte, 8), next())
		wantErr(t, err, check, "account "+q)
		_, err = l.db.Exec(`INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id,
			user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, `+cols["user_data_32"]+`, `+cols["timeout"]+`, `+cols["ledger"]+`, `+cols["code"]+`, `+cols["flags"]+`, ?)`,
			id16(0x43), id16(1), id16(2), z16, z16, id16(0), make([]byte, 8), next())
		wantErr(t, err, check, "transfer "+q)
	}
	for _, v := range []string{"-1", "4294967296"} {
		_, err := l.db.Exec(`INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id,
			user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, `+v+`, 1, 1, 0, ?)`,
			id16(0x44), id16(1), id16(2), z16, z16, id16(0), make([]byte, 8), next())
		wantErr(t, err, check, "transfer timeout="+v)
	}
	wantErr(t, insertAccount(l.db, id16(0x41), 0), check, "timestamp 0")
	wantErr(t, insertAccount(l.db, id16(0x42), 1), "immutable", "duplicate timestamp")
}

func TestForeignKeys(t *testing.T) {
	l, _ := freshLedger(t)
	if err := insertTransfer(l.db, id16(3), id16(9), make([]byte, 16), id16(0), 0, 3); err == nil {
		t.Error("transfer between missing accounts accepted")
	}
}

func TestOneResolutionPerPending(t *testing.T) {
	l, _ := freshLedger(t)
	seedRows(t, l)
	pending := id16(0x50)
	z := make([]byte, 16)
	const post, void = 4, 8
	if err := insertTransfer(l.db, id16(0x51), z, pending, id16(0), post, 20); err != nil {
		t.Fatalf("first resolution: %v", err)
	}
	for i, flags := range []int{post, void} {
		err := insertTransfer(l.db, id16(byte(0x52+i)), z, pending, id16(0), flags, 21+i)
		if err == nil || !strings.Contains(err.Error(), "ledger rows are immutable") {
			t.Errorf("second resolution (flags %d): err = %v, want the immutable guard", flags, err)
		}
	}
	// REPLACE must not be a way to swap the first resolution for another.
	if _, err := l.db.Exec(`INSERT OR REPLACE INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id,
		user_data_128, user_data_64, user_data_32, timeout, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 1, 8, 40)`,
		id16(0x56), id16(1), id16(2), z, pending, id16(0), make([]byte, 8)); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("INSERT OR REPLACE over a resolution: err = %v", err)
	}
	// Rows that are not resolutions may share a pending_id (zero for plain and pending transfers).
	if err := insertTransfer(l.db, id16(0x54), id16(9), z, id16(0), 0, 30); err != nil {
		t.Errorf("plain transfer: %v", err)
	}
	if err := insertTransfer(l.db, id16(0x55), id16(9), z, id16(0), 2, 31); err != nil {
		t.Errorf("pending transfer: %v", err)
	}
}
