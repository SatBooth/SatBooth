// Package setup holds what the setup command shares between its stages: secret generation, the bitcoind rpcauth
// line, the hub's pairing links and the two handoff files an operator copies from one host to the other.
package setup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// NewSecret reads 32 bytes from r and returns them as 64 lowercase hex characters.
func NewSecret(r io.Reader) (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", errors.New("setup: cannot read 32 random bytes")
	}
	return hex.EncodeToString(b[:]), nil
}

// RPCAuthLine returns "rpcauth=<user>:<salt>$<hmac>": salt is 32 lowercase hex characters (16 bytes from r), hmac is
// HMAC-SHA256 with the salt's hex text as the key over the password, as Bitcoin Core's share/rpcauth/rpcauth.py
// computes it. user must match [A-Za-z0-9._-]{1,64}; password must be 1 to 128 bytes of 0x21..0x7e.
func RPCAuthLine(r io.Reader, user, password string) (string, error) {
	if !validName(user) {
		return "", errors.New("setup: the rpc user must be 1 to 64 characters of letters, digits, '.', '_' and '-'")
	}
	if len(password) < 1 || len(password) > 128 {
		return "", errors.New("setup: the rpc password must be 1 to 128 characters")
	}
	for i := 0; i < len(password); i++ {
		if password[i] < 0x21 || password[i] > 0x7e {
			return "", errors.New("setup: the rpc password may hold only printable characters other than space")
		}
	}
	var s [16]byte
	if _, err := io.ReadFull(r, s[:]); err != nil {
		return "", errors.New("setup: cannot read 16 random bytes")
	}
	salt := hex.EncodeToString(s[:])
	m := hmac.New(sha256.New, []byte(salt))
	m.Write([]byte(password))
	return "rpcauth=" + user + ":" + salt + "$" + hex.EncodeToString(m.Sum(nil)), nil
}

// validName reports whether s matches [A-Za-z0-9._-]{1,64}.
func validName(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// isHex reports whether s is exactly n lowercase hex characters.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
