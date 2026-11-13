package store

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Close returns only after SQLite has closed the file. A transaction whose context is cancelled is rolled back, and
// its connection closed, by a goroutine of database/sql, possibly after sql.DB.Close has returned; Close waits for
// it, so no connection is open once Close returns.
func TestCloseWaitsForCancelledTx(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx := context.Background()
		dir := t.TempDir()
		s, err := Open(ctx, filepath.Join(dir, "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		tctx, cancel := context.WithCancel(ctx)
		tx, err := s.db.BeginTx(tctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		// A write gives the rollback work to do under the write lock.
		if _, err := tx.ExecContext(tctx, `UPDATE pair SET generation = generation + 1, nonce = '`+strings.Repeat("e", 32)+`'`); err != nil {
			t.Fatal(err)
		}
		cancel()
		// Let database/sql's own goroutine start the rollback first, as it does when a worker's context is cancelled
		// at shutdown; the explicit Rollback then finds the transaction done and returns at once.
		runtime.Gosched()
		_ = tx.Rollback()
		if err := s.Close(); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		if n := s.db.Stats().OpenConnections; n != 0 {
			t.Fatalf("iteration %d: %d connections open after Close", i, n)
		}
	}
}
