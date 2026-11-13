package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/setup"
)

// While another handle holds the directory, every form of the command is refused before it asks a question or reads
// an answers file, and changes nothing. With the holder gone, the same command runs.
func TestCommandIsRefusedBeforeItsFirstQuestionWhileTheDirectoryIsHeld(t *testing.T) {
	d, aDir := tmp(t), tmp(t)
	pubFile := writeJSON(t, aDir, "pub.json", everyRailAnswers())
	// Another process holds the directory (this process's own Lock would be reused by the command).
	release := holdInChild(t, d)
	before := tree(t, d)
	const busy = "another satbooth-setup run is using this directory"
	missing := filepath.Join(aDir, "no-such-answers.json") // read first, it would give another error
	for name, args := range map[string][]string{
		"public init":       {"public", "init", "-dir", d, "-host-dir", "/srv/satbooth", "-answers", missing},
		"public init asks":  {"public", "init", "-dir", d, "-host-dir", "/srv/satbooth"},
		"hub init":          {"hub", "init", "-dir", d, "-handoff", missing, "-answers", missing},
		"hub pair asks":     {"hub", "pair", "-dir", d},
		"public finish":     {"public", "finish", "-dir", d, "-handoff", missing},
		"discard":           {"public", "init", "-dir", d, "-discard-incomplete"},
		"discard hub pair":  {"hub", "pair", "-dir", d, "-discard-incomplete"},
		"discard w/ answer": {"public", "init", "-dir", d, "-answers", pubFile, "-discard-incomplete"},
	} {
		r := do("everything the prompts would read\n", args...)
		if r.code != 1 || !strings.Contains(r.errOut, busy) {
			t.Errorf("%s: %d %q", name, r.code, r.errOut)
		}
		// No prompt text: the only output is the one refusal line.
		if r.out != "" || strings.Count(strings.TrimSpace(r.errOut), "\n") != 0 {
			t.Errorf("%s: output beyond the refusal: %q / %q", name, r.out, r.errOut)
		}
		sameTree(t, name, before, tree(t, d))
	}
	release()
	if r := do("", "public", "init", "-dir", d, "-host-dir", "/srv/satbooth", "-answers", pubFile); r.code != 0 {
		t.Fatalf("after the holder ended: %d %s", r.code, r.errOut)
	}
}

// syncBuf is a writer the test can read while the command writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const tryEnv = "SATBOOTH_SETUP_CMD_LOCK_TRY"

// TestLockTryProcess is the child of tryLock: it tries to lock the directory once and says what happened. Run on its
// own it does nothing.
func TestLockTryProcess(t *testing.T) {
	dir := os.Getenv(tryEnv)
	if dir == "" {
		t.Skip("only the child of the lock tests runs this")
	}
	l, err := setup.LockDir(dir)
	if err != nil {
		os.Stdout.WriteString("refused: " + err.Error() + "\n")
		return
	}
	l.Close()
	os.Stdout.WriteString("locked\n")
}

// tryLock asks another process to take dir's lock once, and returns what it said.
func tryLock(t *testing.T, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLockTryProcess$")
	cmd.Env = append(os.Environ(), tryEnv+"="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the child failed: %v", err)
	}
	line, _, _ := strings.Cut(string(out), "\n") // the test binary adds its own PASS line
	return strings.TrimSpace(line)
}

// The lock is held while the command waits at its first question: a child process that tries the directory then is
// refused. (A command that released the lock between its preflight and its stage would let the child in.) When the
// command has returned, a child can take the lock: the command released it.
func TestCommandHoldsTheLockThroughItsQuestionsAndReleasesIt(t *testing.T) {
	d := tmp(t)
	in, w := io.Pipe()
	var out, errOut syncBuf
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"public", "init", "-dir", d, "-host-dir", "/srv/satbooth"}, in, &out, &errOut)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(errOut.String(), "Network (") { // the first prompt
		if time.Now().After(deadline) {
			t.Fatalf("no prompt: %q", errOut.String())
		}
		select {
		case code := <-done:
			t.Fatalf("the command ended before its first prompt: %d %s", code, errOut.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	got := tryLock(t, d)
	if !strings.HasPrefix(got, "refused:") || !strings.Contains(got, "another satbooth-setup run is using this directory") {
		t.Fatalf("a child at the command's first prompt: %q", got)
	}
	w.Close() // the input ends: the command stops at its question
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("the command ended with %d", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the command did not end")
	}
	if got := tryLock(t, d); got != "locked" {
		t.Fatalf("a child after the command returned: %q (the command kept its lock)", got)
	}
	// A command that finishes its stage releases it too.
	aDir := tmp(t)
	pubFile := writeJSON(t, aDir, "pub.json", everyRailAnswers())
	if r := do("", "public", "init", "-dir", d, "-host-dir", "/srv/satbooth", "-answers", pubFile); r.code != 0 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if got := tryLock(t, d); got != "locked" {
		t.Fatalf("a child after a finished stage: %q", got)
	}
}

const holderEnv = "SATBOOTH_SETUP_CMD_LOCK_HOLDER"

// TestLockHolderProcess is the child of the tests above: it locks the directory, says so, and holds the lock until its
// standard input ends. Run on its own it does nothing.
func TestLockHolderProcess(t *testing.T) {
	dir := os.Getenv(holderEnv)
	if dir == "" {
		t.Skip("only the child of the lock tests runs this")
	}
	l, err := setup.LockDir(dir)
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	bufio.NewReader(os.Stdin).ReadString(0)
	l.Close()
}

// holdInChild starts a process that holds dir's lock and returns the function that ends it.
func holdInChild(t *testing.T, dir string) (release func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLockHolderProcess$")
	cmd.Env = append(os.Environ(), holderEnv+"="+dir)
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
	done := false
	release = func() {
		if !done {
			done = true
			in.Close()
			cmd.Wait()
		}
	}
	t.Cleanup(release)
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "locked" {
		t.Fatalf("the child said %q", line)
	}
	return release
}
