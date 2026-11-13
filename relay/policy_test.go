package relay

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

var (
	polNow  = time.Unix(1_800_000_000, 0)
	polLife = time.Hour
)

const polOutsider = 99

// Key numbers: pair i (0-based) has wallet 2i+1 and client 2i+2.
func polW(i int) int { return 2*i + 1 }
func polC(i int) int { return 2*i + 2 }

func polPolicy(t testing.TB, n int) *Policy {
	c := &Config{}
	for i := 0; i < n; i++ {
		c.Pairs = append(c.Pairs, Pair{Wallet: testKey(t, polW(i)), Client: testKey(t, polC(i))})
	}
	return NewPolicy(c)
}

func polPayload() string { return strings.Repeat("A", 132) }

// polTags returns grammar-valid tags for kind with the given p value ("" omits the p tag).
func polTags(kind int, p string) [][]string {
	var ts [][]string
	add := func(t ...string) { ts = append(ts, t) }
	if p != "" {
		add("p", p)
	}
	switch kind {
	case nostr.KindNWCInfo:
		add("encryption", "nip44_v2")
	case nostr.KindNWCRequest:
		add("encryption", "nip44_v2")
	case nostr.KindNWCResponse:
		add("e", strings.Repeat("b", 64))
	case nostr.KindNWCNotificationNIP04:
		return nil
	}
	return ts
}

func polContent(kind int) string {
	switch kind {
	case nostr.KindNWCInfo:
		return "pay_invoice get_balance"
	case nostr.KindNWCNotificationNIP04:
		return "x"
	case 5, 1:
		return ""
	}
	return polPayload()
}

// polSigned builds an event signed by test key n.
func polSigned(t testing.TB, n, kind int, created int64, tags [][]string, content string) *nostr.Event {
	t.Helper()
	sk, err := nostr.ParseSecretKeyHex(fmt.Sprintf("%064x", n))
	if err != nil {
		t.Fatal(err)
	}
	e := &nostr.Event{Kind: kind, CreatedAt: created, Tags: tags, Content: content}
	if err := nostr.Sign(e, sk); err != nil {
		t.Fatal(err)
	}
	return e
}

// polStd builds a valid event of kind by key n, p tag the key m (0 omits), created now.
func polStd(t testing.TB, n, kind, m int) *nostr.Event {
	p := ""
	if m != 0 {
		p = testKey(t, m)
	}
	return polSigned(t, n, kind, polNow.Unix(), polTags(kind, p), polContent(kind))
}

func polBadSig(e *nostr.Event) {
	if strings.HasSuffix(e.Sig, "0") {
		e.Sig = e.Sig[:len(e.Sig)-1] + "1"
	} else {
		e.Sig = e.Sig[:len(e.Sig)-1] + "0"
	}
}

func polEvent(p *Policy, l Listener, authed []string, e *nostr.Event) Verdict {
	return p.Event(l, authed, e, polNow, polLife)
}

func TestPolicyValidBaseline(t *testing.T) {
	p := polPolicy(t, 2)
	c1 := testKey(t, polC(0))
	for _, kind := range []int{13194, 23195, 23197} {
		if v := polEvent(p, HubListener, nil, polStd(t, polW(0), kind, polC(0))); !v.Accept || v.Msg != "" {
			t.Errorf("hub %d: %+v", kind, v)
		}
	}
	if v := polEvent(p, ClientListener, []string{c1}, polStd(t, polC(0), 23194, polW(0))); !v.Accept {
		t.Errorf("client 23194: %+v", v)
	}
}

