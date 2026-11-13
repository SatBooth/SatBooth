package ledger

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Restart tests. They show what a clean close and a copy of the live files
// keep. A kill during fsync, or a power cut, cannot be simulated in go test: durability against
// that rests on SQLite's WAL with synchronous=FULL and fullfsync, which these tests do not
// exercise. What they do show is that the files hold exactly the committed rows, and that
// recovery from a WAL that was never checkpointed works.

// snapshot reads every account, transfer and balance of the fixture ledger, in a form two
// ledgers can be compared with.
func snapshot(t *testing.T, l *Ledger, accounts, transfers []uint64) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, id := range accounts {
		a, found, err := l.LookupAccount(ctx, u(id))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("account %d %v %+v", id, found, a))
		if found {
			b, err := l.Balance(ctx, u(id))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("balance %d %+v", id, b))
		}
	}
	for _, id := range transfers {
		x, found, err := l.LookupTransfer(ctx, u(id))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("transfer %d %v %+v", id, found, x))
	}
	return out
}

// fixtureIDs are the ids the fixture creates (transfer 99 is refused and never stored).
var (
	fixtureAccounts  = []uint64{1, 2, 3, 4}
	fixtureTransfers = []uint64{10, 11, 12, 13, 14, 15, 99}
)

// fillFixture commits single-phase, pending, posted, voided, expired and refused transfers.
func fillFixture(t *testing.T, l *Ledger, clk *testClock) {
	t.Helper()
	mustAcct(t, l, 1, 1, 0)
	mustAcct(t, l, 2, 1, AccountDebitsMustNotExceedCredits)
	mustAcct(t, l, 3, 1, 0)
	mustAcct(t, l, 4, 2, 0)
	mustCreate(t, l, tr(10, 1, 2, u(500)))
	mustCreate(t, l, pend(11, u(100), 0)) // 1 -> 2, to be posted in part
	mustCreate(t, l, pend(12, u(50), 0))  // to be voided
	mustCreate(t, l, resolve(13, 11, TransferPostPendingTransfer, u(60)))
	mustCreate(t, l, resolve(14, 12, TransferVoidPendingTransfer, u(0)))
	mustCreate(t, l, pend(15, u(7), 5)) // expires: 5 s later
	clk.advance(10_000_000_000)
	if got := create(t, l, tr(99, 2, 3, u(1_000_000))); got != TransferExceedsCredits {
		t.Fatal(got)
	}
	wantBal(t, l, 1, u(0), u(560), u(0), u(0)) // also records the expiry of transfer 15
}

func TestReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	clk := &testClock{ns: clockStart}
	l, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	fillFixture(t, l, clk)
	before := snapshot(t, l, fixtureAccounts, fixtureTransfers)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	after := snapshot(t, l2, fixtureAccounts, fixtureTransfers)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("reopen changed the ledger:\n%s\n--- vs ---\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	// The refused transfer is still refused, and the resolved ones still are.
	if got := create(t, l2, tr(99, 2, 3, u(1))); got != TransferIDAlreadyFailed {
		t.Fatal(got)
	}
	if got := create(t, l2, resolve(20, 11, TransferVoidPendingTransfer, u(0))); got != TransferPendingTransferAlreadyPosted {
		t.Fatal(got)
	}
	if got := create(t, l2, resolve(21, 15, TransferPostPendingTransfer, u(7))); got != TransferPendingTransferExpired {
		t.Fatal(got)
	}
}

// copyFile copies src to dst; it reports whether src existed.
func copyFile(t *testing.T, src, dst string) bool {
	t.Helper()
	in, err := os.Open(src)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return true
}

// copyLedgerFiles copies ledger.db and ledger.db-wal (never ledger.db-shm: the shared-memory
// index is rebuilt from the WAL when the copy is opened, which the tests below prove) of a live
// ledger into a new directory, and returns the copy's path.
func copyLedgerFiles(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	dst := filepath.Join(dir, "ledger.db")
	if !copyFile(t, path, dst) {
		t.Fatal("no database file")
	}
	if !copyFile(t, path+"-wal", dst+"-wal") {
		t.Fatal("no WAL file: the test would not exercise recovery")
	}
	return dst
}

// With no checkpoint, the committed rows live (at least partly) only in the WAL. Copying the
// two files of a live ledger, as a file-level backup would, and opening the copy shows them all.
func TestCopyCommittedFiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	clk := &testClock{ns: clockStart}
	l, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fillFixture(t, l, clk)
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected an WAL with the commits in it: %v", err)
	}
	want := snapshot(t, l, fixtureAccounts, fixtureTransfers)

	cp := copyLedgerFiles(t, path)
	c, err := Open(ctx, cp, WithClock(clk.now))
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer c.Close()
	got := snapshot(t, c, fixtureAccounts, fixtureTransfers)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("copy differs:\n%s\n--- vs ---\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The copy is a full ledger: it accepts new work, independent of the original.
	mustCreate(t, c, tr(30, 1, 3, u(1)))
	if _, found, _ := l.LookupTransfer(ctx, u(30)); found {
		t.Fatal("the original sees a write made to the copy")
	}
}

