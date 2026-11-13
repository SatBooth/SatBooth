package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh ledger has the one pair row with no id, generation 0, no nonce and no pending value; SetPair writes it, 0
// and "" meaning no pending value and no nonce.
func TestPairFreshAndSet(t *testing.T) {
	ctx := context.Background()
	l, path := freshLedger(t)
	if p, err := l.Pair(ctx); err != nil || p != (PairStamp{}) {
		t.Fatalf("fresh pair %+v %v", p, err)
	}
	a, n := strings.Repeat("0f", 16), strings.Repeat("e1", 16)
	for _, w := range []PairStamp{{ID: a}, {ID: a, Pending: 1, PendingNonce: n}, {ID: a, Gen: 1, Nonce: n}} {
		if err := l.SetPair(ctx, w); err != nil {
			t.Fatalf("SetPair(%+v): %v", w, err)
		}
		if p, err := l.Pair(ctx); err != nil || p != w {
			t.Fatalf("pair %+v %v, want %+v", p, err, w)
		}
	}
	var isNull bool
	if err := l.db.QueryRow(`SELECT pending IS NULL AND pending_nonce IS NULL FROM pair`).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("no pending value is not stored as NULL: %v %v", isNull, err)
	}
	l.Close()
	l2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if p, err := l2.Pair(ctx); err != nil || p != (PairStamp{ID: a, Gen: 1, Nonce: n}) {
		t.Fatalf("after reopen %+v %v", p, err)
	}
}

