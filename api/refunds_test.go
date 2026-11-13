package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/refund"
)

const (
	rwKey   = "test-refunds-write-key"
	rrKey   = "test-refunds-read-key"
	refID   = "0123456789abcdef0123456789abcdef"
	ordID   = "fedcba9876543210fedcba9876543210"
	secret1 = "lnbc1secretinvoicetext"
	secret2 = "secretapprovalhex"
)

// handler is the API over the environment's store, with its clock, the given refunder and figures.
func (e *env) handler(rf Refunder, fig Figures) http.Handler {
	h := newHandler(e.keys, e.st, e.res, base, e.pr, e.clk.Now).(*handler)
	h.rf, h.fig = rf, fig
	return h
}

// fakeRefunder records its calls and returns what the test sets.
type fakeRefunder struct {
	mu    sync.Mutex
	view  refund.View
	found bool
	err   error
	calls []string
}

func (f *fakeRefunder) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeRefunder) Claim(_ context.Context, approval, invoice string) (refund.View, error) {
	f.rec("claim " + approval + " " + invoice)
	return f.view, f.err
}
func (f *fakeRefunder) Get(_ context.Context, id string) (refund.View, bool, error) {
	f.rec("get " + id)
	return f.view, f.found, f.err
}
func (f *fakeRefunder) Void(_ context.Context, id, approval string) (refund.View, error) {
	f.rec("void " + id + " " + approval)
	return f.view, f.err
}
func (f *fakeRefunder) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// refundEnv is the environment with two refund keys and a fake refunder (nil when withRefunder is false).
func refundEnv(t *testing.T, withRefunder bool) (*env, *fakeRefunder) {
	t.Helper()
	e := newEnv(t)
	keys, err := LoadKeys(writeKeyFile(t, 0o600, keysBody(keyEntry("w", CapWrite, writeKey), keyEntry("r", CapRead, readKey),
		keyEntry("rw", CapRefundsWrite, rwKey), keyEntry("rr", CapRefundsRead, rrKey))))
	if err != nil {
		t.Fatal(err)
	}
	e.keys = keys
	f := &fakeRefunder{found: true, view: refund.View{ID: refID, OrderID: ordID, State: refund.StateClaimed, AmountMsat: 21000,
		ClaimedAt: epoch}}
	var rf Refunder
	if withRefunder {
		rf = f
	}
	e.h = e.handler(rf, nil)
	return e, f
}

const (
	claimBody = `{"approval":"` + secret2 + `","invoice":"` + secret1 + `"}`
	voidBody  = `{"approval":"` + secret2 + `"}`
)

var (
	pathClaim = "/v1/refunds"
	pathGet   = "/v1/refunds/" + refID
	pathVoid  = "/v1/refunds/" + refID + "/void"
)

func TestRefundRoutesByCapability(t *testing.T) {
	routes := []struct {
		name, method, path, body, need string
		ok                             int
	}{
		{"claim", "POST", pathClaim, claimBody, rwKey, 202},
		{"get", "GET", pathGet, "", rrKey, 200},
		{"void", "POST", pathVoid, voidBody, rwKey, 200},
	}
	keys := map[string]string{"orders:write": writeKey, "orders:read": readKey, "refunds:write": rwKey, "refunds:read": rrKey}
	for _, rt := range routes {
		for capName, key := range keys {
			e, f := refundEnv(t, true)
			code, body, _ := e.do(rt.method, rt.path, key, rt.body)
			want := 403
			if key == rt.need {
				want = rt.ok
			}
			if code != want {
				t.Errorf("%s with %s = %d %s, want %d", rt.name, capName, code, body, want)
			}
			if want == 403 {
				if string(body) != `{"error":"forbidden"}`+"\n" || len(f.callList()) != 0 {
					t.Errorf("%s with %s: body %q calls %v", rt.name, capName, body, f.callList())
				}
			}
		}
		e, f := refundEnv(t, true)
		for key, want := range map[string]int{"": 401, "wrong": 401} {
			if code, _, _ := e.do(rt.method, rt.path, key, rt.body); code != want {
				t.Errorf("%s with key %q = %d, want %d", rt.name, key, code, want)
			}
		}
		if len(f.callList()) != 0 {
			t.Errorf("%s: refunder called without a key", rt.name)
		}
	}
}

