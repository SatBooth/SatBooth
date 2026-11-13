package nostr

import (
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

const (
	authRelay     = "wss://relay.example"
	authChallenge = "0123456789abcdef0123456789abcdef"
)

var authNow = time.Unix(1700000000, 0)

func allowAll(string) bool { return true }

func newAuthEvent(t testing.TB) (*Event, *SecretKey) {
	t.Helper()
	k, err := ParseSecretKeyHex(testKeyA)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewAuth(k, authRelay, authChallenge, authNow)
	if err != nil {
		t.Fatal(err)
	}
	return e, k
}

func TestAuthRoundTrip(t *testing.T) {
	e, k := newAuthEvent(t)
	if e.Kind != KindAuth || e.Content != "" || e.CreatedAt != authNow.Unix() || e.PubKey != k.PublicKeyHex() {
		t.Fatalf("unexpected event %+v", e)
	}
	if err := CheckAuth(e, authRelay, authChallenge, authNow, allowAll); err != nil {
		t.Fatal(err)
	}
	// Both window edges are accepted.
	for _, d := range []time.Duration{-AuthMaxSkew, AuthMaxSkew} {
		if err := CheckAuth(e, authRelay, authChallenge, authNow.Add(d), allowAll); err != nil {
			t.Errorf("skew %v: %v", d, err)
		}
	}
	// The allowlist sees the event's pubkey.
	var seen string
	_ = CheckAuth(e, authRelay, authChallenge, authNow, func(p string) bool { seen = p; return true })
	if seen != k.PublicKeyHex() {
		t.Error("allowed not called with the pubkey")
	}
	// NewAuth refuses input outside the grammar.
	for _, c := range []struct{ url, ch string }{
		{"http://relay.example", authChallenge}, {authRelay, "ABCDEF0123456789ABCDEF0123456789"},
		{authRelay, strings.Repeat("a", 31)}, {"wss://" + strings.Repeat("a", 507), authChallenge},
	} {
		if _, err := NewAuth(k, c.url, c.ch, authNow); !errors.Is(err, ErrAuthGrammar) {
			t.Errorf("NewAuth(%q,%q) = %v", c.url, c.ch, err)
		}
	}
	if _, err := NewAuth(nil, authRelay, authChallenge, authNow); err == nil {
		t.Error("NewAuth accepted a nil key")
	}
}

// mutateAuth applies f to a fresh valid event and re-signs it with the original key (id recomputed), so
// the rule under test, not the signature, is what fires.
func mutateAuth(t testing.TB, f func(*Event)) *Event {
	e, k := newAuthEvent(t)
	f(e)
	if err := Sign(e, k); err != nil {
		if e.CreatedAt >= 0 {
			t.Fatal(err)
		}
		// Sign refuses a negative created_at (ParseEvent would); sign by hand so CheckAuth's own
		// time rule is still what fires.
		e.PubKey = k.PublicKeyHex()
		idHex, err := ComputeID(e)
		if err != nil {
			t.Fatal(err)
		}
		var id [32]byte
		if _, err := hex.Decode(id[:], []byte(idHex)); err != nil {
			t.Fatal(err)
		}
		sig, err := signID(k, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.ID, e.Sig = idHex, sig
	}
	return e
}

func TestAuthRejects(t *testing.T) {
	skew := int64(AuthMaxSkew / time.Second)
	type tc struct {
		name      string
		mod       func(*Event)
		url, ch   string
		now       time.Time
		allowed   func(string) bool
		want      error
		noResign  bool
		afterSign func(*Event)
	}
	cases := []tc{
		{name: "relay with trailing slash", url: authRelay + "/", want: ErrAuthRelay},
		{name: "relay different case", url: "wss://Relay.example", want: ErrAuthRelay},
		{name: "relay other host", url: "wss://other.example", want: ErrAuthRelay},
		{name: "challenge other", ch: strings.Repeat("f", 32), want: ErrAuthChallenge},
		{name: "challenge longer", ch: authChallenge + "0", want: ErrAuthChallenge},
		{name: "created_at too old", mod: func(e *Event) { e.CreatedAt = authNow.Unix() - skew - 1 }, want: ErrAuthTime},
		{name: "created_at too new", mod: func(e *Event) { e.CreatedAt = authNow.Unix() + skew + 1 }, want: ErrAuthTime},
		{name: "created_at negative", mod: func(e *Event) { e.CreatedAt = -1 }, want: ErrAuthTime},
		{name: "clock moved forward", now: authNow.Add(AuthMaxSkew + time.Second), want: ErrAuthTime},
		{name: "clock moved back", now: authNow.Add(-AuthMaxSkew - time.Second), want: ErrAuthTime},
		{name: "not allowed", allowed: func(string) bool { return false }, want: ErrAuthDenied},
		{name: "nil allowed", allowed: nil, want: ErrAuthDenied},
		{name: "kind 22243", mod: func(e *Event) { e.Kind = 22243 }, want: ErrAuthKind},
		{name: "kind nwc request", mod: func(e *Event) { e.Kind = KindNWCRequest }, want: ErrAuthKind},
		{name: "extra tag", mod: func(e *Event) { e.Tags = append(e.Tags, []string{"p", testKeyB}) }, want: ErrAuthGrammar},
		{name: "duplicate relay", mod: func(e *Event) { e.Tags = append(e.Tags, e.Tags[0]) }, want: ErrAuthGrammar},
		{name: "missing challenge", mod: func(e *Event) { e.Tags = e.Tags[:1] }, want: ErrAuthGrammar},
		{name: "content x", mod: func(e *Event) { e.Content = "x" }, want: ErrAuthGrammar},
		{name: "bad signature", noResign: true, afterSign: func(e *Event) { e.Sig = flipHex(e.Sig, 0) }, want: ErrBadSig},
		{name: "bad id", noResign: true, afterSign: func(e *Event) { e.ID = flipHex(e.ID, 0) }, want: ErrBadID},
		{name: "nil event", noResign: true, afterSign: nil, want: errNilEvent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mutateAuth(t, func(e *Event) {
				if c.mod != nil {
					c.mod(e)
				}
			})
			if c.afterSign != nil {
				c.afterSign(e)
			}
			if c.name == "nil event" {
				e = nil
			}
			url, ch, now, allowed := c.url, c.ch, c.now, c.allowed
			if url == "" {
				url = authRelay
			}
			if ch == "" {
				ch = authChallenge
			}
			if now.IsZero() {
				now = authNow
			}
			if allowed == nil && c.name != "nil allowed" {
				allowed = allowAll
			}
			err := CheckAuth(e, url, ch, now, allowed)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if err != nil && (strings.Contains(err.Error(), "relay.example") || strings.Contains(err.Error(), authChallenge)) {
				t.Errorf("error echoes input: %v", err)
			}
		})
	}
}

func TestAuthTimeEdges(t *testing.T) {
	skew := int64(AuthMaxSkew / time.Second)
	for d, want := range map[int64]error{-skew - 1: ErrAuthTime, -skew: nil, 0: nil, skew: nil, skew + 1: ErrAuthTime} {
		e := mutateAuth(t, func(e *Event) { e.CreatedAt = authNow.Unix() + d })
		if err := CheckAuth(e, authRelay, authChallenge, authNow, allowAll); !errors.Is(err, want) && err != want {
			t.Errorf("offset %d: got %v, want %v", d, err, want)
		}
	}
}

// TestAuthPropertyMutations changes one field of a valid event at random, re-signs so the intended rule
// is reached, and asserts refusal.
func TestAuthPropertyMutations(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 4242))
	skew := int64(AuthMaxSkew / time.Second)
	hexdigits := "0123456789abcdef"
	randHex := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = hexdigits[rng.IntN(16)]
		}
		return string(b)
	}
	for i := 0; i < 3000; i++ {
		var want error
		var mod func(*Event)
		switch rng.IntN(8) {
		case 0: // relay differs
			v := authRelay + string(rune('a'+rng.IntN(26)))
			if rng.IntN(2) == 0 {
				v = "wss://" + randHex(1+rng.IntN(40))
			}
			mod, want = func(e *Event) { e.Tags[0][1] = v }, ErrAuthRelay
		case 1: // challenge differs, still well formed
			v := randHex(32 + rng.IntN(97))
			if v == authChallenge {
				continue
			}
			mod, want = func(e *Event) { e.Tags[1][1] = v }, ErrAuthChallenge
		case 2: // too far in time
			d := skew + 1 + rng.Int64N(1<<40)
			if rng.IntN(2) == 0 {
				d = -d
			}
			mod, want = func(e *Event) { e.CreatedAt = authNow.Unix() + d }, ErrAuthTime
		case 3: // other kind
			k := rng.IntN(65536)
			if k == KindAuth {
				continue
			}
			mod, want = func(e *Event) { e.Kind = k }, ErrAuthKind
		case 4: // content
			v := string(rune(0x20 + rng.IntN(0x5f)))
			mod, want = func(e *Event) { e.Content = v }, ErrAuthGrammar
		case 5: // extra tag
			name := string(rune('a' + rng.IntN(26)))
			mod, want = func(e *Event) { e.Tags = append(e.Tags, []string{name, randHex(4)}) }, ErrAuthGrammar
		case 6: // drop or reorder-with-duplicate a tag
			j := rng.IntN(2)
			mod, want = func(e *Event) { e.Tags = [][]string{e.Tags[j]} }, ErrAuthGrammar
		case 7: // malformed challenge or relay
			if rng.IntN(2) == 0 {
				v := randHex(rng.IntN(32))
				mod, want = func(e *Event) { e.Tags[1][1] = v }, ErrAuthGrammar
			} else {
				v := []string{"http://x", "wss://", "ws://a b", "wss://a\"b", "", "WSS://x"}[rng.IntN(6)]
				mod, want = func(e *Event) { e.Tags[0][1] = v }, ErrAuthGrammar
			}
		}
		e := mutateAuth(t, mod)
		if err := CheckAuth(e, authRelay, authChallenge, authNow, allowAll); !errors.Is(err, want) {
			t.Fatalf("iteration %d: got %v, want %v (event %+v)", i, err, want, e)
		}
	}
}

