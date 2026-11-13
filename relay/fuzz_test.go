package relay

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

func FuzzParseClientMessage(f *testing.F) {
	h := strings.Repeat("a", 64)
	for _, s := range []string{
		`["REQ","1:",{"kinds":[23194],"#p":["` + h + `"]}]`,
		`["REQ","1:",{"kinds":[23194],"since":1700000000,"#p":["` + h + `"]}]`,
		`["REQ","s",{},{"authors":[],"limit":0,"until":5,"#e":["` + h + `"]}]`,
		`["CLOSE","s"]`,
		`["EVENT",{"id":"` + strings.Repeat("1", 64) + `","pubkey":"` + h + `","created_at":1,"kind":23194,"tags":[["p","x"]],"content":"c","sig":"` + strings.Repeat("2", 128) + `"}]`,
		`["REQ","s",{"kinds":[23195],"#p":["` + h + `"],"authors":["` + h + `"]}]`,
		`["REQ","s",{"kinds":[23195],"#p":["` + h + `"],"authors":["` + strings.Repeat("b", 64) + `"]}]`,
		`["REQ","s",{"kinds":[23195],"#p":["` + h + `"],"authors":["` + h + `","` + strings.Repeat("b", 64) + `"]}]`,
		`["REQ","s",{"kinds":[23195,23197],"#p":["` + h + `","` + strings.Repeat("c", 64) + `"],"authors":["` + strings.Repeat("b", 64) + `","` + strings.Repeat("d", 64) + `"]}]`,
		`["REQ","s",{"ids":[]}]`, `["REQ","s",{"kinds":[1.0]}]`, `[]`, `{}`, ``, `["REQ"]`, `["CLOSE","a\u001fb"]`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		m, err := ParseClientMessage(in)
		if err != nil {
			return
		}
		if m.Label == "REQ" && (len(m.Filters) == 0 || len(m.Filters) > MaxFiltersPerReq || !validSubID(m.SubID)) {
			t.Fatalf("accepted bad REQ: %q", in)
		}
		for _, fl := range m.Filters {
			if len(fl.Kinds) > 64 || len(fl.Authors) > 64 || len(fl.P) > 64 || len(fl.E) > 64 {
				t.Fatalf("list over 64: %q", in)
			}
		}
		enc := encodeMessage(m)
		m2, err := ParseClientMessage(enc)
		if err != nil {
			t.Fatalf("re-encoding %q does not parse: %q: %v", in, enc, err)
		}
		if !reflect.DeepEqual(m, m2) {
			t.Fatalf("round trip differs: %q -> %q", in, enc)
		}
		if enc2 := encodeMessage(m2); string(enc2) != string(enc) {
			t.Fatalf("encoding not stable: %q vs %q", enc, enc2)
		}
	})
}

// fuzzPolicyFilterOK is an independent statement of the filter shapes a listener serves.
func fuzzPolicyFilterOK(p *Policy, l Listener, authed []string, fl Filter) bool {
	var wallets []string
	if l == HubListener {
		for _, pr := range p.c.Pairs {
			wallets = append(wallets, pr.Wallet)
		}
	} else {
		for _, a := range authed {
			if pr, ok := p.c.PairByClient(a); ok {
				wallets = append(wallets, pr.Wallet)
			}
		}
	}
	subset := func(vs, set []string) bool {
		for _, v := range vs {
			if !inStrings(set, v) {
				return false
			}
		}
		return true
	}
	kindsIn := func(allowed ...int) bool {
		if len(fl.Kinds) == 0 {
			return false
		}
		for _, k := range fl.Kinds {
			ok := false
			for _, a := range allowed {
				ok = ok || k == a
			}
			if !ok {
				return false
			}
		}
		return true
	}
	if kindsIn(nostr.KindNWCInfo) {
		return len(fl.Authors) > 0 && subset(fl.Authors, wallets)
	}
	if l == HubListener {
		return kindsIn(nostr.KindNWCRequest) && len(fl.Authors) == 0 && len(fl.P) > 0 && subset(fl.P, wallets)
	}
	if !kindsIn(nostr.KindNWCResponse, nostr.KindNWCNotification) || len(fl.P) == 0 || !subset(fl.P, authed) {
		return false
	}
	var paired []string // the wallets paired with this filter's p keys
	for _, k := range fl.P {
		pr, ok := p.c.PairByClient(k)
		if !ok {
			return false
		}
		paired = append(paired, pr.Wallet)
	}
	if fl.Authors != nil && (len(fl.Authors) == 0 || !subset(fl.Authors, paired)) {
		return false
	}
	return true
}

