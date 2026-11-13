package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBindNetworkRecordsOnceAndReadsBack(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	if n, err := s.Network(ctx); err != nil || n != "" {
		t.Fatalf("a fresh store: %q %v", n, err)
	}
	if err := s.BindNetwork(ctx, "signet"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Network(ctx); err != nil || n != "signet" {
		t.Fatalf("read back: %q %v", n, err)
	}
	// The same network again writes nothing: the database's data version does not move.
	watcher, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	watcher.SetMaxOpenConns(1)
	before := dataVersion(t, watcher)
	for i := 0; i < 2; i++ {
		if err := s.BindNetwork(ctx, "signet"); err != nil {
			t.Fatal(err)
		}
	}
	if after := dataVersion(t, watcher); after != before {
		t.Fatalf("the same network wrote: %d -> %d", before, after)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM store_network`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows %d %v", rows, err)
	}
}

// dataVersion is SQLite's data_version seen from a raw connection (SetMaxOpenConns(1), so one connection): it changes
// when another connection commits a write.
func dataVersion(t *testing.T, c *sql.DB) int {
	t.Helper()
	var n int
	if err := c.QueryRow(`PRAGMA data_version`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBindNetworkRefusesAnotherAndNamesTheStoredOne(t *testing.T) {
	ctx := context.Background()
	s, path, clk := freshStore(t)
	if err := s.BindNetwork(ctx, "regtest"); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"mainnet", "testnet", "signet"} {
		err := s.BindNetwork(ctx, other)
		if !errors.Is(err, ErrNetwork) || !strings.Contains(err.Error(), "regtest") || strings.Contains(err.Error(), other) {
			t.Fatalf("bind %s: %v", other, err)
		}
		var ne *NetworkError
		if !errors.As(err, &ne) || ne.Network != "regtest" {
			t.Errorf("BindNetwork(%s): the error does not carry the stored network: %v", other, err)
		}
	}
	if n, _ := s.Network(ctx); n != "regtest" {
		t.Fatalf("the stored network changed: %q", n)
	}
	// It survives a reopen.
	s.Close()
	s2, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.BindNetwork(ctx, "mainnet"); !errors.Is(err, ErrNetwork) {
		t.Fatalf("after reopen: %v", err)
	}
	if err := s2.BindNetwork(ctx, "regtest"); err != nil {
		t.Fatal(err)
	}
}

func TestBindNetworkRefusesAnInvalidName(t *testing.T) {
	ctx := context.Background()
	s, _, _ := freshStore(t)
	for _, n := range []string{"", "Mainnet", "mainnet ", "main", "stagenet", "regtest\x00", "mainnet\n", "bitcoin", "'; DROP TABLE orders; --"} {
		if err := s.BindNetwork(ctx, n); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrNetwork) {
			t.Errorf("BindNetwork(%q): %v", n, err)
		}
	}
	if n, err := s.Network(ctx); err != nil || n != "" {
		t.Fatalf("an invalid name wrote: %q %v", n, err)
	}
	// Once bound, an invalid name is still ErrInvalid, not ErrNetwork.
	if err := s.BindNetwork(ctx, "mainnet"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindNetwork(ctx, "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

// The raw connection has none of the store's pragmas: the table still refuses an update, a delete, a second row (plain,
// OR REPLACE and upsert) and a name that is not a network.
func TestNetworkTableGuardsOnRawConnection(t *testing.T) {
	ctx := context.Background()
	s, path, _ := freshStore(t)
	raw, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, bad := range []string{"'bitcoin'", "''", "NULL", "'Mainnet'", "1", "'regtest' || char(0)"} {
		q := `INSERT INTO store_network (network) VALUES (` + bad + `)`
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("%s succeeded", q)
		}
	}
	if n, _ := s.Network(ctx); n != "" {
		t.Fatalf("a bad name was written: %q", n)
	}
	if _, err := raw.Exec(`INSERT INTO store_network (network) VALUES ('testnet')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE store_network SET network = 'mainnet'`,
		`DELETE FROM store_network`,
		`INSERT INTO store_network (network) VALUES ('testnet')`,
		`INSERT INTO store_network (network) VALUES ('mainnet')`,
		`INSERT OR REPLACE INTO store_network (network) VALUES ('mainnet')`,
		`REPLACE INTO store_network (network) VALUES ('mainnet')`,
		`INSERT INTO store_network (network) VALUES ('mainnet') ON CONFLICT DO UPDATE SET network = 'mainnet'`,
	} {
		if _, err := raw.Exec(q); err == nil {
			t.Errorf("%q succeeded", q)
		}
	}
	if n, err := s.Network(ctx); err != nil || n != "testnet" {
		t.Fatalf("after the refused statements: %q %v", n, err)
	}
	var rows int
	if err := raw.QueryRow(`SELECT count(*) FROM store_network`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows %d %v", rows, err)
	}
}

// A store made by migration 9 opens at 10 with no network row and every earlier row intact; its schema is a fresh
// store's, statement for statement; its first bind records the configured network.
func TestMigrationTenAddsTheNetworkTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	e1 := hexOf('1')
	stmts := []string{`CREATE TABLE store_schema_version (version INTEGER NOT NULL) STRICT`}
	stmts = append(stmts, migrations[:9]...)
	stmts = append(stmts, `INSERT INTO store_schema_version (version) VALUES (9)`,
		fmt.Sprintf(`INSERT INTO orders VALUES ('%s', '%s', 'a', 'r', 1000, 0, 0, 5, 6)`, strings.Repeat("a", 32), strings.Repeat("d", 32)),
		fmt.Sprintf(`INSERT INTO events VALUES ('%s', 'order.paid', '%s', 1, 1000)`, e1, strings.Repeat("a", 32)),
		fmt.Sprintf(`INSERT INTO webhook_attempts VALUES ('main', '%s', 1, 1100, 200, 1)`, e1))
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
	if n, err := s.Network(ctx); err != nil || n != "" {
		t.Fatalf("a migrated store has a network: %q %v", n, err)
	}
	if _, err := s.OrderByID(ctx, strings.Repeat("a", 32)); err != nil {
		t.Fatalf("the order did not survive: %v", err)
	}
	if got := rawAttemptRows(t, s.db, `SELECT endpoint, event_id, seq, at, status, ok FROM webhook_attempts`); len(got) != 1 {
		t.Fatalf("attempts %v", got)
	}
	fresh, _, _ := freshStore(t)
	if got, want := schemaText(t, s.db), schemaText(t, fresh.db); got != want {
		t.Fatalf("upgraded schema differs from a fresh one:\n%s\n---\n%s", got, want)
	}
	if err := s.BindNetwork(ctx, "mainnet"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Network(ctx); n != "mainnet" {
		t.Fatalf("%q", n)
	}
}

// Many first calls with two different networks at once: one network is recorded, every call for it succeeds and every
// call for the other is ErrNetwork.
func TestBindNetworkConcurrentFirstCalls(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		s, _, _ := freshStore(t)
		const n = 16
		errs := make([]error, n)
		nets := make([]string, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			nets[i] = []string{"mainnet", "regtest"}[i%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.BindNetwork(ctx, nets[i])
			}()
		}
		close(start)
		wg.Wait()
		winner, _ := s.Network(ctx)
		if winner != "mainnet" && winner != "regtest" {
			t.Fatalf("round %d: winner %q", round, winner)
		}
		for i := range errs {
			if nets[i] == winner && errs[i] != nil {
				t.Fatalf("round %d: the winner's call %d failed: %v", round, i, errs[i])
			}
			if nets[i] != winner && !errors.Is(errs[i], ErrNetwork) {
				t.Fatalf("round %d: call %d for the loser: %v", round, i, errs[i])
			}
		}
	}
}
