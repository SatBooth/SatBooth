package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aliasCases changes a stopped rig's data directory (dir) so that one of its database files is an alias of another
// file, or not a regular file. Each returns the data directory the backup or the start must refuse.
var aliasCases = map[string]func(t *testing.T, dir string) string{
	// A directory B of symbolic links to the live directory A's databases. B has a lock of its own, so
	// without the check a backup of B runs while SatBooth runs on A.
	"a directory of symlinks to the live databases": func(t *testing.T, dir string) string {
		b := filepath.Join(t.TempDir(), "b")
		if err := os.Mkdir(b, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"store.db", "ledger.db", "store.db-wal", "store.db-shm", "ledger.db-wal", "ledger.db-shm"} {
			if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
				continue
			}
			if err := os.Symlink(filepath.Join(dir, n), filepath.Join(b, n)); err != nil {
				t.Fatal(err)
			}
		}
		return b
	},
	"store.db a symlink": func(t *testing.T, dir string) string {
		moveAndLink(t, dir, "store.db")
		return dir
	},
	"ledger.db a symlink": func(t *testing.T, dir string) string {
		moveAndLink(t, dir, "ledger.db")
		return dir
	},
	"store.db hard-linked": func(t *testing.T, dir string) string {
		if err := os.Link(filepath.Join(dir, "store.db"), filepath.Join(t.TempDir(), "other")); err != nil {
			t.Fatal(err)
		}
		return dir
	},
	"ledger.db hard-linked": func(t *testing.T, dir string) string {
		if err := os.Link(filepath.Join(dir, "ledger.db"), filepath.Join(t.TempDir(), "other")); err != nil {
			t.Fatal(err)
		}
		return dir
	},
	"a -wal file a symlink": func(t *testing.T, dir string) string {
		other := filepath.Join(t.TempDir(), "wal")
		if err := os.WriteFile(other, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		removeIfExists(t, filepath.Join(dir, "ledger.db-wal"))
		if err := os.Symlink(other, filepath.Join(dir, "ledger.db-wal")); err != nil {
			t.Fatal(err)
		}
		return dir
	},
	"a -shm file hard-linked": func(t *testing.T, dir string) string {
		p := filepath.Join(dir, "store.db-shm")
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Link(p, filepath.Join(t.TempDir(), "other")); err != nil {
			t.Fatal(err)
		}
		return dir
	},
	"store.db a directory": func(t *testing.T, dir string) string {
		removeDB(t, dir, "store.db")
		if err := os.Mkdir(filepath.Join(dir, "store.db"), 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	},
}

// moveAndLink moves name out of dir and leaves a symbolic link to it in its place.
func moveAndLink(t *testing.T, dir, name string) {
	t.Helper()
	moved := filepath.Join(t.TempDir(), name)
	if err := os.Rename(filepath.Join(dir, name), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func removeIfExists(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// satbooth backup refuses a data directory whose database files are aliases (a symlink, a hard link) or not regular
// files, before it opens them and before it creates the backup directory. The message names no path.
func TestBackupRefusesAliasedFiles(t *testing.T) {
	for name, alias := range aliasCases {
		t.Run(name, func(t *testing.T) {
			r := stoppedRig(t)
			cfg := *r.cfg
			cfg.DataDir = alias(t, r.cfg.DataDir)
			to := filepath.Join(t.TempDir(), "b")
			if _, err := Backup(context.Background(), &cfg, to, backupNow); err == nil || err.Error() != msgDBFiles {
				t.Fatalf("err %v, want %q", err, msgDBFiles)
			}
			wantNoDir(t, to)
		})
	}
	if strings.Contains(msgDBFiles, "/") {
		t.Fatal("the message names a path")
	}
}

// A start refuses the same data directories, with the reason data_dir, before it opens a database.
func TestRunRefusesAliasedFiles(t *testing.T) {
	for name, alias := range aliasCases {
		t.Run(name, func(t *testing.T) {
			r := stoppedRig(t)
			cfg := *r.cfg
			cfg.DataDir = alias(t, r.cfg.DataDir)
			startRefuses(t, &cfg, "data_dir", msgDBFiles)
		})
	}
}

// A fresh data directory (no database yet) and a stopped one with regular files start and back up as before.
func TestDBFilesRegularAccepted(t *testing.T) {
	if err := checkDBFiles(t.TempDir()); err != nil {
		t.Fatalf("an empty data directory: %v", err)
	}
	r := stoppedRig(t)
	if err := checkDBFiles(r.cfg.DataDir); err != nil {
		t.Fatalf("a stopped data directory: %v", err)
	}
	if _, err := Backup(context.Background(), r.cfg, filepath.Join(t.TempDir(), "b"), backupNow); err != nil {
		t.Fatalf("backup: %v", err)
	}
}
