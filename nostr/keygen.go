package nostr

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"syscall"
)

// GenerateSecretKey draws a fresh secret key from crypto/rand. It redraws until the 32 bytes are a valid
// scalar (1..n-1, the check ParseSecretKeyHex applies); a redraw is astronomically rare.
func GenerateSecretKey() (*SecretKey, error) {
	var raw [32]byte
	var h [64]byte
	defer clear(raw[:])
	defer clear(h[:])
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, fmt.Errorf("nostr: cannot read random bytes: %w", err)
		}
		hex.Encode(h[:], raw[:])
		if k, err := parseSecretKeyBytes(h[:]); err == nil {
			return k, nil
		}
	}
}

// WriteNewSecretKeyFile creates path with mode 0600 and writes the key as 64 lowercase hex digits and "\n".
// It never overwrites: an existing file, directory or symlink (even a dangling one) is an error and is left
// untouched. If a write fails after the create, the new file is removed. Errors never contain the key.
func WriteNewSecretKeyFile(path string, k *SecretKey) error {
	if k == nil || k.key == nil {
		return errNoKey
	}
	var raw [32]byte
	copy(raw[:], k.key.Serialize())
	var buf [65]byte
	defer clear(raw[:])
	defer clear(buf[:])
	hex.Encode(buf[:64], raw[:])
	buf[64] = '\n'

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("nostr: cannot create secret key file: %w", err)
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("nostr: cannot write secret key file: %w", err)
	}
	if _, err := f.Write(buf[:]); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("nostr: cannot write secret key file: %w", err)
	}
	return nil
}
