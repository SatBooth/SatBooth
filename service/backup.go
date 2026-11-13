package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/store"
)

// The backup's refusals (plan 10). Each is a fixed message: no path, id or generation.
var (
	ErrBackupExists       = errors.New("the backup directory already exists")
	ErrBackupInside       = errors.New("the backup directory must not be inside the data directory")
	ErrBackupPending      = errors.New("the store and the ledger are not at a finished start: start SatBooth once to finish its last start")
	ErrBackupStoreBehind  = errors.New("the store's schema is older than this build: start SatBooth once to upgrade it")
	ErrBackupLedgerBehind = errors.New("the ledger's schema is older than this build: start SatBooth once to upgrade it")
	ErrBackupStoreNewer   = errors.New("the store's schema is newer than this binary")
	ErrBackupLedgerNewer  = errors.New("the ledger's schema is newer than this binary")
)

// manifestFile is one entry of the manifest's files.
type manifestFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// manifest is manifest.json, its fields in the order of the backup format.
type manifest struct {
	Format     string         `json:"format"`
	CreatedAt  int64          `json:"created_at"`
	PairID     string         `json:"pair_id"`
	Generation int64          `json:"generation"`
	Files      []manifestFile `json:"files"`
}

// Backup copies the store and the ledger of cfg's data directory into the new directory to, as one matched pair, and
// returns the pair's generation (plan 10). It is offline: it takes the data directory's lock, so it fails while
// SatBooth runs. It refuses unless both stamps are equal at a finished start (a mismatch gets ErrPairOlderStore,
// ErrPairOlderLedger or ErrPairDifferent, an incomplete state ErrBackupPending), and it never migrates or stamps: it
// changes no row and no schema (SQLite may checkpoint the WAL). Every refusal comes before to is created.
//
// to must not exist and must not be inside the data directory; it is created with mode 0700. Each database is
// copied with VACUUM INTO, set to mode 0600, fsynced and checked with quick_check; then manifest.json (0600) is
// written under a temporary name, fsynced and renamed, and the directory is fsynced. A backup that fails after to
// exists leaves no manifest.json, so it never looks like a valid backup.
//
// Every error is a fixed message, as in Run: no path and no driver text.
func Backup(ctx context.Context, cfg *Config, to string, now func() time.Time) (int64, error) {
	if now == nil {
		now = time.Now
	}
	if to == "" {
		return 0, errors.New("the backup directory is missing")
	}
	// Absolute from here on: SQLite reads a relative path that starts with "file:" as a URI.
	to, err := filepath.Abs(to)
	if err != nil {
		return 0, errors.New("cannot create the backup directory")
	}
	dataDir := cfg.DataDir
	if fi, err := os.Stat(dataDir); errors.Is(err, os.ErrNotExist) {
		return 0, errors.New("data_dir does not exist")
	} else if err == nil && !fi.IsDir() {
		return 0, errors.New("data_dir is not a directory")
	}
	// prepareDataDir creates only a missing directory, which the Stat above has excluded; here it checks the mode.
	if err := prepareDataDir(dataDir); err != nil {
		return 0, err
	}
	in, err := insideDir(dataDir, to)
	if err != nil {
		return 0, errors.New("cannot read data_dir")
	}
	if in {
		return 0, ErrBackupInside
	}
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := checkDBFiles(dataDir); err != nil {
		return 0, err
	}

	lg, err := ledger.OpenExisting(ctx, filepath.Join(dataDir, "ledger.db"))
	if errors.Is(err, ledger.ErrSchemaBehind) {
		return 0, ErrBackupLedgerBehind
	} else if errors.Is(err, ledger.ErrSchemaNewer) {
		return 0, ErrBackupLedgerNewer
	} else if err != nil {
		return 0, errors.New("cannot open the ledger")
	}
	defer lg.Close()
	st, err := store.OpenExisting(ctx, filepath.Join(dataDir, "store.db"))
	if errors.Is(err, store.ErrSchemaBehind) {
		return 0, ErrBackupStoreBehind
	} else if errors.Is(err, store.ErrSchemaNewer) {
		return 0, ErrBackupStoreNewer
	} else if err != nil {
		return 0, errors.New("cannot open the store")
	}
	defer st.Close()

	// The stamps are only read: checkPair would stamp or advance them.
	sp, err := st.Pair(ctx)
	if err != nil {
		return 0, errors.New("cannot read the store")
	}
	lp, err := lg.Pair(ctx)
	if err != nil {
		return 0, errors.New("cannot read the ledger")
	}
	// The one state a backup takes: equal ids, equal generations of a finished start (at least 1) with equal nonces,
	// no advance pending. A mismatch gets the start's own refusal; an incomplete state (a pending advance, a first
	// stamp, generation 0) is finished by the next start.
	switch state, err := classifyPair(sp, lp); {
	case state == pairRefused:
		return 0, err
	case state != pairEqual || lp.Gen < 1:
		return 0, ErrBackupPending
	}

	if err := os.Mkdir(to, 0o700); errors.Is(err, os.ErrExist) {
		return 0, ErrBackupExists
	} else if err != nil {
		return 0, errors.New("cannot create the backup directory")
	}
	// The umask can only clear bits; set the mode exactly.
	if err := os.Chmod(to, 0o700); err != nil {
		return 0, errors.New("cannot create the backup directory")
	}
	if err := syncDir(filepath.Dir(filepath.Clean(to))); err != nil {
		return 0, errors.New("cannot sync the backup directory")
	}

	m := manifest{Format: "satbooth-backup/v1", CreatedAt: now().Unix(), PairID: lp.ID, Generation: lp.Gen}
	copies := []struct {
		name, what string
		vac        func(context.Context, string) error
		check      func(context.Context, string) error
	}{
		{"ledger.db", "the ledger", lg.VacuumInto, ledger.QuickCheck},
		{"store.db", "the store", st.VacuumInto, store.QuickCheck},
	}
	for _, c := range copies {
		path, what := filepath.Join(to, c.name), c.what
		if err := c.vac(ctx, path); err != nil {
			return 0, errors.New("cannot copy " + what)
		}
		// SQLite creates the copy with the umask's mode (0644 under umask 022).
		if err := os.Chmod(path, 0o600); err != nil {
			return 0, errors.New("cannot set the mode of the copy of " + what)
		}
		if err := syncFile(path); err != nil {
			return 0, errors.New("cannot sync the copy of " + what)
		}
		if err := c.check(ctx, path); err != nil {
			return 0, errors.New("the copy of " + what + " failed its integrity check")
		}
		n, sum, err := hashFile(path)
		if err != nil {
			return 0, errors.New("cannot read the copy of " + what)
		}
		m.Files = append(m.Files, manifestFile{Name: c.name, Bytes: n, SHA256: sum})
	}

	if err := writeManifest(to, m); err != nil {
		return 0, errors.New("cannot write the backup manifest")
	}
	if err := syncDir(to); err != nil {
		// A failed backup never has a manifest, even when only this last sync failed.
		os.Remove(filepath.Join(to, "manifest.json"))
		return 0, errors.New("cannot sync the backup directory")
	}
	return lp.Gen, nil
}

