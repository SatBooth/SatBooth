package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/store"
)

var backupNow = func() time.Time { return time.Unix(1_700_000_000, 0) }

// stoppedRig starts the service once and stops it: a data directory at generation 1.
func stoppedRig(t *testing.T) *rig {
	t.Helper()
	r := startRig(t)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	checkCompletedStart(t, r.cfg.DataDir, 1)
	return r
}

// wantNoDir: a refused backup created no backup directory.
func wantNoDir(t *testing.T, to string) {
	t.Helper()
	if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused backup left its directory: %v", err)
	}
}

func TestBackupRefusesWhileRunning(t *testing.T) {
	r := startRig(t)
	to := filepath.Join(t.TempDir(), "b")
	if _, err := Backup(context.Background(), r.cfg, to, backupNow); err == nil || err.Error() != "another SatBooth process holds the data directory" {
		t.Fatalf("err %v", err)
	}
	wantNoDir(t, to)
}

// An incomplete pair state (a pending advance, a first stamp, generation 0) is refused with "start SatBooth once to
// finish its last start", and the refusal writes nothing: the stamps stay as they were (the backup never runs
// checkPair).
func TestBackupRefusesPending(t *testing.T) {
	ctx := context.Background()
	pn := strings.Repeat("9a", 16)
	for name, set := range map[string]func(t *testing.T, st *store.Store, lg *ledger.Ledger, l ledger.PairStamp){
		"ledger marked, store not written": func(t *testing.T, _ *store.Store, lg *ledger.Ledger, l ledger.PairStamp) {
			if err := lg.SetPair(ctx, ledger.PairStamp{ID: l.ID, Gen: 1, Nonce: l.Nonce, Pending: 2, PendingNonce: pn}); err != nil {
				t.Fatal(err)
			}
		},
		"store written, ledger not committed": func(t *testing.T, st *store.Store, lg *ledger.Ledger, l ledger.PairStamp) {
			if err := lg.SetPair(ctx, ledger.PairStamp{ID: l.ID, Gen: 1, Nonce: l.Nonce, Pending: 2, PendingNonce: pn}); err != nil {
				t.Fatal(err)
			}
			if err := st.SetPair(ctx, store.PairStamp{ID: l.ID, Gen: 2, Nonce: pn}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := stoppedRig(t)
			dataDir := r.cfg.DataDir
			lg, err := ledger.Open(ctx, filepath.Join(dataDir, "ledger.db"))
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(ctx, filepath.Join(dataDir, "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			l, err := lg.Pair(ctx)
			if err != nil {
				t.Fatal(err)
			}
			set(t, st, lg, l)
			st.Close()
			lg.Close()
			s0, l0 := pairOf(t, dataDir)
			to := filepath.Join(t.TempDir(), "b")
			if _, err := Backup(ctx, r.cfg, to, backupNow); !errors.Is(err, ErrBackupPending) {
				t.Fatalf("err %v", err)
			}
			wantNoDir(t, to)
			if s1, l1 := pairOf(t, dataDir); s1 != s0 || l1 != l0 {
				t.Fatalf("the refused backup changed the stamps: S %+v L %+v", s1, l1)
			}
		})
	}
	t.Run("never stamped", func(t *testing.T) {
		r := stoppedRig(t)
		cfg := *r.cfg
		cfg.DataDir = filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(cfg.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		pairOf(t, cfg.DataDir) // creates both files, unstamped
		to := filepath.Join(t.TempDir(), "b")
		if _, err := Backup(ctx, &cfg, to, backupNow); !errors.Is(err, ErrBackupPending) {
			t.Fatalf("err %v", err)
		}
		wantNoDir(t, to)
		if s, l := pairOf(t, cfg.DataDir); s != (store.PairStamp{}) || l != (ledger.PairStamp{}) {
			t.Fatalf("the refused backup stamped the pair: S %+v L %+v", s, l)
		}
	})
	t.Run("first stamp without its advance", func(t *testing.T) {
		r := stoppedRig(t)
		cfg := *r.cfg
		cfg.DataDir = filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(cfg.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		lg, err := ledger.Open(ctx, filepath.Join(cfg.DataDir, "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "store.db"))
		if err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("e7", 16)
		if err := lg.SetPair(ctx, ledger.PairStamp{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetPair(ctx, store.PairStamp{ID: id}); err != nil {
			t.Fatal(err)
		}
		st.Close()
		lg.Close()
		to := filepath.Join(t.TempDir(), "b")
		if _, err := Backup(ctx, &cfg, to, backupNow); !errors.Is(err, ErrBackupPending) {
			t.Fatalf("err %v", err)
		}
		wantNoDir(t, to)
	})
	if !strings.Contains(ErrBackupPending.Error(), "start SatBooth once to finish its last start") {
		t.Fatalf("ErrBackupPending %q", ErrBackupPending)
	}
}

// A mismatched pair gets the start's own refusal, and the stamps stay as they were.
func TestBackupRefusesMismatch(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		set  func(t *testing.T, r *rig, st *store.Store, lg *ledger.Ledger, id string)
		want error
	}{
		"older store": {func(t *testing.T, _ *rig, _ *store.Store, lg *ledger.Ledger, id string) {
			if err := lg.SetPair(ctx, ledger.PairStamp{ID: id, Gen: 2, Nonce: strings.Repeat("9b", 16)}); err != nil {
				t.Fatal(err)
			}
		}, ErrPairOlderStore},
		"older ledger": {func(t *testing.T, _ *rig, st *store.Store, _ *ledger.Ledger, id string) {
			if err := st.SetPair(ctx, store.PairStamp{ID: id, Gen: 2, Nonce: strings.Repeat("9b", 16)}); err != nil {
				t.Fatal(err)
			}
		}, ErrPairOlderLedger},
		"same generation, another nonce": {func(t *testing.T, r *rig, _ *store.Store, _ *ledger.Ledger, _ string) {
			// A store from another timeline of this pair: same id, same generation, another nonce.
			db, err := sql.Open("sqlite3", "file:"+filepath.Join(r.cfg.DataDir, "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE pair SET nonce = ?`, strings.Repeat("9c", 16)); err != nil {
				t.Fatal(err)
			}
		}, ErrPairDifferent},
		"different ids": {nil, ErrPairDifferent},
	} {
		t.Run(name, func(t *testing.T) {
			r := stoppedRig(t)
			dataDir := r.cfg.DataDir
			if c.set != nil {
				lg, err := ledger.Open(ctx, filepath.Join(dataDir, "ledger.db"))
				if err != nil {
					t.Fatal(err)
				}
				st, err := store.Open(ctx, filepath.Join(dataDir, "store.db"))
				if err != nil {
					t.Fatal(err)
				}
				sp, err := st.Pair(ctx)
				if err != nil {
					t.Fatal(err)
				}
				c.set(t, r, st, lg, sp.ID)
				st.Close()
				lg.Close()
			} else {
				// The store of another installation, at the same generation.
				other := stoppedRig(t)
				removeDB(t, dataDir, "store.db")
				copyFile(t, filepath.Join(other.cfg.DataDir, "store.db"), filepath.Join(dataDir, "store.db"))
			}
			s0, l0 := pairOf(t, dataDir)
			to := filepath.Join(t.TempDir(), "b")
			if _, err := Backup(ctx, r.cfg, to, backupNow); !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			wantNoDir(t, to)
			if s1, l1 := pairOf(t, dataDir); s1 != s0 || l1 != l0 {
				t.Fatalf("the refused backup changed the stamps: S %+v L %+v", s1, l1)
			}
		})
	}
}

func TestBackupRefusesExistingDir(t *testing.T) {
	r := stoppedRig(t)
	to := t.TempDir()
	if _, err := Backup(context.Background(), r.cfg, to, backupNow); !errors.Is(err, ErrBackupExists) {
		t.Fatalf("err %v", err)
	}
	if ents, err := os.ReadDir(to); err != nil || len(ents) != 0 {
		t.Fatalf("the refused backup wrote into the existing directory: %d %v", len(ents), err)
	}
}

// A -to inside the data directory is refused, also when reached through a symbolic link or a "..".
func TestBackupRefusesInsideDataDir(t *testing.T) {
	r := stoppedRig(t)
	dataDir := r.cfg.DataDir
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dataDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A link outside the data directory to a directory inside it (not to the data directory itself), so the
	// lexical ancestors of the target never reach the data directory.
	subLink := filepath.Join(t.TempDir(), "outside-link")
	if err := os.Symlink(filepath.Join(dataDir, "sub"), subLink); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{
		filepath.Join(dataDir, "b"),
		filepath.Join(link, "b"),
		filepath.Join(dataDir, "sub", "b"),
		dataDir + "/sub/../b",
		filepath.Join(subLink, "backup"),
		filepath.Join(subLink, "deeper", "backup"),
	} {
		if _, err := Backup(context.Background(), r.cfg, to, backupNow); !errors.Is(err, ErrBackupInside) {
			t.Fatalf("%q: err %v", strings.TrimPrefix(to, dataDir), err)
		}
		wantNoDir(t, filepath.Clean(to))
	}
	// The same directory under another spelling: on a case-insensitive file system the data directory upper-cased is
	// the same directory, which a comparison of path strings misses.
	upper := strings.ToUpper(dataDir)
	fi, err1 := os.Stat(dataDir)
	ui, err2 := os.Stat(upper)
	if err1 != nil || err2 != nil || !os.SameFile(fi, ui) {
		t.Log("case-sensitive file system: the upper-cased case is skipped")
		return
	}
	if _, err := Backup(context.Background(), r.cfg, filepath.Join(upper, "b"), backupNow); !errors.Is(err, ErrBackupInside) {
		t.Fatalf("upper-cased: err %v", err)
	}
	wantNoDir(t, filepath.Join(dataDir, "b"))
}

// A relative -to that looks like a SQLite URI ("file:...") is made absolute before anything uses it, so VACUUM INTO
// never reads it as a URI: the copies land inside the backup directory, 0600, and nothing is written elsewhere. A form
// whose parent does not exist is refused, writing nothing.
func TestBackupURILookingTo(t *testing.T) {
	r := stoppedRig(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Mkdir("x", 0o700); err != nil {
		t.Fatal(err)
	}
	if gen, err := Backup(context.Background(), r.cfg, "file:x", backupNow); err != nil || gen != 1 {
		t.Fatalf("file:x: gen %d err %v", gen, err)
	}
	for _, name := range []string{"ledger.db", "store.db", "manifest.json"} {
		fi, err := os.Stat(filepath.Join(cwd, "file:x", name))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("file:x/%s: %v %v", name, fi, err)
		}
	}
	if _, err := Backup(context.Background(), r.cfg, "file:./y", backupNow); err == nil {
		t.Fatal("file:./y: a backup into a missing parent was written")
	}
	// Nothing outside file:x: x stays empty, no y, no file: directory.
	if ents, err := os.ReadDir(filepath.Join(cwd, "x")); err != nil || len(ents) != 0 {
		t.Fatalf("x holds %d entries (%v): a copy was written outside the backup directory", len(ents), err)
	}
	ents, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"file:x", "x"}) {
		t.Fatalf("the working directory holds %v", names)
	}
}

// A cancelled context stops the backup: it fails and leaves no manifest.
func TestBackupCancelled(t *testing.T) {
	r := stoppedRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	to := filepath.Join(t.TempDir(), "b")
	if _, err := Backup(ctx, r.cfg, to, backupNow); err == nil {
		t.Fatal("a cancelled backup succeeded")
	}
	if _, err := os.Stat(filepath.Join(to, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cancelled backup left a manifest: %v", err)
	}
}

// A ledger or a store whose schema is behind or ahead of this build's gets a fixed message (no path, no version) and
// is left as it was.
func TestBackupRefusesSchemaVersions(t *testing.T) {
	for _, c := range []struct {
		name, file, table string
		version           int
		drop              bool
		want              error
	}{
		{"ledger behind", "ledger.db", "schema_version", 3, true, ErrBackupLedgerBehind},
		{"ledger newer", "ledger.db", "schema_version", 99, false, ErrBackupLedgerNewer},
		{"store newer", "store.db", "store_schema_version", 99, false, ErrBackupStoreNewer},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := stoppedRig(t)
			db, err := sql.Open("sqlite3", "file:"+filepath.Join(r.cfg.DataDir, c.file))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			q := fmt.Sprintf(`UPDATE %s SET version = %d`, c.table, c.version)
			if c.drop {
				q = `DROP TABLE pair; ` + q
			}
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
			to := filepath.Join(t.TempDir(), "b")
			if _, err := Backup(context.Background(), r.cfg, to, backupNow); !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			wantNoDir(t, to)
			var v int
			if err := db.QueryRow(fmt.Sprintf(`SELECT version FROM %s`, c.table)).Scan(&v); err != nil || v != c.version {
				t.Fatalf("the refused backup changed the version: %d %v", v, err)
			}
		})
	}
	for _, e := range []error{ErrBackupLedgerNewer, ErrBackupStoreNewer} {
		if !strings.Contains(e.Error(), "newer than this binary") || strings.ContainsAny(e.Error(), "0123456789/") {
			t.Fatalf("%q", e)
		}
	}
}

// A store whose schema is behind this build's, or a missing database, is refused and left as it was: the backup
// never migrates and never creates a file.
func TestBackupRefusesBehindOrMissing(t *testing.T) {
	ctx := context.Background()
	r := stoppedRig(t)
	dataDir := r.cfg.DataDir
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE pair; UPDATE store_schema_version SET version = 6`); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(t.TempDir(), "b")
	if _, err := Backup(ctx, r.cfg, to, backupNow); !errors.Is(err, ErrBackupStoreBehind) ||
		!strings.Contains(err.Error(), "start SatBooth once to upgrade it") {
		t.Fatalf("err %v", err)
	}
	wantNoDir(t, to)
	var v int
	if err := db.QueryRow(`SELECT version FROM store_schema_version`).Scan(&v); err != nil || v != 6 {
		t.Fatalf("the refused backup changed the version: %d %v", v, err)
	}
	db.Close()

	removeDB(t, dataDir, "store.db")
	if _, err := Backup(ctx, r.cfg, to, backupNow); err == nil || err.Error() != "cannot open the store" {
		t.Fatalf("err %v", err)
	}
	wantNoDir(t, to)
	if _, err := os.Stat(filepath.Join(dataDir, "store.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the backup created store.db: %v", err)
	}
}

// The manifest is byte for byte the backup format's, its sizes and hashes match the files, the directory is 0700 and
// holds exactly the three files, each 0600 (SQLite makes its output 0644 under umask 022, so this pins the chmod), and
// each copy is in rollback journal mode (header bytes 18 and 19 both 1), unchanged by the integrity check. A quote in
// the path pins the bound parameter.
func TestBackupManifest(t *testing.T) {
	r := startRig(t)
	r.createOrder("manifest-1")
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(t.TempDir(), "it's a backup")
	gen, err := Backup(context.Background(), r.cfg, to, backupNow)
	if err != nil || gen != 1 {
		t.Fatalf("gen %d err %v", gen, err)
	}
	fi, err := os.Stat(to)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v %v", fi.Mode(), err)
	}
	ents, err := os.ReadDir(to)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v %v", e.Name(), info.Mode(), err)
		}
	}
	if !slices.Equal(names, []string{"ledger.db", "manifest.json", "store.db"}) {
		t.Fatalf("files %v", names)
	}
	sp, _ := pairOf(t, r.cfg.DataDir)
	want := fmt.Sprintf(`{"format":"satbooth-backup/v1","created_at":1700000000,"pair_id":"%s","generation":1,"files":[`, sp.ID)
	for i, name := range []string{"ledger.db", "store.db"} {
		b, err := os.ReadFile(filepath.Join(to, name))
		if err != nil {
			t.Fatal(err)
		}
		if b[18] != 1 || b[19] != 1 {
			t.Fatalf("%s: header bytes 18, 19: %d %d, want 1 1", name, b[18], b[19])
		}
		h := sha256.Sum256(b)
		if i > 0 {
			want += ","
		}
		want += fmt.Sprintf(`{"name":"%s","bytes":%d,"sha256":"%s"}`, name, len(b), hex.EncodeToString(h[:]))
	}
	want += "]}"
	got, err := os.ReadFile(filepath.Join(to, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("manifest\n got %s\nwant %s", got, want)
	}
	// The backup changed no stamp.
	checkCompletedStart(t, r.cfg.DataDir, 1)
}

// serveOnce starts Run on cfg, reads each order's view over the private API while it runs, and stops it.
func serveOnce(t *testing.T, cfg *Config, key string, ids []string, opts ...Option) []string {
	t.Helper()
	logs := &syncBuf{}
	pub, prv := lnr(t), lnr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, append([]Option{WithWallet(newFakeWallet()), WithListeners(pub, prv),
			WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))}, opts...)...)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(logs.String(), logStarted) {
		select {
		case err := <-done:
			t.Fatalf("Run: %v\n%s", err, logs.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not start")
		}
	}
	var views []string
	for _, id := range ids {
		code, b := do(t, "GET", "http://"+prv.Addr().String()+"/v1/orders/"+id, key, nil)
		if code != http.StatusOK {
			t.Fatalf("get %s: %d %s", id, code, b)
		}
		views = append(views, string(b))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	return views
}

// A service with orders and a settled refund stops; it is backed up; the data directory is wiped and the two files
// restored; Run starts, the stamp advances by one, and the API shows the same orders, refund figures included.
func TestBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	withReader := func(dir string, m map[string]any) {
		payoutRig()(dir, m)
		if err := os.WriteFile(m["api_keys_file"].(string), keysJSON([3]string{"merchant", "orders:write", testAPIKey},
			[3]string{"reader", "orders:read", testReadKey}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := startRigWith(t, withReader, WithPayoutWallet(newPayoutWallet()))
	paid, open := r.createOrder("round-1"), r.createOrder("round-2")
	if code, _ := do(t, "POST", paid.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	if hashes := r.wallet.settleAll(); len(hashes) != 1 {
		t.Fatalf("want one invoice, got %d", len(hashes))
	}
	eventually(t, "the order to be paid", func() bool { return r.getOrder(paid.ID).State == "paid" })
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	dataDir := r.cfg.DataDir

	// A settled refund of the paid order.
	st, err := store.Open(ctx, filepath.Join(dataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	id, keyID := strings.Repeat("c", 32), strings.Repeat("d", 16)
	exp := now.Add(24 * time.Hour)
	ap := hex.EncodeToString([]byte("satbooth/refund-approval/v1")) + "03" + keyID + id + paid.ID +
		hex.EncodeToString(be64(1_000_000)) + hex.EncodeToString(be64(exp.Unix())) + strings.Repeat("5a", 64)
	if err := st.AddRefund(ctx, store.Refund{ID: id, OrderID: paid.ID, KeyID: keyID, Approval: ap, Invoice: "lnbcrt" + strings.Repeat("q", 40),
		AmountMsat: 1_000_000, ClaimedAt: now, ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRefundSend(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRefundResult(ctx, store.RefundResult{RefundID: id, Outcome: store.RefundOutcomeSettled, Reason: store.RefundReasonSettled, At: now}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	ids := []string{paid.ID, open.ID}
	before := serveOnce(t, r.cfg, testReadKey, ids, WithPayoutWallet(newPayoutWallet()))
	if !strings.Contains(before[0], `"refunded_msat":1000000`) {
		t.Fatalf("the paid order's view has no refund: %s", before[0])
	}
	checkCompletedStart(t, dataDir, 2)

	to := filepath.Join(t.TempDir(), "b")
	gen, err := Backup(ctx, r.cfg, to, backupNow)
	if err != nil || gen != 2 {
		t.Fatalf("gen %d err %v", gen, err)
	}

	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(to, "store.db"), filepath.Join(dataDir, "store.db"))
	copyFile(t, filepath.Join(to, "ledger.db"), filepath.Join(dataDir, "ledger.db"))

	after := serveOnce(t, r.cfg, testReadKey, ids, WithPayoutWallet(newPayoutWallet()))
	checkCompletedStart(t, dataDir, 3)
	if !slices.Equal(before, after) {
		t.Fatalf("the restored service shows other orders\nbefore %v\nafter  %v", before, after)
	}
}

// Restored as docs/backup-and-restore.md says, but store.db from one backup and ledger.db from a later one (a start
// between them): the
// start is refused, the store is older.
func TestRestoreMixedRefused(t *testing.T) {
	ctx := context.Background()
	r := stoppedRig(t)
	dataDir := r.cfg.DataDir
	b1 := filepath.Join(t.TempDir(), "b1")
	if gen, err := Backup(ctx, r.cfg, b1, backupNow); err != nil || gen != 1 {
		t.Fatalf("backup 1: %d %v", gen, err)
	}
	if err, logs, _ := runAgain(t, r.cfg); err != nil {
		t.Fatalf("second start: %v\n%s", err, logs)
	}
	b2 := filepath.Join(t.TempDir(), "b2")
	if gen, err := Backup(ctx, r.cfg, b2, backupNow); err != nil || gen != 2 {
		t.Fatalf("backup 2: %d %v", gen, err)
	}
	removeDB(t, dataDir, "store.db")
	removeDB(t, dataDir, "ledger.db")
	copyFile(t, filepath.Join(b1, "store.db"), filepath.Join(dataDir, "store.db"))
	copyFile(t, filepath.Join(b2, "ledger.db"), filepath.Join(dataDir, "ledger.db"))
	err, logs, infos := runAgain(t, r.cfg)
	wantRefused(t, err, logs, infos, ErrPairOlderStore)
}
