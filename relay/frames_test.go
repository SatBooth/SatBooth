package relay

import (
	"reflect"
	"strings"
	"testing"
)

func TestFrameEventShape(t *testing.T) {
	e := testEvent(t, "a<b>&\"c\\", nil)
	e.PubKey = strings.Repeat("3", 64)
	got := string(FrameEvent("1:sub", e))
	want := `["EVENT","1:sub",{"id":"` + strings.Repeat("1", 64) + `","pubkey":"` + strings.Repeat("3", 64) +
		`","created_at":1700000000,"kind":23194,"tags":[],"content":"a<b>&\"c\\","sig":"` + strings.Repeat("2", 128) + `"}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Empty (non-nil) tags give [] too, and real tags keep their order.
	e.Tags = [][]string{}
	if !strings.Contains(string(FrameEvent("s", e)), `"tags":[],`) {
		t.Error("empty tags not []")
	}
	e.Tags = [][]string{{"p", "x"}, {"expiration", "5"}}
	if !strings.Contains(string(FrameEvent("s", e)), `"tags":[["p","x"],["expiration","5"]],`) {
		t.Error("tags encoding")
	}
	// The frame, re-labelled, parses back to the same event.
	e.Content = "plain"
	frame := FrameEvent("s", e)
	m, err := ParseClientMessage(append([]byte(`["EVENT",`), frame[len(`["EVENT","s",`):]...))
	if err != nil || !reflect.DeepEqual(m.Event, e) {
		t.Fatalf("round trip: %v %+v", err, m)
	}
	// go-nostr's fast path: sub id right after ["EVENT","; id is the first key.
	if !strings.HasPrefix(string(frame), `["EVENT","s",{"id":"`) {
		t.Errorf("prefix: %s", frame)
	}
}

func TestFrames(t *testing.T) {
	cases := []struct{ got, want string }{
		{string(FrameOK("ab", true, "")), `["OK","ab",true,""]`},
		{string(FrameOK("ab", false, "invalid: x")), `["OK","ab",false,"invalid: x"]`},
		{string(FrameEOSE("1:")), `["EOSE","1:"]`},
		{string(FrameClosed("1:", "blocked: too many subscriptions")), `["CLOSED","1:","blocked: too many subscriptions"]`},
		{string(FrameNotice("invalid: bad subscription id")), `["NOTICE","invalid: bad subscription id"]`},
		{string(FrameAuth("c0ffee")), `["AUTH","c0ffee"]`},
		{string(FrameNotice("a<b>&")), `["NOTICE","a<b>&"]`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %s want %s", c.got, c.want)
		}
	}
}