func TestPolicyEventOrder(t *testing.T) {
	p := polPolicy(t, 2)
	c1 := testKey(t, polC(0))
	old := polNow.Add(-polLife).Unix()
	past := []string{"expiration", fmt.Sprint(polNow.Unix())}
	reqTags := func(pk int, extra ...[]string) [][]string {
		return append([][]string{{"p", testKey(t, pk)}, {"encryption", "nip44_v2"}}, extra...)
	}
	rows := []struct {
		name   string
		l      Listener
		authed []string
		e      *nostr.Event
		want   string
	}{
		{"1 vs 2: no AUTH beats a bad grammar", ClientListener, nil,
			polSigned(t, polC(0), 23194, old, [][]string{{"p", "zz"}}, "bad"), msgAuthRequired},
		{"1 vs 2: kind 5 from a wallet beats grammar", ClientListener, []string{c1},
			polSigned(t, polW(0), 5, old, [][]string{{"e", "bad"}}, "bad"), msgDeletion},
		{"1 vs 2: kind 5 on hub", HubListener, nil,
			polSigned(t, polW(0), 5, old, [][]string{{"e", "bad"}}, "bad"), msgDeletion},
		{"1 vs 2: kind off the listener beats grammar", HubListener, nil,
			polSigned(t, polW(0), 1, old, nil, ""), msgKind},
		{"2 vs 3: bad grammar beats an unpaired author", HubListener, nil,
			polSigned(t, polOutsider, 23195, old, nil, "bad"), msgGrammar},
		{"3 vs 4: unpaired author beats a stale created_at", HubListener, nil,
			polSigned(t, polOutsider, 23197, old, polTags(23197, testKey(t, polC(0))), polPayload()), msgNotPaired},
		{"3 vs 4: wrong p beats a stale created_at", HubListener, nil,
			polSigned(t, polW(0), 23197, old, polTags(23197, testKey(t, polC(1))), polPayload()), msgWrongPair},
		{"3 vs 4: unauthenticated author beats a stale created_at", ClientListener, []string{testKey(t, polC(1))},
			polSigned(t, polC(0), 23194, old, reqTags(polW(0)), polPayload()), msgNotAuthed},
		{"4 vs 5: stale created_at beats expiry", ClientListener, []string{c1},
			polSigned(t, polC(0), 23194, old, reqTags(polW(0), past), polPayload()), msgTooOld},
		{"4 vs 5: future created_at beats expiry", ClientListener, []string{c1},
			polSigned(t, polC(0), 23194, polNow.Add(nostr.AuthMaxSkew).Unix()+1, reqTags(polW(0), past), polPayload()), msgTooNew},
		{"5 vs 6: expiry beats a bad signature", ClientListener, []string{c1},
			polSigned(t, polC(0), 23194, polNow.Unix(), reqTags(polW(0), past), polPayload()), msgExpired},
	}
	for _, r := range rows {
		if r.want == msgExpired {
			polBadSig(r.e)
		}
		v := polEvent(p, r.l, r.authed, r.e)
		if v.Accept || v.Drop || v.Msg != r.want {
			t.Errorf("%s: got %+v want %q", r.name, v, r.want)
		}
	}
	// 6 alone: a bad signature on an otherwise valid event.
	e := polStd(t, polC(0), 23194, polW(0))
	polBadSig(e)
	if v := polEvent(p, ClientListener, []string{c1}, e); v.Accept || v.Msg != msgBadSig {
		t.Errorf("bad sig: %+v", v)
	}
}

func TestPolicyMessagesAreFixed(t *testing.T) {
	for _, m := range []string{msgAuthRequired, msgDeletion, msgKind, msgGrammar, msgNotPaired, msgNotAuthed,
		msgWrongPair, msgLegacy, msgTooOld, msgTooNew, msgExpired, msgBadSig, msgFilterScope} {
		ok := false
		for _, pre := range []string{"invalid:", "restricted:", "auth-required:", "blocked:"} {
			ok = ok || strings.HasPrefix(m, pre+" ")
		}
		if !ok {
			t.Errorf("message %q has no NIP-01 prefix", m)
		}
	}
}

