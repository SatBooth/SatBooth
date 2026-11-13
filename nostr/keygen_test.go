package nostr

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

func TestGenerateSecretKey(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		k, err := GenerateSecretKey()
		if err != nil {
			t.Fatal("GenerateSecretKey failed")
		}
		pub := k.PublicKeyHex()
		if seen[pub] {
			t.Fatal("duplicate key generated")
		}
		seen[pub] = true
		if err := ValidatePubKey(pub); err != nil {
			t.Fatal("generated public key invalid")
		}
		k2, err := ParseSecretKeyHex(hex32(k))
		if err != nil || k2.PublicKeyHex() != pub {
			t.Fatal("generated key hex does not parse back")
		}
	}
}

// hex32 returns the key's hex for test comparisons only.
func hex32(k *SecretKey) string {
	const digits = "0123456789abcdef"
	b := k.key.Serialize()
	out := make([]byte, 0, 64)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func TestWriteNewSecretKeyFileRoundTrip(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	k, err := GenerateSecretKey()
	if err != nil {
		t.Fatal("generate failed")
	}
	p := filepath.Join(t.TempDir(), "key")
	if err := WriteNewSecretKeyFile(p, k); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	got, err := LoadSecretKeyFile(p)
	if err != nil || got.PublicKeyHex() != k.PublicKeyHex() {
		t.Fatal("round trip mismatch")
	}
	data, _ := os.ReadFile(p)
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(data) {
		t.Fatal("file content is not 64 lowercase hex plus newline")
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func dirNames(t *testing.T, d string) string {
	t.Helper()
	es, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	return strings.Join(n, ",")
}

func TestWriteNewSecretKeyFileNeverOverwrites(t *testing.T) {
	k, _ := GenerateSecretKey()
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep me\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o640); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "adir")
	os.Mkdir(subdir, 0o755)
	toMissing := filepath.Join(dir, "link-missing")
	os.Symlink(filepath.Join(dir, "no-such-target"), toMissing)
	toExisting := filepath.Join(dir, "link-existing")
	os.Symlink(existing, toExisting)

	before := dirNames(t, dir)
	for _, p := range []string{existing, subdir, toMissing, toExisting} {
		if err := WriteNewSecretKeyFile(p, k); err == nil {
			t.Fatalf("no error for %s", filepath.Base(p))
		}
	}
	if after := dirNames(t, dir); after != before {
		t.Fatalf("directory changed: %q -> %q", before, after)
	}
	data, _ := os.ReadFile(existing)
	fi, _ := os.Stat(existing)
	if string(data) != "keep me\n" || fi.Mode().Perm() != 0o640 {
		t.Fatal("existing file changed")
	}
	if _, err := os.Lstat(filepath.Join(dir, "no-such-target")); err == nil {
		t.Fatal("symlink target was created")
	}
}

func TestKeyErrorsHoldNoKey(t *testing.T) {
	k, _ := GenerateSecretKey()
	h := hex32(k)
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	for _, p := range []string{filepath.Join(dir, "missing", "key"), filepath.Join(ro, "key"), dir} {
		err := WriteNewSecretKeyFile(p, k)
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), h) {
			t.Fatal("write error holds the key")
		}
	}
	if err := WriteNewSecretKeyFile(filepath.Join(dir, "x"), nil); err == nil {
		t.Fatal("nil key accepted")
	}
}
