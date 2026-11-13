package relay

import (
	"errors"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

// Listener names the two listeners; the admission rules differ between them.
type Listener int

const (
	// HubListener is the listener the Alby Hub connects to: wallet keys publish, and the hub reads requests.
	HubListener Listener = iota
	// ClientListener is the private listener for SatBooth's client keys; it requires NIP-42 AUTH first.
	ClientListener
)

// Verdict is the policy's answer for one EVENT. Accept stores and broadcasts the event. Drop is the
// legacy 23196 case: answer OK true and never store or forward it. Otherwise Msg is the OK false reason.
// Msg is always one of the fixed strings below: never a key or any event content.
type Verdict struct {
	Accept, Drop bool
	Msg          string
}

// Fixed reasons, with the NIP-01 prefixes.
const (
	msgAuthRequired = "auth-required: authenticate before using this listener"
	msgDeletion     = "blocked: deletion is not supported"
	msgKind         = "restricted: this kind is not accepted on this listener"
	msgGrammar      = "invalid: event is outside the NWC grammar"
	msgNotPaired    = "restricted: author is not a paired key"
	msgNotAuthed    = "restricted: author has not authenticated"
	msgWrongPair    = "restricted: p tag does not match the author's pair"
	msgLegacy       = "restricted: legacy NIP-04 notifications are not accepted"
	msgTooOld       = "invalid: created_at is too old"
	msgTooNew       = "invalid: created_at is too far in the future"
	msgExpired      = "invalid: event has expired"
	msgBadSig       = "invalid: bad id or signature"
	msgFilterScope  = "restricted: filter is outside what this listener serves"
)

// Policy decides which events and filters each listener admits. It holds only the configured pairs.
type Policy struct {
	c *Config
}

// NewPolicy returns the admission rules for the pairs in c.
func NewPolicy(c *Config) *Policy { return &Policy{c: c} }

func refuse(msg string) Verdict { return Verdict{Msg: msg} }

// pTags returns the value of every p tag of e.
func pTags(e *nostr.Event) []string {
	var out []string
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == "p" {
			out = append(out, t[1])
		}
	}
	return out
}

// allEqual reports whether vs is non-empty and every value is want.
func allEqual(vs []string, want string) bool {
	if len(vs) == 0 {
		return false
	}
	for _, v := range vs {
		if v != want {
			return false
		}
	}
	return true
}

// Event runs checks 1 to 6 of the EVENT order. Duplicates are the window's job. e must be non-nil.
//
// A legacy 23196 that only claims a paired wallet is answered OK and dropped unread, before Verify: it is
// never stored on that path and its content is never logged (a fixed string is).
//
// authed is the connection's authenticated client keys; on the hub listener it is always empty.
// A forged event that breaks an earlier rule reports that earlier rule, because every check before the
// signature is cheap.
func (p *Policy) Event(l Listener, authed []string, e *nostr.Event, now time.Time, lifetime time.Duration) Verdict {
	// 1. Listener rule.
	if l == ClientListener && len(authed) == 0 {
		return refuse(msgAuthRequired)
	}
	wp, fromWallet := p.c.PairByWallet(e.PubKey)
	if e.Kind == 5 && fromWallet {
		return refuse(msgDeletion)
	}
	switch l {
	case HubListener:
		switch e.Kind {
		case nostr.KindNWCInfo, nostr.KindNWCResponse, nostr.KindNWCNotification, nostr.KindNWCNotificationNIP04:
		default:
			return refuse(msgKind)
		}
	case ClientListener:
		if e.Kind != nostr.KindNWCRequest {
			return refuse(msgKind)
		}
	default:
		return refuse(msgKind)
	}

	// 2. Grammar.
	if err := nostr.ValidateNWC(e); err != nil {
		if errors.Is(err, nostr.ErrLegacyNIP04) {
			if fromWallet {
				return Verdict{Drop: true}
			}
			return refuse(msgLegacy)
		}
		return refuse(msgGrammar)
	}

	// 3. Pairing: the author and the p tag.
	ps := pTags(e)
	if l == HubListener {
		if !fromWallet {
			return refuse(msgNotPaired)
		}
		// 13194 may omit p (the grammar currently requires it), but when present it must be the client.
		if e.Kind == nostr.KindNWCInfo {
			for _, v := range ps {
				if v != wp.Client {
					return refuse(msgWrongPair)
				}
			}
		} else if !allEqual(ps, wp.Client) {
			return refuse(msgWrongPair)
		}
	} else {
		cp, ok := p.c.PairByClient(e.PubKey)
		if !ok {
			return refuse(msgNotPaired)
		}
		if !inStrings(authed, e.PubKey) {
			return refuse(msgNotAuthed)
		}
		if !allEqual(ps, cp.Wallet) {
			return refuse(msgWrongPair)
		}
	}

	// 4. created_at is greater than now - lifetime and at most now + AuthMaxSkew.
	if e.CreatedAt <= now.Add(-lifetime).Unix() {
		return refuse(msgTooOld)
	}
	if e.CreatedAt > now.Add(nostr.AuthMaxSkew).Unix() {
		return refuse(msgTooNew)
	}

	// 5. NIP-40 expiration.
	if expired(e, now) {
		return refuse(msgExpired)
	}

	// 6. id and signature.
	if nostr.Verify(e) != nil {
		return refuse(msgBadSig)
	}
	return Verdict{Accept: true}
}