func TestPolicyPairs(t *testing.T) {
	p := polPolicy(t, 2)
	keys := []int{polW(0), polW(1), polC(0), polC(1)}
	authed := []string{testKey(t, polC(0)), testKey(t, polC(1))}
	for _, l := range []Listener{HubListener, ClientListener} {
		for _, kind := range []int{13194, 23194, 23195, 23197} {
			for _, a := range keys {
				for _, pk := range keys {
					want := false
					for i := 0; i < 2; i++ {
						switch l {
						case HubListener:
							want = want || (kind != 23194 && a == polW(i) && pk == polC(i))
						case ClientListener:
							want = want || (kind == 23194 && a == polC(i) && pk == polW(i))
						}
					}
					v := polEvent(p, l, authed, polStd(t, a, kind, pk))
					if v.Accept != want {
						t.Errorf("listener %d kind %d author %d p %d: accept %v want %v (%q)", l, kind, a, pk, v.Accept, want, v.Msg)
					}
				}
			}
		}
	}
	// Subscribing across pairs: a connection authed as client 0 only.
	only := []string{testKey(t, polC(0))}
	k := func(n int) []string { return []string{testKey(t, n)} }
	cross := []Filter{
		{Kinds: []int{23195}, P: k(polC(1))},
		{Kinds: []int{23197}, P: k(polC(1))},
		{Kinds: []int{23195}, P: k(polC(0)), Authors: k(polW(1))},
		{Kinds: []int{13194}, Authors: k(polW(1))},
		{Kinds: []int{23195}, P: k(polW(0))},
		{Kinds: []int{23194}, P: k(polW(0))},
	}
	for i, f := range cross {
		if ok, msg := p.Filters(ClientListener, only, []Filter{f}); ok || msg == "" {
			t.Errorf("client cross filter %d accepted", i)
		}
	}
	for _, f := range []Filter{{Kinds: []int{23194}, P: k(polC(0))}, {Kinds: []int{13194}, Authors: k(polC(0))},
		{Kinds: []int{23195}, P: k(polC(0))}, {Kinds: []int{23194}, P: k(polOutsider)}} {
		if ok, _ := p.Filters(HubListener, nil, []Filter{f}); ok {
			t.Errorf("hub accepted %+v", f)
		}
	}
	// A mixed REQ is refused as a whole.
	if ok, _ := p.Filters(HubListener, nil, []Filter{{Kinds: []int{23194}, P: k(polW(0))}, {Kinds: []int{23194}, P: k(polC(0))}}); ok {
		t.Error("mixed REQ accepted")
	}
}

func TestPolicyHubFilters(t *testing.T) {
	p := polPolicy(t, 2)
	w0, w1 := testKey(t, polW(0)), testKey(t, polW(1))
	out := testKey(t, polOutsider)
	since := int64(1_700_000_000)
	lim := 5
	accept := []Filter{
		{Kinds: []int{23194}, P: []string{w0}},
		{Kinds: []int{23194}, P: []string{w0, w1}, Since: &since, Until: &since, Limit: &lim},
		{Kinds: []int{13194}, Authors: []string{w0, w1}},
	}
	for i, f := range accept {
		if ok, msg := p.Filters(HubListener, nil, []Filter{f}); !ok || msg != "" {
			t.Errorf("accept %d: %v %q", i, ok, msg)
		}
	}
	if ok, _ := p.Filters(HubListener, nil, accept); !ok {
		t.Error("all together refused")
	}
	if ok, _ := p.Filters(HubListener, nil, nil); !ok {
		t.Error("REQ without filters refused")
	}
	refuse := []Filter{
		{},
		{Kinds: []int{23194}},
		{Kinds: []int{23194}, P: []string{out}},
		{Kinds: []int{23194}, P: []string{w0, out}},
		{Kinds: []int{23194}, P: []string{w0}, Authors: []string{w0}},
		{Kinds: []int{23194}, P: []string{w0}, E: []string{hex64('a')}},
		{Kinds: []int{23194, 23195}, P: []string{w0}},
		{Kinds: []int{23195}, P: []string{w0}},
		{Kinds: []int{13194}},
		{Kinds: []int{13194}, Authors: []string{out}},
		{Kinds: []int{13194}, Authors: []string{w0}, P: []string{w0}},
		{Authors: []string{w0}},
		// Empty lists match nothing; the policy refuses them rather than serving nothing.
		{Kinds: []int{}, P: []string{w0}},
		{Kinds: []int{23194}, P: []string{}},
		{Kinds: []int{13194}, Authors: []string{}},
		{Kinds: []int{23194}, P: []string{w0}, Authors: []string{}},
		{Kinds: []int{23194}, P: []string{w0}, E: []string{}},
	}
	for i, f := range refuse {
		if ok, msg := p.Filters(HubListener, nil, []Filter{f}); ok || msg != msgFilterScope {
			t.Errorf("refuse %d: %v %q", i, ok, msg)
		}
	}
}

