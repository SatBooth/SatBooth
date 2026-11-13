package nostr

import (
	"errors"
	"fmt"
	"sort"
)

// Event kinds in the NWC (NIP-47) grammar, plus NIP-42 AUTH.
const (
	KindNWCInfo              = 13194
	KindNWCRequest           = 23194
	KindNWCResponse          = 23195
	KindNWCNotificationNIP04 = 23196
	KindNWCNotification      = 23197
	KindAuth                 = 22242
)

// ErrLegacyNIP04 is returned by ValidateNWC for a well-formed kind 23196 event. The relay answers OK
// and drops such an event unread.
var ErrLegacyNIP04 = errors.New("nostr: legacy NIP-04 notification")

const (
	maxInfoContent  = 4096
	maxNames        = 1024
	minPayload      = 132
	maxPayload      = 87472
	maxRelayURL     = 512
	encryptionNIP44 = "nip44_v2"
)

// isNames reports whether s is words separated by single spaces: ^[a-z_]+( [a-z_]+)*$, or with
// digits allowed ^[a-z0-9_]+( [a-z0-9_]+)*$ when digits is true.
func isNames(s string, digits bool) bool {
	if s == "" {
		return false
	}
	prevSpace := true // a leading space is as bad as a double space
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			if prevSpace {
				return false
			}
			prevSpace = true
		case c >= 'a' && c <= 'z', c == '_', digits && c >= '0' && c <= '9':
			prevSpace = false
		default:
			return false
		}
	}
	return !prevSpace
}

