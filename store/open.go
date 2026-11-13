// Package store is SatBooth's insert-only storage for orders, the quotes of priced orders, Lightning
// invoices, invoice attempts, the webhook outbox and the accounts that have a paid order, in its
// own SQLite file beside the ledger's. A priced order's quote (CreateQuotedOrder, QuoteForOrder) is
// written with the order and keeps the rate it was converted at.
//
// No row is ever updated or deleted: the schema's triggers refuse UPDATE, DELETE and REPLACE on
// every connection, whatever its pragmas. The exceptions are the one row of store_schema_version
// (the migrations update it) and the one row of pair (the pair stamp: its id is set once and its
// generation advances, see Pair and SetPair). An order's status is not stored. It is derived by the
// caller from these rows, the ledger (an order is paid when the ledger holds the transfer derived
// from its invoice's payment hash) and the clock.
//
// Two kinds of attempt exist and are named apart. An invoice attempt (AddInvoiceAttempt,
// AttemptsSince) records one call to the hub's MakeInvoice, for the daily limits. A delivery
// attempt (Attempt, AddAttempt, AttemptsFor) records one POST of a webhook event to one endpoint; RegisterEndpoint
// records an endpoint's first delivery pass (webhook_endpoints, insert-only like the other tables).
//
// Times are stored as whole Unix seconds (UTC); sub-second parts are dropped.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// The only driver import, as in package ledger. Swapping SQLite drivers means editing this
	// file and ledger/open.go.
	_ "github.com/ncruces/go-sqlite3/driver"
)

// dsnOptions are the connection settings every connection gets, the same as the ledger's:
// immediate write transactions, WAL, full fsync, foreign keys, a 5 s busy timeout. The guard
// triggers in the schema do not depend on any of them. Temporary data (sorts, index builds, temp
// tables) stays in memory because the release image has no writable temp directory. The busy
// timeout comes first, so every later pragma waits for a lock instead of failing at once.
const dsnOptions = "_txlock=immediate" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(wal)" +
	"&_pragma=synchronous(full)" +
	"&_pragma=foreign_keys(on)" +
	"&_pragma=fullfsync(on)" +
	"&_pragma=checkpoint_fullfsync(on)" +
	"&_pragma=recursive_triggers(on)" +
	"&_pragma=temp_store(memory)"

// openDB opens the database at path with the store's settings and a single connection.
func openDB(ctx context.Context, path string) (*sql.DB, error) { return openWith(ctx, path, "") }

// openWith is openDB with extra URI parameters ahead of the settings: "mode=rw&" opens an existing file and never
// creates one.
func openWith(ctx context.Context, path, extra string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	// The path is escaped so that '?', '#' or '%' in it cannot change the options.
	u := url.URL{Scheme: "file", Opaque: escapePath(path)}
	db, err := sql.Open("sqlite3", u.String()+"?"+extra+dsnOptions)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// escapePath percent-encodes the characters that are special in a file: URI.
func escapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '%', '?', '#':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Store is the order, invoice and webhook-outbox store.
type Store struct {
	db  *sql.DB
	now func() time.Time

	mu sync.Mutex // serialises write transactions

	// eventWatch and alertWatch hold the subscribers of WatchEvents and WatchAlerts. They are separate from mu: a
	// wake-up is raised after the write lock is released.
	eventWatch, alertWatch watchers
}

// watchers is a set of wake-up channels guarded by its own mutex. Its zero value is ready to use.
type watchers struct {
	mu sync.Mutex
	m  map[chan struct{}]struct{}
}

// add subscribes a new channel with a buffer of one and returns it with the function that ends the subscription (safe
// to call twice). The channel is never closed.
func (w *watchers) add() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.m == nil {
		w.m = map[chan struct{}]struct{}{}
	}
	w.m[ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.m, ch)
		w.mu.Unlock()
	}
}

