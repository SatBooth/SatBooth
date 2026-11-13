package setup

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// lockHolderEnv names the directory a re-executed test binary locks for TestOneRunPerDirectoryAcrossProcesses.
const lockHolderEnv = "SATBOOTH_SETUP_LOCK_HOLDER"

// TestLockHolderProcess is the child of TestOneRunPerDirectoryAcrossProcesses: it locks the directory, says so, and
// holds the lock until its standard input ends. Run on its own it does nothing.
func TestLockHolderProcess(t *testing.T) {
	dir := os.Getenv(lockHolderEnv)
	if dir == "" {
		t.Skip("only the child of the two-process test runs this")
	}
	b, err := LockDir(dir)
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	bufio.NewReader(os.Stdin).ReadString(0) // until the parent closes the pipe
	b.Close()
}

// leftoverRun puts into dir an unfinished run of stage 1 that Discard would clear: a journal and the state file's
// scratch name, as the writer journals them.
func leftoverRun(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup/state.json.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := `{"version":1,"stage":"public-init","dirs":["setup"],"files":["setup/state.json.tmp"],"sha256":["` +
		sumHex([]byte("x")) + `"],"prev_state":null}`
	if err := os.WriteFile(filepath.Join(dir, "setup/journal.json"), []byte(j), 0o600); err != nil {
		t.Fatal(err)
	}
}

// While one run holds a directory, a stage, a preflight and a discard on it are refused with ErrBusy and change
// nothing; once the run ends, the same calls work.
func TestOneRunPerDirectory(t *testing.T) {
	f := fxLightning(t)
	e := newEnv(t, f)
	held := baseDir(t)
	holder, err := LockDir(held)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, held)
	if _, err := PublicInit(held, &e.pub, "http://127.0.0.1:8080", detRand(1)); !errors.Is(err, ErrBusy) {
		t.Fatalf("stage: %v", err)
	}
	if err := Preflight(held, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("preflight: %v", err)
	}
	if _, _, err := Discard(held, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard: %v", err)
	}
	if err := ErrBusy; err.Error() != "setup: another satbooth-setup run is using this directory" {
		t.Fatalf("text %q", err)
	}
	sameTree(t, "refused runs", before, snapshot(t, held))
	// A refused run does not release the holder's lock.
	if err := Preflight(held, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("preflight after a refusal: %v", err)
	}
	holder.Close()
	if _, err := PublicInit(held, &e.pub, "http://127.0.0.1:8080", detRand(1)); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if _, err := PublicInit(held, &e.pub, "http://127.0.0.1:8080", detRand(1)); errors.Is(err, ErrBusy) {
		t.Fatal("a finished run still holds the lock")
	}

	// Discard: the same, on a directory with a leftover run it would clear.
	left := baseDir(t)
	leftoverRun(t, left)
	holder, err = LockDir(left)
	if err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, left)
	if _, _, err := Discard(left, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard of a leftover run: %v", err)
	}
	sameTree(t, "refused discard", before, snapshot(t, left))
	holder.Close()
	if removed, _, err := Discard(left, StagePublicInit); err != nil || len(removed) == 0 {
		t.Fatalf("discard after release: %v %v", removed, err)
	}
}

// A second process is refused while the first holds the directory, and runs once the first has ended.
func TestOneRunPerDirectoryAcrossProcesses(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := baseDir(t)
	cmd := exec.Command(exe, "-test.run=^TestLockHolderProcess$")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+dir)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			in.Close()
			cmd.Wait()
		}
	}
	defer release()
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		t.Fatalf("the child said %q", line)
	}
	e := newEnv(t, fxLightning(t))
	before := snapshot(t, dir)
	if _, err := PublicInit(dir, &e.pub, "http://127.0.0.1:8080", detRand(1)); !errors.Is(err, ErrBusy) {
		t.Fatalf("stage: %v", err)
	}
	if _, _, err := Discard(dir, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard: %v", err)
	}
	sameTree(t, "refused runs", before, snapshot(t, dir))
	release()
	if _, err := PublicInit(dir, &e.pub, "http://127.0.0.1:8080", detRand(1)); err != nil {
		t.Fatalf("after the first process ended: %v", err)
	}
}

// A second LockDir of a directory is refused whether it is made by this process or another, and a stage called through
// the package-level wrapper while a Lock is held is refused and changes nothing; after Close it runs. The Lock's own
// methods run on its directory.
func TestLockDirIsExclusive(t *testing.T) {
	e := newEnv(t, fxLightning(t))
	dir := baseDir(t)
	l, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := LockDir(dir); !errors.Is(err, ErrBusy) {
		if err == nil {
			second.Close()
		}
		t.Fatalf("a second LockDir in the same process: %v", err)
	}
	before := snapshot(t, dir)
	if _, err := PublicInit(dir, &e.pub, "http://127.0.0.1:8080", detRand(1)); !errors.Is(err, ErrBusy) {
		t.Fatalf("a wrapper while a Lock is held: %v", err)
	}
	if err := Preflight(dir, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("preflight wrapper: %v", err)
	}
	if _, _, err := Discard(dir, StagePublicInit); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard wrapper: %v", err)
	}
	sameTree(t, "refused wrappers", before, snapshot(t, dir))
	// The Lock's own methods work through its root.
	if err := l.Preflight(StagePublicInit); err != nil {
		t.Fatalf("Lock.Preflight: %v", err)
	}
	if _, err := l.PublicInit(&e.pub, "http://127.0.0.1:8080", detRand(1)); err != nil {
		t.Fatalf("Lock.PublicInit: %v", err)
	}
	if _, _, err := l.Discard(StagePublicInit); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("Lock.Discard after a finished stage: %v", err) // nothing unfinished to discard
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing released it: a new Lock, and the wrappers, work again.
	l2, err := LockDir(dir)
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	l2.Close()
	if err := Preflight(dir, StagePublicInit); errors.Is(err, ErrBusy) {
		t.Fatalf("after Close the wrapper is still refused: %v", err)
	}
}

// A second Close does nothing, and every method of a closed Lock refuses with ErrClosed before it touches anything.
func TestClosedLockRefusesEveryMethod(t *testing.T) {
	e := newEnv(t, fxLightning(t))
	dir := baseDir(t)
	leftoverRun(t, dir)
	l, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("the second Close: %v", err)
	}
	before := snapshot(t, dir)
	calls := map[string]func() error{
		"Preflight":    func() error { return l.Preflight(StagePublicInit) },
		"Discard":      func() error { _, _, err := l.Discard(StagePublicInit); return err },
		"PublicInit":   func() error { _, err := l.PublicInit(&e.pub, "http://127.0.0.1:8080", detRand(1)); return err },
		"PublicFinish": func() error { _, err := l.PublicFinish([]byte("{}"), Digest([]byte("{}")), nil); return err },
		"HubInit": func() error {
			_, err := l.HubInit(&e.hub, []byte("{}"), Digest([]byte("{}")), detRand(1))
			return err
		},
		"HubPair": func() error { _, err := l.HubPair(e.wallets[0], ""); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Errorf("%s on a closed Lock: %v", name, err)
		}
	}
	sameTree(t, "closed Lock", before, snapshot(t, dir))
	if ErrClosed.Error() != "setup: the directory lock is closed" || strings.Contains(ErrClosed.Error(), dir) {
		t.Fatalf("text %q", ErrClosed)
	}
}
