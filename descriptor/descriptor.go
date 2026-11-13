// Package descriptor is the intake grammar for a merchant's watch-only descriptor. It accepts
// exactly wpkh(KEY/0/*) and tr(KEY/0/*), one extended public key with an optional key origin, and
// refuses everything else, above all private key material, before any RPC call is made.
// It is an allowlist scanner written by hand, not a regexp and not a blocklist.
package descriptor

import "errors"

// Fixed errors. None of them carries any part of the input.
var (
	ErrPrivateKey = errors.New("descriptor: contains private key material")
	ErrSyntax     = errors.New("descriptor: not an accepted watch-only descriptor")
	ErrTooLong    = errors.New("descriptor: too long")
)

const (
	maxScan    = 1024 // the private-material scan reads at most this many bytes
	maxLen     = 256  // longest accepted descriptor
	keyLen     = 111  // base58 characters of an extended public key
	maxOrigin  = 10   // steps in a key origin
	maxIndex   = 2147483647
	checkAlpha = "qpzry9x8gf2tvdw0s3jn54khce6mua7l" // bech32 charset, as in descriptor checksums
)

// Descriptor is a parsed, accepted descriptor.
type Descriptor struct {
	Func      string // "wpkh" or "tr"
	Origin    string // "" or "[fingerprint/path]" with h markers
	Key       string // the extended public key as given
	Testnet   bool   // tpub
	Canonical string // Func(Origin Key/0/*), no checksum: the string handed to getdescriptorinfo
}

// AddressType is the bitcoind address type the descriptor yields.
func (d Descriptor) AddressType() string {
	if d.Func == "tr" {
		return "bech32m"
	}
	return "bech32"
}

var privPrefixes = []string{"xprv", "tprv", "yprv", "zprv", "uprv", "vprv", "Yprv", "Zprv", "Uprv", "Vprv"}

// hasPrivateMaterial scans the raw bytes (at most the first 1024) for extended private keys by
// their prefixes and for any maximal base58 run that decodes as an extended private key or a WIF.
func hasPrivateMaterial(s string) bool {
	if len(s) > maxScan {
		s = s[:maxScan]
	}
	for _, p := range privPrefixes {
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				return true
			}
		}
	}
	for i := 0; i < len(s); {
		if !isBase58(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isBase58(s[j]) {
			j++
		}
		// An extended key is 111 characters and a WIF 51 or 52; a longer run cannot decode to
		// 78 bytes with a nonzero first byte.
		if n := j - i; n >= 50 && n <= 120 {
			if b, ok := decodeCheck(s[i:j]); ok && isPrivate(b) {
				return true
			}
		}
		i = j
	}
	return false
}

func isPrivate(b []byte) bool {
	switch len(b) {
	case 78:
		// Byte 45 is 0x00 for a private key (BIP32 pads it to 33 bytes), whatever the version:
		// an xpub-versioned payload carrying 0x00 || key is a private key too.
		if b[45] == 0x00 {
			return true
		}
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		return v == 0x0488ADE4 || v == 0x04358394
	case 33, 34:
		return b[0] == 0x80 || b[0] == 0xEF
	}
	return false
}

