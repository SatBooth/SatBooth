package rates

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	// Scale is the number of decimal places a Decimal holds: its units are the value × 10^Scale.
	Scale = 18
	// MaxIntDigits and MaxFracDigits bound the digits before and after the point.
	MaxIntDigits  = 20
	MaxFracDigits = 18
	// MaxDecimalLen is the longest text ParseDecimal accepts.
	MaxDecimalLen = MaxIntDigits + 1 + MaxFracDigits
)

// ErrDecimal is wrapped by every ParseDecimal refusal. The error text never contains the input.
var ErrDecimal = errors.New("rates: invalid decimal")

// Decimal is a non-negative exact decimal: units is the value × 10^Scale, and nil means 0. It is
// immutable after construction: no method changes units, and Units returns a copy. Compare two Decimals
// with Cmp.
type Decimal struct {
	// A func-typed field makes Decimal incomparable: == on two Decimals would compare the pointers, not
	// the values, so it is a compile error instead.
	_     [0]func()
	units *big.Int
}

// The fixed reasons of a grammar refusal; no reason carries the input.
const (
	whyEmpty     = "empty"
	whyTooLong   = "too long"
	whyByte      = "a byte other than a digit or a point"
	whyPoints    = "more than one point"
	whyNoInt     = "no digit before the point"
	whyIntDigits = "too many integer digits"
	whyNoFrac    = "no digit after the point"
	whyFracDigit = "too many fraction digits"
)

// splitDigits checks s against the grammar: 1 to maxInt ASCII digits, then optionally one '.' and 1 to
// maxFrac digits, and nothing else. It returns the two digit runs, or a fixed reason.
func splitDigits(s string, maxInt, maxFrac int) (intPart, fracPart, why string) {
	if s == "" {
		return "", "", whyEmpty
	}
	if len(s) > maxInt+1+maxFrac {
		return "", "", whyTooLong
	}
	point := -1
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
		case c == '.':
			if point >= 0 {
				return "", "", whyPoints
			}
			point = i
		default:
			return "", "", whyByte
		}
	}
	intPart = s
	if point >= 0 {
		intPart, fracPart = s[:point], s[point+1:]
	}
	switch {
	case intPart == "":
		return "", "", whyNoInt
	case len(intPart) > maxInt:
		return "", "", whyIntDigits
	case point >= 0 && fracPart == "":
		return "", "", whyNoFrac
	case len(fracPart) > maxFrac:
		return "", "", whyFracDigit
	}
	return intPart, fracPart, ""
}

// ParseDecimal reads s: 1 to MaxIntDigits ASCII digits, optionally followed by one '.' and 1 to
// MaxFracDigits digits. Nothing else is accepted: no sign, no exponent, no space, no other byte. Leading
// zeros are accepted, and zero parses (callers that need a positive value check IsZero).
func ParseDecimal(s string) (Decimal, error) {
	intPart, fracPart, why := splitDigits(s, MaxIntDigits, MaxFracDigits)
	if why != "" {
		return Decimal{}, fmt.Errorf("%w: %s", ErrDecimal, why)
	}
	u, ok := new(big.Int).SetString(intPart+fracPart+strings.Repeat("0", Scale-len(fracPart)), 10)
	if !ok { // unreachable: the grammar admits only digits
		return Decimal{}, fmt.Errorf("%w: %s", ErrDecimal, whyByte)
	}
	if u.Sign() == 0 {
		return Decimal{}, nil
	}
	return Decimal{units: u}, nil
}

// value returns the units, or a fresh zero for the zero Decimal. Callers never mutate the result.
func (d Decimal) value() *big.Int {
	if d.units == nil {
		return new(big.Int)
	}
	return d.units
}

// IsZero reports whether d is 0.
func (d Decimal) IsZero() bool { return d.units == nil || d.units.Sign() == 0 }

// Cmp returns -1, 0 or +1 as d is less than, equal to or greater than e.
func (d Decimal) Cmp(e Decimal) int { return d.value().Cmp(e.value()) }

// Units returns a copy of d's units (the value × 10^Scale).
func (d Decimal) Units() *big.Int { return new(big.Int).Set(d.value()) }

// String returns the canonical text: no leading zeros beyond a single 0 before the point, no trailing
// fraction zeros, and no point when d is integral. ParseDecimal reads it back to d.
func (d Decimal) String() string {
	if d.IsZero() {
		return "0"
	}
	digits := d.units.String()
	if len(digits) <= Scale {
		digits = strings.Repeat("0", Scale+1-len(digits)) + digits
	}
	intPart := digits[:len(digits)-Scale]
	fracPart := strings.TrimRight(digits[len(digits)-Scale:], "0")
	if fracPart == "" {
		return intPart
	}
	return intPart + "." + fracPart
}
