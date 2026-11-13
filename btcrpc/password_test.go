package btcrpc

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// refPassword is the reference predicate: one optional final newline stripped, then 1 to 128 bytes of
// printable ASCII (0x21 to 0x7e).
func refPassword(content []byte) ([]byte, bool) {
	if n := len(content); n > 0 && content[n-1] == '\n' {
		content = content[:n-1]
	}
	if len(content) < 1 || len(content) > 128 {
		return nil, false
	}
	for _, b := range content {
		if b < 0x21 || b > 0x7e {
			return nil, false
		}
	}
	return content, true
}

func writePw(t testing.TB, path string, content []byte, mode os.FileMode) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPasswordBasics(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pw")

	writePw(t, p, []byte("s3cret-Pass!\n"), 0o600)
	got, err := LoadPassword(p)
	if err != nil || string(got) != "s3cret-Pass!" {
		t.Fatalf("trailing newline: %q, %v", got, err)
	}
	writePw(t, p, []byte("s3cret-Pass!"), 0o400)
	if got, err = LoadPassword(p); err != nil || string(got) != "s3cret-Pass!" {
		t.Fatalf("mode 0400 is stricter than 0600: %q, %v", got, err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o700, 0o601} {
		writePw(t, p, []byte("abc"), mode)
		if _, err := LoadPassword(p); !errors.Is(err, ErrPasswordFile) {
			t.Errorf("mode %o: want ErrPasswordFile, got %v", mode, err)
		}
	}
	writePw(t, p, bytes.Repeat([]byte("a"), 129), 0o600)
	if _, err := LoadPassword(p); !errors.Is(err, ErrPasswordFile) {
		t.Errorf("129 bytes: %v", err)
	}
	writePw(t, p, append(bytes.Repeat([]byte("a"), 128), '\n'), 0o600)
	if _, err := LoadPassword(p); err != nil {
		t.Errorf("128 bytes and a newline: %v", err)
	}
	writePw(t, p, []byte("ab\tc"), 0o600)
	if _, err := LoadPassword(p); !errors.Is(err, ErrPasswordFile) {
		t.Errorf("tab: %v", err)
	}
	writePw(t, p, []byte("abc\n\n"), 0o600)
	if _, err := LoadPassword(p); !errors.Is(err, ErrPasswordFile) {
		t.Errorf("two newlines: %v", err)
	}
	writePw(t, p, []byte("\n"), 0o600)
	if _, err := LoadPassword(p); !errors.Is(err, ErrPasswordFile) {
		t.Errorf("empty after the newline: %v", err)
	}

	// a symlink to a good 0600 file is refused
	target := filepath.Join(dir, "target")
	writePw(t, target, []byte("abc"), 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPassword(link); err == nil {
		t.Error("symlink accepted")
	}
	// a directory and a missing file
	if _, err := LoadPassword(dir); err == nil {
		t.Error("directory accepted")
	}
	if _, err := LoadPassword(filepath.Join(dir, "nope")); err == nil {
		t.Error("missing file accepted")
	}
}

// The error never carries the content of the file.
func TestLoadPasswordErrorHasNoContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	writePw(t, p, []byte("TopSecret\tvalue"), 0o600)
	_, err := LoadPassword(p)
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("TopSecret")) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadPasswordProperty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	r := rand.New(rand.NewPCG(7, 9))
	classes := []byte{0x00, 0x09, 0x0a, 0x0d, 0x1f, 0x20, 0x21, 0x7e, 0x7f, 0x80, 0xff}
	n := 0
	for length := 0; length <= 130; length++ {
		for k := 0; k < 16; k++ {
			content := make([]byte, length)
			for i := range content {
				content[i] = byte(0x21 + r.IntN(0x7e-0x21+1))
			}
			switch {
			case length > 0 && k == 0: // a bad class in a random position
				content[r.IntN(length)] = classes[r.IntN(len(classes))]
			case length > 0 && k == 1: // a final newline
				content[length-1] = '\n'
			case length > 0 && k == 2: // a space or newline in the first or last position
				content[0] = []byte{0x20, 0x0a}[r.IntN(2)]
			case length > 0 && k == 3:
				content[length-1] = classes[r.IntN(len(classes))]
			case length > 1 && k == 4: // newline in the middle
				content[r.IntN(length-1)] = '\n'
			case k > 4 && length > 0: // random bytes of every class
				for i := range content {
					content[i] = byte(r.IntN(256))
				}
			}
			writePw(t, p, content, 0o600)
			want, ok := refPassword(content)
			got, err := LoadPassword(p)
			if (err == nil) != ok || (ok && !bytes.Equal(got, want)) {
				t.Fatalf("length %d class %d: predicate says %v, LoadPassword gave err %v", length, k, ok, err)
			}
			n++
		}
	}
	if n < 2000 {
		t.Fatalf("only %d files", n)
	}
}

// 0x20, 0x7f and 0x0a (a newline anywhere but last is part of the content) in every position of every
// length from 1 to 130.
func TestLoadPasswordEveryPosition(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	for length := 1; length <= 130; length++ {
		for pos := 0; pos < length; pos++ {
			for _, b := range []byte{0x20, 0x7f, 0x0a} {
				content := bytes.Repeat([]byte("a"), length)
				content[pos] = b
				writePw(t, p, content, 0o600)
				want, ok := refPassword(content)
				got, err := LoadPassword(p)
				if (err == nil) != ok || (ok && !bytes.Equal(got, want)) {
					t.Fatalf("length %d pos %d byte %#x: predicate says %v, got err %v", length, pos, b, ok, err)
				}
			}
		}
	}
}

// A FIFO is refused at once; the open must not wait for a writer.
func TestLoadPasswordFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on Windows")
	}
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadPassword(p); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrPasswordFile) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadPassword hangs on a FIFO")
	}
}

func FuzzLoadPassword(f *testing.F) {
	for _, s := range []string{"", "a", "a\n", "a b", "abc\n\n", "\x00", "p\xffw"} {
		f.Add([]byte(s))
	}
	p := filepath.Join(f.TempDir(), "pw")
	f.Fuzz(func(t *testing.T, content []byte) {
		writePw(t, p, content, 0o600)
		want, ok := refPassword(content)
		got, err := LoadPassword(p)
		if (err == nil) != ok || (ok && !bytes.Equal(got, want)) {
			t.Fatalf("predicate says %v, LoadPassword gave err %v", ok, err)
		}
	})
}