// isPayload reports whether s is standard padded base64 of 132..87472 characters.
func isPayload(s string) bool {
	if len(s) < minPayload || len(s) > maxPayload || len(s)%4 != 0 {
		return false
	}
	pad := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			if pad > 0 {
				return false
			}
		case c == '=':
			pad++
			if pad > 2 || i < len(s)-2 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isExpiration reports whether s matches ^[1-9][0-9]{0,17}$.
func isExpiration(s string) bool {
	return len(s) <= 18 && canonicalInt(s) && s != "0"
}

// isRelayURL reports whether s matches ^wss?://[!#-\[\]-~]+$ and is at most 512 bytes.
func isRelayURL(s string) bool {
	if len(s) > maxRelayURL {
		return false
	}
	var rest string
	switch {
	case len(s) > 6 && s[:6] == "wss://":
		rest = s[6:]
	case len(s) > 5 && s[:5] == "ws://":
		rest = s[5:]
	default:
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c <= ' ' || c > '~' || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// isChallenge reports whether s matches ^[0-9a-f]{32,128}$.
func isChallenge(s string) bool {
	return len(s) >= 32 && lowerHex(s, len(s)) && len(s) <= 128
}

type tagRule struct {
	required bool
	ok       func(string) error // nil if the value is acceptable
}

func boolRule(f func(string) bool, why string) func(string) error {
	return func(s string) error {
		if f(s) {
			return nil
		}
		return errors.New(why)
	}
}

var (
	hex64Rule = boolRule(func(s string) bool { return lowerHex(s, 64) }, "not 64 lowercase hex digits")
	// names: length is checked before the alphabet so the reason is stable.
	namesRule = func(s string) error {
		if len(s) > maxNames {
			return errors.New("names too long")
		}
		if !isNames(s, true) {
			return errors.New("names alphabet")
		}
		return nil
	}
)

// tagSet is a kind's tag rules plus its required names in sorted order, computed once so that checking an
// event does not sort.
type tagSet struct {
	rules    map[string]tagRule
	required []string
}

func newTagSet(rules map[string]tagRule) tagSet {
	ts := tagSet{rules: rules}
	for name, r := range rules {
		if r.required {
			ts.required = append(ts.required, name)
		}
	}
	sort.Strings(ts.required) // deterministic: the first missing tag in name order is reported
	return ts
}

// checkTags requires each tag to have exactly two elements, a name in ts, a value accepted by its rule,
// no name twice, and every required name present.
func checkTags(tags [][]string, ts tagSet) error {
	rules := ts.rules
	seen := make(map[string]bool, len(rules))
	for _, t := range tags {
		if len(t) != 2 {
			return errTagShape
		}
		r, known := rules[t[0]]
		if !known {
			return errors.New("nostr: unknown tag")
		}
		if seen[t[0]] {
			return errors.New("nostr: duplicate tag")
		}
		seen[t[0]] = true
		if err := r.ok(t[1]); err != nil {
			return fmt.Errorf("nostr: bad value in %q tag: %w", t[0], err)
		}
	}
	for _, name := range ts.required {
		if !seen[name] {
			return fmt.Errorf("nostr: missing %q tag", name)
		}
	}
	return nil
}

var (
	infoRules = newTagSet(map[string]tagRule{
		"encryption":    {true, namesRule},
		"p":             {true, hex64Rule},
		"notifications": {false, namesRule},
		"extensions":    {false, namesRule},
	})
	requestRules = newTagSet(map[string]tagRule{
		"p":          {true, hex64Rule},
		"encryption": {true, boolRule(func(s string) bool { return s == encryptionNIP44 }, "not nip44_v2")},
		"expiration": {false, boolRule(isExpiration, "bad expiration")},
	})
	responseRules = newTagSet(map[string]tagRule{
		"p": {true, hex64Rule},
		"e": {true, hex64Rule},
	})
	notificationRules = newTagSet(map[string]tagRule{
		"p": {true, hex64Rule},
	})
	authRules = newTagSet(map[string]tagRule{
		"relay":     {true, boolRule(isRelayURL, "bad relay URL")},
		"challenge": {true, boolRule(isChallenge, "bad challenge")},
	})
)

var (
	errPayloadLength   = errors.New("nostr: NIP-44 payload length")
	errPayloadAlphabet = errors.New("nostr: NIP-44 payload alphabet")
	errInfoLength      = errors.New("nostr: info content length")
	errInfoAlphabet    = errors.New("nostr: info content alphabet")
	errTagShape        = errors.New("nostr: tag does not have exactly two elements")
)

// checkPayload reports why s is not a NIP-44 payload (length first, then alphabet and padding). The check
// is syntactic: it accepts base64 with non-zero trailing bits, which nip44.Decrypt's strict decoder
// refuses. That is intended for a relay, which never decrypts.
func checkPayload(s string) error {
	if len(s) < minPayload || len(s) > maxPayload || len(s)%4 != 0 {
		return errPayloadLength
	}
	if !isPayload(s) {
		return errPayloadAlphabet
	}
	return nil
}

// ValidateNWC checks e against the per-kind grammar of design.md section 2.4 and refuses anything
// outside it. It returns ErrLegacyNIP04, and nothing else, for a well-formed kind 23196 event (whose
// content is never parsed). It does not check id or signature.
func ValidateNWC(e *Event) error {
	if e == nil {
		return errors.New("nostr: nil event")
	}
	if !printable(e.Content) {
		return errors.New("nostr: content holds a byte outside 0x20-0x7e")
	}
	for _, t := range e.Tags {
		for _, v := range t {
			if !printable(v) {
				return errors.New("nostr: tag holds a byte outside 0x20-0x7e")
			}
		}
	}
	switch e.Kind {
	case KindNWCInfo:
		if len(e.Content) > maxInfoContent {
			return errInfoLength
		}
		if !isNames(e.Content, false) {
			return errInfoAlphabet
		}
		return checkTags(e.Tags, infoRules)
	case KindNWCRequest:
		if err := checkPayload(e.Content); err != nil {
			return err
		}
		return checkTags(e.Tags, requestRules)
	case KindNWCResponse:
		if err := checkPayload(e.Content); err != nil {
			return err
		}
		return checkTags(e.Tags, responseRules)
	case KindNWCNotification:
		if err := checkPayload(e.Content); err != nil {
			return err
		}
		return checkTags(e.Tags, notificationRules)
	case KindNWCNotificationNIP04:
		// Never parsed: any tags of exactly two elements (printable, checked above).
		for _, t := range e.Tags {
			if len(t) != 2 {
				return errTagShape
			}
		}
		return ErrLegacyNIP04
	case KindAuth:
		if e.Content != "" {
			return errors.New("nostr: AUTH content must be empty")
		}
		return checkTags(e.Tags, authRules)
	}
	return fmt.Errorf("nostr: kind %d is outside the grammar", e.Kind)
}