// A transaction that has written but not committed is not in a copy of the files, even when its
// pages have been spilled into the WAL (a tiny page cache forces that): without a commit frame
// recovery ignores them.
func TestUncommittedNotVisible(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	clk := &testClock{ns: clockStart}
	l, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fillFixture(t, l, clk)
	want := snapshot(t, l, fixtureAccounts, fixtureTransfers)

	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{`PRAGMA cache_size=5`, `BEGIN IMMEDIATE`} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	walSize := func() int64 {
		fi, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	walBefore := walSize()
	const uncommitted = 300
	for i := range uncommitted {
		id := u(uint64(1000 + i)).Bytes()
		if _, err := conn.ExecContext(ctx, `INSERT INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
			VALUES (?, ?, ?, 0, 1, 1, 0, ?)`, id[:], id16(0), make([]byte, 8), int64(clockStart)*2+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if walSize() <= walBefore {
		t.Fatal("the uncommitted pages did not reach the WAL: the test would prove nothing")
	}
	cp := copyLedgerFiles(t, path) // taken while the transaction is open
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}

	c, err := Open(ctx, cp, WithClock(clk.now))
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	defer c.Close()
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&n); err != nil || n != len(fixtureAccounts) {
		t.Fatalf("copy has %d accounts, err %v, want %d", n, err, len(fixtureAccounts))
	}
	if _, found, err := c.LookupAccount(ctx, u(1000)); err != nil || found {
		t.Fatalf("uncommitted account visible in the copy: %v %v", found, err)
	}
	got := snapshot(t, c, fixtureAccounts, fixtureTransfers)
	if strings.Join(want, "\n") != strings.Join(got, "\n") {
		t.Fatalf("copy differs from the committed state")
	}
	// The original, after the rollback, is unchanged too.
	if err := l.db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&n); err != nil || n != len(fixtureAccounts) {
		t.Fatalf("original has %d accounts, err %v", n, err)
	}
}

// A v1 database (the first migration only) upgrades to the current schema with its data intact,
// and the guards of later migrations are then in force on a raw connection.
func TestUpgradeV1ToCurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`PRAGMA journal_mode=wal`, `CREATE TABLE schema_version (version INTEGER NOT NULL) STRICT`,
		migrations[0], `INSERT INTO schema_version (version) VALUES (1)`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Rows written by the v1 schema: two accounts, a transfer and a refused-transfer record.
	if err := insertAccount(raw, id16(1), 1); err != nil {
		t.Fatal(err)
	}
	if err := insertAccount(raw, id16(2), 2); err != nil {
		t.Fatal(err)
	}
	amt := u(9).Bytes()
	if err := insertTransfer(raw, id16(3), amt[:], make([]byte, 16), id16(0), 0, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO failed_transfers (id, result) VALUES (?, 'exceeds_credits')`, id16(4)); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	l, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var v, n int
	if err := l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("version %d err %v, want %d", v, err, len(migrations))
	}
	// Data intact.
	a1, _ := U128FromBytes(id16(1))
	a2, _ := U128FromBytes(id16(2))
	t3, _ := U128FromBytes(id16(3))
	t4, _ := U128FromBytes(id16(4))
	for _, id := range []U128{a1, a2} {
		if _, found, err := l.LookupAccount(ctx, id); err != nil || !found {
			t.Fatalf("account %v: %v %v", id, found, err)
		}
	}
	if x, found, err := l.LookupTransfer(ctx, t3); err != nil || !found || x.Amount != u(9) || x.Timestamp != 3 {
		t.Fatalf("transfer: %+v %v %v", x, found, err)
	}
	if b, err := l.Balance(ctx, a1); err != nil || b.DebitsPosted != u(9) {
		t.Fatalf("balance %+v %v", b, err)
	}
	// The refused transfer stays refused.
	if got := create(t, l, Transfer{ID: t4, DebitAccountID: a1, CreditAccountID: a2, Amount: u(1), Ledger: 1, Code: 1}); got != TransferIDAlreadyFailed {
		t.Fatal(got)
	}
	// The tables and guards of the later migrations are in force on a raw connection.
	if err := l.db.QueryRow(`SELECT count(*) FROM expired_transfers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired_transfers: %d %v", n, err)
	}
	raw2, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	wantErr(t, insertAccount(raw2, id16(1), 50), "", "duplicate account id")
	_, err = raw2.Exec(`INSERT OR REPLACE INTO accounts (id, user_data_128, user_data_64, user_data_32, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, 0, 1, 1, 0, 51)`, id16(1), id16(0), make([]byte, 8))
	wantErr(t, err, "", "INSERT OR REPLACE over an account")
	_, err = raw2.Exec(`UPDATE transfers SET amount = ?`, make([]byte, 16))
	wantErr(t, err, "immutable", "update a transfer")
	_, err = raw2.Exec(`DELETE FROM accounts`)
	wantErr(t, err, "immutable", "delete an account")
	if _, err := raw2.Exec(`INSERT INTO transfers (id, debit_account_id, credit_account_id, amount, pending_id, user_data_128,
		user_data_64, user_data_32, timeout, ledger, code, flags, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 1, 0, 60)`, id16(7), id16(1), id16(0x55), amt[:], make([]byte, 16), id16(0), make([]byte, 8)); err == nil {
		t.Fatal("a transfer to an unknown account was accepted on a raw connection")
	}
	if err := raw2.QueryRow(`SELECT count(*) FROM accounts`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d accounts after the refused writes, err %v", n, err)
	}
}

// An in-memory database cannot be in WAL mode, which the ledger's durability rests on: refused.
func TestOpenMemoryRefused(t *testing.T) {
	l, err := Open(context.Background(), ":memory:")
	if err == nil {
		l.Close()
		t.Fatal(`Open(":memory:") succeeded`)
	}
	if !strings.Contains(err.Error(), "journal_mode") {
		t.Fatalf(`Open(":memory:"): %v`, err)
	}
}
