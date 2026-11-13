// Package nip44 implements NIP-44 version 2 payload encryption: secp256k1 ECDH, HKDF-SHA256,
// ChaCha20 and an HMAC-SHA256 over nonce and ciphertext.
//
// Decrypt parses untrusted input and never panics. It checks, in this order: the "#" marker, the
// base64 length, the base64 alphabet, the version byte, the decoded length, the MAC (in constant
// time, before any decryption), and finally the padding and its length prefix.
package nip44

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20"

	"github.com/satbooth/satbooth/nostr"
)

// Sentinel errors, one per identifiable rule.
var (
	ErrPlaintextLength    = errors.New("nip44: plaintext must be 1 to 65535 bytes")
	ErrUnsupportedVersion = errors.New("nip44: unsupported encryption version")
	ErrPayloadLength      = errors.New("nip44: payload length out of range")
	ErrBase64             = errors.New("nip44: payload is not canonical standard base64")
	ErrDataLength         = errors.New("nip44: decoded payload length out of range")
	ErrMAC                = errors.New("nip44: invalid MAC")
	ErrPadding            = errors.New("nip44: invalid padding")
	ErrNilKey             = errors.New("nip44: nil secret key")
)

const (
	version       = 2
	maxPlaintext  = 65535
	minPayloadB64 = 132   // base64 length of the shortest decoded payload (99 bytes)
	maxPayloadB64 = 87472 // base64 length of the longest decoded payload (65603 bytes)
	minData       = 99    // 1 version + 32 nonce + 34 padded (2 + 32) + 32 MAC
	maxData       = 65603 // 1 + 32 + (2 + 65536) + 32
)

// ConversationKey derives the NIP-44 v2 conversation key between k and the x-only public key pubHex.
func ConversationKey(k *nostr.SecretKey, pubHex string) ([32]byte, error) {
	var out [32]byte
	if k == nil {
		return out, ErrNilKey
	}
	x, err := k.SharedX(pubHex)
	if err != nil {
		return out, err
	}
	prk, err := hkdf.Extract(sha256.New, x[:], []byte("nip44-v2"))
	clear(x[:]) // best effort, matching SharedX
	if err != nil {
		return out, err
	}
	copy(out[:], prk)
	clear(prk)
	return out, nil
}

// MessageKeys derives the per-message ChaCha20 key and nonce and the HMAC key.
func MessageKeys(conv [32]byte, nonce [32]byte) (chachaKey [32]byte, chachaNonce [12]byte, hmacKey [32]byte, err error) {
	okm, err := hkdf.Expand(sha256.New, conv[:], string(nonce[:]), 76)
	if err != nil {
		return
	}
	copy(chachaKey[:], okm[0:32])
	copy(chachaNonce[:], okm[32:44])
	copy(hmacKey[:], okm[44:76])
	clear(okm)
	return
}

// CalcPaddedLen returns the padded length for a plaintext of n bytes. The official vectors include
// n = 65536, so 1 to 65536 is accepted here; Encrypt and Decrypt enforce 1 to 65535.
func CalcPaddedLen(n int) (int, error) {
	if n < 1 || n > maxPlaintext+1 {
		return 0, ErrPlaintextLength
	}
	if n <= 32 {
		return 32, nil
	}
	next := 1
	for next < n {
		next <<= 1
	}
	chunk := 32
	if next > 256 {
		chunk = next / 8
	}
	return chunk * ((n-1)/chunk + 1), nil
}

func xorChaCha(key [32]byte, nonce [12]byte, data []byte) error {
	c, err := chacha20.NewUnauthenticatedCipher(key[:], nonce[:])
	if err != nil {
		return err
	}
	c.XORKeyStream(data, data)
	return nil
}

func mac(hmacKey [32]byte, nonce [32]byte, ct []byte) []byte {
	h := hmac.New(sha256.New, hmacKey[:])
	h.Write(nonce[:])
	h.Write(ct)
	return h.Sum(nil)
}

// EncryptWithNonce encrypts plaintext with a caller-chosen nonce. Use Encrypt outside tests: a
// repeated nonce under one key leaks the XOR of the plaintexts.
func EncryptWithNonce(conv [32]byte, plaintext string, nonce [32]byte) (string, error) {
	n := len(plaintext)
	if n > maxPlaintext {
		return "", ErrPlaintextLength
	}
	padLen, err := CalcPaddedLen(n)
	if err != nil {
		return "", err
	}
	ck, cn, hk, err := MessageKeys(conv, nonce)
	if err != nil {
		return "", err
	}
	defer clear(ck[:])
	defer clear(hk[:])
	buf := make([]byte, 1+32+2+padLen+32)
	buf[0] = version
	copy(buf[1:33], nonce[:])
	ct := buf[33 : 33+2+padLen]
	binary.BigEndian.PutUint16(ct, uint16(n))
	copy(ct[2:], plaintext)
	if err := xorChaCha(ck, cn, ct); err != nil {
		return "", err
	}
	copy(buf[33+2+padLen:], mac(hk, nonce, ct))
	return base64.StdEncoding.EncodeToString(buf), nil
}

// Encrypt encrypts plaintext (1 to 65535 bytes) with a fresh random nonce.
func Encrypt(conv [32]byte, plaintext string) (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return EncryptWithNonce(conv, plaintext, nonce)
}

// Decrypt authenticates and decrypts a NIP-44 v2 payload. It returns one of the sentinel errors
// above for each rule it enforces.
func Decrypt(conv [32]byte, payload string) (string, error) {
	if len(payload) > 0 && payload[0] == '#' {
		return "", ErrUnsupportedVersion
	}
	if len(payload) < minPayloadB64 || len(payload) > maxPayloadB64 {
		return "", ErrPayloadLength
	}
	// encoding/base64 silently skips \r and \n; NIP-44 payloads never contain them.
	if strings.ContainsAny(payload, "\r\n") {
		return "", ErrBase64
	}
	data, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil {
		return "", ErrBase64
	}
	if len(data) == 0 || data[0] != version {
		return "", ErrUnsupportedVersion
	}
	if len(data) < minData || len(data) > maxData {
		return "", ErrDataLength
	}
	var nonce [32]byte
	copy(nonce[:], data[1:33])
	ct := data[33 : len(data)-32]
	gotMAC := data[len(data)-32:]
	ck, cn, hk, err := MessageKeys(conv, nonce)
	if err != nil {
		return "", err
	}
	defer clear(ck[:])
	defer clear(hk[:])
	if !hmac.Equal(mac(hk, nonce, ct), gotMAC) {
		return "", ErrMAC
	}
	if err := xorChaCha(ck, cn, ct); err != nil {
		return "", err
	}
	if len(ct) < 2 {
		return "", ErrPadding
	}
	n := int(binary.BigEndian.Uint16(ct))
	padLen, err := CalcPaddedLen(n)
	if err != nil || len(ct) != 2+padLen {
		return "", ErrPadding
	}
	return string(ct[2 : 2+n]), nil
}