// allIn reports whether every value of vs is in set (an empty vs passes: callers check non-emptiness).
func allIn(vs, set []string) bool {
	for _, v := range vs {
		if !inStrings(set, v) {
			return false
		}
	}
	return true
}

// onlyKinds reports whether ks is non-empty and every kind is in allowed.
func onlyKinds(ks []int, allowed ...int) bool {
	if len(ks) == 0 {
		return false
	}
	for _, k := range ks {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Filters decides whether a REQ may be served. ok is false for any filter outside the listener's
// shapes; msg then holds the CLOSED reason. A filter with an empty list (which matches nothing) is
// refused too: it fits none of the admitted shapes, every shape requires its lists to be non-empty, and
// the hub never sends one. since, until and limit are unconstrained. A REQ with no filters serves nothing
// and is let through.
func (p *Policy) Filters(l Listener, authed []string, fs []Filter) (ok bool, msg string) {
	if l == ClientListener && len(authed) == 0 {
		return false, msgAuthRequired
	}
	var wallets []string // the wallets this connection may read
	if l == HubListener {
		for _, pr := range p.c.Pairs {
			wallets = append(wallets, pr.Wallet)
		}
	} else {
		for _, k := range authed {
			if pr, ok := p.c.PairByClient(k); ok {
				wallets = append(wallets, pr.Wallet)
			}
		}
	}
	for i := range fs {
		if !p.filterOK(l, authed, wallets, &fs[i]) {
			return false, msgFilterScope
		}
	}
	return true, ""
}

func (p *Policy) filterOK(l Listener, authed, wallets []string, f *Filter) bool {
	// {kinds:[13194], authors in wallets}, on both listeners.
	if onlyKinds(f.Kinds, nostr.KindNWCInfo) {
		return len(f.Authors) > 0 && allIn(f.Authors, wallets) && f.P == nil && f.E == nil
	}
	if l == HubListener {
		// {kinds:[23194], #p in wallets}, with no authors.
		return onlyKinds(f.Kinds, nostr.KindNWCRequest) && f.Authors == nil && f.E == nil &&
			len(f.P) > 0 && allIn(f.P, wallets)
	}
	// Client: responses and notifications addressed to an authenticated key.
	if !onlyKinds(f.Kinds, nostr.KindNWCResponse, nostr.KindNWCNotification) {
		return false
	}
	if len(f.P) == 0 || !allIn(f.P, authed) {
		return false
	}
	for _, k := range f.P { // the caller pairs authed keys; do not rely on it
		if _, ok := p.c.PairByClient(k); !ok {
			return false
		}
	}
	if f.Authors != nil { // only the wallets paired with this filter's p keys
		var paired []string
		for _, k := range f.P {
			if pr, ok := p.c.PairByClient(k); ok {
				paired = append(paired, pr.Wallet)
			}
		}
		if len(f.Authors) == 0 || !allIn(f.Authors, paired) {
			return false
		}
	}
	return f.E == nil || len(f.E) > 0
}