func FuzzCheckAuth(f *testing.F) {
	skew := int64(AuthMaxSkew / time.Second)
	long := func(n int) string { return "wss://" + strings.Repeat("a", n-6) }
	hx := func(n int) string { return strings.Repeat("a", n) }
	for _, ca := range []int64{0, skew, skew + 1, -skew, -skew - 1} {
		f.Add(authRelay, authChallenge, authNow.Unix()+ca, "", true, false, "", "")
	}
	f.Add(long(512), authChallenge, authNow.Unix(), "", true, false, "", "")
	f.Add(long(513), authChallenge, authNow.Unix(), "", true, false, "", "")
	for _, n := range []int{31, 32, 128, 129} {
		f.Add(authRelay, hx(n), authNow.Unix(), "", true, false, "", "")
	}
	f.Add(authRelay+" ", authChallenge, authNow.Unix(), "x", false, false, "", "")
	// Never signed: arbitrary id and sig, no Sign call.
	f.Add(authRelay, authChallenge, authNow.Unix(), "", true, true, hx(64), hx(128))
	f.Add(authRelay, authChallenge, authNow.Unix(), "", true, true, "", "")
	f.Add(authRelay, authChallenge, authNow.Unix(), "", true, true, hx(63), hx(129))
	f.Add(authRelay, authChallenge, authNow.Unix(), "", true, true, hx(65), hx(127))
	f.Add(authRelay, authChallenge, authNow.Unix(), "", true, true, "\xff", "\x00")
	f.Add("wss://\x00\xff", "\xff", int64(-1<<63), " ", true, false, "", "")
	k, err := ParseSecretKeyHex(testKeyA)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, relay, challenge string, createdAt int64, content string, resign, unsigned bool, id, sig string) {
		e := &Event{CreatedAt: createdAt, Kind: KindAuth, Tags: [][]string{{"relay", relay}, {"challenge", challenge}}, Content: content}
		if unsigned {
			e.PubKey, e.ID, e.Sig = k.PublicKeyHex(), id, sig
			if err := CheckAuth(e, authRelay, authChallenge, authNow, allowAll); err == nil {
				t.Fatalf("unsigned event accepted (id %q, sig %q)", id, sig)
			}
			return
		}
		if err := Sign(e, k); err != nil {
			return // not serialisable
		}
		if !resign {
			e.Sig = flipHex(e.Sig, 0)
		}
		err := CheckAuth(e, authRelay, authChallenge, authNow, allowAll)
		valid := resign && content == "" && relay == authRelay && challenge == authChallenge &&
			createdAt >= authNow.Unix()-skew && createdAt <= authNow.Unix()+skew
		if (err == nil) != valid {
			t.Fatalf("CheckAuth = %v, valid = %v", err, valid)
		}
		if err != nil && relay != "" && len(relay) > 8 && strings.Contains(err.Error(), relay) {
			t.Fatalf("error echoes relay: %v", err)
		}
		if CheckAuth(e, authRelay, authChallenge, authNow, nil) == nil {
			t.Fatal("nil allowed accepted")
		}
	})
}

