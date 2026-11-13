package nostr

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

var (
	errNilEvent = errors.New("nostr: nil event")
	errSignFail = errors.New("nostr: signing failed")
	errSignKind = errors.New("nostr: cannot sign: kind is outside 0..65535")
	errSignTime = errors.New("nostr: cannot sign: created_at is negative")
	errSignTag  = errors.New("nostr: cannot sign: a tag is nil (it would marshal as null)")
	errSignSize = errors.New("nostr: cannot sign: the signed event would be over MaxEventJSON bytes")
)

// Errors returned (possibly wrapped) by Verify and CheckAuth.
var (
	// ErrBadFormat means id, pubkey or sig is not lowercase hex of the right length.
	ErrBadFormat = errors.New("nostr: event id, pubkey or sig is not lowercase hex of the right length")
	// ErrBadID means the id is not the hash of the serialised event.
	ErrBadID = errors.New("nostr: event id does not match its content")
	// ErrBadSig means the signature does not verify (or is not a parsable BIP-340 signature).
	ErrBadSig = errors.New("nostr: invalid signature")
)

// signID signs the 32-byte id with BIP-340 and returns the 64-byte signature as lowercase hex. A nil aux
// draws 32 fresh bytes from crypto/rand, as BIP-340 recommends; the tests pass the vectors' aux_rand.
func signID(k *SecretKey, id [32]byte, aux *[32]byte) (string, error) {
	if k == nil || k.key == nil {
		return "", errNoKey
	}
	var a [32]byte
	if aux != nil {
		a = *aux
	} else if _, err := rand.Read(a[:]); err != nil {
		return "", errSignFail
	}
	defer clear(a[:])
	sig, err := schnorr.Sign(k.key, id[:], schnorr.CustomNonce(a))
	if err != nil {
		return "", errSignFail
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// verifyID checks a BIP-340 signature over a 32-byte id. All three inputs are lowercase hex or raw bytes
// as noted; any failure, including an unparsable key or signature, is an error.
func verifyID(pubHex string, id [32]byte, sigHex string) error {
	if !lowerHex(sigHex, 128) {
		return ErrBadFormat
	}
	pub, err := parsePub(pubHex)
	if err != nil {
		return err // ErrBadPubKey
	}
	sb, err := hex.DecodeString(sigHex)
	if err != nil {
		return ErrBadFormat
	}
	sig, err := schnorr.ParseSignature(sb)
	if err != nil {
		return ErrBadSig
	}
	if !sig.Verify(id[:], pub) {
		return ErrBadSig
	}
	return nil
}

// jsonString returns n plus the length of s as encoding/json writes a printable-ASCII string: the two
// quotes, a backslash before '"' and '\', and six bytes (\u003c and so on) for '<', '>' and '&', which
// json.Marshal escapes by default.
func jsonString(n int, s string) int {
	n += 2
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			n += 2
		case '<', '>', '&':
			n += 6
		default:
			n++
		}
	}
	return n
}

// signedJSONLen counts, without serialising, the length of json.Marshal(e) once e carries a 64-hex id and a
// 128-hex sig. It is counted by hand so that nostr stays free of encoding/json's encoder paths: its capslock
// baseline then keeps no NETWORK, RUNTIME, SYSTEM_CALLS or UNSAFE_POINTER artefacts, and a real network
// capability in the key-handling package would still be flagged. TestSignParseProperty cross-checks it
// against json.Marshal.
func signedJSONLen(e *Event) int {
	// {"id":"…","pubkey":"…","created_at":N,"kind":N,"tags":[…],"content":"…","sig":"…"}
	n := len(`{"id":"","pubkey":"","created_at":,"kind":,"tags":[],"content":"","sig":""}`) + 64 + 64 + 128
	n += len(strconv.FormatInt(e.CreatedAt, 10)) + len(strconv.Itoa(e.Kind))
	for i, t := range e.Tags {
		if i > 0 {
			n++
		}
		n += 2 // [ ]
		for j, v := range t {
			if j > 0 {
				n++
			}
			n = jsonString(n, v)
		}
	}
	return jsonString(n, e.Content) - 2 // the template already holds the content's quotes
}

// Sign sets e.PubKey, e.ID and e.Sig. The caller sets CreatedAt, Kind, Tags and Content. A nil Tags
// becomes an empty list, so the signed event round-trips through ParseEvent. If the event cannot be
// signed (Kind outside 0..65535, negative CreatedAt, a nil inner tag, a byte outside 0x20-0x7e, or JSON
// over MaxEventJSON bytes once signed) it returns an error and leaves e unchanged: Sign refuses what
// ParseEvent would refuse, so a signed event always round-trips through json.Marshal and ParseEvent.
func Sign(e *Event, k *SecretKey) error {
	if e == nil {
		return errNilEvent
	}
	if k == nil || k.key == nil {
		return errNoKey
	}
	if e.Kind < 0 || e.Kind > maxKind {
		return errSignKind
	}
	if e.CreatedAt < 0 {
		return errSignTime
	}
	c := *e
	if c.Tags == nil {
		c.Tags = [][]string{}
	}
	for _, t := range c.Tags {
		if t == nil { // json.Marshal writes null, which ParseEvent refuses
			return errSignTag
		}
	}
	c.PubKey = k.PublicKeyHex()
	idHex, err := ComputeID(&c)
	if err != nil {
		return err
	}
	// The size check counts the JSON of the would-be signed event, with placeholders of the real id and sig lengths.
	// Every string is printable ASCII here (ComputeID refused anything else).
	if signedJSONLen(&c) > MaxEventJSON {
		return errSignSize
	}
	var id [32]byte
	if _, err := hex.Decode(id[:], []byte(idHex)); err != nil {
		return errSignFail
	}
	sig, err := signID(k, id, nil)
	if err != nil {
		return err
	}
	e.PubKey, e.ID, e.Sig, e.Tags = c.PubKey, idHex, sig, c.Tags
	return nil
}

// Verify checks that ID, PubKey and Sig are lowercase hex of the right length, that ID is the hash of the
// serialised event, and that Sig is a valid BIP-340 signature of ID by PubKey.
func Verify(e *Event) error {
	if e == nil {
		return errNilEvent
	}
	if !lowerHex(e.ID, 64) || !lowerHex(e.PubKey, 64) || !lowerHex(e.Sig, 128) {
		return ErrBadFormat
	}
	want, err := ComputeID(e)
	if err != nil {
		return err
	}
	if want != e.ID {
		return ErrBadID
	}
	var id [32]byte
	if _, err := hex.Decode(id[:], []byte(e.ID)); err != nil {
		return ErrBadFormat
	}
	return verifyID(e.PubKey, id, e.Sig)
}
