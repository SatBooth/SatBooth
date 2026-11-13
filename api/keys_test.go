package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func hashOf(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

func keyEntry(id, capName, key string) string {
	return fmt.Sprintf(`{"id":%q,"capability":%q,"sha256":%q}`, id, capName, hashOf(key))
}

func writeKeyFile(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func keysBody(entries ...string) string { return `{"keys":[` + strings.Join(entries, ",") + `]}` }

func TestLoadKeysAndCheck(t *testing.T) {
	p := writeKeyFile(t, 0o600, keysBody(keyEntry("shop-w", CapWrite, "write-key"), keyEntry("shop-r", CapRead, "read-key")))
	k, err := LoadKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		key, capName string
		want         bool
	}{
		{"write-key", CapWrite, true}, {"write-key", CapRead, false},
		{"read-key", CapRead, true}, {"read-key", CapWrite, false},
		{"nope", CapWrite, false}, {"", CapRead, false}, {"write-key", "orders:admin", false}, {"write-key", "", false},
	}
	for _, c := range cases {
		if got := k.Check(c.key, c.capName); got != c.want {
			t.Errorf("Check(%q, %q) = %v, want %v", c.key, c.capName, got, c.want)
		}
	}
}

func TestLoadKeysRefusals(t *testing.T) {
	good := keyEntry("a", CapWrite, "k1")
	h2 := keyEntry("b", CapRead, "k2")
	longID := strings.Repeat("x", 65)
	bad := map[string]string{
		"duplicate id":          keysBody(good, keyEntry("a", CapRead, "k2")),
		"duplicate hash":        keysBody(good, keyEntry("b", CapRead, "k1")),
		"unknown capability":    keysBody(keyEntry("a", "orders:admin", "k1")),
		"empty capability":      keysBody(keyEntry("a", "", "k1")),
		"uppercase hash":        keysBody(strings.Replace(good, hashOf("k1"), strings.ToUpper(hashOf("k1")), 1)),
		"short hash":            keysBody(strings.Replace(good, hashOf("k1"), hashOf("k1")[:63], 1)),
		"long hash":             keysBody(strings.Replace(good, hashOf("k1"), hashOf("k1")+"0", 1)),
		"non-hex hash":          keysBody(strings.Replace(good, hashOf("k1"), strings.Repeat("g", 64), 1)),
		"bad id chars":          keysBody(keyEntry("a b", CapWrite, "k1")),
		"empty id":              keysBody(keyEntry("", CapWrite, "k1")),
		"long id":               keysBody(keyEntry(longID, CapWrite, "k1")),
		"no keys":               `{"keys":[]}`,
		"keys not an array":     `{"keys":{}}`,
		"keys null":             `{"keys":null}`,
		"missing keys":          `{}`,
		"unknown top field":     `{"keys":[` + good + `],"extra":1}`,
		"unknown entry field":   `{"keys":[{"id":"a","capability":"orders:write","sha256":"` + hashOf("k1") + `","key":"k1"}]}`,
		"missing field":         `{"keys":[{"id":"a","capability":"orders:write"}]}`,
		"duplicate key":         `{"keys":[` + good + `],"keys":[` + h2 + `]}`,
		"duplicate entry key":   `{"keys":[{"id":"a","id":"b","capability":"orders:write","sha256":"` + hashOf("k1") + `"}]}`,
		"case variant field":    `{"keys":[{"ID":"a","capability":"orders:write","sha256":"` + hashOf("k1") + `"}]}`,
		"trailing document":     keysBody(good) + `{}`,
		"trailing garbage":      keysBody(good) + `x`,
		"not json":              `nope`,
		"empty file":            ``,
		"array top":             `[]`,
		"invalid utf-8":         keysBody(good) + "\xff",
		"id not a string":       `{"keys":[{"id":1,"capability":"orders:write","sha256":"` + hashOf("k1") + `"}]}`,
		"capability null":       `{"keys":[{"id":"a","capability":null,"sha256":"` + hashOf("k1") + `"}]}`,
		"too many keys":         tooMany(),
		"file too large":        keysBody(good) + strings.Repeat(" ", maxKeyFile),
		"element not an object": `{"keys":[1]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := LoadKeys(writeKeyFile(t, 0o600, body))
			if err == nil {
				t.Fatal("accepted")
			}
			if err != ErrKeyFile {
				t.Errorf("error %v is not the fixed ErrKeyFile", err)
			}
			if strings.Contains(err.Error(), hashOf("k1")) {
				t.Error("error contains a hash")
			}
		})
	}
}

func tooMany() string {
	var es []string
	for i := 0; i <= maxKeys; i++ {
		es = append(es, keyEntry(fmt.Sprintf("k%d", i), CapRead, fmt.Sprintf("key-%d", i)))
	}
	return keysBody(es...)
}

func TestLoadKeysFileModeAndKind(t *testing.T) {
	body := keysBody(keyEntry("a", CapWrite, "k1"))
	for _, m := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o666, 0o700, 0o400, 0o200} {
		if _, err := LoadKeys(writeKeyFile(t, m, body)); err == nil {
			t.Errorf("mode %o accepted", m)
		}
	}
	for _, m := range []os.FileMode{0o600} {
		if _, err := LoadKeys(writeKeyFile(t, m, body)); err != nil {
			t.Errorf("mode %o refused: %v", m, err)
		}
	}
	good := writeKeyFile(t, 0o600, body)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeys(link); err == nil {
		t.Error("symbolic link accepted")
	}
	if _, err := LoadKeys(t.TempDir()); err == nil {
		t.Error("directory accepted")
	}
	if _, err := LoadKeys(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
}

// The number of comparisons per Check is the number of stored keys, whether the key matches, matches
// with the wrong capability, or does not exist, and for a match at the first or the last place.
func TestCheckComparesAgainstEveryKey(t *testing.T) {
	const n = 7
	var es []string
	for i := 0; i < n; i++ {
		capName := CapRead
		if i%2 == 0 {
			capName = CapWrite
		}
		es = append(es, keyEntry(fmt.Sprintf("k%d", i), capName, fmt.Sprintf("key-%d", i)))
	}
	k, err := LoadKeys(writeKeyFile(t, 0o600, keysBody(es...)))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	k.cmp = func(a, b []byte) int { calls.Add(1); return subtle.ConstantTimeCompare(a, b) }
	for _, c := range []struct {
		key, capName string
		want         bool
	}{
		{"key-0", CapWrite, true}, {"key-6", CapWrite, true}, {"key-3", CapRead, true},
		{"key-0", CapRead, false}, {"missing", CapWrite, false}, {"", CapWrite, false}, {"key-3", "bogus", false},
	} {
		calls.Store(0)
		if got := k.Check(c.key, c.capName); got != c.want {
			t.Errorf("Check(%q,%q) = %v", c.key, c.capName, got)
		}
		if got := calls.Load(); got != n {
			t.Errorf("Check(%q,%q) made %d comparisons, want %d", c.key, c.capName, got, n)
		}
	}
}

func TestReloadSwapsAndKeepsOldOnFailure(t *testing.T) {
	p := writeKeyFile(t, 0o600, keysBody(keyEntry("a", CapWrite, "old")))
	k, err := LoadKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(mode os.FileMode, body string) {
		t.Helper()
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(0o600, keysBody(keyEntry("a", CapWrite, "new")))
	if err := k.Reload(); err != nil {
		t.Fatal(err)
	}
	if k.Check("old", CapWrite) || !k.Check("new", CapWrite) {
		t.Fatal("reload did not swap")
	}
	rewrite(0o600, `{"keys":[`) // broken
	if err := k.Reload(); err == nil {
		t.Fatal("broken file reloaded")
	}
	rewrite(0o644, keysBody(keyEntry("a", CapWrite, "newer")))
	if err := k.Reload(); err == nil {
		t.Fatal("0644 file reloaded")
	}
	if !k.Check("new", CapWrite) || k.Check("newer", CapWrite) {
		t.Fatal("a failed reload changed the keys")
	}
}

// Readers during reloads see either whole set, never a mix, and never fail. Run with -race.
func TestReloadWhileChecking(t *testing.T) {
	setA := keysBody(keyEntry("a", CapWrite, "key-a"), keyEntry("b", CapRead, "key-b"))
	setB := keysBody(keyEntry("c", CapWrite, "key-c"), keyEntry("d", CapRead, "key-d"))
	p := writeKeyFile(t, 0o600, setA)
	k, err := LoadKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := writeKeyFile(t, 0o600, setA), writeKeyFile(t, 0o600, setB)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a1, b1 := k.Check("key-a", CapWrite), k.Check("key-b", CapRead)
				c1, d1 := k.Check("key-c", CapWrite), k.Check("key-d", CapRead)
				_ = a1 || b1 || c1 || d1 // each read is of one set; a torn set cannot be seen from one call
			}
		}()
	}
	for i := 0; i < 200; i++ {
		src := pa
		if i%2 == 1 {
			src = pb
		}
		b, _ := os.ReadFile(src)
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, p); err != nil {
			t.Fatal(err)
		}
		if err := k.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if !k.Check("key-d", CapRead) || k.Check("key-a", CapWrite) {
		t.Fatal("final set is not set B")
	}
}
