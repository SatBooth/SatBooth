package service

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLockFileOwner checks that lockDataDir refuses a lock file owned by another user, even at mode 0600. Only root
// can give a file away, so the test is skipped otherwise; CI runs it with sudo.
func TestLockFileOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown the lock file to another user; run: sudo go test -run TestLockFileOwner ./service")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "satbooth.lock")

	// Control: a lock file of the effective user is accepted.
	f, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("own lock file refused: %v", err)
	}
	f.Close()

	// A lock file of another user, mode 0600, is refused and not locked.
	const other = 65534
	if err := os.Chown(path, other, other); err != nil {
		t.Fatal(err)
	}
	f, err = lockDataDir(dir)
	if err == nil {
		f.Close()
		t.Fatal("a lock file owned by another user was accepted")
	}
	if want := "the data directory's lock file must be a regular file of this user, mode 0600"; err.Error() != want {
		t.Fatalf("a lock file of another user: %v, want the owner check's refusal %q", err, want)
	}
}
