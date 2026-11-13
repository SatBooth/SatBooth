package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenPragmas(t *testing.T) {
	s, _, _ := freshStore(t)
	for _, w := range []struct{ name, val string }{
		{"journal_mode", "wal"}, {"synchronous", "2"}, {"foreign_keys", "1"}, {"busy_timeout", "5000"},
		{"fullfsync", "1"}, {"checkpoint_fullfsync", "1"}, {"recursive_triggers", "1"},
	} {
		var got string
		if err := s.db.QueryRow("PRAGMA " + w.name).Scan(&got); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if got != w.val {
			t.Errorf("PRAGMA %s = %q, want %q", w.name, got, w.val)
		}
	}
	var v, rows int
	if err := s.db.QueryRow(`SELECT max(version), count(*) FROM store_schema_version`).Scan(&v, &rows); err != nil || v != len(migrations) || rows != 1 {
		t.Fatalf("version %d rows %d err %v", v, rows, err)
	}
}

func TestOpenRefusals(t *testing.T) {
	ctx := context.Background()
	if s, err := Open(ctx, ""); err == nil {
		s.Close()
		t.Fatal("empty path accepted")
	}
	if s, err := Open(ctx, ":memory:"); err == nil || !strings.Contains(err.Error(), "journal_mode") {
		if s != nil {
			s.Close()
		}
		t.Fatalf(":memory: err %v", err)
	}
	// A database written by a newer build is refused.
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE store_schema_version SET version = ?`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		if s != nil {
			s.Close()
		}
		t.Fatalf("newer schema: %v", err)
	}
}

// dropMigrationTen returns the statements that take a version-11 database back to version 9: migration 11 undone
// (dropMigrationEleven), then the network table of migration 10 dropped (with its triggers).
func dropMigrationTen() []string {
	return append(dropMigrationEleven(), `DROP TABLE store_network`)
}

// dropMigrationNine returns the statements that take a version-9 database back to the schema of version 8: the
// endpoints table dropped, and the attempts table rebuilt as migration 1 made it (its CREATE TABLE, its index and its
// known-event trigger are cut from migrations[0]; the guards are regenerated). The attempts of main are kept with their
// seq; those of other endpoints have no place in the old table and are lost.
func dropMigrationNine() []string {
	m := migrations[0]
	cut := func(from, to string) string {
		i := strings.Index(m, from)
		if i < 0 {
			panic("setup: migration 1 has no " + from)
		}
		j := strings.Index(m[i:], to)
		if j < 0 {
			panic("setup: migration 1 has no " + to + " after " + from)
		}
		return m[i : i+j+len(to)]
	}
	return []string{`DROP TABLE webhook_endpoints`,
		`ALTER TABLE webhook_attempts RENAME TO webhook_attempts_v9`,
		cut("CREATE TABLE webhook_attempts (", ";"),
		`INSERT INTO webhook_attempts (event_id, seq, at, status, ok) SELECT event_id, seq, at, status, ok FROM webhook_attempts_v9 WHERE endpoint = 'main'`,
		`DROP TABLE webhook_attempts_v9`,
		cut("CREATE INDEX webhook_attempts_ok", ";"),
		immutable("webhook_attempts", "event_id = NEW.event_id AND seq = NEW.seq"),
		cut("CREATE TRIGGER webhook_attempts_known_event", "END;")}
}

// The undo helper is faithful: from a store with attempts, it gives the schema migrations[:8] make, keeps main's
// attempts, and the next Open migrates it to the schema a fresh store has.
func TestDropMigrationNineIsFaithful(t *testing.T) {
	ctx := context.Background()
	s, path, clk := freshStore(t)
	ids := mkEvents(t, s, clk, 2)
	for _, a := range []Attempt{{MainEndpoint, ids[0], t0.Add(time.Minute), 500, false, false}, {MainEndpoint, ids[0], t0.Add(2 * time.Minute), 200, true, false},
		{"other", ids[0], t0.Add(time.Minute), 404, false, false}, {MainEndpoint, ids[1], t0.Add(3 * time.Minute), 0, false, false}} {
		if err := s.AddAttempt(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RegisterEndpoint(ctx, MainEndpoint); err != nil {
		t.Fatal(err)
	}
	fresh, _, _ := freshStore(t)
	want := schemaText(t, fresh.db)
	// The reference: migrations 1 to 8 on an empty database.
	refPath := filepath.Join(t.TempDir(), "ref.db")
	ref, err := openDB(ctx, refPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	for _, q := range append(append([]string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}, migrations[:8]...),
		`INSERT INTO store_schema_version (version) VALUES (8)`) {
		if _, err := ref.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range append(append(dropMigrationTen(), dropMigrationNine()...), `UPDATE store_schema_version SET version = 8`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if got, ref := schemaText(t, s.db), schemaText(t, ref); got != ref {
		t.Fatalf("the undone schema differs from migrations 1 to 8:\n%s\n---\n%s", got, ref)
	}
	if rows := rawAttemptRows(t, s.db, `SELECT 'main', event_id, seq, at, status, ok FROM webhook_attempts`); len(rows) != 3 {
		t.Fatalf("attempts after the undo: %v", rows)
	}
	s.Close()
	s2, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := schemaText(t, s2.db); got != want {
		t.Fatal("the migrated schema differs from a fresh store's")
	}
	got := rawAttemptRows(t, s2.db, `SELECT endpoint, event_id, seq, at, status, ok FROM webhook_attempts`)
	if len(got) != 3 || strings.Contains(strings.Join(got, ";"), "other") {
		t.Fatalf("attempts after the round trip: %v", got)
	}
}

// rawAttemptRows renders the rows of q (six columns) as "a|b|c|d|e|f", ordered by the text.
func rawAttemptRows(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q + ` ORDER BY 1, 2, 3`)
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
	return out
}

// dropMigrationsFourToEight returns the statements that take a database back from version 9 to version 3: migration 9
// undone (dropMigrationNine), then the tables and index of migrations 8, 7, 6, 5 and 4 dropped, children first (their
// triggers go with them).
func dropMigrationsFourToEight() []string {
	out := append(append(dropMigrationTen(), dropMigrationNine()...), `DROP TABLE alert_attempts`, `DROP TABLE alert_hits`, `DROP TABLE alerts`, `DROP TABLE pair`, `DROP INDEX invoices_bolt11_lower`, `DROP TABLE refund_results`, `DROP TABLE refund_sends`, `DROP TABLE refunds`)
	for _, tb := range []string{"monero_pending_settle", "monero_settled", "monero_marks", "monero_cursor", "monero_payments",
		"monero_subaddresses", "order_quote_rates", "order_quotes"} {
		out = append(out, `DROP TABLE `+tb)
	}
	return out
}

// A database at version 1 made by an earlier build of plan 6 (the on-chain tables, no onchain_settled) gains the
// table at Open, and its rows stay.
func TestMigrationTwoUpgradesAVersionOneDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	o := mkChainOrder(t, s, "v1", 1000, t0.Add(time.Hour))
	stmts := append(dropMigrationsFourToEight(), `DROP TABLE onchain_settled`, `UPDATE store_schema_version SET version = 1`)
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("version %d (%d migrations) %v", v, len(migrations), err)
	}
	if _, err := s.OrderByID(ctx, o.ID); err != nil {
		t.Fatalf("the order did not survive: %v", err)
	}
	if !strings.Contains(strings.Join(tableNames(t, s.db), " "), "onchain_settled") {
		t.Fatal("no onchain_settled table")
	}
}

// Migration 3 lets a settled fact be deleted only while its output's current mark is dead (RecordScan reopens such an
// output when it is listed mined): a version 2 database gains the guard at Open, and every other delete, update and
// replace stays refused.
func TestMigrationThreeAllowsOnlyADeadSettledFactToGo(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	o := mkChainOrder(t, s, "v2", 1000, t0.Add(time.Hour))
	mkAddr(t, s, o, 0)
	keys := map[string]byte{"dead": 'a', "mempool": 'b', "mined": 'c', "unmarked": 'd', "dead then mempool": 'e'}
	for _, d := range keys {
		if _, err := s.RecordScan(ctx, []TXO{{TxID: txid(d), Vout: 0, Address: addr(0), AmountSat: 5}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for name, kinds := range map[string][]string{"dead": {MarkDead}, "mempool": {MarkMempool}, "mined": {MarkMined},
		"dead then mempool": {MarkDead, MarkMempool}} {
		for _, k := range kinds {
			m := Mark{TxID: txid(keys[name]), Vout: 0, Kind: k}
			if k == MarkMined {
				m.Height, m.BlockHash = 1, txid('1')
			}
			if err := s.AddMark(ctx, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range keys {
		if _, err := s.AddSettled(ctx, TXOKey{TxID: txid(d)}, 6); err != nil {
			t.Fatal(err)
		}
	}
	// Back to version 2: the old guard, which refuses every delete.
	for _, q := range append(dropMigrationsFourToEight(), `DROP TRIGGER onchain_settled_delete_dead`,
		`CREATE TRIGGER onchain_settled_no_delete BEFORE DELETE ON onchain_settled BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END`,
		`UPDATE store_schema_version SET version = 2`) {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM onchain_settled WHERE txid = ?`, txid('a')); err == nil {
		t.Fatal("version 2 accepted a delete")
	}
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("version %d (%d migrations) %v", v, len(migrations), err)
	}
	for name, d := range keys {
		_, err := s.db.Exec(`DELETE FROM onchain_settled WHERE txid = ?`, txid(d))
		if (err == nil) != (name == "dead") {
			t.Errorf("%s: delete %v", name, err)
		}
	}
	for name, q := range map[string]string{
		"update":  `UPDATE onchain_settled SET height = 8`,
		"replace": `INSERT OR REPLACE INTO onchain_settled VALUES ('` + txid('b') + `', 0, 7, 5)`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("SQL accepted %s", name)
		}
	}
}