func TestPolicyClientFilters(t *testing.T) {
	p := polPolicy(t, 2)
	c0, c1 := testKey(t, polC(0)), testKey(t, polC(1))
	w0, w1 := testKey(t, polW(0)), testKey(t, polW(1))
	both := []string{c0, c1}
	if ok, msg := p.Filters(ClientListener, nil, []Filter{{Kinds: []int{23195}, P: []string{c0}}}); ok || msg != msgAuthRequired {
		t.Errorf("no AUTH: %v %q", ok, msg)
	}
	if ok, msg := p.Filters(ClientListener, nil, nil); ok || msg != msgAuthRequired {
		t.Errorf("no AUTH, no filters: %v %q", ok, msg)
	}
	for i, f := range []Filter{
		{Kinds: []int{23195}, P: []string{c0}},
		{Kinds: []int{23195, 23197}, P: []string{c0, c1}},
		{Kinds: []int{23195}, P: []string{c0}, Authors: []string{w0}, E: []string{hex64('a')}},
		{Kinds: []int{23197}, P: []string{c1}, Authors: []string{w1}},
		{Kinds: []int{23195, 23197}, P: []string{c0, c1}, Authors: []string{w0, w1}},
		{Kinds: []int{13194}, Authors: []string{w0, w1}},
	} {
		if ok, msg := p.Filters(ClientListener, both, []Filter{f}); !ok || msg != "" {
			t.Errorf("accept %d: %v %q", i, ok, msg)
		}
	}
	for i, f := range []Filter{
		{Kinds: []int{23195}},
		{Kinds: []int{23195}, P: []string{}},
		{Kinds: []int{23195}, P: []string{c0}, Authors: []string{}},
		{Kinds: []int{23197}, P: []string{c1}, Authors: []string{w0, w1}},
		{Kinds: []int{23195}, P: []string{c0}, E: []string{}},
		{Kinds: []int{23195, 23194}, P: []string{c0}},
		{Kinds: []int{23195, 13194}, P: []string{c0}},
		{Kinds: []int{23195}, P: []string{testKey(t, polOutsider)}},
		{Kinds: []int{13194}, Authors: []string{c0}},
		{Kinds: []int{13194}, Authors: []string{}},
		{Kinds: []int{13194}, Authors: []string{w0}, E: []string{hex64('a')}},
		{Kinds: []int{23194}, P: []string{w0}},
		{},
	} {
		if ok, msg := p.Filters(ClientListener, both, []Filter{f}); ok || msg != msgFilterScope {
			t.Errorf("refuse %d: %v %q", i, ok, msg)
		}
	}
	// An authed key that is not a configured client reads nothing.
	if ok, _ := p.Filters(ClientListener, []string{testKey(t, polOutsider)}, []Filter{{Kinds: []int{23195}, P: []string{testKey(t, polOutsider)}}}); ok {
		t.Error("unpaired authed key served")
	}
}

func TestClientAuthorsMustBePairedWallets(t *testing.T) {
	p := polPolicy(t, 2)
	c1, c2 := testKey(t, polC(0)), testKey(t, polC(1))
	w1, w2 := testKey(t, polW(0)), testKey(t, polW(1))
	cases := []struct {
		name   string
		authed []string
		f      Filter
		want   bool
	}{
		{"own wallet", []string{c1}, Filter{Kinds: []int{23195}, P: []string{c1}, Authors: []string{w1}}, true},
		{"other wallet", []string{c1}, Filter{Kinds: []int{23195}, P: []string{c1}, Authors: []string{w2}}, false},
		{"own and other wallet", []string{c1}, Filter{Kinds: []int{23195}, P: []string{c1}, Authors: []string{w1, w2}}, false},
		{"both authed, p is c1, other wallet", []string{c1, c2}, Filter{Kinds: []int{23195}, P: []string{c1}, Authors: []string{w2}}, false},
		{"both authed, p is c1, both wallets", []string{c1, c2}, Filter{Kinds: []int{23195}, P: []string{c1}, Authors: []string{w1, w2}}, false},
		{"both clients, both wallets", []string{c1, c2}, Filter{Kinds: []int{23195}, P: []string{c1, c2}, Authors: []string{w1, w2}}, true},
	}
	for _, tc := range cases {
		ok, msg := p.Filters(ClientListener, tc.authed, []Filter{tc.f})
		if ok != tc.want || (!ok && msg != msgFilterScope) || (ok && msg != "") {
			t.Errorf("%s: got %v %q, want %v", tc.name, ok, msg, tc.want)
		}
	}
}