// Parse accepts only the grammar
//
//	desc   := func "(" [origin] key "/0/*" ")" ["#" checksum]
//	func   := "wpkh" | "tr"
//	origin := "[" 8 lowercase hex ("/" index ("h" | "'")?){0,10} "]"
//
// The optional "#checksum" (8 characters of the bech32 charset) is dropped unverified: the key has
// its own Base58Check, the origin path does not change addresses, and bitcoind verifies the
// checksum of the canonical form it is given.
//
// and returns a fixed error otherwise. Private material is checked first, over the raw bytes.
func Parse(s string) (Descriptor, error) {
	if hasPrivateMaterial(s) {
		return Descriptor{}, ErrPrivateKey
	}
	if len(s) > maxScan {
		return Descriptor{}, ErrTooLong
	}
	if len(s) == 0 || len(s) > maxLen {
		return Descriptor{}, ErrSyntax
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return Descriptor{}, ErrSyntax
		}
	}
	var d Descriptor
	p := 0
	switch {
	case hasAt(s, p, "wpkh("):
		d.Func, p = "wpkh", 5
	case hasAt(s, p, "tr("):
		d.Func, p = "tr", 3
	default:
		return Descriptor{}, ErrSyntax
	}
	if p < len(s) && s[p] == '[' {
		o, np, ok := scanOrigin(s, p)
		if !ok {
			return Descriptor{}, ErrSyntax
		}
		d.Origin, p = o, np
	}
	if p+keyLen > len(s) {
		return Descriptor{}, ErrSyntax
	}
	key := s[p : p+keyLen]
	testnet, ok := checkKey(key)
	if !ok {
		return Descriptor{}, ErrSyntax
	}
	d.Key, d.Testnet = key, testnet
	p += keyLen
	if !hasAt(s, p, "/0/*)") {
		return Descriptor{}, ErrSyntax
	}
	p += 5
	if p < len(s) {
		if s[p] != '#' || len(s)-p-1 != 8 {
			return Descriptor{}, ErrSyntax
		}
		for _, c := range []byte(s[p+1:]) {
			if !inSet(c, checkAlpha) {
				return Descriptor{}, ErrSyntax
			}
		}
	}
	d.Canonical = d.Func + "(" + d.Origin + d.Key + "/0/*)"
	return d, nil
}

func hasAt(s string, p int, lit string) bool {
	return len(s)-p >= len(lit) && s[p:p+len(lit)] == lit
}

func inSet(c byte, set string) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == c {
			return true
		}
	}
	return false
}

func isHexLower(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

// scanOrigin reads "[8hex(/index(h|')?){0,10}]" at s[p] and returns it normalised to h markers.
func scanOrigin(s string, p int) (string, int, bool) {
	out := make([]byte, 0, 64)
	out = append(out, '[')
	p++
	if p+8 > len(s) {
		return "", 0, false
	}
	for i := 0; i < 8; i++ {
		if !isHexLower(s[p+i]) {
			return "", 0, false
		}
	}
	out = append(out, s[p:p+8]...)
	p += 8
	steps := 0
	for p < len(s) && s[p] == '/' {
		if steps == maxOrigin {
			return "", 0, false
		}
		steps++
		p++
		start := p
		for p < len(s) && s[p] >= '0' && s[p] <= '9' {
			p++
		}
		digits := s[start:p]
		if len(digits) == 0 || len(digits) > 10 || (len(digits) > 1 && digits[0] == '0') {
			return "", 0, false
		}
		var v int64
		for i := 0; i < len(digits); i++ {
			v = v*10 + int64(digits[i]-'0')
		}
		if v > maxIndex {
			return "", 0, false
		}
		out = append(out, '/')
		out = append(out, digits...)
		if p < len(s) && (s[p] == 'h' || s[p] == '\'') {
			out = append(out, 'h')
			p++
		}
	}
	if p >= len(s) || s[p] != ']' {
		return "", 0, false
	}
	out = append(out, ']')
	return string(out), p + 1, true
}

// checkKey validates an extended public key (BIP32 serialisation: version 4, depth 1, parent
// fingerprint 4, child number 4, chain code 32, key 33) and reports whether it is a testnet key.
func checkKey(k string) (testnet, ok bool) {
	if len(k) != keyLen {
		return false, false
	}
	b, ok := decodeCheck(k)
	if !ok || len(b) != 78 {
		return false, false
	}
	switch uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) {
	case 0x0488B21E:
	case 0x043587CF:
		testnet = true
	default:
		return false, false
	}
	if b[45] != 0x02 && b[45] != 0x03 {
		return false, false
	}
	if b[4] == 0 { // a master key has no parent and child number 0 (BIP32)
		for _, c := range b[5:13] {
			if c != 0 {
				return false, false
			}
		}
	}
	return testnet, true
}