// A database at version 3 (built by applying the first three migrations) gains order_quotes, order_quote_rates and
// their triggers (and migration 5) at Open; its orders stay, and have no quote.
func TestMigrationFourAddsQuotes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:3]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (3)`,
		`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('`+
			strings.Repeat("a", 32)+`', '`+strings.Repeat("b", 32)+`', 'acct-1', 'v3', 1000, 0, 0, 5, 6)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(tableNames(t, db)); n != 12 {
		t.Fatalf("version 3 has %d tables", n)
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil || v != 11 || len(migrations) != 11 {
		t.Fatalf("version %d (%d migrations) %v", v, len(migrations), err)
	}
	if got := triggersOf(t, s.db, "order_quotes"); got != "order_quotes_known_order order_quotes_no_delete order_quotes_no_replace order_quotes_no_update" {
		t.Fatalf("triggers %q", got)
	}
	if got := triggersOf(t, s.db, "order_quote_rates"); got != "order_quote_rates_known_quote order_quote_rates_no_delete order_quote_rates_no_replace order_quote_rates_no_update" {
		t.Fatalf("rate triggers %q", got)
	}
	o, err := s.OrderByID(ctx, strings.Repeat("a", 32))
	if err != nil {
		t.Fatalf("the order did not survive: %v", err)
	}
	if _, ok, err := s.QuoteForOrder(ctx, o.ID); err != nil || ok {
		t.Fatalf("an old order has a quote: %v %v", ok, err)
	}
	mkQuotedOrder(t, s, "v4", 1000, 0, t0.Add(time.Hour), testQuote())
	// The upgraded schema is the one a fresh store makes, statement for statement.
	fresh, _, _ := freshStore(t)
	if got, want := schemaText(t, s.db), schemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
}

// triggersOf returns the names of the triggers on table, sorted and joined by spaces.
func triggersOf(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND tbl_name = ? ORDER BY name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return strings.Join(out, " ")
}

// A database at version 4 (the first four migrations) gains the Monero tables and their triggers at Open, the
// settled table with the dead-mark delete rule; its orders and quotes stay, and the result is a fresh store's schema.
func TestMigrationFiveAddsMonero(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:4]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (4)`,
		`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('`+
			strings.Repeat("a", 32)+`', '`+strings.Repeat("b", 32)+`', 'acct-1', 'v4', 0, 0, 7000, 5, 6)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(tableNames(t, db)); n != 14 {
		t.Fatalf("version 4 has %d tables", n)
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil || v != 11 || len(migrations) != 11 {
		t.Fatalf("version %d (%d migrations) %v", v, len(migrations), err)
	}
	for tb, want := range map[string]string{
		"monero_subaddresses":   "monero_subaddresses_index_increases monero_subaddresses_known_order monero_subaddresses_no_delete monero_subaddresses_no_replace monero_subaddresses_no_update monero_subaddresses_order_monero",
		"monero_payments":       "monero_payments_known_subaddress monero_payments_no_delete monero_payments_no_replace monero_payments_no_update",
		"monero_marks":          "monero_marks_known_payment monero_marks_no_delete monero_marks_no_replace monero_marks_no_update",
		"monero_cursor":         "monero_cursor_no_delete monero_cursor_no_replace monero_cursor_no_update",
		"monero_settled":        "monero_settled_delete_dead monero_settled_known_payment monero_settled_no_replace monero_settled_no_update",
		"monero_pending_settle": "monero_pending_settle_known_order monero_pending_settle_no_replace monero_pending_settle_no_update",
	} {
		if got := triggersOf(t, s.db, tb); got != want {
			t.Errorf("%s triggers %q", tb, got)
		}
	}
	o, err := s.OrderByID(ctx, strings.Repeat("a", 32))
	if err != nil || o.AmountPiconero != 7000 {
		t.Fatalf("the order did not survive: %+v %v", o, err)
	}
	fresh, _, _ := freshStore(t)
	if got, want := schemaText(t, s.db), schemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
}

// Ruling R5: a version-5 store made by an early plan 8 build before monero_cursor gained wallet_height (migration 5
// was edited in place, no release existed) is refused at Open with ErrOldMoneroSchema, as a store from before plan 8
// is refused with ErrOldSchema; nothing is migrated or written. Opening it used to succeed
// and fail later in MoneroCursor with "no such column: wallet_height".
func TestOldMoneroCursorGuard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	const walletHeight = "\twallet_height INTEGER NOT NULL CHECK (wallet_height BETWEEN 0 AND height),\n"
	if strings.Count(migrations[4], walletHeight) != 1 {
		t.Fatal("setup: migration 5 has no wallet_height line to remove")
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:4]...)
	stmts = append(stmts, strings.Replace(migrations[4], walletHeight, "", 1), `INSERT INTO store_schema_version (version) VALUES (5)`,
		`INSERT INTO monero_cursor (seq, height, at) VALUES (1, 200, 5)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := schemaText(t, db)
	db.Close()
	s, err := Open(ctx, path)
	if err == nil {
		_, _, cerr := s.MoneroCursor(ctx)
		s.Close()
		t.Fatalf("the old version-5 store opened (MoneroCursor: %v)", cerr)
	}
	if !errors.Is(err, ErrOldMoneroSchema) {
		t.Fatalf("err = %v, want ErrOldMoneroSchema", err)
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

// schemaText returns every entry of sqlite_master (type, name, table and SQL), in a fixed order.
func schemaText(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, tbl, text string
		if err := rows.Scan(&typ, &name, &tbl, &text); err != nil {
			t.Fatal(err)
		}
		b.WriteString(typ + " " + name + " " + tbl + "\n" + text + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a?b#c%d.db", "%3Fx.db", "sp ace.db", "q?_pragma=journal_mode(delete).db"} {
		path := filepath.Join(dir, name)
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		s.Close()
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
}

// fixture fills every table with one row (order_quote_rates with two) and returns the order, which has all three
// rails and is priced with testQuote2.
func fixture(t *testing.T, s *Store, clk *testClock) Order {
	t.Helper()
	ctx := context.Background()
	o := mkQuoted(t, s, Order{AccountID: "acct-1", MerchantRef: "fx", AmountMsat: 4000, AmountSat: 900, AmountPiconero: 7000,
		ExpiresAt: t0.Add(time.Hour)}, testQuote2())
	inv := mkInvoice(t, s, o, 'a', t0.Add(20*time.Minute))
	clk.set(inv.ExpiresAt)
	if _, err := s.CloseInvoice(ctx, inv.PaymentHash, inv.ExpiresAt, CloseLookup); err != nil {
		t.Fatal(err)
	}
	if err := s.AddInvoiceAttempt(ctx, o.ID, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAttempt(ctx, Attempt{Endpoint: MainEndpoint, EventID: EventID(EventPaid, o.ID, 1), Status: 500, Final: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterEndpoint(ctx, MainEndpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.BindNetwork(ctx, "regtest"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccountPaid(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	mkAddr(t, s, o, 0)
	if _, err := s.RecordScan(ctx, []TXO{{TxID: txid('a'), Vout: 0, Address: addr(0), AmountSat: 900}}, Cursor{BlockHash: txid('1'), Height: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMark(ctx, Mark{TxID: txid('a'), Vout: 0, Kind: MarkMempool}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSettled(ctx, TXOKey{TxID: txid('a'), Vout: 0}, 6); err != nil {
		t.Fatal(err)
	}
	mkSub(t, s, o, 1)
	if _, err := s.RecordMoneroScan(ctx, []Payment{{TxID: txid('b'), Address: sub(1), AmountPiconero: 7000}}, MoneroCursor{Height: 40, WalletHeight: 40}); err != nil {
		t.Fatal(err)
	}
	pk := PaymentKey{TxID: txid('b'), Address: sub(1)}
	if err := s.AddMoneroMark(ctx, MoneroMark{TxID: pk.TxID, Address: pk.Address, Kind: MarkMined, Height: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMoneroSettled(ctx, pk, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitMoneroPass(ctx, nil, MoneroCursor{Height: 40, WalletHeight: 40}, nil, []string{o.ID}); err != nil {
		t.Fatal(err)
	}
	r := mkRefund(t, s, o, 'a', 1000)
	if err := s.AddRefundSend(ctx, r.ID, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRefundResult(ctx, RefundResult{RefundID: r.ID, Outcome: RefundOutcomeSettled, Reason: RefundReasonSettled, At: t0}); err != nil {
		t.Fatal(err)
	}
	// An alert with one hit and one failed attempt.
	al := Alert{Message: "ALERT: x", Fields: "{}"}
	for range 2 {
		if _, err := s.AddAlert(ctx, al); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: AlertID(al.Message, al.Fields, clk.now()), Status: 500}); err != nil {
		t.Fatal(err)
	}
	return o
}

// tableNames lists the insert-only tables: every table but store_schema_version and pair, the two whose one row is
// updated (pair's guards have their own tests, in pair_test.go).
func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('store_schema_version', 'pair') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestEveryTableStrictWithoutRowid(t *testing.T) {
	s, _, _ := freshStore(t)
	names := tableNames(t, s.db)
	if len(names) != 29 {
		t.Fatalf("tables %v: a new table needs this test and the raw-connection test reviewed", names)
	}
	for _, n := range names {
		var sqlText string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = ?`, n).Scan(&sqlText); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sqlText, "STRICT, WITHOUT ROWID") {
			t.Errorf("%s is not STRICT, WITHOUT ROWID", n)
		}
	}
}

// On a connection with none of our pragmas (an operator's sqlite3 shell), UPDATE, DELETE and the
// REPLACE forms are refused for every table, and a row that breaks a CHECK or names a missing
// parent is refused too. The one exception is DELETE on monero_pending_settle, the store's work
// queue (ruling S4): its rows are deleted once their order has settled, and the same on webhook_abandoned, the outbox
// of the give-up lines: its rows are deleted once the line is logged.
func TestTriggersOnRawConnection(t *testing.T) {
	s, path, clk := freshStore(t)
	fixture(t, s, clk)
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	count := func(table string) int {
		var n int
		if err := raw.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tb := range tableNames(t, raw) {
		before := count(tb)
		if want := map[bool]int{true: 2, false: 1}[tb == "order_quote_rates"]; before != want {
			t.Fatalf("%s: fixture has %d rows, want %d", tb, before, want)
		}
		var col string
		var dummy1, dummy2, dummy3, dummy4 any
		var cid int
		if err := raw.QueryRow(`SELECT * FROM pragma_table_info(?) LIMIT 1`, tb).Scan(&cid, &col, &dummy1, &dummy2, &dummy3, &dummy4); err != nil {
			t.Fatal(err)
		}
		queue := tb == "monero_pending_settle" || tb == "webhook_abandoned"
		for _, q := range []string{
			`UPDATE ` + tb + ` SET ` + col + ` = ` + col,
			`DELETE FROM ` + tb,
			`INSERT OR REPLACE INTO ` + tb + ` SELECT * FROM ` + tb,
			`REPLACE INTO ` + tb + ` SELECT * FROM ` + tb,
			`INSERT INTO ` + tb + ` SELECT * FROM ` + tb + ` ON CONFLICT DO UPDATE SET ` + col + ` = ` + col,
		} {
			if queue && strings.HasPrefix(q, "DELETE") {
				continue
			}
			if _, err := raw.Exec(q); err == nil {
				t.Errorf("%q succeeded", q)
			}
		}
		if count(tb) != before {
			t.Errorf("%s: %d rows after the refused statements, want %d", tb, count(tb), before)
		}
		if queue {
			if _, err := raw.Exec(`DELETE FROM ` + tb); err != nil || count(tb) != 0 {
				t.Errorf("%s: a delete of the work queue: %v, %d rows left", tb, err, count(tb))
			}
		}
	}
	// Bad data and missing parents, without foreign keys or any other pragma.
	ok := strings.Repeat("a", 32)
	h := strings.Repeat("b", 64)
	// For the Monero cases: a Monero-only order without a subaddress, a Lightning-only order, and a second payment to
	// the fixture's subaddress, neither marked nor settled. So that the CHECK cases of other tables reach their CHECK
	// rather than a parent-must-exist trigger: an open invoice hi of ln, an order pa of an account with no paid
	// order, and a second output of the fixture's transaction (vout 1) with no settled fact.
	mo, ln, hx, hp := strings.Repeat("e", 32), strings.Repeat("8", 32), strings.Repeat("d", 64), strings.Repeat("e", 64)
	hi, pa := strings.Repeat("c", 64), strings.Repeat("7", 32)
	for _, q := range []string{
		`INSERT INTO orders VALUES ('` + mo + `', '` + strings.Repeat("f", 32) + `', 'a', 'monero-raw', 0, 0, 7000, 5, 6)`,
		`INSERT INTO orders VALUES ('` + ln + `', '` + strings.Repeat("9", 32) + `', 'a', 'ln-raw', 1000, 0, 0, 5, 6)`,
		`INSERT INTO monero_payments VALUES ('` + hp + `', '` + sub(1) + `', 5, 0, 5, 0)`,
		`INSERT INTO invoices VALUES ('` + hi + `', '` + ln + `', 'ln', 1000, 5, 6)`,
		`INSERT INTO orders VALUES ('` + pa + `', '` + strings.Repeat("6", 32) + `', 'acct-3', 'paid-raw', 1000, 0, 0, 5, 6)`,
		`INSERT INTO onchain_txos SELECT txid, 1, address, amount_sat, first_seen FROM onchain_txos WHERE vout = 0`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Each refused statement must fail with its guard's own text: a trigger's RAISE text, a STRICT type error, or the
	// CHECK's expression (chk + its start), so a case refused by another guard, or a syntax error, fails the test. A
	// BEFORE INSERT trigger fires before the CHECKs, so a few cases pin the trigger that shadows a CHECK on this path
	// (the bad account or order id of account_paid, an upper-case txid of onchain_settled, a bad address of
	// monero_payments, the bad order id and the index of monero_subaddresses); TestMoneroGuardsStandAlone tests those
	// CHECKs with the triggers dropped. Of monero_subaddresses' triggers, the order-has-Monero one fires first.
	const chk = "CHECK constraint failed: "
	refused := func(name, q, want string) {
		t.Helper()
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("%s succeeded", name)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error with %q", name, err, want)
		}
	}
	for name, c := range map[string]struct{ q, want string }{
		"order bad account":      {`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a b', 'r', 1, 0, 0, 5, 6)`, chk + "typeof(account_id) = 'text'"},
		"order amount 0":         {`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', 'r', 0, 0, 0, 5, 6)`, chk + "amount_msat > 0 OR amount_sat > 0 OR amount_piconero > 0"},
		"order NUL account":      {`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a' || char(0) || '!', 'r', 1, 0, 0, 5, 6)`, chk + "typeof(account_id) = 'text'"},
		"order upper id":         {`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + strings.Repeat("A", 32) + `', '` + strings.Repeat("c", 32) + `', 'a', 'r', 1, 0, 0, 5, 6)`, chk + "typeof(id) = 'text'"},
		"order expiry":           {`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', 'r', 1, 0, 0, 5, 5)`, chk + "expires_at > 0 AND expires_at > created_at"},
		"order ref blob":         {`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', x'6162', 1, 0, 0, 5, 6)`, "cannot store BLOB value in TEXT column orders.merchant_ref"},
		"order token dup":        {`INSERT INTO orders SELECT '` + ok + `', token, 'a', 'zz', 1, 0, 0, 5, 6 FROM orders WHERE merchant_ref = 'fx'`, "store rows are immutable"},
		"order piconero 2^53":    {`INSERT INTO orders VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', 'r', 0, 0, 9007199254740992, 5, 6)`, chk + "amount_piconero = 0 OR amount_piconero BETWEEN 1 AND 9007199254740991"},
		"order piconero -1":      {`INSERT INTO orders VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', 'r', 1, 0, -1, 5, 6)`, chk + "amount_piconero = 0 OR amount_piconero BETWEEN 1 AND 9007199254740991"},
		"order piconero text":    {`INSERT INTO orders VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', 'r', 0, 0, '7x', 5, 6)`, "cannot store TEXT value in INTEGER column orders.amount_piconero"},
		"order ref dup":          {`INSERT INTO orders SELECT '` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', merchant_ref, 1, 0, 0, 5, 6 FROM orders WHERE merchant_ref = 'fx'`, "store rows are immutable"},
		"invoice no order":       {`INSERT INTO invoices VALUES ('` + h + `', '` + ok + `', 'ln', 1, 5, 6)`, "unknown order"},
		"invoice amount differs": {`INSERT INTO invoices SELECT '` + h + `', id, 'ln', amount_msat + 1, 5, 6 FROM orders WHERE merchant_ref = 'fx'`, "invoice amount differs from the order"},
		"invoice amount 0":       {`INSERT INTO invoices VALUES ('` + h + `', '` + mo + `', 'ln', 0, 5, 6)`, chk + "amount_msat BETWEEN 1 AND 9007199254740992"},
		"invoice space":          {`INSERT INTO invoices SELECT '` + h + `', id, 'l n', amount_msat, 5, 6 FROM orders WHERE merchant_ref = 'fx'`, chk + "typeof(bolt11) = 'text'"},
		"closed no invoice":      {`INSERT INTO closed_invoices VALUES ('` + h + `', 100, 'lookup')`, "unknown invoice"},
		"closed bad reason":      {`INSERT INTO closed_invoices VALUES ('` + hi + `', 100, 'paid')`, chk + "reason IN ('lookup', 'deadline')"},
		"attempt no order":       {`INSERT INTO invoice_attempts VALUES ('` + ok + `', '` + h[:32] + `', 5, 1)`, "unknown order"},
		"event no order":         {`INSERT INTO events VALUES ('` + ok + `', 'order.paid', '` + h[:32] + `', 1, 5)`, "unknown order"},
		"event bad type":         {`INSERT INTO events SELECT '` + ok + `', 'order.refunded', id, 1, 5 FROM orders WHERE merchant_ref = 'fx'`, chk + "type IN ('order.paid'"},
		"event dup type":         {`INSERT INTO events SELECT '` + ok + `', 'order.paid', order_id, 1, 5 FROM events`, "store rows are immutable"},
		"webhook no event":       {`INSERT INTO webhook_attempts VALUES ('main', '` + ok + `', 1, 5, 200, 1)`, "unknown event"},
		"webhook status":         {`INSERT INTO webhook_attempts SELECT endpoint, event_id, 2, 5, 1000, 0 FROM webhook_attempts`, chk + "status BETWEEN 0 AND 999"},
		"abandoned no event":     {`INSERT INTO webhook_abandoned VALUES ('main', '` + ok + `', 5)`, "unknown event"},
		"abandoned endpoint":     {`INSERT INTO webhook_abandoned SELECT 'a b', id, 5 FROM events`, chk + "typeof(endpoint) = 'text'"},
		"abandoned time 0":       {`INSERT INTO webhook_abandoned SELECT 'x', id, 0 FROM events`, chk + "at > 0"},
		"webhook ok":             {`INSERT INTO webhook_attempts SELECT endpoint, event_id, 2, 5, 200, 2 FROM webhook_attempts`, chk + "ok IN (0, 1)"},
		"paid no order":          {`INSERT INTO account_paid VALUES ('acct-2', '` + ok + `', 5)`, "unknown order for the account"},
		"paid other account":     {`INSERT INTO account_paid SELECT 'acct-2', id, 5 FROM orders WHERE merchant_ref = 'fx'`, "unknown order for the account"},
		"paid bad account":       {`INSERT INTO account_paid SELECT 'a b', id, 5 FROM orders WHERE merchant_ref = 'fx'`, "unknown order for the account"},
		"paid bad order id":      {`INSERT INTO account_paid VALUES ('acct-2', 'x', 5)`, "unknown order for the account"},
		"paid time 0":            {`INSERT INTO account_paid VALUES ('acct-3', '` + pa + `', 0)`, chk + "created_at > 0"},
		"settled no output":      {`INSERT INTO onchain_settled VALUES ('` + h + `', 0, 6, 5)`, "unknown output"},
		"settled height 0":       {`INSERT INTO onchain_settled SELECT txid, 1, 0, 5 FROM onchain_txos WHERE vout = 1`, chk + "height >= 1"},
		"settled upper txid":     {`INSERT INTO onchain_settled VALUES ('` + strings.Repeat("A", 64) + `', 0, 6, 5)`, "unknown output"},
		"settled time 0":         {`INSERT INTO onchain_settled SELECT txid, 1, 6, 0 FROM onchain_txos WHERE vout = 1`, chk + "at > 0"},
		// Monero. The subaddress cases use mo, a Monero-only order with no subaddress, so each reaches the check it is
		// named for; hx is a txid with no Monero row, and hp the txid of a stored payment with no mark and no settled
		// fact (a case that reused the fixture's key would be refused by the replace guard instead).
		"sub index 0":           {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + mo + `', 0, 5)`, "subaddress index must exceed every stored index"},
		"sub index not above":   {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + mo + `', 1, 5)`, "subaddress index must exceed every stored index"},
		"sub no order":          {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + ok + `', 2, 5)`, "the order has no Monero amount"},
		"sub order no Monero":   {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + ln + `', 2, 5)`, "the order has no Monero amount"},
		"sub index 2^31":        {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + mo + `', 2147483648, 5)`, chk + "idx BETWEEN 1 AND 2147483647"},
		"sub index -1":          {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + mo + `', -1, 5)`, "subaddress index must exceed every stored index"},
		"sub primary address":   {`INSERT INTO monero_subaddresses VALUES ('4` + sub(2)[1:] + `', '` + mo + `', 2, 5)`, chk + "typeof(address) = 'text'"},
		"sub 94 bytes":          {`INSERT INTO monero_subaddresses VALUES ('` + sub(2)[1:] + `', '` + mo + `', 2, 5)`, chk + "typeof(address) = 'text'"},
		"sub 96 bytes":          {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `u', '` + mo + `', 2, 5)`, chk + "typeof(address) = 'text'"},
		"sub letter l":          {`INSERT INTO monero_subaddresses VALUES ('` + sub(2)[:94] + `l', '` + mo + `', 2, 5)`, chk + "typeof(address) = 'text'"},
		"sub NUL":               {`INSERT INTO monero_subaddresses VALUES ('` + sub(2)[:94] + `' || char(0), '` + mo + `', 2, 5)`, chk + "typeof(address) = 'text'"},
		"sub bad order id":      {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + strings.ToUpper(mo) + `', 2, 5)`, "the order has no Monero amount"},
		"sub time 0":            {`INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + mo + `', 2, 0)`, chk + "created_at > 0"},
		"sub order taken":       {`INSERT INTO monero_subaddresses SELECT '` + sub(2) + `', order_id, 2, 5 FROM monero_subaddresses`, "store rows are immutable"},
		"sub address taken":     {`INSERT INTO monero_subaddresses VALUES ('` + sub(1) + `', '` + mo + `', 2, 5)`, "store rows are immutable"},
		"payment no sub":        {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(2) + `', 1, 0, 5, 0)`, "unknown subaddress"},
		"payment 0":             {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1) + `', 0, 0, 5, 0)`, chk + "amount_piconero BETWEEN 1 AND 9007199254740991"},
		"payment 2^53":          {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1) + `', 9007199254740992, 0, 5, 0)`, chk + "amount_piconero BETWEEN 1 AND 9007199254740991"},
		"payment unlock -1":     {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1) + `', 1, -1, 5, 0)`, chk + "unlock_time >= 0"},
		"payment height -1":     {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1) + `', 1, 0, 5, -1)`, chk + "first_seen_height >= 0"},
		"payment time 0":        {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1) + `', 1, 0, 0, 0)`, chk + "first_seen > 0"},
		"payment upper txid":    {`INSERT INTO monero_payments VALUES ('` + strings.Repeat("D", 64) + `', '` + sub(1) + `', 1, 0, 5, 0)`, chk + "typeof(txid) = 'text'"},
		"payment bad address":   {`INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1)[:94] + `l', 1, 0, 5, 0)`, "unknown subaddress"},
		"mark no payment":       {`INSERT INTO monero_marks VALUES ('` + hx + `', '` + sub(1) + `', 1, 'dead', 0, 5, 0)`, "unknown payment"},
		"mark mined height 0":   {`INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 1, 'mined', 0, 5, 0)`, chk + "((kind = 'mined' AND height >= 1 AND"},
		"mark dead height 1":    {`INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 1, 'dead', 1, 5, 0)`, chk + "((kind = 'mined' AND height >= 1 AND"},
		"mark mempool height 1": {`INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 1, 'mempool', 1, 5, 0)`, chk + "((kind = 'mined' AND height >= 1 AND"},
		"mark bad kind":         {`INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 1, 'gone', 0, 5, 0)`, chk + "kind IN ('mempool', 'mined', 'dead')"},
		"mark seq 0":            {`INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 0, 'dead', 0, 5, 0)`, chk + "seq >= 1"},
		"mark time 0":           {`INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 1, 'dead', 0, 0, 0)`, chk + "at > 0"},
		"cursor height -1":      {`INSERT INTO monero_cursor VALUES (2, -1, 0, 5)`, chk + "height >= 0"},
		"cursor seq 0":          {`INSERT INTO monero_cursor VALUES (0, 1, 1, 5)`, chk + "seq >= 1"},
		"cursor time 0":         {`INSERT INTO monero_cursor VALUES (2, 1, 1, 0)`, chk + "at > 0"},
		"cursor wallet -1":      {`INSERT INTO monero_cursor VALUES (2, 1, -1, 5)`, chk + "wallet_height BETWEEN 0 AND height"},
		"cursor wallet above":   {`INSERT INTO monero_cursor VALUES (2, 1, 2, 5)`, chk + "wallet_height BETWEEN 0 AND height"},
		"monero settled no pay": {`INSERT INTO monero_settled VALUES ('` + hx + `', '` + sub(1) + `', 6, 5)`, "unknown payment"},
		"monero settled h 0":    {`INSERT INTO monero_settled VALUES ('` + hp + `', '` + sub(1) + `', 0, 5)`, chk + "height >= 1"},
		"monero settled time 0": {`INSERT INTO monero_settled VALUES ('` + hp + `', '` + sub(1) + `', 6, 0)`, chk + "at > 0"},
		"pending no order":      {`INSERT INTO monero_pending_settle VALUES ('` + strings.Repeat("d", 32) + `', 5)`, "unknown order"},
		"pending time 0":        {`INSERT INTO monero_pending_settle VALUES ('` + mo + `', 0)`, chk + "at > 0"},
		"pending upper id":      {`INSERT INTO monero_pending_settle VALUES ('` + strings.ToUpper(mo) + `', 5)`, "unknown order"},
		"alert no prefix":       {`INSERT INTO alerts VALUES ('` + ok + `', 'alert: x', '{}', 0, 5)`, chk + "typeof(message) = 'text'"},
		"alert control char":    {`INSERT INTO alerts VALUES ('` + ok + `', 'ALERT: ' || char(10), '{}', 0, 5)`, chk + "typeof(message) = 'text'"},
		"alert fields array":    {`INSERT INTO alerts VALUES ('` + ok + `', 'ALERT: y', '[]', 0, 5)`, chk + "typeof(fields) = 'text'"},
		"alert fields bad json": {`INSERT INTO alerts VALUES ('` + ok + `', 'ALERT: y', '{"a"}', 0, 5)`, chk + "typeof(fields) = 'text'"},
		"alert dropped -1":      {`INSERT INTO alerts VALUES ('` + ok + `', 'ALERT: y', '{}', -1, 5)`, chk + "dropped BETWEEN 0 AND"},
		"hit no alert":          {`INSERT INTO alert_hits VALUES ('` + ok + `', 1, 5)`, "unknown alert"},
		"hit seq 100":           {`INSERT INTO alert_hits SELECT alert_id, 100, 5 FROM alert_hits`, chk + "seq BETWEEN 1 AND 99"},
		"alert attempt orphan":  {`INSERT INTO alert_attempts VALUES ('` + ok + `', 1, 5, 200, 1)`, "unknown alert"},
		"alert attempt status":  {`INSERT INTO alert_attempts SELECT alert_id, 2, 5, 1000, 0 FROM alert_attempts`, chk + "status BETWEEN 0 AND 999"},
		"rate no quote":         {`INSERT INTO order_quote_rates VALUES ('` + strings.Repeat("d", 32) + `', 'BTC', '1', '[{"provider":"kraken","rate":"1","at":5}]')`, "unknown quote"},
	} {
		refused(name, c.q, c.want)
	}
	// The positive controls of the Monero cases: the same rows with the one bad value put right are stored. The
	// largest index, 2^31 − 1, is accepted.
	for name, q := range map[string]string{
		"sub index 2^31 - 1": `INSERT INTO monero_subaddresses VALUES ('` + sub(2) + `', '` + mo + `', 2147483647, 5)`,
		"payment 2^53 - 1":   `INSERT INTO monero_payments VALUES ('` + hx + `', '` + sub(1) + `', 9007199254740991, 0, 5, 0)`,
		"mark mined":         `INSERT INTO monero_marks VALUES ('` + hp + `', '` + sub(1) + `', 1, 'mined', 1, 5, 0)`,
		"cursor":             `INSERT INTO monero_cursor VALUES (2, 1, 1, 5)`,
		"monero settled":     `INSERT INTO monero_settled VALUES ('` + hp + `', '` + sub(1) + `', 1, 5)`,
		"pending":            `INSERT INTO monero_pending_settle VALUES ('` + mo + `', 5)`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Errorf("%s on the raw connection: %v", name, err)
		}
	}
	// A close before the invoice's time, on the raw connection.
	if _, err := raw.Exec(`INSERT INTO invoices SELECT '` + h + `', id, 'ln', amount_msat, ` + itoa(t0.Unix()) + `, ` + itoa(t0.Unix()+100) + ` FROM orders WHERE merchant_ref = 'fx'`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO closed_invoices VALUES ('` + h + `', ` + itoa(t0.Unix()+99) + `, 'lookup')`,
		`INSERT INTO closed_invoices VALUES ('` + h + `', ` + itoa(t0.Unix()+100+86399) + `, 'deadline')`,
	} {
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("%q succeeded", q)
		}
	}
	if _, err := raw.Exec(`INSERT INTO closed_invoices VALUES ('` + h + `', ` + itoa(t0.Unix()+100) + `, 'lookup')`); err != nil {
		t.Errorf("a valid close on the raw connection: %v", err)
	}
	// A quote that breaks a CHECK or names a missing order, for an order that has none yet; then a valid one.
	if _, err := raw.Exec(`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('` + ok + `', '` + strings.Repeat("c", 32) + `', 'a', 'r2', 1, 0, 0, 5, 6)`); err != nil {
		t.Fatal(err)
	}
	quote := func(id, price, currency, margin, at string) string {
		return `INSERT INTO order_quotes VALUES ('` + id + `', ` + price + `, ` + currency + `, ` + margin + `, ` + at + `)`
	}
	for name, c := range map[string]struct{ q, want string }{
		"quote no order":        {quote(strings.Repeat("d", 32), `'1'`, `'USD'`, `0`, `5`), "unknown order"},
		"quote bad order id":    {quote("x", `'1'`, `'USD'`, `0`, `5`), "unknown order"},
		"quote price 1.":        {quote(ok, `'1.'`, `'USD'`, `0`, `5`), chk + "typeof(price) = 'text'"},
		"quote price .5":        {quote(ok, `'.5'`, `'USD'`, `0`, `5`), chk + "typeof(price) = 'text'"},
		"quote price NUL":       {quote(ok, `'1' || char(0)`, `'USD'`, `0`, `5`), chk + "typeof(price) = 'text'"},
		"quote price blob":      {quote(ok, `x'31'`, `'USD'`, `0`, `5`), "cannot store BLOB value in TEXT column order_quotes.price"},
		"quote price 21 digits": {quote(ok, `'`+strings.Repeat("1", 21)+`'`, `'USD'`, `0`, `5`), chk + "typeof(price) = 'text'"},
		"quote currency BTC":    {quote(ok, `'1'`, `'BTC'`, `0`, `5`), chk + "typeof(currency) = 'text'"},
		"quote currency XMR":    {quote(ok, `'1'`, `'XMR'`, `0`, `5`), chk + "typeof(currency) = 'text'"},
		"quote currency usd":    {quote(ok, `'1'`, `'usd'`, `0`, `5`), chk + "typeof(currency) = 'text'"},
		"quote margin 5001":     {quote(ok, `'1'`, `'USD'`, `5001`, `5`), chk + "margin_bps BETWEEN -5000 AND 5000"},
		"quote margin -5001":    {quote(ok, `'1'`, `'USD'`, `-5001`, `5`), chk + "margin_bps BETWEEN -5000 AND 5000"},
		"quote time 0":          {quote(ok, `'1'`, `'USD'`, `0`, `0`), chk + "quoted_at > 0"},
	} {
		refused(name, c.q, c.want)
	}
	if _, err := raw.Exec(quote(ok, `'1'`, `'USD'`, `5000`, `5`)); err != nil {
		t.Errorf("a valid quote on the raw connection: %v", err)
	}
	// Its rate rows: a row that breaks a CHECK, then a valid one, then the same asset again.
	src := `'[{"provider":"kraken","rate":"1","at":5}]'`
	rate := func(id, asset, r, sources string) string {
		return `INSERT INTO order_quote_rates VALUES ('` + id + `', ` + asset + `, ` + r + `, ` + sources + `)`
	}
	for name, c := range map[string]struct{ q, want string }{
		"rate unknown quote":  {rate(strings.Repeat("d", 32), `'BTC'`, `'1'`, src), "unknown quote"},
		"rate bad order id":   {rate("x", `'BTC'`, `'1'`, src), "unknown quote"},
		"rate asset ETH":      {rate(ok, `'ETH'`, `'1'`, src), chk + "asset IN ('BTC', 'XMR')"},
		"rate asset btc":      {rate(ok, `'btc'`, `'1'`, src), chk + "asset IN ('BTC', 'XMR')"},
		"rate two points":     {rate(ok, `'BTC'`, `'1.2.3'`, src), chk + "typeof(rate) = 'text'"},
		"rate blob":           {rate(ok, `'BTC'`, `x'31'`, src), "cannot store BLOB value in TEXT column order_quote_rates.rate"},
		"rate sources object": {rate(ok, `'BTC'`, `'1'`, `'{"provider":"kraken"}'`), chk + "json_valid(sources)"},
		"rate sources broken": {rate(ok, `'BTC'`, `'1'`, `'[{'`), chk + "json_valid(sources)"},
		"rate sources empty":  {rate(ok, `'BTC'`, `'1'`, `'[]'`), chk + "json_valid(sources)"},
		"rate sources nine":   {rate(ok, `'BTC'`, `'1'`, `'[1,2,3,4,5,6,7,8,9]'`), chk + "json_valid(sources)"},
		"rate sources long":   {rate(ok, `'BTC'`, `'1'`, `'["`+strings.Repeat("a", 4093)+`"]'`), chk + "json_valid(sources)"},
	} {
		refused(name, c.q, c.want)
	}
	if _, err := raw.Exec(rate(ok, `'BTC'`, `'1'`, `'["`+strings.Repeat("a", 4092)+`"]'`)); err != nil {
		t.Errorf("a valid rate on the raw connection: %v", err)
	}
	if _, err := raw.Exec(rate(ok, `'BTC'`, `'2'`, src)); err == nil {
		t.Error("a second BTC rate for one quote succeeded")
	}
	if _, err := raw.Exec(rate(ok, `'XMR'`, `'2'`, src)); err != nil {
		t.Errorf("an XMR rate beside the BTC one: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: t0}
	s, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	o := fixture(t, s, clk)
	plain := mkOrder(t, s, "plain", 2500, t0.Add(time.Hour))
	s.Close()

	s2, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	verifyFixture(t, s2, o)
	// An explicit order's idempotency survives the restart too: same reference, account and amounts, same order.
	if again, created, err := s2.CreateOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "plain", AmountMsat: 2500, ExpiresAt: t0.Add(2 * time.Hour)}); err != nil || created || again != plain {
		t.Fatalf("explicit after restart: %+v %v %v", again, created, err)
	}
	// The idempotency survives the restart: same reference, price, currency and rails, same order and rate back.
	moved := testQuote2()
	moved.Rates[1].Rate, moved.Rates[1].Sources = "1", moved.Rates[1].Sources[:1]
	again, created, err := s2.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "fx", AmountMsat: 1, AmountSat: 1, AmountPiconero: 1,
		ExpiresAt: t0.Add(time.Hour)}, moved)
	if err != nil || created || again != o {
		t.Fatalf("after restart: %+v %v %v", again, created, err)
	}
	// Without the Monero rail it is another rail set: a conflict.
	if _, _, err := s2.CreateQuotedOrder(ctx, Order{AccountID: "acct-1", MerchantRef: "fx", AmountMsat: 1, AmountSat: 1,
		ExpiresAt: t0.Add(time.Hour)}, testQuote()); !errors.Is(err, ErrConflict) {
		t.Fatalf("after restart, without Monero: %v", err)
	}
}

func verifyFixture(t *testing.T, s *Store, o Order) {
	t.Helper()
	ctx := context.Background()
	got, err := s.OrderByToken(ctx, o.Token)
	if err != nil || got != o {
		t.Fatalf("order: %+v %v", got, err)
	}
	if inv, _ := s.InvoicesForOrder(ctx, o.ID); len(inv) != 1 {
		t.Fatalf("invoices %+v", inv)
	}
	if open, _ := s.OpenInvoices(ctx); len(open) != 0 {
		t.Fatalf("open %+v", open)
	}
	if n, m, _ := s.AttemptsSince(ctx, t0); n != 1 || m != 4000 {
		t.Fatalf("invoice attempts %d %d", n, m)
	}
	if a, _ := s.AttemptsFor(ctx, MainEndpoint, EventID(EventPaid, o.ID, 1)); len(a) != 1 || a[0].Status != 500 {
		t.Fatalf("attempts %+v", a)
	}
	if added, err := s.AddEvent(ctx, Event{Type: EventPaid, OrderID: o.ID}); err != nil || added {
		t.Fatalf("event again: %v %v", added, err)
	}
	q, ok, err := s.QuoteForOrder(ctx, o.ID)
	if err != nil || !ok {
		t.Fatalf("quote: %v %v", ok, err)
	}
	sameQuote(t, q, stored(testQuote2(), o.ID))
	// The Monero rows: the subaddress, the payment with its first-seen time and height, the cursor, the mark and the
	// settled fact (WatchedPayments leaves it out, and AddMoneroSettled is a replay).
	if a, err := s.SubaddressForOrder(ctx, o.ID); err != nil || a.Address != sub(1) || a.Index != 1 || !a.CreatedAt.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("subaddress %+v %v", a, err)
	}
	pk := PaymentKey{TxID: txid('b'), Address: sub(1)}
	if p, err := s.Payment(ctx, pk); err != nil || p.AmountPiconero != 7000 || p.FirstSeenHeight != 40 || p.UnlockTime != 0 ||
		!p.FirstSeen.Equal(t0.Add(20*time.Minute)) || p.Confirmed {
		t.Fatalf("payment %+v %v", p, err)
	}
	if c, ok, err := s.MoneroCursor(ctx); err != nil || !ok || c.Height != 40 {
		t.Fatalf("cursor %+v %v %v", c, ok, err)
	}
	if m, ok, err := s.CurrentMoneroMark(ctx, pk); err != nil || !ok || m.Kind != MarkMined || m.Height != 40 || m.Seq != 1 {
		t.Fatalf("mark %+v %v %v", m, ok, err)
	}
	if w, err := s.WatchedPayments(ctx, t0); err != nil || len(w) != 0 {
		t.Fatalf("watched %+v %v", w, err)
	}
	if added, err := s.AddMoneroSettled(ctx, pk, 60); err != nil || added {
		t.Fatalf("settled again: %v %v", added, err)
	}
	if idx, ok, err := s.MaxSubaddressIndex(ctx); err != nil || !ok || idx != 1 {
		t.Fatalf("max index %d %v %v", idx, ok, err)
	}
}

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

// Copying the db and wal files of a live store (a file-level backup, or the disk after a crash)
// and opening the copy shows every committed row.
func TestCopyCommittedFiles(t *testing.T) {
	s, path, clk := freshStore(t)
	o := fixture(t, s, clk)
	dst := filepath.Join(t.TempDir(), "copy.db")
	if !copyFile(t, path, dst) {
		t.Fatal("no database file")
	}
	if !copyFile(t, path+"-wal", dst+"-wal") {
		t.Fatal("no WAL file: the test would not exercise recovery")
	}
	c, err := Open(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	verifyFixture(t, c, o)
}

// A write that fails or panics inside the transaction leaves nothing behind and does not wedge the writer.
func TestWriteRollsBack(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		_ = s.write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES (?, ?, 'a', 'r', 1, 0, 0, 5, 6)`, strings.Repeat("a", 32), strings.Repeat("b", 32)); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d orders after a panic, err %v", n, err)
	}
	mkOrder(t, s, "after", 1000, t0.Add(time.Hour))
}

// A seeded property test: random operations never break the derived views' invariants.
func TestRandomOperations(t *testing.T) {
	ctx := context.Background()
	s, _, clk := freshStore(t)
	rng := rand.New(rand.NewSource(7))
	var orders []Order
	invCount := 0
	for i := 0; i < 300; i++ {
		clk.set(t0.Add(2*time.Hour + time.Duration(i)*time.Second))
		switch r := rng.Intn(5); {
		case r == 0 || len(orders) == 0:
			o, created, err := s.CreateOrder(ctx, Order{AccountID: "a", MerchantRef: "r" + itoa(int64(rng.Intn(40))), AmountMsat: 1000, ExpiresAt: t0.Add(10 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if created {
				orders = append(orders, o)
			}
		case r == 1:
			o := orders[rng.Intn(len(orders))]
			h := strings.Repeat(string("0123456789abcdef"[rng.Intn(16)]), 64)
			err := s.AddInvoice(ctx, Invoice{OrderID: o.ID, PaymentHash: h, Bolt11: "ln", AmountMsat: 1000, CreatedAt: t0, ExpiresAt: t0.Add(30 * time.Minute)})
			if err == nil {
				invCount++
			} else if err != ErrDuplicate {
				t.Fatal(err)
			}
		case r == 2:
			if _, err := s.CloseInvoice(ctx, strings.Repeat(string("0123456789abcdef"[rng.Intn(16)]), 64), t0.Add(2*time.Hour), CloseLookup); err != nil && err != ErrNotFound {
				t.Fatal(err)
			}
		default:
			if err := s.AddInvoiceAttempt(ctx, orders[rng.Intn(len(orders))].ID, clk.now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	all, _ := s.OrdersByAccount(ctx, "a")
	if len(all) != len(orders) {
		t.Fatalf("%d orders, want %d", len(all), len(orders))
	}
	open, _ := s.OpenInvoices(ctx)
	var closed int
	for _, o := range orders {
		cl, _ := s.ClosuresForOrder(ctx, o.ID)
		closed += len(cl)
	}
	if len(open)+closed != invCount {
		t.Fatalf("open %d + closed %d != %d invoices", len(open), closed, invCount)
	}
}

// The deadline trigger's grace is generated from DeadlineGrace; pin the value.
func TestDeadlineGrace(t *testing.T) {
	if DeadlineGrace != 24*time.Hour {
		t.Fatalf("DeadlineGrace = %v", DeadlineGrace)
	}
	s, _, _ := freshStore(t)
	var sqlText string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'closed_invoices_not_early'`).Scan(&sqlText); err != nil || !strings.Contains(sqlText, "+ 86400)") {
		t.Fatalf("trigger %q: %v", sqlText, err)
	}
}

// A context that ends in the middle of a write leaves no row behind and does not wedge the writer.
func TestContextEndsMidWrite(t *testing.T) {
	s, _, _ := freshStore(t)
	count := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM orders`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Cancelled between two statements of one transaction.
	ctx, cancel := context.WithCancel(context.Background())
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES (?, ?, 'a', 'r', 1, 0, 0, 5, 6)`, strings.Repeat("a", 32), strings.Repeat("b", 32)); err != nil {
			return err
		}
		cancel()
		_, err := tx.ExecContext(ctx, `INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES (?, ?, 'a', 'r2', 1, 0, 0, 5, 6)`, strings.Repeat("c", 32), strings.Repeat("d", 32))
		return err
	})
	if err == nil {
		t.Fatal("write succeeded after the context ended")
	}
	if count() != 0 {
		t.Fatalf("%d orders left behind", count())
	}
	// Already expired before the call.
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer dcancel()
	if _, _, err := s.CreateOrder(dctx, Order{AccountID: "a", MerchantRef: "r", AmountMsat: 1000, ExpiresAt: t0.Add(time.Hour)}); err == nil {
		t.Fatal("CreateOrder succeeded with an expired context")
	}
	if count() != 0 {
		t.Fatalf("%d orders left behind", count())
	}
	// The writer is not wedged, and the same reference can be created now.
	mkOrder(t, s, "r", 1000, t0.Add(time.Hour))
	if count() != 1 {
		t.Fatalf("%d orders", count())
	}
}

// Ruling 8-A6: monero_marks gained confirmations in migration 5, edited in place again (pre-release). A version-5 store
// whose monero_marks lacks it (a store from before this change, with wallet_height already in its cursor) is refused
// at Open with ErrOldMoneroSchema, as the store without wallet_height is (ruling R5); nothing is migrated or written.
func TestOldMoneroMarksGuard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	const confs = "\tconfirmations INTEGER NOT NULL,\n"
	if strings.Count(migrations[4], confs) != 1 {
		t.Fatal("setup: migration 5 has no confirmations line to remove")
	}
	old := strings.Replace(migrations[4], confs, "", 1)
	old = strings.Replace(old, checkMoneroMarkShape("kind", "height", "confirmations"),
		`((kind = 'mined' AND height >= 1) OR (kind IN ('mempool', 'dead') AND height = 0))`, 1)
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:4]...)
	stmts = append(stmts, old, `INSERT INTO store_schema_version (version) VALUES (5)`,
		`INSERT INTO monero_cursor (seq, height, wallet_height, at) VALUES (1, 200, 200, 5)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := schemaText(t, db)
	db.Close()
	s, err := Open(ctx, path)
	if err == nil {
		_, _, cerr := s.CurrentMoneroMark(ctx, PaymentKey{TxID: txid('a'), Address: sub(1)})
		s.Close()
		t.Fatalf("the version-5 store without monero_marks.confirmations opened (CurrentMoneroMark: %v)", cerr)
	}
	if !errors.Is(err, ErrOldMoneroSchema) {
		t.Fatalf("err = %v, want ErrOldMoneroSchema", err)
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

// Ruling S4: a version-5 store made before migration 5 gained monero_pending_settle (edited in place, no release
// existed) is refused at Open with ErrOldMoneroSchema, and nothing is migrated or written.
func TestOldMoneroPendingGuard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(migrations[4], "\nCREATE TABLE monero_pending_settle")
	if i < 0 {
		t.Fatal("setup: migration 5 has no monero_pending_settle")
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:4]...)
	stmts = append(stmts, migrations[4][:i], `INSERT INTO store_schema_version (version) VALUES (5)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := schemaText(t, db)
	db.Close()
	s, err := Open(ctx, path)
	if err == nil {
		_, perr := s.MoneroPendingSettle(ctx)
		s.Close()
		t.Fatalf("the version-5 store without monero_pending_settle opened (MoneroPendingSettle: %v)", perr)
	}
	if !errors.Is(err, ErrOldMoneroSchema) {
		t.Fatalf("err = %v, want ErrOldMoneroSchema", err)
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