func TestPolicyTime(t *testing.T) {
	p := polPolicy(t, 1)
	c0, w0 := testKey(t, polC(0)), testKey(t, polW(0))
	authed := []string{c0}
	mk := func(created int64, extra ...[]string) *nostr.Event {
		tags := append([][]string{{"p", w0}, {"encryption", "nip44_v2"}}, extra...)
		return polSigned(t, polC(0), 23194, created, tags, polPayload())
	}
	now := polNow.Unix()
	life := int64(polLife / time.Second)
	skew := int64(nostr.AuthMaxSkew / time.Second)
	exp := func(s int64) []string { return []string{"expiration", fmt.Sprint(s)} }
	rows := []struct {
		name string
		e    *nostr.Event
		want string // "" accepts
	}{
		{"now-lifetime", mk(now - life), msgTooOld},
		{"now-lifetime+1", mk(now - life + 1), ""},
		{"now+skew", mk(now + skew), ""},
		{"now+skew+1", mk(now + skew + 1), msgTooNew},
		{"expiration == now", mk(now, exp(now)), msgExpired},
		{"expiration == now-1", mk(now, exp(now-1)), msgExpired},
		{"expiration == now+1", mk(now, exp(now+1)), ""},
	}
	for _, r := range rows {
		v := polEvent(p, ClientListener, authed, r.e)
		if r.want == "" && !v.Accept || r.want != "" && (v.Accept || v.Msg != r.want) {
			t.Errorf("%s: %+v", r.name, v)
		}
	}
	// A sub-second clock: the bounds follow the real-valued time.
	frac := polNow.Add(500 * time.Millisecond)
	for _, c := range []struct {
		created int64
		want    bool
	}{{now - life, false}, {now - life + 1, true}, {now + skew, true}, {now + skew + 1, false}} {
		v := p.Event(ClientListener, authed, mk(c.created), frac, polLife)
		if v.Accept != c.want {
			t.Errorf("fractional now, created %d: %+v", c.created, v)
		}
	}
}

func TestPolicyLegacy(t *testing.T) {
	p := polPolicy(t, 2)
	// From a paired wallet: dropped, even with a stale time and a bad signature (dropped unread).
	e := polStd(t, polW(1), 23196, 0)
	if v := polEvent(p, HubListener, nil, e); !v.Drop || v.Accept || v.Msg != "" {
		t.Errorf("paired wallet: %+v", v)
	}
	stale := polSigned(t, polW(0), 23196, 1, nil, "x")
	polBadSig(stale)
	if v := polEvent(p, HubListener, nil, stale); !v.Drop {
		t.Errorf("stale legacy: %+v", v)
	}
	// From anyone else: restricted.
	for _, n := range []int{polOutsider, polC(0), polC(1)} {
		v := polEvent(p, HubListener, nil, polStd(t, n, 23196, 0))
		if v.Drop || v.Accept || !strings.HasPrefix(v.Msg, "restricted:") {
			t.Errorf("author %d: %+v", n, v)
		}
	}
	// Not accepted on the client listener, whoever sent it.
	v := polEvent(p, ClientListener, []string{testKey(t, polC(0))}, polStd(t, polW(0), 23196, 0))
	if v.Drop || v.Accept || !strings.HasPrefix(v.Msg, "restricted:") {
		t.Errorf("client listener: %+v", v)
	}
	// A malformed 23196 is invalid, not dropped.
	bad := polSigned(t, polW(0), 23196, polNow.Unix(), [][]string{{"p"}}, "x")
	if v := polEvent(p, HubListener, nil, bad); v.Drop || !strings.HasPrefix(v.Msg, "invalid:") {
		t.Errorf("malformed legacy: %+v", v)
	}
}

// --- property test ---------------------------------------------------------------------------------

func polKeyNums() []int { return []int{1, 2, 3, 4, 5, 6, polOutsider} } // 3 pairs and an outsider

