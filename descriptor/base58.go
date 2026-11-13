package descriptor

import (
	"crypto/sha256"
	"math/big"
)

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// b58val maps a byte to its base58 digit value, or -1 when it is not in the alphabet.
var b58val = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return
}()

func isBase58(c byte) bool { return b58val[c] >= 0 }

var big58 = big.NewInt(58)

// decodeCheck decodes Base58Check: standard alphabet, each leading '1' is a leading zero byte, the
// last four bytes are the first four bytes of SHA-256(SHA-256(rest)). It returns the bytes before
// the checksum (version byte(s) included). Any malformed input gives (nil, false).
// Reference: https://en.bitcoin.it/wiki/Base58Check_encoding (checked against BIP32 test vector 1,
// see descriptor_test.go).
func decodeCheck(s string) ([]byte, bool) {
	if len(s) < 5 || len(s) > 1024 {
		return nil, false
	}
	n := new(big.Int)
	for i := 0; i < len(s); i++ {
		v := b58val[s[i]]
		if v < 0 {
			return nil, false
		}
		n.Mul(n, big58)
		n.Add(n, big.NewInt(int64(v)))
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	body := n.Bytes()
	raw := make([]byte, zeros+len(body))
	copy(raw[zeros:], body)
	if len(raw) < 5 {
		return nil, false
	}
	data, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	h := sha256.Sum256(data)
	h = sha256.Sum256(h[:])
	if h[0] != sum[0] || h[1] != sum[1] || h[2] != sum[2] || h[3] != sum[3] {
		return nil, false
	}
	return data, true
}