func FuzzPolicy(f *testing.F) {
	pol := polPolicy(f, 2)
	c0, c1 := testKey(f, polC(0)), testKey(f, polC(1))
	w0, w1 := testKey(f, polW(0)), testKey(f, polW(1))
	out := testKey(f, polOutsider)
	// The authed set is chosen by mask: bit 0 client 0, bit 1 client 1, bit 2 wallet 0, bit 3 an outsider.
	keys := []string{c0, c1, w0, out}
	q := func(vs ...string) string { b, _ := json.Marshal(vs); return string(b) }
	req := func(fl string) []byte { return []byte(`["REQ","s",` + fl + `]`) }
	// Seeds from TestPolicyClientFilters and TestClientAuthorsMustBePairedWallets.
	for _, fl := range []string{
		fmt.Sprintf(`{"kinds":[23195],"#p":%s}`, q(c0)),
		fmt.Sprintf(`{"kinds":[23195,23197],"#p":%s}`, q(c0, c1)),
		fmt.Sprintf(`{"kinds":[23195],"#p":%s,"authors":%s,"#e":%s}`, q(c0), q(w0), q(hex64('a'))),
		fmt.Sprintf(`{"kinds":[23197],"#p":%s,"authors":%s}`, q(c1), q(w1)),
		fmt.Sprintf(`{"kinds":[23195,23197],"#p":%s,"authors":%s}`, q(c0, c1), q(w0, w1)),
		fmt.Sprintf(`{"kinds":[13194],"authors":%s}`, q(w0, w1)),
		`{"kinds":[23195]}`,
		`{"kinds":[23195],"#p":[]}`,
		fmt.Sprintf(`{"kinds":[23195],"#p":%s,"authors":[]}`, q(c0)),
		fmt.Sprintf(`{"kinds":[23197],"#p":%s,"authors":%s}`, q(c1), q(w0, w1)),
		fmt.Sprintf(`{"kinds":[23195],"#p":%s,"#e":[]}`, q(c0)),
		fmt.Sprintf(`{"kinds":[23195,23194],"#p":%s}`, q(c0)),
		fmt.Sprintf(`{"kinds":[23195,13194],"#p":%s}`, q(c0)),
		fmt.Sprintf(`{"kinds":[23195],"#p":%s}`, q(out)),
		fmt.Sprintf(`{"kinds":[13194],"authors":%s}`, q(c0)),
		`{"kinds":[13194],"authors":[]}`,
		fmt.Sprintf(`{"kinds":[13194],"authors":%s,"#e":%s}`, q(w0), q(hex64('a'))),
		fmt.Sprintf(`{"kinds":[23194],"#p":%s}`, q(w0)),
		fmt.Sprintf(`{"kinds":[23194],"#p":%s,"authors":%s}`, q(w0), q(w0)),
		`{}`,
		fmt.Sprintf(`{"kinds":[23195],"#p":%s,"authors":%s}`, q(c0), q(w1)),
		fmt.Sprintf(`{"kinds":[23195],"#p":%s,"authors":%s}`, q(c0), q(w0, w1)),
	} {
		for _, l := range []uint8{0, 1} {
			for _, mask := range []uint8{0, 1, 3, 8, 15} {
				f.Add(req(fl), l, mask, uint8(0), uint8(0), uint8(0), int64(0), "")
			}
		}
	}
	// Seeds for the event half: author index, kind index, p index.
	for a := uint8(0); a < 5; a++ {
		for k := uint8(0); k < 8; k++ {
			f.Add([]byte(`["CLOSE","s"]`), uint8(k&1), uint8(15), a, k, a+1, int64(k)*100, "")
		}
	}
	f.Add([]byte(`["REQ","s",{"kinds":[23195],"#p":["x"]}]`), uint8(1), uint8(3), uint8(1), uint8(1), uint8(1), int64(-7200), "x\"\\y")

	nums := []int{polW(0), polC(0), polW(1), polC(1), polOutsider}
	kinds := []int{13194, 23194, 23195, 23196, 23197, 5, 1, 0}
	f.Fuzz(func(t *testing.T, in []byte, lb, mask, ai, ki, pi uint8, off int64, extra string) {
		l := Listener(lb & 1)
		var authed []string
		if l == ClientListener {
			for i, k := range keys {
				if mask&(1<<i) != 0 {
					authed = append(authed, k)
				}
			}
		}
		now := polNow
		// One client message: a REQ's filters go through Filters.
		if m, err := ParseClientMessage(in); err == nil {
			switch m.Label {
			case "REQ":
				if ok, msg := pol.Filters(l, authed, m.Filters); ok {
					if msg != "" {
						t.Fatalf("accepted REQ with a message %q", msg)
					}
					for _, fl := range m.Filters {
						if !fuzzPolicyFilterOK(pol, l, authed, fl) {
							t.Fatalf("accepted a filter outside the listener's scope: %+v authed=%q listener=%d", fl, authed, l)
						}
					}
				} else if msg == "" {
					t.Fatal("refused REQ without a reason")
				}
			case "EVENT":
				checkPolicyEvent(t, pol, l, authed, m.Event, now)
			}
		}
		// The raw bytes as an event, and a signed event built from the other inputs.
		if e, err := nostr.ParseEvent(in); err == nil {
			checkPolicyEvent(t, pol, l, authed, e, now)
		}
		author := nums[int(ai)%len(nums)]
		kind := kinds[int(ki)%len(kinds)]
		p := ""
		if pi%6 != 0 {
			p = testKey(t, nums[int(pi)%len(nums)])
		}
		tags := polTags(kind, p)
		if extra != "" {
			tags = append(tags, []string{"x", extra})
		}
		e := &nostr.Event{Kind: kind, CreatedAt: now.Unix() + off%7200 - 3600, Tags: tags, Content: polContent(kind)}
		sk, err := nostr.ParseSecretKeyHex(fmt.Sprintf("%064x", author))
		if err != nil {
			t.Fatal(err)
		}
		if nostr.Sign(e, sk) == nil {
			checkPolicyEvent(t, pol, l, authed, e, now)
		}
	})
}

