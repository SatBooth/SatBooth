package webhook

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
)

var alertSecret = bytes.Repeat([]byte{0xa5}, 32)

func (f *fixture) alertDeliverer(url string) *Deliverer {
	f.t.Helper()
	d, err := NewAlertDeliverer(url, alertSecret, f.st, f.clk.now, nil, slog.New(slog.NewTextHandler(f.log, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) alert(a store.Alert) string {
	f.t.Helper()
	if queued, err := f.st.AddAlert(context.Background(), a); err != nil || !queued {
		f.t.Fatalf("AddAlert: %v %v", queued, err)
	}
	return store.AlertID(a.Message, a.Fields, f.clk.now())
}

// An alert is posted signed with the alerts secret, never the merchant's, and its body is the alert payload byte for
// byte. The merchant deliverer on the same store sends no alert, and the alert deliverer no event.
func TestAlertDelivery(t *testing.T) {
	f := newFixture(t)
	fields := `{"code":-28,"order":"0123456789abcdef0123456789abcdef"}`
	a := store.Alert{Message: "ALERT: an order was paid on both rails", Fields: fields, Dropped: 1}
	id := f.alert(a)
	f.alert(a) // a repeat in the same hour
	ev := f.event("order.paid")
	alerts, merchant := newSink(t, f.clk, 204), newSink(t, f.clk, 204)
	f.clk.advance(5 * time.Second)
	pass(t, f.alertDeliverer(alerts.srv.URL))
	pass(t, f.deliverer(merchant.srv.URL, nil))
	got := alerts.requests()
	if len(got) != 1 {
		t.Fatalf("%d alert requests", len(got))
	}
	want := `{"id":"` + id + `","type":"alert","message":"ALERT: an order was paid on both rails","fields":` + fields +
		`,"dropped":1,"repeats":2,"created_at":1800000000}`
	if string(got[0].body) != want {
		t.Fatalf("body\n got %s\nwant %s", got[0].body, want)
	}
	if got[0].ctype != "application/json" {
		t.Fatalf("content type %q", got[0].ctype)
	}
	if err := Verify(alertSecret, got[0].header, got[0].body, f.clk.now(), RecommendedWindow); err != nil {
		t.Fatalf("alerts secret: %v", err)
	}
	if err := Verify(testSecret, got[0].header, got[0].body, f.clk.now(), RecommendedWindow); err != ErrSignature {
		t.Fatalf("merchant secret verified an alert: %v", err)
	}
	m := merchant.requests()
	if len(m) != 1 || !strings.Contains(string(m[0].body), `"id":"`+ev.ID+`"`) || strings.Contains(string(m[0].body), "alert") {
		t.Fatalf("merchant requests %+v", m)
	}
	// Delivered: not sent again, even when the alert repeats (no hit is recorded once delivered).
	f.alert(a)
	pass(t, f.alertDeliverer(alerts.srv.URL))
	if n := len(alerts.requests()); n != 1 {
		t.Fatalf("%d alert requests after a repeat", n)
	}
}

// The alert deliverer keeps the retry policy and gives up after 24 hours with its own line, which carries no ALERT:
// prefix, once across restarts.
func TestAlertGivesUpWithoutPrefix(t *testing.T) {
	f := newFixture(t)
	f.alert(store.Alert{Message: "ALERT: x", Fields: "{}"})
	s := newSink(t, f.clk, http.StatusInternalServerError)
	for range 40 {
		pass(t, f.alertDeliverer(s.srv.URL))
		f.clk.advance(time.Hour)
	}
	if n := f.log.count(AlertGiveUpMessage); n != 1 {
		t.Fatalf("%d give-up lines:\n%s", n, f.log)
	}
	if strings.Contains(f.log.String(), "ALERT") {
		t.Fatalf("the alert deliverer logged ALERT:\n%s", f.log)
	}
	if n := len(s.requests()); n < 20 || n > 40 {
		t.Fatalf("%d attempts", n)
	}
	if !strings.Contains(f.log.String(), "alert delivery failed, will retry") {
		t.Fatalf("no retry line:\n%s", f.log)
	}
}

func TestNewAlertDelivererValidation(t *testing.T) {
	f := newFixture(t)
	if _, err := NewAlertDeliverer("https://192.0.2.1/", alertSecret, nil, f.clk.now, nil, nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewAlertDeliverer("ftp://192.0.2.1/", alertSecret, f.st, f.clk.now, nil, nil); err == nil {
		t.Fatal("ftp accepted")
	}
	if _, err := NewAlertDeliverer("https://192.0.2.1/", alertSecret[:31], f.st, f.clk.now, nil, nil); err == nil {
		t.Fatal("short secret accepted")
	}
}

// The payload is not HTML-escaped: a message with <, > and & is sent as stored.
func TestAlertPayloadNotHTMLEscaped(t *testing.T) {
	f := newFixture(t)
	id := f.alert(store.Alert{Message: "ALERT: a < b & c > d", Fields: "{}"})
	s := newSink(t, f.clk, 204)
	pass(t, f.alertDeliverer(s.srv.URL))
	got := s.requests()
	want := `{"id":"` + id + `","type":"alert","message":"ALERT: a < b & c > d","fields":{},"dropped":0,"repeats":1,"created_at":1800000000}`
	if len(got) != 1 || string(got[0].body) != want {
		t.Fatalf("body %+v", got)
	}
}