// insideDir reports whether to, an absolute path that need not exist yet, is dir or inside it. It compares files, not
// path strings: to and each of its ancestors that exists is checked with os.SameFile against dir (Stat follows
// symbolic links), so a link, another spelling on a case-insensitive file system, or a second mount point of the same
// directory cannot hide it. The walk is done twice: over to as given, and over the deepest part of to that exists with
// its symbolic links resolved (filepath.EvalSymlinks), so a link outside dir to a directory inside it is walked from
// its target, whose ancestors reach dir. A part that cannot be resolved is an error.
func insideDir(dir, to string) (bool, error) {
	d, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	to = filepath.Clean(to)
	existing := to
	for {
		if _, err := os.Lstat(existing); err == nil || existing == filepath.Dir(existing) {
			break
		}
		existing = filepath.Dir(existing)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return false, err
	}
	for _, start := range []string{to, resolved} {
		for p := start; ; p = filepath.Dir(p) {
			if fi, err := os.Stat(p); err == nil && os.SameFile(fi, d) {
				return true, nil
			}
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	return false, nil
}

// writeManifest writes m as manifest.json in dir (mode 0600): to a temporary name first, fsynced, then renamed, so
// a failure never leaves a manifest.json behind.
func writeManifest(dir string, m manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "manifest.json")); err != nil {
		return err
	}
	ok = true
	return nil
}

// syncFile fsyncs the file at path.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir fsyncs the directory at path, so the entries made in it survive a power loss.
func syncDir(path string) error { return syncFile(path) }

// hashFile returns the size and the lowercase hex SHA-256 of the file at path.
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