func TestPolicyProperty(t *testing.T) {
	p := polPolicy(t, 3)
	rng := rand.New(rand.NewSource(20261102))
	nums := polKeyNums()
	isWallet := func(n int) (int, bool) { return (n - 1) / 2, n%2 == 1 && n <= 5 }
	isClient := func(n int) (int, bool) { return (n - 2) / 2, n%2 == 0 && n <= 6 }
	kinds := []int{13194, 23194, 23195, 23196, 23197, 5, 1}
	life := int64(polLife / time.Second)
	skew := int64(nostr.AuthMaxSkew / time.Second)
	now := polNow.Unix()
	times := []int64{now, now - life, now - life + 1, now + skew, now + skew + 1}
	accepted := 0
	for it := 0; it < 6000; it++ {
		l := Listener(rng.Intn(2))
		var authedN []int
		if l == ClientListener {
			for _, n := range nums {
				if rng.Intn(2) == 0 {
					authedN = append(authedN, n)
				}
			}
		}
		author := nums[rng.Intn(len(nums))]
		kind := kinds[rng.Intn(len(kinds))]
		pn := 0
		if rng.Intn(8) != 0 {
			pn = nums[rng.Intn(len(nums))]
			if rng.Intn(2) == 0 && author < polOutsider { // half the time, the author's partner
				pn = author + 1 - 2*((author+1)%2)
			}
		}
		created := times[rng.Intn(len(times))]
		expMode := rng.Intn(3) // 0 none, 1 past, 2 future
		badSig := rng.Intn(5) == 0

		pv := ""
		if pn != 0 {
			pv = testKey(t, pn)
		}
		tags := polTags(kind, pv)
		if kind == 23194 {
			switch expMode {
			case 1:
				tags = append(tags, []string{"expiration", fmt.Sprint(now - rng.Int63n(3))})
			case 2:
				tags = append(tags, []string{"expiration", fmt.Sprint(now + 1 + rng.Int63n(3))})
			}
		}
		if kind == 5 || kind == 1 {
			tags = nil
		}
		e := polSigned(t, author, kind, created, tags, polContent(kind))
		if badSig {
			polBadSig(e)
		}
		var authed []string
		for _, n := range authedN {
			authed = append(authed, testKey(t, n))
		}

		// Reference: a plain truth table over key numbers.
		wantAccept, wantDrop := false, false
		aw, authorIsWallet := isWallet(author)
		ac, authorIsClient := isClient(author)
		hasAuthor := func() bool {
			for _, n := range authedN {
				if n == author {
					return true
				}
			}
			return false
		}
		switch {
		case kind == 23196 && l == HubListener && authorIsWallet:
			wantDrop = true
		case pn == 0 && kind != 5 && kind != 1 && kind != 23196:
			// the grammar requires a p tag
		case l == HubListener && authorIsWallet && (kind == 13194 || kind == 23195 || kind == 23197):
			okP := pn != 0 && pn == polC(aw)
			wantAccept = okP
		case l == ClientListener && len(authedN) > 0 && kind == 23194 && authorIsClient && hasAuthor():
			wantAccept = pn != 0 && pn == polW(ac)
		}
		if wantAccept {
			timeOK := created > now-life && created <= now+skew
			expOK := kind != 23194 || expMode != 1
			wantAccept = timeOK && expOK && !badSig
		}

		v := polEvent(p, l, authed, e)
		if v.Accept != wantAccept || v.Drop != wantDrop {
			t.Fatalf("it %d: listener %d authed %v kind %d author %d p %d created %+d exp %d badSig %v: got %+v want accept %v drop %v",
				it, l, authedN, kind, author, pn, created-now, expMode, badSig, v, wantAccept, wantDrop)
		}
		if v.Accept {
			accepted++
			// Independent cross-pair check, straight from the event.
			pw, pc := -1, -1
			for i := 0; i < 3; i++ {
				if e.PubKey == testKey(t, polW(i)) {
					pw = i
				}
				if e.PubKey == testKey(t, polC(i)) {
					pc = i
				}
			}
			pt := pTags(e)
			if l == HubListener && (pw < 0 || len(pt) != 1 || pt[0] != testKey(t, polC(pw))) ||
				l == ClientListener && (pc < 0 || len(pt) != 1 || pt[0] != testKey(t, polW(pc))) {
				t.Fatalf("it %d: cross-pair event accepted", it)
			}
		}
		if !v.Accept && !v.Drop && v.Msg == "" {
			t.Fatalf("it %d: refusal without a message", it)
		}
	}
	if accepted < 100 {
		t.Errorf("only %d accepted events: the generator does not reach the accept branch", accepted)
	}
}

