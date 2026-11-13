package nostr

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"time"
)

// AuthMaxSkew is how far an AUTH event's created_at may be from the checker's clock, in either direction.
// A created_at exactly AuthMaxSkew away is accepted; one second further is refused.
const AuthMaxSkew = 10 * time.Minute

// Sentinels returned (possibly wrapped) by CheckAuth. None of them carries the relay URL or challenge.
var (
	ErrAuthGrammar   = errors.New("nostr: AUTH event is outside the grammar")
	ErrAuthKind      = errors.New("nostr: not an AUTH event")
	ErrAuthRelay     = errors.New("nostr: AUTH relay tag does not match")
	ErrAuthChallenge = errors.New("nostr: AUTH challenge does not match")
	ErrAuthTime      = errors.New("nostr: AUTH created_at is outside the allowed window")
	ErrAuthDenied    = errors.New("nostr: AUTH pubkey is not allowed")
)

// NewAuth builds and signs a NIP-42 AUTH event (kind 22242) answering challenge for relayURL, with
// created_at set to now. A relay URL or challenge outside the grammar is refused before signing.
func NewAuth(k *SecretKey, relayURL, challenge string, now time.Time) (*Event, error) {
	e := &Event{
		CreatedAt: now.Unix(),
		Kind:      KindAuth,
		Tags:      [][]string{{"relay", relayURL}, {"challenge", challenge}},
	}
	if err := ValidateNWC(e); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthGrammar, err)
	}
	if err := Sign(e, k); err != nil {
		return nil, err
	}
	return e, nil
}

// CheckAuth reports whether e is a valid AUTH answer to challenge for relayURL at time now from a pubkey
// that allowed accepts. Order: kind, grammar, time window, relay, challenge, signature and id, allowlist,
// so the cheap checks run before BIP-340 verification. A nil event is refused first. The relay URL and
// challenge are compared byte for byte (no normalisation) in constant time. A nil allowed refuses
// everyone. Errors never echo the URL or the challenge.
func CheckAuth(e *Event, relayURL, challenge string, now time.Time, allowed func(pubkey string) bool) error {
	if e == nil {
		return errNilEvent
	}
	if e.Kind != KindAuth {
		return ErrAuthKind
	}
	if err := ValidateNWC(e); err != nil {
		return fmt.Errorf("%w: %w", ErrAuthGrammar, err)
	}
	skew := int64(AuthMaxSkew / time.Second)
	if e.CreatedAt < now.Unix()-skew || e.CreatedAt > now.Unix()+skew {
		return ErrAuthTime
	}
	// The grammar guarantees exactly one relay and one challenge tag and nothing else.
	var gotRelay, gotChallenge string
	for _, t := range e.Tags {
		switch t[0] {
		case "relay":
			gotRelay = t[1]
		case "challenge":
			gotChallenge = t[1]
		}
	}
	if subtle.ConstantTimeCompare([]byte(gotRelay), []byte(relayURL)) != 1 {
		return ErrAuthRelay
	}
	if subtle.ConstantTimeCompare([]byte(gotChallenge), []byte(challenge)) != 1 {
		return ErrAuthChallenge
	}
	if err := Verify(e); err != nil {
		return err
	}
	if allowed == nil || !allowed(e.PubKey) {
		return ErrAuthDenied
	}
	return nil
}
