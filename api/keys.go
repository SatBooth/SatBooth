package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
)

// The capabilities a key can hold. A key holds exactly one.
const (
	CapWrite = "orders:write"
	CapRead  = "orders:read"
	// CapRefundsWrite claims and voids refunds; CapRefundsRead reads one. Neither reaches an order route, and no
	// orders key reaches a refund route.
	CapRefundsWrite = "refunds:write"
	CapRefundsRead  = "refunds:read"
)

const (
	bitWrite uint8 = 1 << iota
	bitRead
	bitRefundsWrite
	bitRefundsRead
)

const (
	maxKeyFile = 64 << 10
	maxKeys    = 256
)

func capBit(c string) uint8 {
	switch c {
	case CapWrite:
		return bitWrite
	case CapRead:
		return bitRead
	case CapRefundsWrite:
		return bitRefundsWrite
	case CapRefundsRead:
		return bitRefundsRead
	}
	return 0
}

// ErrKeyFile is every refusal of a key file. The text never includes the file's content, so a
// hash, an id or a parse error cannot reach a log through it.
var ErrKeyFile = errors.New("api: key file refused (it must be a regular file with mode 0600, " +
	`holding {"keys":[{"id","capability","sha256"}]}: one to 256 keys, ids of 1 to 64 characters of [A-Za-z0-9._-] ` +
	`and unique, capability "orders:write", "orders:read", "refunds:write" or "refunds:read", sha256 of 64 distinct lowercase hex digits; ` +
	"strict JSON with no unknown or duplicate keys)")

type entry struct {
	hash [sha256.Size]byte
	bits uint8
}

// Keys is the set of API keys. Only the SHA-256 of each key is held. It is safe for concurrent use:
// Reload swaps the whole set atomically, so a request sees the old set or the new one, never a mix.
type Keys struct {
	path string
	set  atomic.Pointer[[]entry]
	// cmp is the comparison Check makes against every stored hash (a seam so a test can count the
	// calls). It is set before first use and never changed.
	cmp func(a, b []byte) int
}

// LoadKeys reads the key file at path: a regular file (symbolic links are refused) with no group or
// other permission bits, holding
//
//	{"keys":[{"id":"<name>","capability":"orders:write"|"orders:read"|"refunds:write"|"refunds:read","sha256":"<64 lowercase hex>"}]}
//
// The key itself is never stored; sha256 is the SHA-256 of the key's bytes. Ids and hashes must be
// unique. Reload re-reads the same path.
func LoadKeys(path string) (*Keys, error) {
	set, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	k := &Keys{path: path, cmp: subtle.ConstantTimeCompare}
	k.set.Store(&set)
	return k, nil
}

// Reload re-reads the key file and swaps the keys atomically. If the file is refused the old keys
// stay in force and the error says so.
func (k *Keys) Reload() error {
	set, err := readKeyFile(k.path)
	if err != nil {
		return err
	}
	k.set.Store(&set)
	return nil
}

func readKeyFile(path string) ([]entry, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("api: opening the key file: " + errString(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.New("api: reading the key file")
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return nil, ErrKeyFile
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, errors.New("api: reading the key file")
	}
	if len(raw) > maxKeyFile {
		return nil, ErrKeyFile
	}
	return parseKeys(raw)
}

// errString is the operating system's reason for a failed open (it names the path, never content).
func errString(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "error"
}

func parseKeys(raw []byte) ([]entry, error) {
	top, ok := strictObject(raw, []string{"keys"}, nil)
	if !ok {
		return nil, ErrKeyFile
	}
	// strictObject is for objects; the list is an array of them. Its elements were already checked
	// for duplicate keys by rates.CheckJSON over the whole document.
	list, ok := rawArray(top["keys"])
	if !ok || len(list) < 1 || len(list) > maxKeys {
		return nil, ErrKeyFile
	}
	ids := map[string]bool{}
	hashes := map[[sha256.Size]byte]bool{}
	out := make([]entry, 0, len(list))
	for _, el := range list {
		m, ok := strictObject(el, []string{"id", "capability", "sha256"}, nil)
		if !ok {
			return nil, ErrKeyFile
		}
		id, ok1 := rawString(m["id"])
		capName, ok2 := rawString(m["capability"])
		hx, ok3 := rawString(m["sha256"])
		if !ok1 || !ok2 || !ok3 || !validName(id) || capBit(capName) == 0 || !lowerHex(hx, 64) {
			return nil, ErrKeyFile
		}
		var e entry
		if _, err := hex.Decode(e.hash[:], []byte(hx)); err != nil {
			return nil, ErrKeyFile
		}
		e.bits = capBit(capName)
		if ids[id] || hashes[e.hash] {
			return nil, ErrKeyFile
		}
		ids[id], hashes[e.hash] = true, true
		out = append(out, e)
	}
	return out, nil
}

func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// identify returns the capability bits of the key whose SHA-256 is that of presented, or 0. It
// compares against every stored hash, whatever the result, and combines the results without a
// branch on them, so its running time does not depend on which key (if any) matched.
func (k *Keys) identify(presented string) uint8 {
	sum := sha256.Sum256([]byte(presented))
	var bits uint8
	for _, e := range *k.set.Load() {
		bits |= e.bits * uint8(k.cmp(sum[:], e.hash[:]))
	}
	return bits
}

// Check reports whether presented is a key that holds the capability. It makes one constant-time
// comparison per stored key and never stops early.
func (k *Keys) Check(presented string, capability string) bool {
	bits := k.identify(presented)
	want := capBit(capability)
	return want != 0 && bits&want != 0
}
