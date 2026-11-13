// Package ledger is SatBooth's double-entry ledger, modelled on TigerBeetle.
package ledger

import (
	"errors"
	"math/bits"
	"strconv"
)

// U128 is an unsigned 128-bit integer. Hi holds the high 64 bits.
type U128 struct{ Hi, Lo uint64 }

var (
	U128Zero U128
	U128Max  = U128{^uint64(0), ^uint64(0)}
)

// U128From64 widens v.
func U128From64(v uint64) U128 { return U128{0, v} }

// Bytes returns the big-endian encoding. Byte order equals numeric order.
func (a U128) Bytes() [16]byte {
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(a.Hi >> (56 - 8*uint(i)))
		b[8+i] = byte(a.Lo >> (56 - 8*uint(i)))
	}
	return b
}

// U128FromBytes decodes exactly 16 big-endian bytes.
func U128FromBytes(b []byte) (U128, error) {
	if len(b) != 16 {
		return U128{}, errors.New("ledger: u128 needs exactly 16 bytes")
	}
	var v U128
	for i := 0; i < 8; i++ {
		v.Hi = v.Hi<<8 | uint64(b[i])
		v.Lo = v.Lo<<8 | uint64(b[8+i])
	}
	return v, nil
}

// Add returns a+b mod 2^128 and false if the sum overflowed.
func (a U128) Add(b U128) (U128, bool) {
	lo, c := bits.Add64(a.Lo, b.Lo, 0)
	hi, c := bits.Add64(a.Hi, b.Hi, c)
	return U128{hi, lo}, c == 0
}

// Sub returns a-b mod 2^128 and false if it underflowed (b > a).
func (a U128) Sub(b U128) (U128, bool) {
	lo, br := bits.Sub64(a.Lo, b.Lo, 0)
	hi, br := bits.Sub64(a.Hi, b.Hi, br)
	return U128{hi, lo}, br == 0
}

// Cmp returns -1, 0 or +1.
func (a U128) Cmp(b U128) int {
	switch {
	case a.Hi < b.Hi:
		return -1
	case a.Hi > b.Hi:
		return 1
	case a.Lo < b.Lo:
		return -1
	case a.Lo > b.Lo:
		return 1
	}
	return 0
}

func (a U128) IsZero() bool { return a == U128Zero }
func (a U128) IsMax() bool  { return a == U128Max }

const (
	pow10x19  = 10_000_000_000_000_000_000 // 10^19 fits in uint64
	maxDigits = 39
)

// String returns the decimal form.
func (a U128) String() string {
	if a.Hi == 0 {
		return strconv.FormatUint(a.Lo, 10)
	}
	// a = Q*10^19 + c0, where Q = q1:q0 is a 128-bit quotient with q1 <= 1.
	q1, r1 := bits.Div64(0, a.Hi, pow10x19)
	q0, c0 := bits.Div64(r1, a.Lo, pow10x19)
	// Q = top*10^19 + c1. q1 <= 1 < 10^19, so Div64 cannot overflow.
	top, c1 := bits.Div64(q1, q0, pow10x19)
	if top == 0 {
		return strconv.FormatUint(c1, 10) + pad19(c0)
	}
	return strconv.FormatUint(top, 10) + pad19(c1) + pad19(c0)
}

func pad19(v uint64) string {
	s := strconv.FormatUint(v, 10)
	return "0000000000000000000"[len(s):] + s
}

// ParseU128 parses canonical decimal: digits only, no sign, no leading zeros
// (except "0" itself), at most 39 digits, value at most 2^128-1.
func ParseU128(s string) (U128, error) {
	if len(s) == 0 || len(s) > maxDigits {
		return U128{}, errors.New("ledger: u128 must be 1 to 39 decimal digits")
	}
	if len(s) > 1 && s[0] == '0' {
		return U128{}, errors.New("ledger: u128 has a leading zero")
	}
	var v U128
	for i := 0; i < len(s); i++ {
		d := s[i]
		if d < '0' || d > '9' {
			return U128{}, errors.New("ledger: u128 has a non-digit character")
		}
		// v = v*10 + d, checking every carry.
		h1, l1 := bits.Mul64(v.Lo, 10)
		h2, l2 := bits.Mul64(v.Hi, 10)
		if h2 != 0 {
			return U128{}, errors.New("ledger: u128 overflows 128 bits")
		}
		hi, c := bits.Add64(l2, h1, 0)
		if c != 0 {
			return U128{}, errors.New("ledger: u128 overflows 128 bits")
		}
		lo, c := bits.Add64(l1, uint64(d-'0'), 0)
		hi, c = bits.Add64(hi, 0, c)
		if c != 0 {
			return U128{}, errors.New("ledger: u128 overflows 128 bits")
		}
		v = U128{hi, lo}
	}
	return v, nil
}