func TestPolicyFiltersProperty(t *testing.T) {
	p := polPolicy(t, 3)
	rng := rand.New(rand.NewSource(77))
	nums := polKeyNums()
	memo := map[int]string{}
	keyOf := func(n int) string {
		if _, ok := memo[n]; !ok {
			memo[n] = testKey(t, n)
		}
		return memo[n]
	}
	pickKeys := func() []string {
		switch rng.Intn(5) {
		case 0:
			return nil
		case 1:
			return []string{}
		}
		out := []string{}
		for i, n := 0, 1+rng.Intn(3); i < n; i++ {
			out = append(out, keyOf(nums[rng.Intn(len(nums))]))
		}
		return out
	}
	pickKinds := func() []int {
		switch rng.Intn(6) {
		case 0:
			return nil
		case 1:
			return []int{}
		}
		all := []int{13194, 23194, 23195, 23197, 1}
		out := []int{}
		for i, n := 0, 1+rng.Intn(2); i < n; i++ {
			out = append(out, all[rng.Intn(len(all))])
		}
		return out
	}
	walletNum := func(k string) (int, bool) {
		for i := 0; i < 3; i++ {
			if k == keyOf(polW(i)) {
				return i, true
			}
		}
		return 0, false
	}
	clientNum := func(k string) (int, bool) {
		for i := 0; i < 3; i++ {
			if k == keyOf(polC(i)) {
				return i, true
			}
		}
		return 0, false
	}
	accepted := 0
	for it := 0; it < 60000; it++ {
		l := Listener(rng.Intn(2))
		var authed []string
		if l == ClientListener {
			for _, n := range nums {
				if rng.Intn(2) == 0 {
					authed = append(authed, keyOf(n))
				}
			}
		}
		f := Filter{Kinds: pickKinds(), Authors: pickKeys(), P: pickKeys(), E: pickKeys()}
		if rng.Intn(2) == 0 {
			s := int64(rng.Intn(100))
			f.Since = &s
		}

		// Reference. Readable wallets: all of them on the hub, those of authed paired keys on the client.
		readable := map[string]bool{}
		authedPaired := map[string]bool{}
		for i := 0; i < 3; i++ {
			if l == HubListener {
				readable[keyOf(polW(i))] = true
			}
			for _, a := range authed {
				if a == keyOf(polC(i)) {
					readable[keyOf(polW(i))] = true
					authedPaired[a] = true
				}
			}
		}
		kindSet := map[int]bool{}
		for _, k := range f.Kinds {
			kindSet[k] = true
		}
		sub := func(vs []string, ok func(string) bool) bool {
			if len(vs) == 0 {
				return false
			}
			for _, v := range vs {
				if !ok(v) {
					return false
				}
			}
			return true
		}
		isReadableWallet := func(v string) bool { _, w := walletNum(v); return w && readable[v] }
		isAuthedClient := func(v string) bool { _, c := clientNum(v); return c && authedPaired[v] }
		pairedWithP := map[string]bool{} // wallets paired with the filter's p keys
		for _, v := range f.P {
			if i, c := clientNum(v); c {
				pairedWithP[keyOf(polW(i))] = true
			}
		}
		isPairedWithP := func(v string) bool { return pairedWithP[v] }
		want := false
		switch {
		case l == ClientListener && len(authed) == 0:
		case len(kindSet) == 1 && kindSet[13194]:
			want = sub(f.Authors, isReadableWallet) && f.P == nil && f.E == nil
		case l == HubListener && len(kindSet) == 1 && kindSet[23194]:
			want = f.Authors == nil && f.E == nil && sub(f.P, isReadableWallet)
		case l == ClientListener && len(kindSet) > 0 && !kindSet[13194] && !kindSet[23194] && !kindSet[1]:
			want = sub(f.P, isAuthedClient) && (f.Authors == nil || sub(f.Authors, isPairedWithP)) &&
				(f.E == nil || len(f.E) > 0)
		}
		got, msg := p.Filters(l, authed, []Filter{f})
		if got != want || (!got && msg == "") || (got && msg != "") {
			t.Fatalf("it %d: listener %d authed %d filter %+v: got %v %q want %v", it, l, len(authed), f, got, msg, want)
		}
		if got {
			accepted++
		}
	}
	if accepted < 100 {
		t.Errorf("only %d accepted filters", accepted)
	}
}