// SetPair refuses a bad id or nonce, a negative generation or pending value, a nonce at generation 0 or none above
// it, a pending value without its nonce or a nonce without it, a change of a set id and a generation that goes back;
// none of them changes the row.
func TestSetPairRefusals(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	a, n := strings.Repeat("a", 32), strings.Repeat("e", 32)
	for name, p := range map[string]PairStamp{
		"empty id":                {},
		"short id":                {ID: strings.Repeat("a", 31)},
		"long id":                 {ID: strings.Repeat("a", 33)},
		"upper id":                {ID: strings.Repeat("A", 32)},
		"non-hex id":              {ID: strings.Repeat("g", 32)},
		"negative gen":            {ID: a, Gen: -1},
		"negative pending":        {ID: a, Pending: -1, PendingNonce: n},
		"nonce at 0":              {ID: a, Nonce: n},
		"no nonce above 0":        {ID: a, Gen: 1},
		"short nonce":             {ID: a, Gen: 1, Nonce: strings.Repeat("e", 31)},
		"upper nonce":             {ID: a, Gen: 1, Nonce: strings.Repeat("E", 32)},
		"pending without nonce":   {ID: a, Pending: 1},
		"pending nonce, no value": {ID: a, PendingNonce: n},
		"long pending nonce":      {ID: a, Pending: 1, PendingNonce: strings.Repeat("e", 33)},
		"non-hex pending nonce":   {ID: a, Pending: 1, PendingNonce: strings.Repeat("g", 32)},
	} {
		if err := l.SetPair(ctx, p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := l.SetPair(ctx, PairStamp{ID: a, Gen: 3, Nonce: n}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetPair(ctx, PairStamp{ID: strings.Repeat("b", 32), Gen: 3, Nonce: n}); err == nil {
		t.Error("a set id was changed")
	}
	if err := l.SetPair(ctx, PairStamp{ID: a, Gen: 2, Nonce: n}); err == nil {
		t.Error("the generation went back")
	}
	if p, err := l.Pair(ctx); err != nil || p != (PairStamp{ID: a, Gen: 3, Nonce: n}) {
		t.Fatalf("pair %+v %v", p, err)
	}
}

// The pair table is the one ledger table whose row is updated (with schema_version). On a connection with none of
// our pragmas the SQL still keeps exactly one row of the right shape.
func TestPairGuardsOnRawConnection(t *testing.T) {
	ctx := context.Background()
	l, path := freshLedger(t)
	a, n, pn := strings.Repeat("c", 32), strings.Repeat("e", 32), strings.Repeat("f", 32)
	if err := l.SetPair(ctx, PairStamp{ID: a, Gen: 2, Nonce: n, Pending: 3, PendingNonce: pn}); err != nil {
		t.Fatal(err)
	}
	var sqlText string
	if err := l.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'pair'`).Scan(&sqlText); err != nil || !strings.Contains(sqlText, "STRICT, WITHOUT ROWID") {
		t.Fatalf("pair table %q %v", sqlText, err)
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	b := strings.Repeat("d", 32)
	for name, q := range map[string]string{
		"delete":            `DELETE FROM pair`,
		"second row":        `INSERT INTO pair (one, pair_id, generation, pending) VALUES (2, NULL, 0, NULL)`,
		"same key":          `INSERT INTO pair (one, pair_id, generation, pending) VALUES (1, NULL, 0, NULL)`,
		"replace":           `INSERT OR REPLACE INTO pair (one, pair_id, generation, pending) VALUES (1, '` + b + `', 9, NULL)`,
		"replace into":      `REPLACE INTO pair SELECT * FROM pair`,
		"upsert":            `INSERT INTO pair (one, pair_id, generation, pending) VALUES (1, NULL, 0, NULL) ON CONFLICT DO UPDATE SET generation = 9`,
		"change id":         `UPDATE pair SET pair_id = '` + b + `'`,
		"clear id":          `UPDATE pair SET pair_id = NULL`,
		"move key":          `UPDATE pair SET one = 2`,
		"gen back":          `UPDATE pair SET generation = 1`,
		"gen negative":      `UPDATE pair SET generation = -1`,
		"gen text":          `UPDATE pair SET generation = 'x'`,
		"no generation":     `UPDATE pair SET generation = NULL`,
		"pending zero":      `UPDATE pair SET pending = 0`,
		"pending text":      `UPDATE pair SET pending = 'x'`,
		"pending negative":  `UPDATE pair SET pending = -1`,
		"no nonce":          `UPDATE pair SET nonce = NULL`,
		"nonce text":        `UPDATE pair SET nonce = 'x'`,
		"no pending nonce":  `UPDATE pair SET pending_nonce = NULL`,
		"no pending value":  `UPDATE pair SET pending = NULL`,
		"pending nonce bad": `UPDATE pair SET pending_nonce = '` + strings.Repeat("G", 32) + `'`,
	} {
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("%s: %q succeeded", name, q)
		}
	}
	var rows int
	var id, nonce, pnonce string
	var gen, pending int64
	if err := raw.QueryRow(`SELECT count(*), max(pair_id), max(generation), max(nonce), max(pending), max(pending_nonce) FROM pair`).
		Scan(&rows, &id, &gen, &nonce, &pending, &pnonce); err != nil || rows != 1 || id != a || gen != 2 || nonce != n || pending != 3 || pnonce != pn {
		t.Fatalf("pair after the refused statements: %d rows, %q %d %q %d %q %v", rows, id, gen, nonce, pending, pnonce, err)
	}
	if _, err := raw.Exec(`UPDATE pair SET generation = 3, nonce = pending_nonce, pending = NULL, pending_nonce = NULL`); err != nil {
		t.Fatalf("a commit on a raw connection: %v", err)
	}
	l2, path2 := freshLedger(t)
	l2.Close()
	raw2, err := openRawForTest(path2)
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	for _, bad := range []string{strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32), strings.Repeat("g", 32), ""} {
		if _, err := raw2.Exec(`UPDATE pair SET pair_id = ?`, bad); err == nil {
			t.Errorf("id %q accepted", bad)
		}
	}
	// At generation 0 there is no nonce; above it there is one. A pending value and its nonce come together.
	for name, q := range map[string]string{
		"nonce at 0":                 `UPDATE pair SET nonce = '` + n + `'`,
		"generation 1 without nonce": `UPDATE pair SET generation = 1`,
		"pending without its nonce":  `UPDATE pair SET pending = 1`,
		"pending nonce alone":        `UPDATE pair SET pending_nonce = '` + n + `'`,
	} {
		if _, err := raw2.Exec(q); err == nil {
			t.Errorf("%s: %q succeeded", name, q)
		}
	}
}

// A pair table without its one row is refused by Pair and SetPair, as a wrong row count of schema_version is by Open.
func TestPairRowCount(t *testing.T) {
	ctx := context.Background()
	l, _ := freshLedger(t)
	if _, err := l.db.Exec(`DROP TRIGGER pair_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec(`DELETE FROM pair`); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Pair(ctx); err == nil {
		t.Fatal("an empty pair table read")
	}
	if err := l.SetPair(ctx, PairStamp{ID: strings.Repeat("a", 32), Gen: 1, Nonce: strings.Repeat("e", 32)}); err == nil {
		t.Fatal("SetPair on an empty pair table")
	}
}

// A database at version 3 (the first three migrations, with an account in it) gains the pair row with no id,
// generation 0, no nonce and no pending value at Open.
func TestPairMigrationExisting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "l.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:3]...)
	stmts = append(stmts, `INSERT INTO schema_version (version) VALUES (3)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertAccount(db, id16(1), 1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	l, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer l.Close()
	if p, err := l.Pair(ctx); err != nil || p != (PairStamp{}) {
		t.Fatalf("migrated pair %+v %v", p, err)
	}
	var v, n int
	if err := l.db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the account did not survive: %d %v", n, err)
	}
	if err := l.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations) || v != 4 {
		t.Fatalf("version %d (%d migrations) %v", v, len(migrations), err)
	}
	// The upgraded schema is the one a fresh ledger makes, statement for statement.
	fresh, _ := freshLedger(t)
	if got, want := ledgerSchemaText(t, l.db), ledgerSchemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
}

// ledgerSchemaText returns every entry of sqlite_master (type, name, table and SQL), in a fixed order.
func ledgerSchemaText(t *testing.T, db *sql.DB) string {
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
		fmt.Fprintf(&b, "%s %s %s\n%s\n", typ, name, tbl, text)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
