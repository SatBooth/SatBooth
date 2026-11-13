// Package lninvoice checks the syntax of a claimant's BOLT 11 invoice (plan 9): the length, the lowercase
// bech32 characters, the human-readable part for the configured network, and the BIP-173 checksum. The one field
// it reads is the amount in the human-readable part (AmountMsat), so a refund can refuse an invoice that does not
// carry exactly the approved amount. It reads nothing from the data part: no payee, expiry or payment hash. The hub
// decides whether the invoice can be paid.
package lninvoice

import (
	"errors"

	"github.com/satbooth/satbooth/approval"
)

// ErrInvalid is the only error: the text is not a well-formed invoice for the network. It carries no input.
var ErrInvalid = errors.New("lninvoice: invalid invoice")

const (
	maxMsat    = approval.MaxAmountMsat // the largest amount AmountMsat returns, 2^53 msat
	maxLen     = 2048
	minDataLen = 117 // 7 timestamp + 104 signature + 6 checksum characters
	charset    = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
)

var prefixes = [...]string{approval.Mainnet: "lnbc", approval.Testnet: "lntb", approval.Signet: "lntbs", approval.Regtest: "lnbcrt"}

var charValue = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(charset); i++ {
		t[charset[i]] = int8(i)
	}
	return
}()

func polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if top>>uint(i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// checksumInput is the BIP-173 expansion of hrp followed by the 5-bit data values.
func checksumInput(hrp string, data []byte) []byte {
	v := make([]byte, 0, 2*len(hrp)+1+len(data))
	for i := 0; i < len(hrp); i++ {
		v = append(v, hrp[i]>>5)
	}
	v = append(v, 0)
	for i := 0; i < len(hrp); i++ {
		v = append(v, hrp[i]&31)
	}
	return append(v, data...)
}

// validAmount reports whether s is empty or matches [0-9]+[munp]?.
func validAmount(s string) bool {
	if s == "" {
		return true
	}
	if last := s[len(s)-1]; last == 'm' || last == 'u' || last == 'n' || last == 'p' {
		s = s[:len(s)-1]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Check returns nil when s is a syntactically valid BOLT 11 invoice for net, else ErrInvalid.
func Check(s string, net approval.Network) error {
	if int(net) >= len(prefixes) || len(s) < 1 || len(s) > maxLen {
		return ErrInvalid
	}
	// Lowercase printable ASCII only: this refuses uppercase, mixed case, whitespace, control bytes and
	// anything outside ASCII before any structure is looked at.
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e || (c >= 'A' && c <= 'Z') {
			return ErrInvalid
		}
	}
	sep := -1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '1' {
			sep = i
			break
		}
	}
	if sep < 0 {
		return ErrInvalid
	}
	hrp, dataPart := s[:sep], s[sep+1:]
	// ln + network code, then an optional amount. The prefix is the configured network's, so "lnbc" +
	// "rt..." (regtest) is not an amount and is refused for mainnet, and "lntb" + "s..." likewise.
	p := prefixes[net]
	if len(hrp) < len(p) || hrp[:len(p)] != p || !validAmount(hrp[len(p):]) {
		return ErrInvalid
	}
	if len(dataPart) < minDataLen {
		return ErrInvalid
	}
	data := make([]byte, len(dataPart))
	for i := 0; i < len(dataPart); i++ {
		v := charValue[dataPart[i]]
		if v < 0 {
			return ErrInvalid
		}
		data[i] = byte(v)
	}
	if polymod(checksumInput(hrp, data)) != 1 {
		return ErrInvalid
	}
	return nil
}

// AmountMsat returns the amount in the human-readable part of s, in msat, and whether s carries one. s must pass
// Check for net (AmountMsat runs it first); nothing but the amount is read. BOLT 11: the digits are BTC, scaled by
// the optional multiplier m (10^-3), u (10^-6), n (10^-9) or p (10^-12), and 1 BTC is 10^11 msat. Deny by default,
// so these are ErrInvalid: an amount with a leading zero (zero included), a p amount whose last digit is not 0 (it
// would be a fraction of a msat), and an amount above 2^53 msat.
func AmountMsat(s string, net approval.Network) (msat int64, present bool, err error) {
	if Check(s, net) != nil {
		return 0, false, ErrInvalid
	}
	sep := len(s) - 1
	for s[sep] != '1' { // Check found a '1'
		sep--
	}
	digits := s[len(prefixes[net]):sep]
	if digits == "" {
		return 0, false, nil
	}
	mul, pico := int64(100_000_000_000), false
	switch digits[len(digits)-1] {
	case 'm':
		mul = 100_000_000
	case 'u':
		mul = 100_000
	case 'n':
		mul = 100
	case 'p':
		mul, pico = 1, true
	}
	if digits[len(digits)-1] > '9' {
		digits = digits[:len(digits)-1]
	}
	// Check guarantees at least one digit; a leading zero is refused, and with it a zero amount.
	if digits[0] == '0' || (pico && digits[len(digits)-1] != '0') {
		return 0, false, ErrInvalid
	}
	var v int64
	for i := 0; i < len(digits); i++ {
		// 10 * 2^53 is the most any multiplier maps to 2^53 msat or less (p divides by 10); past it the amount is
		// refused, and v*10 stays far from int64's limit.
		if v = v*10 + int64(digits[i]-'0'); v > 10*maxMsat {
			return 0, false, ErrInvalid
		}
	}
	if pico {
		v /= 10
	} else if v > maxMsat/mul {
		return 0, false, ErrInvalid
	} else {
		v *= mul
	}
	if v > maxMsat {
		return 0, false, ErrInvalid
	}
	return v, true, nil
}
