// Package approval parses and signs the two Ed25519 approvals that authorise a Lightning refund: the refund
// approval (148 bytes, 296 lowercase hex characters) and the void approval (120 bytes, 240 characters). The
// layouts are exact (plan 9); anything else is refused. Verification checks, in this order: the
// exact length and lowercase hex, the domain, the network, the key id, the signature, and only then the
// signed fields (expiry window, amount, nonzero ids). Error values carry no input.
package approval

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

// Network is the Bitcoin network a Lightning invoice and an approval belong to.
type Network uint8

// The networks, with the byte each has in an approval.
const (
	Mainnet Network = 0
	Testnet Network = 1
	Signet  Network = 2
	Regtest Network = 3
)

// ParseNetwork maps the configuration name of a network to its Network.
func ParseNetwork(s string) (Network, error) {
	switch s {
	case "mainnet":
		return Mainnet, nil
	case "testnet":
		return Testnet, nil
	case "signet":
		return Signet, nil
	case "regtest":
		return Regtest, nil
	}
	return 0, ErrNetwork
}

// MaxLifetime is the longest an approval may be valid for, counted from the moment it is checked.
const MaxLifetime = 30 * 24 * time.Hour

// MaxAmountMsat is the largest refund amount, 2^53 msat. Amounts are whole sats: a multiple of 1000.
const MaxAmountMsat = 1 << 53

// The errors. Each is fixed text: none carries any part of the input.
var (
	ErrMalformed  = errors.New("approval: malformed")
	ErrNetwork    = errors.New("approval: wrong network")
	ErrUnknownKey = errors.New("approval: unknown key")
	ErrSignature  = errors.New("approval: bad signature")
	ErrExpired    = errors.New("approval: expired")
	ErrLifetime   = errors.New("approval: valid for too long")
	ErrAmount     = errors.New("approval: amount out of range")
)

const (
	refundDomain = "satbooth/refund-approval/v1" // 27 bytes
	voidDomain   = "satbooth/refund-void/v1"     // 23 bytes
	sigLen       = ed25519.SignatureSize         // 64
	refundBody   = 84                            // signed bytes of a refund approval
	voidBody     = 56                            // signed bytes of a void approval
	refundLen    = refundBody + sigLen           // 148
	voidLen      = voidBody + sigLen             // 120
)

// Refund is a decoded refund approval: the admin system's permission to pay AmountMsat for OrderID.
type Refund struct {
	Network    Network
	KeyID      [8]byte
	RefundID   [16]byte
	OrderID    [16]byte
	AmountMsat uint64
	ExpiresAt  int64 // Unix seconds
}

// Void is a decoded void approval: the permission to end a refund the hub has no settled record of.
type Void struct {
	Network   Network
	KeyID     [8]byte
	RefundID  [16]byte
	ExpiresAt int64 // Unix seconds
}

// KeyID is the first 8 bytes of SHA-256 of the 32-byte public key.
func KeyID(pub ed25519.PublicKey) [8]byte {
	h := sha256.Sum256(pub)
	return [8]byte(h[:8])
}

// Keys is the set of configured approval public keys, by key id.
type Keys struct {
	byID map[[8]byte]ed25519.PublicKey
}

// NewKeys builds the key set from 1 to 8 distinct public keys, each exactly 64 lowercase hex characters.
// Two keys with the same key id are refused too, so a key id always names one key.
func NewKeys(hexKeys []string) (*Keys, error) {
	if len(hexKeys) < 1 || len(hexKeys) > 8 {
		return nil, ErrMalformed
	}
	k := &Keys{byID: make(map[[8]byte]ed25519.PublicKey, len(hexKeys))}
	for _, s := range hexKeys {
		if len(s) != 2*ed25519.PublicKeySize {
			return nil, ErrMalformed
		}
		b, ok := decodeHex(s)
		if !ok {
			return nil, ErrMalformed
		}
		if hasSmallOrder(b) {
			return nil, ErrMalformed
		}
		pub := ed25519.PublicKey(b)
		id := KeyID(pub)
		if _, dup := k.byID[id]; dup {
			return nil, ErrMalformed
		}
		k.byID[id] = pub
	}
	return k, nil
}

// Has reports whether a key with this id is configured. A nil *Keys has none.
func (k *Keys) Has(id [8]byte) bool {
	if k == nil {
		return false
	}
	_, ok := k.byID[id]
	return ok
}

// smallOrder lists the encodings of the points of small order on Ed25519, as libsodium's blacklist does:
// 0 (order 4), 1 (order 1), the two points of order 8, p-1 (order 2), and the non-canonical encodings p
// (= 0) and p+1 (= 1). Source: jedisct1/libsodium, tag 1.0.18-RELEASE (commit
// 940ef42797baa0278df6b7fd9e67c7590f87744b), src/libsodium/crypto_core/ed25519/ref10/ed25519_ref10.c,
// ge25519_has_small_order. Go's ed25519.Verify is cofactorless and has no such check: with a small-order
// public key, R = identity and S = 0 verify against any message.
var smallOrder = [7][32]byte{
	{}, // 0
	{0x01},
	{0x26, 0xe8, 0x95, 0x8f, 0xc2, 0xb2, 0x27, 0xb0, 0x45, 0xc3, 0xf4, 0x89, 0xf2, 0xef, 0x98, 0xf0,
		0xd5, 0xdf, 0xac, 0x05, 0xd3, 0xc6, 0x33, 0x39, 0xb1, 0x38, 0x02, 0x88, 0x6d, 0x53, 0xfc, 0x05},
	{0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f, 0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f,
		0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6, 0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a},
	{0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xee, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
}

// hasSmallOrder reports whether the 32-byte public key encoding is on the blacklist, with or without the
// sign bit: it compares the first 31 bytes and the last byte with its high bit masked, as libsodium does.
func hasSmallOrder(pub []byte) bool {
	for _, bad := range smallOrder {
		if string(pub[:31]) == string(bad[:31]) && pub[31]&0x7f == bad[31] {
			return true
		}
	}
	return false
}

// decodeHex decodes lowercase hex only: encoding/hex also takes uppercase.
func decodeHex(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i++ {
		var v byte
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		default:
			return nil, false
		}
		if i%2 == 0 {
			out[i/2] = v << 4
		} else {
			out[i/2] |= v
		}
	}
	return out, true
}

