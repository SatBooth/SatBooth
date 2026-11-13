package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh store has the one pair row with no id, generation 0 and no nonce; SetPair writes it.
func TestPairFreshAndSet(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	if p, err := s.Pair(ctx); err != nil || p != (PairStamp{}) {
		t.Fatalf("fresh pair %+v %v", p, err)
	}
	a, n := strings.Repeat("a1", 16), strings.Repeat("e1", 16)
	if err := s.SetPair(ctx, PairStamp{ID: a}); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := s.db.QueryRow(`SELECT nonce IS NULL FROM pair`).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("an empty nonce is not stored as NULL: %v %v", isNull, err)
	}
	if err := s.SetPair(ctx, PairStamp{ID: a, Gen: 1, Nonce: n}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if p, err := s2.Pair(ctx); err != nil || p != (PairStamp{ID: a, Gen: 1, Nonce: n}) {
		t.Fatalf("after reopen %+v %v", p, err)
	}
}

// SetPair refuses an id or a nonce that is not 32 lowercase hex characters, a negative generation, a nonce at
// generation 0 or none above it, a change of a set id and a generation that goes back; none of them changes the row.
func TestSetPairRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	a, n := strings.Repeat("a", 32), strings.Repeat("e", 32)
	for name, p := range map[string]PairStamp{
		"empty id":         {},
		"short id":         {ID: strings.Repeat("a", 31)},
		"long id":          {ID: strings.Repeat("a", 33)},
		"upper id":         {ID: strings.Repeat("A", 32)},
		"non-hex id":       {ID: strings.Repeat("g", 32)},
		"negative gen":     {ID: a, Gen: -1},
		"nonce at 0":       {ID: a, Nonce: n},
		"no nonce above 0": {ID: a, Gen: 1},
		"short nonce":      {ID: a, Gen: 1, Nonce: strings.Repeat("e", 31)},
		"long nonce":       {ID: a, Gen: 1, Nonce: strings.Repeat("e", 33)},
		"upper nonce":      {ID: a, Gen: 1, Nonce: strings.Repeat("E", 32)},
		"non-hex nonce":    {ID: a, Gen: 1, Nonce: strings.Repeat("g", 32)},
	} {
		if err := s.SetPair(ctx, p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := s.SetPair(ctx, PairStamp{ID: a, Gen: 3, Nonce: n}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPair(ctx, PairStamp{ID: strings.Repeat("b", 32), Gen: 3, Nonce: n}); err == nil {
		t.Error("a set id was changed")
	}
	if err := s.SetPair(ctx, PairStamp{ID: a, Gen: 2, Nonce: n}); err == nil {
		t.Error("the generation went back")
	}
	if p, err := s.Pair(ctx); err != nil || p != (PairStamp{ID: a, Gen: 3, Nonce: n}) {
		t.Fatalf("pair %+v %v", p, err)
	}
}

// The pair table is the one table whose row is updated (with the schema version table). On a connection with none of
// our pragmas the SQL still keeps exactly one row of the right shape: no delete, no second row, no replace or upsert,
// no change of a set id, no generation going back, and the CHECKs.
func TestPairGuardsOnRawConnection(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	a, n := strings.Repeat("c", 32), strings.Repeat("e", 32)
	if err := s.SetPair(ctx, PairStamp{ID: a, Gen: 2, Nonce: n}); err != nil {
		t.Fatal(err)
	}
	var sqlText string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'pair'`).Scan(&sqlText); err != nil || !strings.Contains(sqlText, "STRICT, WITHOUT ROWID") {
		t.Fatalf("pair table %q %v", sqlText, err)
	}
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	b := strings.Repeat("d", 32)
	for name, q := range map[string]string{
		"delete":        `DELETE FROM pair`,
		"second row":    `INSERT INTO pair (one, pair_id, generation) VALUES (2, NULL, 0)`,
		"same key":      `INSERT INTO pair (one, pair_id, generation) VALUES (1, NULL, 0)`,
		"replace":       `INSERT OR REPLACE INTO pair (one, pair_id, generation) VALUES (1, '` + b + `', 9)`,
		"replace into":  `REPLACE INTO pair SELECT * FROM pair`,
		"upsert":        `INSERT INTO pair (one, pair_id, generation) VALUES (1, NULL, 0) ON CONFLICT DO UPDATE SET generation = 9`,
		"change id":     `UPDATE pair SET pair_id = '` + b + `'`,
		"clear id":      `UPDATE pair SET pair_id = NULL`,
		"move key":      `UPDATE pair SET one = 2`,
		"gen back":      `UPDATE pair SET generation = 1`,
		"gen negative":  `UPDATE pair SET generation = -1`,
		"gen text":      `UPDATE pair SET generation = 'x'`,
		"no generation": `UPDATE pair SET generation = NULL`,
		"no nonce":      `UPDATE pair SET nonce = NULL`,
		"nonce text":    `UPDATE pair SET nonce = 'x'`,
		"nonce upper":   `UPDATE pair SET nonce = '` + strings.Repeat("E", 32) + `'`,
	} {
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("%s: %q succeeded", name, q)
		}
	}
	var rows int
	var id string
	var gen int64
	var nonce string
	if err := raw.QueryRow(`SELECT count(*), max(pair_id), max(generation), max(nonce) FROM pair`).Scan(&rows, &id, &gen, &nonce); err != nil || rows != 1 || id != a || gen != 2 || nonce != n {
		t.Fatalf("pair after the refused statements: %d rows, %q %d %q %v", rows, id, gen, nonce, err)
	}
	if _, err := raw.Exec(`UPDATE pair SET generation = 3, nonce = ?`, strings.Repeat("f", 32)); err != nil {
		t.Fatalf("an advance on a raw connection: %v", err)
	}
	// The shape CHECKs, on a database whose id is not set yet.
	s2, path2, _ := freshStore(t)
	s2.Close()
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
	// At generation 0 there is no nonce; above it there is one.
	if _, err := raw2.Exec(`UPDATE pair SET nonce = ?`, n); err == nil {
		t.Error("a nonce at generation 0 accepted")
	}
	if _, err := raw2.Exec(`UPDATE pair SET generation = 1`); err == nil {
		t.Error("generation 1 without a nonce accepted")
	}
}

// A pair table without its one row (which the guards prevent; the key CHECK allows no second row) is refused by
// Pair and SetPair, as a wrong row count of store_schema_version is by Open.
func TestPairRowCount(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	if _, err := s.db.Exec(`DROP TRIGGER pair_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM pair`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pair(ctx); err == nil {
		t.Fatal("an empty pair table read")
	}
	if err := s.SetPair(ctx, PairStamp{ID: strings.Repeat("a", 32), Gen: 1, Nonce: strings.Repeat("e", 32)}); err == nil {
		t.Fatal("SetPair on an empty pair table")
	}
}

// A database at version 6 (built by the first six migrations, with an order in it) gains the pair row with no id
// generation 0 and no nonce at Open, and its schema is a fresh store's.
func TestPairMigrationExisting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:6]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (6)`,
		`INSERT INTO orders (id, token, account_id, merchant_ref, amount_msat, amount_sat, amount_piconero, created_at, expires_at) VALUES ('`+
			strings.Repeat("a", 32)+`', '`+strings.Repeat("b", 32)+`', 'acct-1', 'v6', 1000, 0, 0, 5, 6)`)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if p, err := s.Pair(ctx); err != nil || p != (PairStamp{}) {
		t.Fatalf("migrated pair %+v %v", p, err)
	}
	if _, err := s.OrderByID(ctx, strings.Repeat("a", 32)); err != nil {
		t.Fatalf("the order did not survive: %v", err)
	}
	fresh, _, _ := freshStore(t)
	if got, want := schemaText(t, s.db), schemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
}