// checkPolicyEvent runs e through the policy and asserts the invariants of an accepted EVENT.
func checkPolicyEvent(t *testing.T, pol *Policy, l Listener, authed []string, e *nostr.Event, now time.Time) {
	t.Helper()
	v := pol.Event(l, authed, e, now, polLife)
	if v.Accept && (v.Drop || v.Msg != "") {
		t.Fatalf("inconsistent verdict %+v", v)
	}
	if !v.Accept && !v.Drop && v.Msg == "" {
		t.Fatal("refused EVENT without a reason")
	}
	if !v.Accept {
		return
	}
	if err := nostr.ValidateNWC(e); err != nil {
		t.Fatalf("accepted an event outside the grammar: %v", err)
	}
	if err := nostr.Verify(e); err != nil {
		t.Fatalf("accepted an event that does not verify: %v", err)
	}
	_, isWallet := pol.c.PairByWallet(e.PubKey)
	_, isClient := pol.c.PairByClient(e.PubKey)
	if !isWallet && !isClient {
		t.Fatal("accepted an event from an unpaired key")
	}
	if l == HubListener && !isWallet || l == ClientListener && (!isClient || e.Kind != nostr.KindNWCRequest || !inStrings(authed, e.PubKey)) {
		t.Fatalf("accepted an event from the wrong side of listener %d: kind %d", l, e.Kind)
	}
}