func TestRefundRoutesNotFound(t *testing.T) {
	upper := strings.ToUpper(refID)
	cases := []struct{ method, path string }{
		{"GET", "/v1/refunds"}, {"PUT", "/v1/refunds"}, {"DELETE", "/v1/refunds"}, {"POST", "/v1/refunds/"},
		{"GET", "/v1/refunds/"}, {"POST", pathGet}, {"PUT", pathGet}, {"DELETE", pathGet},
		{"GET", pathVoid}, {"PUT", pathVoid}, {"POST", pathVoid + "/"}, {"POST", pathVoid + "/x"}, {"GET", pathGet + "/"},
		{"GET", "/v1/refunds/" + upper}, {"POST", "/v1/refunds/" + upper + "/void"},
		{"GET", "/v1/refunds/" + refID[:31]}, {"GET", "/v1/refunds/" + refID + "0"}, {"GET", "/v1/refunds/zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"GET", "/v1/refunds/void"}, {"POST", "/v1/refunds//void"}, {"GET", "/v1/refund"}, {"GET", "/v1/refundsx"},
		{"GET", "/v1/refunds/" + refID + "/void"}, {"POST", "/v1/refunds/" + refID + "/voidx"},
	}
	e, f := refundEnv(t, true)
	for _, c := range cases {
		for _, key := range []string{"", rwKey, rrKey} {
			code, body, _ := e.do(c.method, c.path, key, claimBody)
			if code != 404 || string(body) != `{"error":"not_found"}`+"\n" {
				t.Errorf("%s %s key %q = %d %s", c.method, c.path, key, code, body)
			}
		}
	}
	if len(f.callList()) != 0 {
		t.Errorf("refunder called: %v", f.callList())
	}
}

func TestRefundDisabled(t *testing.T) {
	e, _ := refundEnv(t, false)
	for _, rt := range []struct{ method, path, body, key string }{
		{"POST", pathClaim, claimBody, rwKey}, {"GET", pathGet, "", rrKey}, {"POST", pathVoid, voidBody, rwKey},
		// Not even a bad body is looked at first.
		{"POST", pathClaim, `{`, rwKey}, {"POST", pathVoid, ``, rwKey},
	} {
		code, body, _ := e.do(rt.method, rt.path, rt.key, rt.body)
		if code != 404 || string(body) != `{"error":"refunds_disabled"}`+"\n" {
			t.Errorf("%s %s = %d %s", rt.method, rt.path, code, body)
		}
	}
	// The bearer and capability checks still come first.
	if code, _, _ := e.do("POST", pathClaim, "", claimBody); code != 401 {
		t.Errorf("no key = %d", code)
	}
	if code, _, _ := e.do("POST", pathClaim, rrKey, claimBody); code != 403 {
		t.Errorf("wrong capability = %d", code)
	}
	if code, _, _ := e.do("POST", pathClaim, writeKey, claimBody); code != 403 {
		t.Errorf("orders key = %d", code)
	}
}

func TestRefundBodies(t *testing.T) {
	big := `{"approval":"` + strings.Repeat("a", MaxBodyBytes) + `","invoice":"x"}`
	claim := map[string]string{
		"empty": ``, "not json": `nope`, "array": `[]`, "null": `null`, "string": `"x"`, "truncated": `{"approval":"a","invoice":"b"`,
		"trailing": `{"approval":"a","invoice":"b"} {}`, "missing invoice": `{"approval":"a"}`, "missing approval": `{"invoice":"b"}`,
		"empty object": `{}`, "unknown key": `{"approval":"a","invoice":"b","extra":"c"}`,
		"case variant": `{"Approval":"a","invoice":"b"}`, "case variant 2": `{"approval":"a","invoice":"b","INVOICE":"c"}`,
		"duplicate": `{"approval":"a","approval":"a","invoice":"b"}`, "duplicate invoice": `{"approval":"a","invoice":"b","invoice":"b"}`,
		"null approval": `{"approval":null,"invoice":"b"}`, "null invoice": `{"approval":"a","invoice":null}`,
		"number approval": `{"approval":1,"invoice":"b"}`, "number invoice": `{"approval":"a","invoice":2}`,
		"bool": `{"approval":true,"invoice":"b"}`, "object": `{"approval":{},"invoice":"b"}`, "array value": `{"approval":["a"],"invoice":"b"}`,
	}
	void := map[string]string{
		"empty": ``, "array": `[]`, "null": `null`, "missing": `{}`, "unknown key": `{"approval":"a","invoice":"b"}`,
		"duplicate": `{"approval":"a","approval":"b"}`, "null approval": `{"approval":null}`, "number": `{"approval":1}`,
		"bool": `{"approval":false}`, "case variant": `{"APPROVAL":"a"}`, "claim body": claimBody,
	}
	e, f := refundEnv(t, true)
	check := func(path, name, body string, want int, wantCode string) {
		t.Helper()
		code, b, _ := e.do("POST", path, rwKey, body)
		if code != want || string(b) != `{"error":"`+wantCode+`"}`+"\n" {
			t.Errorf("%s %s = %d %s", path, name, code, b)
		}
	}
	for name, body := range claim {
		check(pathClaim, name, body, 400, "invalid_json")
	}
	for name, body := range void {
		check(pathVoid, name, body, 400, "invalid_json")
	}
	check(pathClaim, "too large", big, 400, "body_too_large")
	check(pathVoid, "too large", big, 400, "body_too_large")
	if len(f.callList()) != 0 {
		t.Errorf("refunder called for a bad body: %v", f.callList())
	}
	// The exact bodies reach the refunder with the values and the URL's id.
	e.do("POST", pathClaim, rwKey, claimBody)
	e.do("POST", pathVoid, rwKey, voidBody)
	e.do("GET", pathGet, rrKey, "")
	want := []string{"claim " + secret2 + " " + secret1, "void " + refID + " " + secret2, "get " + refID}
	if got := f.callList(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	// Whitespace around a well-formed body is fine.
	if code, _, _ := e.do("POST", pathVoid, rwKey, " \n"+voidBody+"\n "); code != 200 {
		t.Errorf("padded body = %d", code)
	}
}

// TestRefundVoidClaimed: a void of a claimed refund answers 200, failed with the reason cancelled, and the response
// still has only the allowlisted fields.
func TestRefundVoidClaimed(t *testing.T) {
	e, f := refundEnv(t, true)
	f.view = refund.View{ID: refID, OrderID: ordID, State: refund.StateFailed, Reason: "cancelled", AmountMsat: 21000,
		ClaimedAt: epoch, ResolvedAt: epoch.Add(90 * time.Second)}
	code, body, _ := e.do("POST", pathVoid, rwKey, voidBody)
	if code != 200 {
		t.Fatalf("void = %d %s", code, body)
	}
	m := asMap(t, body)
	want := sorted([]string{"amount_msat", "claimed_at", "id", "order_id", "state", "reason", "resolved_at"})
	if got := keysOf(m); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	if m["state"] != "failed" || m["reason"] != "cancelled" {
		t.Errorf("state %v reason %v", m["state"], m["reason"])
	}
	if got, want := f.callList(), []string{"void " + refID + " " + secret2}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestRefundErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
		name string
	}{
		{refund.ErrApproval, 400, "invalid_approval"}, {refund.ErrApprovalExpired, 400, "approval_expired"},
		{refund.ErrRefundExists, 409, "refund_exists"}, {refund.ErrUnknownOrder, 404, "unknown_order"},
		{refund.ErrInvoice, 400, "invalid_invoice"}, {refund.ErrOwnInvoice, 400, "own_invoice"},
		{refund.ErrExceedsRefundable, 409, "exceeds_refundable"}, {refund.ErrDailyCap, 429, "daily_cap"},
		{refund.ErrTooManyOpen, 429, "too_many_open"}, {refund.ErrInFlight, 409, "refund_in_flight"},
		{refund.ErrHubUnavailable, 503, "hub_unavailable"}, {refund.ErrState, 409, "invalid_state"},
		{refund.ErrNotFound, 404, "not_found"},
		{fmt.Errorf("wrapped: %w", refund.ErrDailyCap), 429, "daily_cap"},
		{errors.New("secret detail " + secret1), 500, "internal"},
	}
	for _, c := range cases {
		e, f := refundEnv(t, true)
		f.err = c.err
		want := `{"error":"` + c.name + `"}` + "\n"
		for _, rt := range []struct{ method, path, body, key string }{
			{"POST", pathClaim, claimBody, rwKey}, {"POST", pathVoid, voidBody, rwKey},
		} {
			code, body, _ := e.do(rt.method, rt.path, rt.key, rt.body)
			if code != c.code || string(body) != want {
				t.Errorf("%v on %s = %d %s, want %d %s", c.err, rt.path, code, body, c.code, want)
			}
		}
		// Get: an error is internal whatever it is; a missing refund is not_found.
		code, body, _ := e.do("GET", pathGet, rrKey, "")
		if code != 500 || string(body) != `{"error":"internal"}`+"\n" {
			t.Errorf("get with %v = %d %s", c.err, code, body)
		}
	}
	e, f := refundEnv(t, true)
	f.found = false
	if code, body, _ := e.do("GET", pathGet, rrKey, ""); code != 404 || string(body) != `{"error":"not_found"}`+"\n" {
		t.Errorf("get missing = %d %s", code, body)
	}
}

func TestRefundViewAllowlist(t *testing.T) {
	t0 := epoch
	t1 := epoch.Add(90 * time.Second)
	base := refund.View{ID: refID, OrderID: ordID, AmountMsat: 21000, ClaimedAt: t0}
	mk := func(state, reason string, resolved time.Time) refund.View {
		v := base
		v.State, v.Reason, v.ResolvedAt = state, reason, resolved
		return v
	}
	common := []string{"amount_msat", "claimed_at", "id", "order_id", "state"}
	with := func(extra ...string) []string { return append(append([]string(nil), common...), extra...) }
	cases := []struct {
		name string
		v    refund.View
		keys []string
	}{
		{"claimed", mk(refund.StateClaimed, "", time.Time{}), with()},
		{"sent", mk(refund.StateSent, "", time.Time{}), with()},
		{"settled", mk(refund.StateSettled, "", t1), with("resolved_at")},
		{"failed", mk(refund.StateFailed, "voided", t1), with("reason", "resolved_at")},
	}
	for _, c := range cases {
		e, f := refundEnv(t, true)
		f.view = c.v
		for _, rt := range []struct {
			method, path, body, key string
			code                    int
		}{
			{"POST", pathClaim, claimBody, rwKey, 202}, {"GET", pathGet, "", rrKey, 200}, {"POST", pathVoid, voidBody, rwKey, 200},
		} {
			code, body, hdr := e.do(rt.method, rt.path, rt.key, rt.body)
			if code != rt.code {
				t.Fatalf("%s %s = %d %s", c.name, rt.path, code, body)
			}
			m := asMap(t, body)
			want := append([]string(nil), c.keys...)
			if got := keysOf(m); !reflect.DeepEqual(got, sorted(want)) {
				t.Errorf("%s %s keys = %v, want %v", c.name, rt.path, got, sorted(want))
			}
			if m["id"] != refID || m["order_id"] != ordID || m["amount_msat"] != float64(21000) || m["state"] != c.v.State ||
				m["claimed_at"] != t0.Format(time.RFC3339) {
				t.Errorf("%s fields: %v", c.name, m)
			}
			if c.v.Reason != "" && m["reason"] != c.v.Reason {
				t.Errorf("reason = %v", m["reason"])
			}
			if !c.v.ResolvedAt.IsZero() && m["resolved_at"] != t1.Format(time.RFC3339) {
				t.Errorf("resolved_at = %v", m["resolved_at"])
			}
			if strings.Contains(string(body), secret1) || strings.Contains(string(body), secret2) {
				t.Errorf("body echoes the request: %s", body)
			}
			if hdr.Get("Content-Type") != "application/json" || hdr.Get("Cache-Control") != "no-store" {
				t.Errorf("headers: %v", hdr)
			}
		}
	}
}

func sorted(s []string) []string { return slices.Sorted(slices.Values(s)) }

func TestOrderViewLightningFigures(t *testing.T) {
	for _, withRefunder := range []bool{true, false} {
		e, _ := refundEnv(t, withRefunder)
		var rf Refunder
		if withRefunder {
			rf = &fakeRefunder{}
		}
		// Without Figures the three members are absent (the state stays).
		e.h = e.handler(rf, nil)
		code, body := e.post(orderBody)
		if code != 201 {
			t.Fatalf("create = %d %s", code, body)
		}
		id := asMap(t, body)["id"].(string)
		ln := asMap(t, body)["lightning"].(map[string]any)
		if !reflect.DeepEqual(keysOf(ln), []string{"state"}) {
			t.Errorf("no figures: lightning = %v", ln)
		}

		var gotID string
		fail := error(nil)
		fig := func(_ context.Context, orderID string) (int64, int64, int64, error) {
			gotID = orderID
			return 21000, 5000, 3000, fail
		}
		e.h = e.handler(rf, fig)
		for _, key := range []string{readKey, writeKey} {
			path, method, b := "/v1/orders/"+id, "GET", ""
			if key == writeKey {
				path, method, b = "/v1/orders", "POST", orderBody
			}
			code, body, _ = e.do(method, path, key, b)
			if code != 200 {
				t.Fatalf("order = %d %s", code, body)
			}
			ln = asMap(t, body)["lightning"].(map[string]any)
			if !reflect.DeepEqual(keysOf(ln), []string{"paid_msat", "refund_open_msat", "refunded_msat", "state"}) ||
				ln["paid_msat"] != float64(21000) || ln["refunded_msat"] != float64(5000) || ln["refund_open_msat"] != float64(3000) {
				t.Errorf("refunder=%v lightning = %v", withRefunder, ln)
			}
			if gotID != id {
				t.Errorf("figures asked for %q, want %q", gotID, id)
			}
		}
		// Zero figures are present, not omitted.
		e.h = e.handler(rf, func(context.Context, string) (int64, int64, int64, error) { return 0, 0, 0, nil })
		_, body, _ = e.do("GET", "/v1/orders/"+id, readKey, "")
		ln = asMap(t, body)["lightning"].(map[string]any)
		if len(ln) != 4 || ln["paid_msat"] != float64(0) {
			t.Errorf("zero figures: %v", ln)
		}
		// A failing Figures is an internal error and leaks nothing.
		fail = errors.New("secret " + secret1)
		e.h = e.handler(rf, fig)
		code, body, _ = e.do("GET", "/v1/orders/"+id, readKey, "")
		if code != 500 || string(body) != `{"error":"internal"}`+"\n" {
			t.Errorf("figures error = %d %s", code, body)
		}
	}
}

func TestRefundKeyGrammar(t *testing.T) {
	for _, c := range []string{CapRefundsWrite, CapRefundsRead} {
		if _, err := LoadKeys(writeKeyFile(t, 0o600, keysBody(keyEntry("k", c, "key")))); err != nil {
			t.Errorf("%s refused: %v", c, err)
		}
	}
	k, err := LoadKeys(writeKeyFile(t, 0o600, keysBody(keyEntry("a", CapRefundsWrite, "kw"), keyEntry("b", CapRefundsRead, "kr"),
		keyEntry("c", CapWrite, "ko"))))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key, cap string
		want     bool
	}{
		{"kw", CapRefundsWrite, true}, {"kw", CapRefundsRead, false}, {"kw", CapWrite, false}, {"kw", CapRead, false},
		{"kr", CapRefundsRead, true}, {"kr", CapRefundsWrite, false}, {"kr", CapRead, false},
		{"ko", CapRefundsWrite, false}, {"ko", CapRefundsRead, false}, {"ko", CapWrite, true},
	} {
		if got := k.Check(c.key, c.cap); got != c.want {
			t.Errorf("Check(%s, %s) = %v", c.key, c.cap, got)
		}
	}
	if _, err := LoadKeys(writeKeyFile(t, 0o600, keysBody(keyEntry("k", "refunds:admin", "key")))); err == nil {
		t.Error("an unknown refunds capability was accepted")
	}
}

