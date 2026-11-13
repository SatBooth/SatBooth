package checkout

import (
	"context"
	"html"
	"io"
	"log/slog"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/qr"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// refValid is a second, slower statement of the token rule.
func refValid(s string) bool {
	if len(s) != 32 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

func TestValidTokenProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"0", "9", "a", "f", "g", "A", "F", "/", "%", ".", " ", "\x00", "€", "\xff", "-"}
	for i := 0; i < 20000; i++ {
		n := rng.Intn(40)
		if rng.Intn(3) == 0 {
			n = 31 + rng.Intn(3)
		}
		var b strings.Builder
		for j := 0; j < n; j++ {
			if rng.Intn(10) == 0 {
				b.WriteString(alphabet[rng.Intn(len(alphabet))])
			} else {
				b.WriteByte("0123456789abcdef"[rng.Intn(16)])
			}
		}
		if s := b.String(); validToken(s) != refValid(s) {
			t.Fatalf("validToken(%q) = %v", s, validToken(s))
		}
	}
}

// FuzzPath sends arbitrary paths. Whatever the path: no panic, the security headers, no redirect, a
// status from the fixed set, and no store lookup unless the third segment is a well-formed token.
func FuzzPath(f *testing.F) {
	for _, s := range []string{"/c/" + strings.Repeat("a", 32), "/c/..", "/c/%2F", "/c/%2e%2e/x", "/c//x", "/", "/static/style.css",
		"/c/" + strings.Repeat("A", 32) + "/pay", "/c/" + strings.Repeat("0", 32) + "/qr.svg", "/c/\x00", "/c/" + strings.Repeat("a", 5000)} {
		f.Add(s, "GET")
		f.Add(s, "POST")
	}
	e := newEnvF(f)
	f.Fuzz(func(t *testing.T, target, method string) {
		u, err := url.ParseRequestURI(target)
		if err != nil || !strings.HasPrefix(target, "/") {
			t.Skip()
		}
		switch method {
		case "GET", "POST", "HEAD", "PUT", "DELETE":
		default:
			method = "GET"
		}
		req := httptest.NewRequest(method, "/", nil)
		req.URL, req.RequestURI = u, target
		before := e.cnt.lookups.Load()
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		r := resp{rec.Code, rec.Body.String(), rec.Header()}
		checkHeaders(t, target, r)
		switch r.code {
		case 200, 303, 404, 405, 500:
		default:
			t.Fatalf("%q: code %d", target, r.code)
		}
		if r.code == 303 && !strings.HasPrefix(r.h.Get("Location"), "/c/") {
			t.Fatalf("%q: redirect to %q", target, r.h.Get("Location"))
		}
		segs := strings.Split(u.Path, "/")
		wellFormed := len(segs) >= 3 && segs[1] == "c" && validToken(segs[2])
		if !wellFormed && e.cnt.lookups.Load() != before {
			t.Fatalf("%q: store lookup for a path without a valid token", target)
		}
		if r.code == 404 && (r.body != notFoundBody) {
			// A 404 from a store miss, a bad path or an order without an invoice is always the fixed body.
			t.Fatalf("%q: 404 body %q", target, r.body)
		}
	})
}

// fixedBackend serves one order whose open invoice expires at a set time.
type fixedBackend struct {
	o   store.Order
	inv store.Invoice
}

func (f *fixedBackend) ByToken(_ context.Context, t string) (status.Order, error) {
	if t != f.o.Token {
		return status.Order{}, store.ErrNotFound
	}
	inv := f.inv
	return status.Order{Order: f.o, State: status.StateNew, Lightning: &lnpay.Status{State: lnpay.StateNew, Invoice: &inv}}, nil
}