const hexDigits = "0123456789abcdef"

func encodeHex(b []byte) string {
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i] = hexDigits[c>>4]
		out[2*i+1] = hexDigits[c&15]
	}
	return string(out)
}

func marshalRefund(r Refund) []byte {
	b := make([]byte, refundBody, refundLen)
	copy(b, refundDomain)
	b[27] = byte(r.Network)
	copy(b[28:], r.KeyID[:])
	copy(b[36:], r.RefundID[:])
	copy(b[52:], r.OrderID[:])
	binary.BigEndian.PutUint64(b[68:], r.AmountMsat)
	binary.BigEndian.PutUint64(b[76:], uint64(r.ExpiresAt))
	return b
}

func marshalVoid(v Void) []byte {
	b := make([]byte, voidBody, voidLen)
	copy(b, voidDomain)
	b[23] = byte(v.Network)
	copy(b[24:], v.KeyID[:])
	copy(b[32:], v.RefundID[:])
	binary.BigEndian.PutUint64(b[48:], uint64(v.ExpiresAt))
	return b
}

// SignRefund returns the lowercase hex approval for r, signed by priv. It signs the fields it is given and
// checks none of them: the checks are Verify's.
func SignRefund(r Refund, priv ed25519.PrivateKey) string {
	b := marshalRefund(r)
	return encodeHex(append(b, ed25519.Sign(priv, b)...))
}

// SignVoid returns the lowercase hex void approval for v, signed by priv.
func SignVoid(v Void, priv ed25519.PrivateKey) string {
	b := marshalVoid(v)
	return encodeHex(append(b, ed25519.Sign(priv, b)...))
}

// open decodes an approval of the exact size, checks its domain and network, finds the key, and checks the
// signature. It returns the signed body. Only an approval that passes all of this has its fields read.
func (k *Keys) open(hexApproval string, size, body int, domain string, net Network) ([]byte, error) {
	if k == nil {
		return nil, ErrUnknownKey
	}
	if len(hexApproval) != 2*size {
		return nil, ErrMalformed
	}
	b, ok := decodeHex(hexApproval)
	if !ok {
		return nil, ErrMalformed
	}
	if string(b[:len(domain)]) != domain {
		return nil, ErrMalformed
	}
	if net > Regtest || Network(b[len(domain)]) != net {
		return nil, ErrNetwork
	}
	pub, ok := k.byID[[8]byte(b[len(domain)+1:len(domain)+9])]
	if !ok {
		return nil, ErrUnknownKey
	}
	if !ed25519.Verify(pub, b[:body], b[body:]) {
		return nil, ErrSignature
	}
	return b[:body], nil
}

// checkWindow requires now < expiresAt <= now + MaxLifetime, in whole seconds.
func checkWindow(expiresAt uint64, now time.Time) (int64, error) {
	if expiresAt > 1<<62 {
		return 0, ErrLifetime
	}
	exp := int64(expiresAt)
	if exp <= now.Unix() {
		return 0, ErrExpired
	}
	if exp > now.Add(MaxLifetime).Unix() {
		return 0, ErrLifetime
	}
	return exp, nil
}

// VerifyRefund decodes and verifies a refund approval for the configured network at time now.
func (k *Keys) VerifyRefund(hexApproval string, net Network, now time.Time) (Refund, error) {
	b, err := k.open(hexApproval, refundLen, refundBody, refundDomain, net)
	if err != nil {
		return Refund{}, err
	}
	var r Refund
	r.Network = Network(b[27])
	r.KeyID = [8]byte(b[28:36])
	r.RefundID = [16]byte(b[36:52])
	r.OrderID = [16]byte(b[52:68])
	r.AmountMsat = binary.BigEndian.Uint64(b[68:76])
	if r.ExpiresAt, err = checkWindow(binary.BigEndian.Uint64(b[76:84]), now); err != nil {
		return Refund{}, err
	}
	if r.AmountMsat == 0 || r.AmountMsat > MaxAmountMsat || r.AmountMsat%1000 != 0 {
		return Refund{}, ErrAmount
	}
	if r.RefundID == [16]byte{} || r.OrderID == [16]byte{} {
		return Refund{}, ErrMalformed
	}
	return r, nil
}

// VerifyVoid decodes and verifies a void approval. It returns the refund id the approval names; the caller
// compares it with the one in the request.
func (k *Keys) VerifyVoid(hexApproval string, net Network, now time.Time) (Void, error) {
	b, err := k.open(hexApproval, voidLen, voidBody, voidDomain, net)
	if err != nil {
		return Void{}, err
	}
	var v Void
	v.Network = Network(b[23])
	v.KeyID = [8]byte(b[24:32])
	v.RefundID = [16]byte(b[32:48])
	if v.ExpiresAt, err = checkWindow(binary.BigEndian.Uint64(b[48:56]), now); err != nil {
		return Void{}, err
	}
	if v.RefundID == [16]byte{} {
		return Void{}, ErrMalformed
	}
	return v, nil
}
