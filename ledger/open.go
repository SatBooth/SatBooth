package ledger

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

	// The only driver import. Swapping SQLite drivers means editing this file.
	_ "github.com/ncruces/go-sqlite3/driver"
)

// dsnOptions are the connection settings every connection gets: immediate write
// transactions, WAL, full fsync, foreign keys, and a 5 s busy timeout. recursive_triggers is
// defence in depth: since migration 2 the BEFORE INSERT guard triggers refuse an insert of an
// existing id on any connection (so INSERT OR REPLACE cannot replace a row), and the pragma also
// makes the delete REPLACE would do fire the delete trigger on this connection. Temporary data
// (sorts, index builds, temp tables) stays in memory because the release image has no writable
// temp directory. The busy timeout comes first, so every later pragma waits for a lock instead
// of failing at once.
const dsnOptions = "_txlock=immediate" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(wal)" +
	"&_pragma=synchronous(full)" +
	"&_pragma=foreign_keys(on)" +
	"&_pragma=fullfsync(on)" +
	"&_pragma=checkpoint_fullfsync(on)" +
	"&_pragma=recursive_triggers(on)" +
	"&_pragma=temp_store(memory)"

// openDB opens the database at path with the ledger's settings and a single connection.
func openDB(ctx context.Context, path string) (*sql.DB, error) { return openWith(ctx, path, "") }

// openWith is openDB with extra URI parameters ahead of the settings: "mode=rw&" opens an existing file and never
// creates one.
func openWith(ctx context.Context, path, extra string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("ledger: empty database path")
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

// Ledger is a TigerBeetle-style ledger on SQLite.
type Ledger struct {
	db  *sql.DB
	now func() time.Time

	mu sync.Mutex // serialises write transactions
}

// Option configures Open.
type Option func(*Ledger)

// WithClock sets the clock used for timestamps (default time.Now). A nil clock is ignored.
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) {
		if now != nil {
			l.now = now
		}
	}
}

// openRawForTest is used only by tests; it lives here so the driver specifics stay in one file.
// It opens path with no pragmas and no connection limit, as an operator's sqlite3
// shell or another program would, to test that the schema protects itself on any connection.
func openRawForTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+escapePath(path))
}

// Open opens (creating if needed) the ledger database at path and applies missing migrations.
func Open(ctx context.Context, path string, opts ...Option) (*Ledger, error) {
	db, err := openDB(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := checkWAL(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	l := &Ledger{db: db, now: time.Now}
	for _, o := range opts {
		o(l)
	}
	if err := l.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// checkWAL refuses a journal mode other than WAL. SQLite falls back to another journal mode without saying so (for
// example on a file system that cannot do shared memory); the ledger's durability rests on WAL, so refuse that.
func checkWAL(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("ledger: journal_mode is %q, want wal (err %v)", mode, err)
	}
	if mode != "wal" {
		return fmt.Errorf("ledger: journal_mode is %q, want wal", mode)
	}
	return nil
}

// ErrSchemaBehind and ErrSchemaNewer: OpenExisting found a database whose schema is older, or newer, than this build's.
var (
	ErrSchemaBehind = errors.New("ledger: the database schema is older than this build: start SatBooth once to upgrade it")
	ErrSchemaNewer  = errors.New("ledger: the database schema is newer than this build")
)

// OpenExisting opens the existing ledger database at path for a backup. Unlike Open it refuses a missing file
// (it creates none: a backup must never copy a new empty database), and it never migrates: a schema behind this
// build's is ErrSchemaBehind, one ahead of it ErrSchemaNewer. It uses Open's settings and journal-mode check.
//
// The open is read-write on purpose. After a crash, committed rows can sit in the -wal file, and a read-write open
// recovers them; a read-only or immutable open could copy stale data. The caller holds the data directory's lock, so
// nothing else writes. SQLite may checkpoint the WAL into the file; no row and no schema changes.
func OpenExisting(ctx context.Context, path string) (*Ledger, error) {
	db, err := openWith(ctx, path, "mode=rw&")
	if err != nil {
		return nil, err
	}
	if err := checkWAL(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	var have, rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(max(version), 0) FROM schema_version`).Scan(&rows, &have); err != nil || rows != 1 {
		db.Close()
		return nil, errors.New("ledger: not a SatBooth ledger database")
	}
	switch {
	case have > len(migrations):
		db.Close()
		return nil, ErrSchemaNewer
	case have < len(migrations):
		db.Close()
		return nil, ErrSchemaBehind
	}
	return &Ledger{db: db, now: time.Now}, nil
}

// VacuumInto writes a copy of the database to path, which must be absolute and must not exist, with SQLite's VACUUM
// INTO; the path is a bound parameter. A relative path is refused: SQLite reads one that starts with "file:" as a URI,
// which could put the copy somewhere else. The copy is in rollback journal mode (Open turns it to WAL). It changes no
// row and no schema.
func (l *Ledger) VacuumInto(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("ledger: the copy's path must be absolute")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.db.ExecContext(ctx, `VACUUM INTO ?`, path)
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
		return errors.New("ledger: quick_check failed")
	}
	return nil
}

// Close closes the database and returns only after SQLite has closed the file. sql.DB.Close closes only the idle
// connection: a transaction whose context was cancelled is rolled back, and its connection closed, by a goroutine of
// database/sql ((*Tx).awaitDone), possibly after sql.DB.Close has returned, and that last close may checkpoint the WAL
// under a lock. database/sql decrements its open count in finalClose after the driver's Close returns, so Close waits
// for Stats().OpenConnections to reach 0: every 10 ms, for at most closeWait. An idle database does not wait.
func (l *Ledger) Close() error {
	err := l.db.Close()
	for deadline := time.Now().Add(closeWait); l.db.Stats().OpenConnections != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return errors.New("ledger: a connection was still open after Close")
		}
	}
	return err
}

// closeWait bounds Close's wait for the last connection to close.
const closeWait = 10 * time.Second

// write runs fn in one BEGIN IMMEDIATE transaction, serialised by a mutex. It commits if fn
// returns nil and rolls back otherwise. l.mu is not reentrant: fn must not call write again, so
// never Balance, CreateAccount or CreateTransfer (which all use it) from inside fn. Nor
// LookupAccount or LookupTransfer: they use l.db, the pool has one connection and this
// transaction holds it, so they would deadlock. Use selectAccount or selectTransfer with tx.
func (l *Ledger) write(ctx context.Context, fn func(*sql.Tx) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
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