// Cheap checks run before signature verification: a forged signature on an event that also fails an
// earlier rule reports the earlier rule, and a nil event is refused without a panic.
func TestCheckAuthOrder(t *testing.T) {
	e, _ := newAuthEvent(t)
	e.Sig = flipHex(e.Sig, 0)
	if err := CheckAuth(e, "wss://other.example", authChallenge, authNow, allowAll); !errors.Is(err, ErrAuthRelay) {
		t.Errorf("bad sig + wrong relay: got %v, want ErrAuthRelay", err)
	}
	if err := CheckAuth(e, authRelay, authChallenge, authNow, allowAll); !errors.Is(err, ErrBadSig) {
		t.Errorf("bad sig only: got %v, want ErrBadSig", err)
	}
	if err := CheckAuth(e, authRelay, authChallenge, authNow, nil); !errors.Is(err, ErrBadSig) {
		t.Errorf("bad sig + nil allowed: got %v, want ErrBadSig", err)
	}
	// Each case breaks two adjacent steps of kind, grammar, window, relay, challenge, signature, allowed,
	// and must report the earlier one.
	const wrongRelay, wrongChallenge = "wss://other.example", "fedcba9876543210fedcba9876543210"
	far := authNow.Unix() - int64(AuthMaxSkew/time.Second) - 1
	badSig := func(e *Event) { e.Sig = flipHex(e.Sig, 0) }
	for _, c := range []struct {
		name             string
		mod              func(*Event) // applied to a re-signed event, unless resign is false
		resign           bool
		relay, challenge string
		allowed          func(string) bool
		want             error
	}{
		{"kind+grammar", func(e *Event) { e.Kind = KindAuth + 1; e.Content = "x" }, true, authRelay, authChallenge, allowAll, ErrAuthKind},
		{"grammar+window", func(e *Event) { e.Content = "x"; e.CreatedAt = far }, true, authRelay, authChallenge, allowAll, ErrAuthGrammar},
		{"window+relay", func(e *Event) { e.CreatedAt = far }, true, wrongRelay, authChallenge, allowAll, ErrAuthTime},
		{"relay+challenge", func(*Event) {}, true, wrongRelay, wrongChallenge, allowAll, ErrAuthRelay},
		{"challenge+signature", badSig, false, authRelay, wrongChallenge, allowAll, ErrAuthChallenge},
		{"signature+allowed", badSig, false, authRelay, authChallenge, func(string) bool { return false }, ErrBadSig},
		{"allowed", func(*Event) {}, true, authRelay, authChallenge, func(string) bool { return false }, ErrAuthDenied},
	} {
		e := mutateAuth(t, func(e *Event) {
			if c.resign {
				c.mod(e)
			}
		})
		if !c.resign {
			c.mod(e)
		}
		if err := CheckAuth(e, c.relay, c.challenge, authNow, c.allowed); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if err := CheckAuth(nil, authRelay, authChallenge, authNow, allowAll); !errors.Is(err, errNilEvent) {
		t.Errorf("nil event: got %v", err)
	}
}
