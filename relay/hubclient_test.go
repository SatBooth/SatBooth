package relay

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nostr"
)

// hubClient behaves like the Alby Hub's go-nostr fork: one REQ per connection, the literal filter
// {"kinds":[23194],"#p":["<wallet>"]}, and on reconnect the same filter with "since":<unix seconds of the
// drop>. It waits up to 7 s for the OK after publishing.
type hubClient struct {
	t       *testing.T
	url     string
	wallet  string
	n       int
	c       *srvConn
	pending []*nostr.Event // EVENT frames that arrived while waiting for an OK
}

func newHubClient(t *testing.T, url, wallet string) *hubClient {
	return &hubClient{t: t, url: url, wallet: wallet}
}

// connect dials and subscribes; since is nil on the first connection.
func (h *hubClient) connect(since *int64) {
	h.t.Helper()
	h.c = srvDial(h.t, h.url)
	h.n++
	filter := `{"kinds":[23194],"#p":["` + h.wallet + `"]}`
	if since != nil {
		filter = fmt.Sprintf(`{"kinds":[23194],"since":%d,"#p":["%s"]}`, *since, h.wallet)
	}
	h.c.send([]byte(fmt.Sprintf(`["REQ","%d:",%s]`, h.n, filter)))
}

// drop closes the connection without a handshake.
func (h *hubClient) drop() {
	h.c.c.CloseNow()
}

// eose reads until the EOSE of the current subscription and returns the events before it.
func (h *hubClient) eose() []*nostr.Event {
	h.t.Helper()
	var evs []*nostr.Event
	for {
		f := h.c.recv(3 * time.Second)
		switch frameLabel(f) {
		case "EOSE":
			return evs
		case "EVENT":
			evs = append(evs, frameEvent(h.t, f))
		default:
			h.t.Fatalf("hub client: unexpected frame %s", f)
		}
	}
}

// next returns the next delivered request, or nil after d.
func (h *hubClient) next(d time.Duration) *nostr.Event {
	h.t.Helper()
	if len(h.pending) > 0 {
		e := h.pending[0]
		h.pending = h.pending[1:]
		return e
	}
	f, ok := h.c.tryRecv(d)
	if !ok {
		return nil
	}
	if frameLabel(f) != "EVENT" {
		h.t.Fatalf("hub client: unexpected frame %s", f)
	}
	return frameEvent(h.t, f)
}

// publish sends e and waits up to 7 s for the OK; it returns the OK's accepted flag and message.
func (h *hubClient) publish(e *nostr.Event) (bool, string) {
	h.t.Helper()
	h.c.send([]byte(`["EVENT",` + string(eventJSON(e)) + `]`))
	deadline := time.Now().Add(7 * time.Second)
	for {
		f, ok := h.c.tryRecv(time.Until(deadline))
		if !ok {
			h.t.Fatal("hub client: no OK within 7 s")
		}
		switch frameLabel(f) {
		case "OK":
			id, ok, msg := frameOK(h.t, f)
			if id != e.ID {
				h.t.Fatalf("hub client: OK for another id: %s", f)
			}
			return ok, msg
		case "EVENT":
			h.pending = append(h.pending, frameEvent(h.t, f))
		}
	}
}

func frameLabel(f string) string {
	var a []json.RawMessage
	if json.Unmarshal([]byte(f), &a) != nil || len(a) == 0 {
		return ""
	}
	var s string
	_ = json.Unmarshal(a[0], &s)
	return s
}

func frameEvent(t testing.TB, f string) *nostr.Event {
	t.Helper()
	var a []json.RawMessage
	if err := json.Unmarshal([]byte(f), &a); err != nil || len(a) != 3 {
		t.Fatalf("not an EVENT frame: %s", f)
	}
	e, err := nostr.ParseEvent(a[2])
	if err != nil {
		t.Fatalf("EVENT frame holds a bad event: %v", err)
	}
	return e
}

func frameOK(t testing.TB, f string) (id string, ok bool, msg string) {
	t.Helper()
	var a []any
	if err := json.Unmarshal([]byte(f), &a); err != nil || len(a) != 4 {
		t.Fatalf("not an OK frame: %s", f)
	}
	id, _ = a[1].(string)
	ok, _ = a[2].(bool)
	msg, _ = a[3].(string)
	return
}
