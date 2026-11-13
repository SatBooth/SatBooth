package btcrpc

import (
	"errors"
	"strconv"
	"strings"
)

// MaxSat is 21 million bitcoin in satoshi. It is this package's own copy of store.MaxSat (importing
// store would pull the SQLite driver into the client's capabilities); a test asserts the two are equal.
const MaxSat int64 = 2_100_000_000_000_000

const (
	satPerBTC  = 100_000_000
	maxBTCText = 25
)

var errAmount = errors.New("btcrpc: not a bitcoin amount")

// ParseBTC parses a bitcoin amount exactly, never through float64, and returns satoshi. The grammar is
// BIP321's amount bounded to 8 decimals: ( 1*digit [ "." 1*8digit ] ) | ( "." 1*8digit ). It refuses the
// empty string, ".", a trailing dot, a sign, an exponent, spaces, more than 25 bytes, and a value over MaxSat.
func ParseBTC(s string) (int64, error) {
	if len(s) == 0 || len(s) > maxBTCText {
		return 0, errAmount
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if hasDot {
		if len(frac) < 1 || len(frac) > 8 {
			return 0, errAmount
		}
	} else if intPart == "" {
		return 0, errAmount
	}
	if !allDigits(intPart) || !allDigits(frac) {
		return 0, errAmount
	}
	intPart = strings.TrimLeft(intPart, "0")
	if len(intPart) > 8 { // 21,000,000 is 8 digits
		return 0, errAmount
	}
	var whole int64
	if intPart != "" {
		w, err := strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return 0, errAmount
		}
		whole = w
	}
	var f int64
	if frac != "" {
		v, err := strconv.ParseInt(frac+strings.Repeat("0", 8-len(frac)), 10, 64)
		if err != nil {
			return 0, errAmount
		}
		f = v
	}
	sat := whole*satPerBTC + f
	if sat > MaxSat {
		return 0, errAmount
	}
	return sat, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseSignedBTC is ParseBTC with one optional leading '-', for the signed amounts of listsinceblock
// (a send is negative). Nothing else about the grammar changes.
func ParseSignedBTC(s string) (int64, error) {
	if rest, neg := strings.CutPrefix(s, "-"); neg {
		v, err := ParseBTC(rest)
		return -v, err
	}
	return ParseBTC(s)
}

// FormatBTC writes sat as bitcoin: the integer part in decimal, then, if there is a fraction, a dot and
// the 8-digit fraction with trailing zeros removed. It panics on a negative amount or one over MaxSat (a
// caller bug: callers validate).
func FormatBTC(sat int64) string {
	if sat < 0 || sat > MaxSat {
		panic("btcrpc: FormatBTC amount out of range")
	}
	whole, frac := sat/satPerBTC, sat%satPerBTC
	s := strconv.FormatInt(whole, 10)
	if frac == 0 {
		return s
	}
	f := strconv.FormatInt(frac, 10)
	f = strings.Repeat("0", 8-len(f)) + f
	return s + "." + strings.TrimRight(f, "0")
}