// The minutes left on the page are rounded up, and at least 1, at and around every minute boundary.
// It drives the real handler.
func TestMinutesRounding(t *testing.T) {
	clk := &clock{t: epoch}
	b := &fixedBackend{o: store.Order{ID: strings.Repeat("1", 32), Token: strings.Repeat("2", 32), AmountMsat: 5000, ExpiresAt: epoch.Add(time.Hour)},
		inv: store.Invoice{Bolt11: "lnbcrt1", AmountMsat: 5000}}
	h := newHandler(b, nil, nil, nil, clk.Now, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	re := regexp.MustCompile(`About (\d+) min left`)
	for _, c := range []struct {
		left time.Duration
		want int
	}{
		{-time.Minute, 1}, {-time.Second, 1}, {0, 1}, {time.Second, 1}, {time.Minute - time.Second, 1}, {time.Minute, 1},
		{time.Minute + time.Second, 2}, {2*time.Minute - time.Second, 2}, {2 * time.Minute, 2}, {2*time.Minute + time.Second, 3},
		{20*time.Minute - time.Second, 20}, {20 * time.Minute, 20}, {20*time.Minute + time.Second, 21}, {30 * time.Minute, 30},
	} {
		b.inv.ExpiresAt = clk.Now().Add(c.left)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/c/"+b.o.Token, nil))
		m := re.FindStringSubmatch(rec.Body.String())
		if rec.Code != 200 || m == nil || m[1] != strconv.Itoa(c.want) {
			t.Errorf("%v left: code %d, %q, want %d min", c.left, rec.Code, m, c.want)
		}
	}
}

func FuzzPaymentURI(f *testing.F) {
	// The addresses are BIP-173's example address under the bcrt, bc and tb prefixes.
	f.Add("bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080", int64(1), "")
	f.Add("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", int64(150000000), "lnbc1abc")
	f.Add("bc1q", int64(-1), "a&b")
	f.Add("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", int64(5), "abc")
	f.Add("tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", store.MaxSat+1, "é")
	f.Fuzz(func(t *testing.T, addr string, sat int64, bolt string) {
		s, err := PaymentURI(addr, sat, bolt)
		ok := store.ValidAddress(addr) && sat >= 1 && sat <= store.MaxSat &&
			(bolt == "" || refValidInvoice(bolt))
		if (err == nil) != ok {
			t.Fatalf("(%q, %d, %q): err %v, validators say %v", addr, sat, bolt, err, ok)
		}
		if err == nil {
			if _, perr := url.Parse(s); perr != nil {
				t.Fatalf("unparsable URI %q: %v", s, perr)
			}
			assertHrefSafe(t, s)
		}
	})
}

// renderInvariants renders the page and the QR of an order and checks what must hold for any invoice text the store
// allows (printable ASCII) and every combination of rails: the link is PaymentURI's output and equals the QR text, the
// QR is shown only when the text fits, html/template never blocked the link, and nothing breaks the markup.
// A price, when not empty, is a quote's Price shown as "<price> EUR": the template escapes whatever it holds. With a
// Monero amount the order's subaddress is xaddr, whatever it holds: its field is escaped, and the Monero link and QR
// are MoneroURI's output, or absent when MoneroURI refuses the address.
func renderInvariants(t *testing.T, msat, sat int64, invoice, price string) {
	t.Helper()
	renderInvariants3(t, msat, sat, 0, invoice, "", price)
}

func renderInvariants3(t *testing.T, msat, sat, pico int64, invoice, xaddr, price string) {
	t.Helper()
	so := stubOrder3(msat, sat, pico, invoice)
	if pico > 0 {
		so.Monero.Address = xaddr
	}
	if price != "" {
		so.Quote = &store.Quote{Price: price, Currency: "EUR"}
	}
	h := stubHandler(so, io.Discard)
	r := getStub(h, so, "")
	if r.code != 200 {
		t.Fatalf("(%d, %d, %.40q): code %d", msat, sat, invoice, r.code)
	}
	checkPlain(t, r.body)
	switch {
	case price == "":
		if strings.Count(r.body, `class="amount"`) != 1 || strings.Contains(r.body, " EUR</p>") {
			t.Fatalf("a currency line without a quote:\n%s", r.body)
		}
	case strings.ContainsAny(price, `<>&"'+`):
		if strings.Contains(r.body, `<p class="amount">`+price+` EUR</p>`) || strings.Contains(price, "<") && !strings.Contains(r.body, "&lt;") {
			t.Fatalf("the price %q is not escaped:\n%s", price, r.body)
		}
	default:
		if !strings.Contains(r.body, `<p class="amount">`+price+` EUR</p>`) {
			t.Fatalf("the price %q is not shown:\n%s", price, r.body)
		}
	}
	if strings.Contains(r.body, "ZgotmplZ") {
		t.Fatalf("a blocked URL in the page for %q", invoice)
	}
	text, has := qrText(so)
	hf, link := href(t, r.body)
	if strings.HasPrefix(hf, "monero:") { // the first link is the Monero one: there is no bitcoin link
		hf, link = "", false
	}
	switch {
	case link && sat > 0:
		if !has || hf != text || !strings.HasPrefix(hf, "bitcoin:") {
			t.Fatalf("link %q, QR text %q (%v)", hf, text, has)
		}
		assertHrefSafe(t, hf)
	case link:
		if !has || !regexp.MustCompile(`^lightning:[A-Z0-9]+$`).MatchString(hf) || !refValidInvoice(invoice) || hf != "lightning:"+strings.ToUpper(invoice) || text != "LIGHTNING:"+strings.ToUpper(invoice) {
			t.Fatalf("Lightning link %q, QR text %q (%v)", hf, text, has)
		}
	case sat > 0:
		t.Fatalf("no link for an order with an address (%.40q)", invoice)
	case has:
		t.Fatalf("a QR text without a link for an invoice-only order: %q", invoice)
	}
	fits := has && len(text) <= qr.MaxInput
	if img := strings.Contains(r.body, `src="/c/`+so.Order.Token+`/qr.svg"`); img != fits || pico == 0 && imgRe.MatchString(r.body) != fits {
		t.Fatalf("img = %v, text of %d bytes (%v)", img, len(text), has)
	}
	q := getStub(h, so, "/qr.svg")
	if (q.code == 200) != fits || q.code != 200 && q.code != 404 {
		t.Fatalf("qr.svg %d, fits %v", q.code, fits)
	}
	if fits {
		if want, err := qr.SVG(text); err != nil || q.body != string(want) {
			t.Fatalf("qr.svg is not the QR of the text: %v", err)
		}
	}
	if sat > 0 && !strings.Contains(r.body, `id="address"`) || msat > 0 && !strings.Contains(r.body, `id="invoice"`) {
		t.Fatalf("a copy field is missing")
	}
	// The Monero member.
	mq := getStub(h, so, "/qr.svg?rail=monero")
	if pico == 0 {
		if strings.Contains(r.body, `id="monero"`) || strings.Contains(r.body, "monero:") || strings.Contains(r.body, " XMR") || mq.code != 404 {
			t.Fatalf("a Monero member on an order without a Monero rail:\n%s", r.body)
		}
		return
	}
	m := regexp.MustCompile(`id="monero" type="text" readonly="readonly" value="([^"]*)"`).FindStringSubmatch(r.body)
	if xaddr == "" { // an empty subaddress is no subaddress: nothing Monero to show (the store never holds one)
		if m != nil || strings.Contains(r.body, "monero:") || strings.Contains(r.body, "Send exactly "+xmrrpc.FormatXMR(pico)+" XMR.") || mq.code != 404 {
			t.Fatalf("a Monero member for an empty subaddress:\n%s", r.body)
		}
		return
	}
	if m == nil || html.UnescapeString(m[1]) != xaddr {
		t.Fatalf("the subaddress field %v, want %q", m, xaddr)
	}
	if strings.ContainsAny(xaddr, `<>"&'`) && m[1] == xaddr { // the field holds the address as written: not escaped
		t.Fatalf("the subaddress %q is not escaped", xaddr)
	}
	uri, err := MoneroURI(xaddr, pico)
	ml := moneroHrefRe.FindStringSubmatch(r.body)
	mimg := strings.Contains(r.body, `src="/c/`+so.Order.Token+`/qr.svg?rail=monero"`)
	switch {
	case err != nil:
		if ml != nil || mimg || mq.code != 404 || strings.Contains(r.body, "monero:") {
			t.Fatalf("a Monero link or QR for an address MoneroURI refuses (%q):\n%s", xaddr, r.body)
		}
	default:
		if ml == nil || html.UnescapeString(ml[1]) != uri || !mimg || mq.code != 200 {
			t.Fatalf("Monero link %v, want %q; img %v, qr %d", ml, uri, mimg, mq.code)
		}
		assertMoneroHrefSafe(t, html.UnescapeString(ml[1]))
		if want, err := qr.SVG(uri); err != nil || mq.body != string(want) {
			t.Fatalf("qr.svg?rail=monero is not the QR of the Monero URI: %v", err)
		}
	}
	if !strings.Contains(r.body, "Send exactly "+xmrrpc.FormatXMR(pico)+" XMR.") {
		t.Fatalf("no Monero amount note:\n%s", r.body)
	}
}

func printable(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func TestRenderInvariantsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const odd = "-_/?&=%#<>\"' .:;+\\"
	for i := 0; i < 400; i++ {
		// Lengths cross the 1,200-byte QR cut and the 4,096-character invoice rule on both sides.
		n := []int{1, 2, 3, 100, 300, 1100, 1150, 1200, 1300, 4090, 4096, 4097, 5000}[rng.Intn(13)]
		b := make([]byte, n)
		for j := range b {
			if rng.Intn(4) == 0 && n < 400 { // odd characters only in short ones, so the long ones reach the length rules
				b[j] = odd[rng.Intn(len(odd))]
			} else {
				b[j] = alnum[rng.Intn(len(alnum))]
			}
		}
		inv := "ln" + string(b)
		xaddr := []string{testSub, "B" + testSub[1:], "7" + testSub[1:], "4" + testSub[1:], testSub[:94], "8<b>" + testSub[4:],
			testSub[:93] + `"'`, "8&amp;" + testSub[6:]}[rng.Intn(8)]
		pico := []int64{0, 1, pico15, store.MaxPiconero}[rng.Intn(4)]
		for _, c := range [][3]int64{{21_000, 21_000, 0}, {21_000, 0, 0}, {0, 21_000, 0}, {21_000, 21_000, pico}, {0, 0, pico}} {
			price := []string{"", "", "12.50", "0.000000000000000001", "<b>1</b>", "1 & 2", `"'+`}[rng.Intn(7)]
			if c[0] == 0 && c[1] == 0 && c[2] == 0 {
				continue
			}
			if c[0] == 0 {
				renderInvariants3(t, 0, c[1], c[2], "", xaddr, price)
				continue
			}
			renderInvariants3(t, c[0], c[1], c[2], inv, xaddr, price)
		}
	}
}

func FuzzRender(f *testing.F) {
	f.Add("lnbcrt1abc", true, true, "", int64(0), "")
	f.Add("lnbc1-x", true, true, "12.50", int64(0), "")
	f.Add("ln\"><script>", true, false, "<script>alert(1)</script>", int64(0), "")
	f.Add("lnbcrt"+strings.Repeat("q", 1300), true, true, "99999999999999999999.999999999999999999", int64(0), "")
	f.Add("lnbcrt"+strings.Repeat("q", 3994), true, true, `1"&'+>`, int64(0), "")
	f.Add("", false, true, "1", int64(0), "")
	f.Add("lnbcrt1abc", true, true, "12.50", int64(pico15), testSub)
	f.Add("", false, false, "", int64(1), "8<script>"+testSub[9:])
	f.Add("", false, false, "", store.MaxPiconero, "B"+testSub[1:])
	f.Add("lnbcrt1abc", false, true, "", store.MaxPiconero+1, "4"+testSub[1:])
	f.Add("", false, false, `"'`, int64(-5), `"><a href="x">`)
	f.Add("", false, true, "0", int64(44), "") // an empty subaddress (found by the fuzzer)
	f.Add("0", true, false, "0", int64(48), "<")
	f.Add("0", false, true, "0", int64(34), "&")
	f.Fuzz(func(t *testing.T, inv string, onchainToo, lightning bool, price string, pico int64, xaddr string) {
		inv, price, xaddr = printable(inv), printable(price), printable(xaddr)
		var msat, sat int64
		if lightning && inv != "" {
			msat = 21_000
		}
		if onchainToo {
			sat = 21_000
		}
		// A view's amount is the store's: 0 (no Monero rail) or 1 to 2^53 − 1.
		if pico < 0 || pico > store.MaxPiconero {
			pico = 0
		}
		if msat == 0 && sat == 0 && pico == 0 {
			t.Skip()
		}
		if msat == 0 {
			inv = ""
		}
		renderInvariants3(t, msat, sat, pico, inv, xaddr, price)
	})
}
