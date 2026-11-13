package xmrrpc

import (
	"strconv"
	"strings"
)

// piconeroPerXMR is 10^12: one XMR in its atomic unit.
const piconeroPerXMR = 1_000_000_000_000

// FormatXMR writes piconero as the exact decimal amount of XMR, with up to 12 places and no trailing zeros
// or trailing point (1000000000000 is "1", 1500000000000 is "1.5", 1 is "0.000000000001"). It refuses
// nothing: 0 is "0", and a negative amount is "-" and the digits.
func FormatXMR(piconero int64) string {
	mag := uint64(piconero) // the magnitude through uint64, so that math.MinInt64 has one
	sign := ""
	if piconero < 0 {
		mag = -mag
		sign = "-"
	}
	whole := strconv.FormatUint(mag/piconeroPerXMR, 10)
	frac := mag % piconeroPerXMR
	if frac == 0 {
		return sign + whole
	}
	f := strconv.FormatUint(frac, 10)
	f = strings.Repeat("0", 12-len(f)) + f
	return sign + whole + "." + strings.TrimRight(f, "0")
}