func TestRefundViewDropsFraction(t *testing.T) {
	e, f := refundEnv(t, true)
	f.view.ClaimedAt = epoch.Add(1234567890 * time.Nanosecond)
	f.view.ResolvedAt = epoch.Add(5*time.Second + 999*time.Millisecond)
	f.view.State = refund.StateSettled
	_, body, _ := e.do("GET", pathGet, rrKey, "")
	m := asMap(t, body)
	if m["claimed_at"] != epoch.Add(time.Second).Format(time.RFC3339) || m["resolved_at"] != epoch.Add(5*time.Second).Format(time.RFC3339) {
		t.Errorf("fraction not dropped: %v %v", m["claimed_at"], m["resolved_at"])
	}
}

func TestRefundKeysReachNoOrderRoute(t *testing.T) {
	e, _ := refundEnv(t, true)
	code, body := e.post(orderBody)
	if code != 201 {
		t.Fatal(code, body)
	}
	id := asMap(t, body)["id"].(string)
	for _, key := range []string{rwKey, rrKey} {
		for _, rt := range []struct{ method, path, body string }{
			{"POST", "/v1/orders", orderBody}, {"GET", "/v1/orders/" + id, ""},
		} {
			code, b, _ := e.do(rt.method, rt.path, key, rt.body)
			if code != 403 || string(b) != `{"error":"forbidden"}`+"\n" {
				t.Errorf("%s %s with %s = %d %s", rt.method, rt.path, key, code, b)
			}
		}
	}
}