// wake sends one wake-up to every subscriber. A send never blocks: a subscriber with a wake-up pending already has
// what this one would say. It must be called with no transaction open and s.mu released.
func (w *watchers) wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.m {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WatchEvents returns a channel that receives after each AddEvent that inserted an event, once its transaction has
// ended, and a function that ends the watch (idempotent). The channel has a buffer of one and is never closed. A send
// never blocks: wake-ups that arrive while one is pending merge into it. A watcher takes the wake-up before it reads,
// so an event committed after that read wakes it again. A wake-up says when to look, never what is there: it is also
// raised when the commit failed, and an event written by another process raises none.
func (s *Store) WatchEvents() (<-chan struct{}, func()) { return s.eventWatch.add() }

// WatchAlerts is WatchEvents for AddAlert, after it inserted a new alert row (not a repeat).
func (s *Store) WatchAlerts() (<-chan struct{}, func()) { return s.alertWatch.add() }

// Option configures Open.
type Option func(*Store)

// WithClock sets the clock used wherever the store stamps a time (default time.Now). A nil clock
// is ignored.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open opens (creating if needed) the store database at path and applies missing migrations.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	db, err := openDB(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := checkWAL(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// checkWAL refuses a journal mode other than WAL. SQLite falls back to another journal mode without saying so (for
// example for an in-memory database); the store's durability rests on WAL, so refuse that.
func checkWAL(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("store: journal_mode is %q, want wal (err %v)", mode, err)
	}
	if mode != "wal" {
		return fmt.Errorf("store: journal_mode is %q, want wal", mode)
	}
	return nil
}

// ErrSchemaBehind and ErrSchemaNewer: OpenExisting found a database whose schema is older, or newer, than this build's.
var (
	ErrSchemaBehind = errors.New("store: the database schema is older than this build: start SatBooth once to upgrade it")
	ErrSchemaNewer  = errors.New("store: the database schema is newer than this build")
)

// OpenExisting opens the existing store database at path for a backup. Unlike Open it refuses a missing file
// (it creates none: a backup must never copy a new empty database), and it never migrates: a schema behind this
// build's is ErrSchemaBehind, one ahead of it ErrSchemaNewer. It uses Open's settings and journal-mode check.
//
// The open is read-write on purpose. After a crash, committed rows can sit in the -wal file, and a read-write open
// recovers them; a read-only or immutable open could copy stale data. The caller holds the data directory's lock, so
// nothing else writes. SQLite may checkpoint the WAL into the file; no row and no schema changes.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	db, err := openWith(ctx, path, "mode=rw&")
	if err != nil {
		return nil, err
	}
	if err := checkWAL(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	var have, rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(version), 0) FROM store_schema_version`).Scan(&rows, &have); err != nil || rows != 1 {
		db.Close()
		return nil, errors.New("store: not a SatBooth store database")
	}
	switch {
	case have > len(migrations):
		db.Close()
		return nil, ErrSchemaNewer
	case have < len(migrations):
		db.Close()
		return nil, ErrSchemaBehind
	}
	return &Store{db: db, now: time.Now}, nil
}

// VacuumInto writes a copy of the database to path, which must be absolute and must not exist, with SQLite's VACUUM
// INTO; the path is a bound parameter. A relative path is refused: SQLite reads one that starts with "file:" as a URI,
// which could put the copy somewhere else. The copy is in rollback journal mode (Open turns it to WAL). It changes no
// row and no schema.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("store: the copy's path must be absolute")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// QuickCheck runs PRAGMA quick_check on the database file at path and refuses any answer but one "ok". The file is
// opened read-only with no journal-mode setting, so the check creates no file and changes nothing in it.
func QuickCheck(ctx context.Context, path string) error {
	u := url.URL{Scheme: "file", Opaque: escapePath(path)}
	db, err := sql.Open("sqlite3", u.String()+"?mode=ro&_pragma=temp_store(memory)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(got) != 1 || got[0] != "ok" {
		return errors.New("store: quick_check failed")
	}
	return nil
}

// Close closes the database and returns only after SQLite has closed the file. sql.DB.Close closes only the idle
// connection: a transaction whose context was cancelled is rolled back, and its connection closed, by a goroutine of
// database/sql ((*Tx).awaitDone), possibly after sql.DB.Close has returned, and that last close may checkpoint the WAL
// under a lock. database/sql decrements its open count in finalClose after the driver's Close returns, so Close waits
// for Stats().OpenConnections to reach 0: every 10 ms, for at most closeWait. An idle database does not wait.
func (s *Store) Close() error {
	err := s.db.Close()
	for deadline := time.Now().Add(closeWait); s.db.Stats().OpenConnections != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return errors.New("store: a connection was still open after Close")
		}
	}
	return err
}

// closeWait bounds Close's wait for the last connection to close.
const closeWait = 10 * time.Second

// write runs fn in one BEGIN IMMEDIATE transaction, serialised by a mutex. It commits if fn
// returns nil and rolls back otherwise, also if fn panics. s.mu is not reentrant and the pool has
// one connection, which the transaction holds: fn must use tx, never s.db or a method of Store.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rolls back if fn returns an error or panics; after a successful Commit it is a no-op.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// queryer is what both *sql.DB and *sql.Tx offer.
type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}
