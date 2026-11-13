package rates

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrPercent is wrapped by every ParsePercent refusal. The error text never contains the input.
var ErrPercent = errors.New("rates: invalid percent")

// The percent grammar: ParseDecimal's, narrowed to 4 integer digits and 2 fraction digits (basis points).
const (
	percentIntDigits  = 4
	percentFracDigits = 2
)

// ParsePercent reads a percentage as basis points: an optional leading '-', then 1 to 4 ASCII digits,
// optionally followed by one '.' and 1 or 2 digits. "1.5" is 150, "-2" is -200, "-0" is 0.
func ParsePercent(s string) (bps int64, err error) {
	neg := len(s) > 0 && s[0] == '-'
	if neg {
		s = s[1:]
	}
	intPart, fracPart, why := splitDigits(s, percentIntDigits, percentFracDigits)
	if why != "" {
		return 0, fmt.Errorf("%w: %s", ErrPercent, why)
	}
	for i := 0; i < len(intPart); i++ {
		bps = bps*10 + int64(intPart[i]-'0')
	}
	for i := 0; i < percentFracDigits; i++ {
		bps *= 10
		if i < len(fracPart) {
			bps += int64(fracPart[i] - '0')
		}
	}
	if neg {
		bps = -bps
	}
	return bps, nil
}

// FormatPercent is the inverse of ParsePercent, in canonical form: 150 is "1.5", -200 is "-2", 0 is "0",
// 1 is "0.01". It formats every int64; ParsePercent reads back those from -999999 to 999999.
func FormatPercent(bps int64) string {
	mag := uint64(bps)
	sign := ""
	if bps < 0 {
		mag = -mag // two's complement: correct for math.MinInt64 too
		sign = "-"
	}
	s := sign + strconv.FormatUint(mag/100, 10)
	switch frac := mag % 100; {
	case frac == 0:
		return s
	case frac%10 == 0:
		return s + "." + strconv.FormatUint(frac/10, 10)
	default:
		return s + "." + string([]byte{byte('0' + frac/10), byte('0' + frac%10)})
	}
}
