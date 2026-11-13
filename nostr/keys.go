package nostr

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

const secretKeyRedacted = "nostr.SecretKey{…}"

// Errors never carry key material, the input or the file path's content.
var (
	errKeyFormat  = errors.New("nostr: secret key must be 64 lowercase hex digits and in range 1..n-1")
	errKeyFile    = errors.New("nostr: secret key file must be a regular file with mode 0600 (no group or other access)")
	errKeyFileFmt = errors.New("nostr: secret key file must hold exactly 64 lowercase hex digits and an optional final newline")
	errKeyFileIO  = errors.New("nostr: cannot read secret key file")
	errNoKey      = errors.New("nostr: no secret key")
)

// ErrBadPubKey means a public key is not 64 lowercase hex digits or is not a point on the curve.
var ErrBadPubKey = errors.New("nostr: invalid public key")

// SecretKey is a secp256k1 secret key. It prints as "nostr.SecretKey{…}" under every fmt verb, and the
// only way to get one is ParseSecretKeyHex or LoadSecretKeyFile. The key lives behind a pointer so that
// fmt, which cannot call methods on unexported fields, prints an address and not the key bytes when a
// SecretKey is nested inside another value.
type SecretKey struct {
	key *btcec.PrivateKey
}

// String returns "nostr.SecretKey{…}".
func (SecretKey) String() string { return secretKeyRedacted }

// GoString returns "nostr.SecretKey{…}".
func (SecretKey) GoString() string { return secretKeyRedacted }

// Format prints "nostr.SecretKey{…}" for every verb and flag.
func (SecretKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, secretKeyRedacted) }

// Zero overwrites the key bytes and drops the key, for shutdown. Afterwards Sign, SharedX and
// WriteNewSecretKeyFile return an error and PublicKeyHex returns "": no method ever computes from zeroed bytes.
// Zero is harmless on a nil or already zeroed key. It is not safe to call while another goroutine uses k.
// A copy of a SecretKey value shares the key pointer, so Zero clears the scalar for the copy too, but the
// copy's own pointer is not reset and its methods are not guarded: do not copy SecretKey values.
func (k *SecretKey) Zero() {
	if k == nil || k.key == nil {
		return
	}
	k.key.Zero()
	k.key = nil
}

// ParseSecretKeyHex parses exactly 64 lowercase hex digits, a value in 1..n-1. It refuses everything
// else (bech32 "nsec", WIF, extended keys, mixed case, whitespace) with an error that does not echo s.
// The copy of s made here is cleared before returning (best effort; the caller's string is not ours).
func ParseSecretKeyHex(s string) (*SecretKey, error) {
	b := []byte(s)
	defer clear(b)
	return parseSecretKeyBytes(b)
}

// parseSecretKeyBytes is ParseSecretKeyHex on bytes. It does not retain or modify b.
func parseSecretKeyBytes(h []byte) (*SecretKey, error) {
	if len(h) != 64 {
		return nil, errKeyFormat
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, errKeyFormat
		}
	}
	var b [32]byte
	defer clear(b[:])
	if _, err := hex.Decode(b[:], h); err != nil {
		return nil, errKeyFormat
	}
	var d secp256k1.ModNScalar
	overflow := d.SetByteSlice(b[:])
	zero := d.IsZero()
	d.Zero()
	if overflow || zero {
		return nil, errKeyFormat
	}
	return &SecretKey{key: secp256k1.PrivKeyFromBytes(b[:])}, nil
}

// LoadSecretKeyFile reads a secret key from a regular file (not a symlink) that grants nothing to group
// or others (mode&0o077 == 0). The content is 64 lowercase hex digits with at most one final "\n".
// Symlinked key files are refused by design; some secret mounts use symlinks, so point at the real file.
// I/O errors wrap the underlying *os.PathError (not found, permission denied, is a directory); no error
// ever contains file content.
func LoadSecretKeyFile(path string) (*SecretKey, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKeyFileIO, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errKeyFile
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // O_NONBLOCK: a FIFO swapped in after Lstat must not hang the open
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKeyFileIO, err)
	}
	defer f.Close()
	// Check the opened file, not only the path: it must be the file we inspected.
	fi2, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKeyFileIO, err)
	}
	if !fi2.Mode().IsRegular() || fi2.Mode().Perm()&0o077 != 0 || !os.SameFile(fi, fi2) {
		return nil, errKeyFile
	}
	buf := make([]byte, 66+1) // 64 digits, "\n", and one byte to detect anything longer
	defer clear(buf)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("%w: %w", errKeyFileIO, err)
	}
	data := buf[:n]
	if len(data) == 65 && data[64] == '\n' {
		data = data[:64]
	}
	if len(data) != 64 {
		return nil, errKeyFileFmt
	}
	k, err := parseSecretKeyBytes(data)
	if err != nil {
		return nil, errKeyFileFmt
	}
	return k, nil
}

// PublicKeyHex returns the x-only public key as 64 lowercase hex digits ("" for a zero SecretKey).
func (k *SecretKey) PublicKeyHex() string {
	if k == nil || k.key == nil {
		return ""
	}
	return hex.EncodeToString(schnorr.SerializePubKey(k.key.PubKey()))
}

// parsePub parses a 64-digit lowercase hex x-only public key; the point must be on the curve.
func parsePub(pubHex string) (*btcec.PublicKey, error) {
	if !lowerHex(pubHex, 64) {
		return nil, ErrBadPubKey
	}
	b, err := hex.DecodeString(pubHex)
	if err != nil {
		return nil, ErrBadPubKey
	}
	p, err := schnorr.ParsePubKey(b)
	if err != nil {
		return nil, ErrBadPubKey
	}
	return p, nil
}

// ValidatePubKey reports whether pubHex is a usable x-only public key: 64 lowercase hex digits whose x
// coordinate is below the field prime and lies on the curve. The error wraps ErrBadPubKey.
func ValidatePubKey(pubHex string) error {
	if _, err := parsePub(pubHex); err != nil {
		return fmt.Errorf("validate public key: %w", err)
	}
	return nil
}

// SharedX returns the x-coordinate of k*P, unhashed, where P is the point with x-coordinate pubHex and
// even y. The y parity of P does not change the result. NIP-44 feeds this to HKDF-extract.
//
// It uses secp256k1.GenerateSharedSecret, whose source (dcrec/secp256k1/v4 ecdh.go) multiplies and
// returns only the 32-byte affine X, without hashing. That multiplication is not constant-time.
func (k *SecretKey) SharedX(pubHex string) ([32]byte, error) {
	var out [32]byte
	if k == nil || k.key == nil {
		return out, errNoKey
	}
	p, err := parsePub(pubHex)
	if err != nil {
		return out, err
	}
	x := secp256k1.GenerateSharedSecret(k.key, p)
	if len(x) != 32 {
		return out, ErrBadPubKey
	}
	copy(out[:], x)
	clear(x)
	return out, nil
}
