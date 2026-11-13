package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestTempStoreMemory(t *testing.T) {
	s, _, _ := freshStore(t)
	var got int
	if err := s.db.QueryRow(`PRAGMA temp_store`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("PRAGMA temp_store = %d, want 2 (memory)", got)
	}
}

// TestTempStoreNoTempDir runs the statements that spill to a temporary file once their data
// outgrows the page cache (an unindexed sort, an index build, a temp table fill) with the VFS's
// temp directory pointing nowhere, as in the release image, which has no writable temp directory.
// The vendored VFS creates its temp files in SQLITE_TMPDIR when it is set (vfs/os_linux.go,
// vfs/os_std_temp.go).
func TestTempStoreNoTempDir(t *testing.T) {
	t.Setenv("SQLITE_TMPDIR", filepath.Join(t.TempDir(), "missing"))
	s, _, _ := freshStore(t)
	ctx := context.Background()
	// 40,000 rows of 100 random bytes, about 4 MB, twice the default 2 MB page cache. Without the
	// pragma the statements below fail from about 20,000 rows on.
	const rows = 40000
	for _, q := range []string{
		`CREATE TABLE spill (x BLOB NOT NULL) STRICT`,
		fmt.Sprintf(`INSERT INTO spill (x) WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i < %d)
			SELECT randomblob(100) FROM c`, rows),
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := sortedScan(ctx, s.db, rows); err != nil {
		t.Errorf("unindexed ORDER BY: %v", err)
	}
	for _, q := range []string{
		`CREATE INDEX spill_x ON spill (x)`,
		`CREATE TEMP TABLE spill_copy AS SELECT x FROM spill`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// sortedScan reads the spill table through an unindexed ORDER BY and checks the order and count.
func sortedScan(ctx context.Context, db *sql.DB, want int) error {
	r, err := db.QueryContext(ctx, `SELECT x FROM spill ORDER BY x`)
	if err != nil {
		return err
	}
	defer r.Close()
	n := 0
	var prev []byte
	for r.Next() {
		var x []byte
		if err := r.Scan(&x); err != nil {
			return err
		}
		if prev != nil && bytes.Compare(prev, x) > 0 {
			return fmt.Errorf("row %d out of order", n)
		}
		prev = x
		n++
	}
	if err := r.Err(); err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("%d rows, want %d", n, want)
	}
	return nil
}
