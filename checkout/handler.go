// Package checkout is the public checkout page: the only surface of SatBooth that faces the internet
// (behind a CDN/WAF). It is server-rendered HTML with no JavaScript; while an invoice is open the page
// refreshes itself with a meta refresh. The routes, and nothing else:
//
//	GET  /c/{token}          the page
//	POST /c/{token}/pay      make (or reuse) the order's address and invoice (the form field rail=bitcoin, the
//	                         default when the order has a bitcoin rail) or its Monero subaddress (rail=monero), then
//	                         303 to the page
//	GET  /c/{token}/qr.svg   the order's payment text (the unified URI, or the invoice) as an SVG QR code; with
//	                         ?rail=monero, the Monero URI's
//	GET  /static/style.css   the stylesheet
//	GET  /static/logo.svg    the logo mark (the header image and the page icon)
//
// Every other request, and every request whose token is not exactly 32 lowercase hex characters or
// names no order, gets the same 404 with the same body and headers; a malformed token is rejected
// before any store access. A press on a known order whose body is not a readable form, or whose rail field is not
// exactly once "bitcoin" or "monero" or names a rail the order lacks, gets one fixed 400. Every response, error responses included, carries the security headers.
// The page shows only the amounts (and, for a priced order, its price), the state, the issued address and the open
// invoice (the QR and "Open in wallet" carry the BIP321 URI of PaymentURI), and the issued Monero subaddress (its QR
// and link carry MoneroURI's); it echoes nothing from the request (the only request state is the query busy=1, matched
// exactly). An order with bitcoin and Monero rails offers both with two forms (the rail picker); once money is seen
// on one family, the other is no longer offered. Its one link out is the fixed "Rates: yadio.io" credit, shown when
// Yadio was a source of the order's quote, as Yadio's terms ask.
package checkout

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/qr"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// DisplayName is the name shown on the page.
const DisplayName = "SatBooth"

// ContentSecurityPolicy is sent on every response.
const ContentSecurityPolicy = "default-src 'none'; img-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

const (
	refreshSeconds = 5
	maxPayBody     = 1 << 10
	notFoundBody   = "not found\n"
	internalBody   = "internal error\n"
	badRequestBody = "bad request\n"
)

// The values of the press's form field rail.
const (
	railBitcoin = "bitcoin"
	railMonero  = "monero"
)

//go:embed page.html
var pageHTML string

//go:embed style.css
var styleCSS []byte

//go:embed logo.svg
var logoSVG []byte

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// resolver is what the handler needs of *status.Resolver.
type resolver interface {
	ByToken(ctx context.Context, token string) (status.Order, error)
}

// Handler returns the checkout handler. The resolver's Ln, Oc and Xm make the invoice, the address and the Monero
// subaddress; Oc and Xm are nil when their rail is not configured (an order with an amount on it then reads "Checking
// payment").
func Handler(st *store.Store, res *status.Resolver, now func() time.Time, log *slog.Logger) http.Handler {
	if st == nil || res == nil || res.Ln == nil || now == nil || log == nil {
		panic("checkout: Handler needs a store, a resolver with a Lightning service, a clock and a logger")
	}
	return newHandler(res, res.Ln, res.Oc, res.Xm, now, log)
}

type handler struct {
	res       resolver
	ln        status.Lightning
	oc        status.Onchain
	xm        status.Monero
	now       func() time.Time
	log       *slog.Logger
	payBudget time.Duration
	mux       *http.ServeMux
	alertMu   sync.Mutex
	alerted   map[string]struct{} // invoices (by payment hash) whose refusal was logged
}

// maxAlerted bounds the alerted set; when full it is cleared (the worst case is one more ALERT per invoice).
const maxAlerted = 4096

// alertOnce logs the refusal of the order's open invoice once per invoice per process.
func (h *handler) alertOnce(so status.Order) {
	key := so.Order.ID
	if so.Lightning != nil && so.Lightning.Invoice != nil && so.Lightning.Invoice.PaymentHash != "" {
		key = so.Lightning.Invoice.PaymentHash
	}
	h.alertMu.Lock()
	_, seen := h.alerted[key]
	if !seen {
		if len(h.alerted) >= maxAlerted {
			clear(h.alerted)
		}
		h.alerted[key] = struct{}{}
	}
	h.alertMu.Unlock()
	if !seen {
		h.log.Error(logURIRefused, "order", so.Order.ID)
	}
}

// payBudget bounds one press: the address (up to onchain.PressWait, 10 s) and the invoice run at the same time under
// it, so the press ends well inside the server's 15 s WriteTimeout.
const defaultPayBudget = 13 * time.Second

// logURIRefused is the one fixed ALERT line for an invoice that PaymentURI refuses (the store accepts any printable
// ASCII, the URI only letters and digits); the invoice itself is never logged.
const logURIRefused = "ALERT: an open invoice is not fit for a payment URI; the QR and the link do not carry it"

// logPartialPress is the fixed line for a press where one step made its invoice or address and the other failed with an
// error that is not a refusal.
const logPartialPress = "checkout: one step of a press failed with an unexpected error; the other one succeeded"

func newHandler(res resolver, ln status.Lightning, oc status.Onchain, xm status.Monero, now func() time.Time, log *slog.Logger) http.Handler {
	h := &handler{res: res, ln: ln, oc: oc, xm: xm, now: now, log: log, payBudget: defaultPayBudget, mux: http.NewServeMux(), alerted: map[string]struct{}{}}
	h.mux.HandleFunc("GET /c/{token}", h.page)
	h.mux.HandleFunc("POST /c/{token}/pay", h.pay)
	h.mux.HandleFunc("GET /c/{token}/qr.svg", h.qr)
	h.mux.HandleFunc("GET /static/style.css", h.style)
	h.mux.HandleFunc("GET /static/logo.svg", h.logo)
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { notFound(w) })
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", ContentSecurityPolicy)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cache-Control", "no-store")
	// A path the mux would redirect to its clean form (dot segments, doubled or trailing slashes) is
	// a 404 here, so no response is a redirect to somewhere else.
	if p := r.URL.Path; p == "" || path.Clean(p) != p {
		notFound(w)
		return
	}
	h.mux.ServeHTTP(w, r)
}

func notFound(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, notFoundBody)
}

func internal(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, internalBody)
}

func badRequest(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	io.WriteString(w, badRequestBody)
}

// validToken reports whether t is exactly 32 lowercase hex characters.
func validToken(t string) bool {
	if len(t) != 32 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// view is what the template shows. Only URI and MoneroURI are template.URL: PaymentURI's output is "bitcoin:" and
// then [A-Z0-9.:?=&] and two lowercase key literals, MoneroURI's "monero:", base58 characters and "?tx_amount=" with
// a decimal (their tests pin that), so neither can carry a script or a quote.
type view struct {
	Name, Token string
	Price       string // a priced order's price and currency, "12.50 EUR"; empty for an explicit order
	Yadio       bool   // Yadio was a source of the order's quote: the page credits it with a link
	Amounts     string // "21,000 sats", "0.00021 BTC", "1.5 XMR", or those of the order joined by " or "
	State       string
	Issued      bool // the QR, copy fields and link block
	Invoice     string
	Address     string
	BTC         string
	QR          bool
	URI         template.URL
	Minutes     int
	Refresh     bool
	Checking    bool // "Checking payment…"
	Busy        bool
	ButtonLabel string
	NewInvoice  bool // the "New Lightning invoice" button
	Seen        bool
	Confs       int
	Required    int    // the order's tier; 0 (paid from the mempool) shows "Seen" without the counts
	SeenSat     string // the sats seen so far, when below the amount
	Stale       bool
	Heading     string
	Refund      bool // a person will refund the extra
	Unconfirmed bool // underpaid with the whole amount seen: not confirmed enough at the watch window's end
	Unseen      bool // underpaid with nothing live: the payment died after order.underpaid (it did not confirm)

	Monero         bool         // the order has a Monero rail (its forms carry the rail field)
	MoneroAddress  string       // the issued subaddress, while it is offered
	XMR            string       // "Send exactly {{.XMR}} XMR.": the whole amount, or what is still missing
	MoneroURI      template.URL // the Monero URI of the QR and the link
	MoneroQR       bool         // the Monero QR
	MoneroSeen     bool         // the Monero confirmations line
	MoneroConfs    int          // the confirmations of the Monero payment
	MoneroRequired int          // the Monero tier; 0 shows "Seen" without the counts
	SeenXMR        string       // the XMR seen so far, when below the amount
	OfferBitcoin   bool         // the picker's "Pay with bitcoin" (or today's label) button
	OfferMonero    bool         // the picker's "Pay with Monero" button
	MoneroStale    bool         // the Monero view is stale (staleChecking says when the page shows it)
}

// load returns the resolved order for a request, or writes the 404/500 itself. Both the 404 of an unknown token and
// the 500 are the fixed ones; an on-chain rail that is off is not an error here (ok is true, unavailable is true).
func (h *handler) load(w http.ResponseWriter, r *http.Request) (so status.Order, unavailable, ok bool) {
	token := r.PathValue("token")
	if !validToken(token) {
		notFound(w)
		return status.Order{}, false, false
	}
	so, err := h.res.ByToken(r.Context(), token)
	switch {
	case err == nil:
		return so, false, true
	case errors.Is(err, status.ErrUnavailable):
		return status.Order{}, true, true
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
	default:
		internal(w)
	}
	return status.Order{}, false, false
}

// openInvoice is the order's open invoice, or "" for none.
func openInvoice(o status.Order) string {
	if o.Lightning != nil && o.Lightning.Invoice != nil && o.State == status.StateNew {
		return o.Lightning.Invoice.Bolt11
	}
	return ""
}

// address is the order's issued address, or "".
func address(o status.Order) string {
	if o.Onchain != nil && o.Onchain.Issued && o.State == status.StateNew {
		return o.Onchain.Address
	}
	return ""
}

// moneroAddress is the order's issued Monero subaddress, or "".
func moneroAddress(o status.Order) string {
	if o.Monero != nil && o.Monero.Issued && o.State == status.StateNew {
		return o.Monero.Address
	}
	return ""
}

// moneroText is the text of the Monero QR and the href of its link: MoneroURI of the issued subaddress and the amount
// (what is still missing, after shown); "" when there is no subaddress or no amount left.
func moneroText(o status.Order) string {
	addr := moneroAddress(o)
	if addr == "" {
		return ""
	}
	uri, err := MoneroURI(addr, o.Order.AmountPiconero)
	if err != nil {
		return ""
	}
	return uri
}

// payLink works out the QR text and the link of an order: with an address, the BIP321 URI (with the open invoice when
// one is open, and the QR text is the link's href); with an invoice only, the QR text is "LIGHTNING:" and the
// uppercase invoice and the link is "lightning:" and the uppercase invoice. Both come only from an invoice that passes
// validInvoice (PaymentURI's own check), so the link holds only [A-Z0-9] after its scheme. refused is true when the
// invoice failed that check: with an address the address-only URI takes its place, without one there is no QR and no
// link.
func payLink(o status.Order) (text, href string, refused bool) {
	inv := openInvoice(o)
	if addr := address(o); addr != "" {
		uri, err := PaymentURI(addr, o.Order.AmountSat, inv)
		if err != nil && inv != "" {
			if uri, err = PaymentURI(addr, o.Order.AmountSat, ""); err == nil {
				refused = true
			}
		}
		if err != nil {
			return "", "", false
		}
		return uri, uri, refused
	}
	if inv != "" {
		link, ok := lightningLink(inv)
		if !ok {
			return "", "", true
		}
		return "LIGHTNING:" + strings.ToUpper(inv), link, false
	}
	return "", "", false
}

// lightningLink is the "open in wallet" link of an invoice: "lightning:" and the uppercase invoice; false when the
// invoice fails validInvoice (PaymentURI's own check), so the link holds only [A-Z0-9] after its scheme.
func lightningLink(bolt11 string) (string, bool) {
	if !validInvoice(bolt11) {
		return "", false
	}
	return "lightning:" + strings.ToUpper(bolt11), true
}

// shown is the order as the page and the QRs show it, so they agree. Once the quote has expired the addresses are no
// longer offered (a payment to them would be late). Once an on-chain payment was seen, Lightning is no longer offered
// (the invoice is dropped) and the URI asks for what is still missing, so a top-up does not overpay; when nothing is
// missing the order has no amount left and so no URI. The Monero rail likewise: once Monero money was seen, its URI
// asks for what is missing, and the bitcoin rails are no longer offered (no address, no invoice); once bitcoin money
// was seen (on-chain, or Lightning paid), the Monero subaddress is no longer offered.
func shown(so status.Order, now time.Time) status.Order {
	expired := !now.Before(so.Order.ExpiresAt)
	xmSeen := so.Monero != nil && so.Monero.Received > 0
	ocSeen := so.Onchain != nil && so.Onchain.ReceivedSat > 0
	lnPaid := so.Lightning != nil && so.Lightning.State == lnpay.StatePaid
	if so.Onchain != nil {
		oc := *so.Onchain
		if expired || xmSeen {
			oc.Issued = false
		}
		so.Onchain = &oc
		if ocSeen {
			so.Order.AmountSat = max(0, so.Order.AmountSat-oc.ReceivedSat)
		}
	}
	if so.Lightning != nil && (ocSeen || xmSeen) {
		ln := *so.Lightning
		ln.Invoice = nil
		so.Lightning = &ln
	}
	if so.Monero != nil {
		xm := *so.Monero
		if expired || ocSeen || lnPaid {
			xm.Issued = false
		}
		so.Monero = &xm
		if xmSeen {
			so.Order.AmountPiconero = max(0, so.Order.AmountPiconero-xm.Received)
		}
	}
	return so
}

// qrText is the text of the QR and of the "Open in wallet" link; false when there is nothing to show.
func qrText(o status.Order) (string, bool) {
	t, _, _ := payLink(o)
	return t, t != ""
}

// thousands writes n with a comma every three digits.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// amounts is the amounts line: each rail's amount, joined with " or ": Lightning in sats, on-chain in BTC, Monero in
// XMR.
func amounts(o store.Order) string {
	var parts []string
	if o.AmountMsat > 0 {
		if o.AmountMsat == 1000 {
			parts = append(parts, "1 sat")
		} else {
			parts = append(parts, thousands(o.AmountMsat/1000)+" sats")
		}
	}
	if o.AmountSat > 0 {
		parts = append(parts, btcrpc.FormatBTC(o.AmountSat)+" BTC")
	}
	if o.AmountPiconero > 0 {
		parts = append(parts, xmrrpc.FormatXMR(o.AmountPiconero)+" XMR")
	}
	return strings.Join(parts, " or ")
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	so, unavailable, ok := h.load(w, r)
	if !ok {
		return
	}
	v := view{Name: DisplayName}
	if unavailable {
		// The token is load's validated one: the refresh comes back to this order's plain URL.
		v.Token, v.Checking, v.Refresh = r.PathValue("token"), true, true
	} else {
		h.fill(&v, so, r)
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, v); err != nil {
		internal(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (h *handler) fill(v *view, so status.Order, r *http.Request) {
	o, now := so.Order, h.now()
	v.Token, v.State, v.Amounts = o.Token, so.State, amounts(o)
	v.Monero = o.AmountPiconero > 0
	hasBTC := o.AmountMsat > 0 || o.AmountSat > 0
	if q := so.Quote; q != nil {
		v.Price = q.Price + " " + q.Currency
		for _, r := range q.Rates {
			for _, s := range r.Sources {
				v.Yadio = v.Yadio || s.Provider == rates.ProviderYadio
			}
		}
	}
	oc, xm := so.Onchain, so.Monero
	lnPaid := so.Lightning != nil && so.Lightning.State == lnpay.StatePaid
	// The stale flags count only for a rail the customer used (staleChecking): an address issued or money seen.
	v.Stale = oc != nil && staleChecking(oc.Stale, oc.Issued, oc.ReceivedSat)
	v.MoneroStale = xm != nil && staleChecking(xm.Stale, xm.Issued, xm.Received)
	// The underpaid texts follow the rail whose view reads underpaid: Monero when it does and on-chain does not.
	if under := so.State == status.StateUnderpaid; xm != nil && xm.State == rail.Underpaid && (oc == nil || oc.State != onchain.RailUnderpaid) {
		v.Unconfirmed = under && xm.Received >= o.AmountPiconero
		v.Unseen = under && xm.Received == 0
	} else {
		v.Unconfirmed = under && oc != nil && oc.ReceivedSat >= o.AmountSat
		v.Unseen = under && oc != nil && oc.ReceivedSat == 0
	}
	switch so.State {
	case status.StatePaid:
		// A paid order with a late or overpaid amount on a rail, or an amount on a rail next to a payment on another,
		// was paid more than once: a person refunds the extra.
		ocRefund := oc != nil && (oc.Overpaid || oc.LateSat > 0 || oc.ReceivedSat > 0 && (!oc.Paid || lnPaid))
		xmRefund := xm != nil && (xm.Overpaid || xm.Late > 0 || xm.Received > 0 && (!xm.Paid || lnPaid || oc != nil && oc.ReceivedSat > 0))
		v.Refund = ocRefund || xmRefund
		return
	case status.StateExpired:
		// bitcoind (or wallet-rpc) cannot confirm that nothing arrived, so the page does not say "Nothing was charged".
		if v.Stale || v.MoneroStale {
			v.State, v.Checking, v.Refresh = status.StateNew, true, true
		}
		return
	case status.StateUnderpaid, status.StateLate:
		return
	}
	expiredNew := !now.Before(o.ExpiresAt)
	sh := shown(so, now)
	inv, addr, xaddr := openInvoice(sh), address(sh), moneroAddress(sh)
	ocSeen := oc != nil && oc.ReceivedSat > 0
	xmSeen := xm != nil && xm.Received > 0
	v.ButtonLabel = "Pay with bitcoin"
	switch {
	case v.Monero:
	case o.AmountMsat > 0 && o.AmountSat > 0:
		v.ButtonLabel = "Pay"
	case o.AmountMsat > 0:
		v.ButtonLabel = "Pay with Lightning"
	}
	switch {
	// Something to pay to, or money seen while the order is open (on both families, when nothing is offered).
	case inv != "" || addr != "" || xaddr != "" || !expiredNew && (ocSeen || xmSeen):
		v.Issued, v.Refresh = true, true
		v.Heading = "Waiting for payment"
		if ocSeen || xmSeen {
			v.Heading = "Payment seen"
		}
		if inv != "" || addr != "" {
			text, href, refused := payLink(sh)
			if refused {
				h.alertOnce(so)
			}
			v.Invoice, v.Address = inv, addr
			if addr != "" && sh.Order.AmountSat > 0 {
				v.BTC = btcrpc.FormatBTC(sh.Order.AmountSat) // the whole amount, or what is still missing
			}
			v.QR = text != "" && len(text) <= qr.MaxInput
			v.URI = template.URL(href)
			if inv != "" {
				left := so.Lightning.Invoice.ExpiresAt.Sub(now)
				v.Minutes = max(1, int((left+time.Minute-1)/time.Minute))
			}
		}
		if xaddr != "" {
			v.MoneroAddress = xaddr
			if sh.Order.AmountPiconero > 0 {
				v.XMR = xmrrpc.FormatXMR(sh.Order.AmountPiconero) // the whole amount, or what is still missing
			}
			if uri := moneroText(sh); uri != "" {
				v.MoneroURI, v.MoneroQR = template.URL(uri), len(uri) <= qr.MaxInput
			}
		}
		h.fillSeen(v, so)
		h.fillMoneroSeen(v, so)
		v.NewInvoice = addr != "" && inv == "" && !ocSeen && !xmSeen && !expiredNew && o.AmountMsat > 0
		// The other family's button, while nothing was seen: bitcoin when nothing bitcoin is issued, Monero when no
		// subaddress is. A stale view never hides it, as on-chain (ruling 8-T8a): a press then answers busy.
		v.OfferBitcoin = v.Monero && hasBTC && inv == "" && addr == "" && !ocSeen && !xmSeen && !expiredNew
		v.OfferMonero = v.Monero && xaddr == "" && !ocSeen && !xmSeen && !expiredNew
	case expiredNew:
		// Past its expiry with nothing open: Pay would refuse, so there is no button; the page waits for the
		// reconcilers to settle the state.
		v.Refresh = true
		if ocSeen || xmSeen {
			h.fillSeen(v, so)
			h.fillMoneroSeen(v, so)
		} else {
			v.Checking = true
		}
	default:
		v.Busy = r.URL.Query().Get("busy") == "1"
		v.OfferBitcoin = hasBTC
		v.OfferMonero = v.Monero
	}
	if v.Issued && r.URL.Query().Get("busy") == "1" {
		v.Busy = true
	}
}

// staleChecking is the one rule, for both rails, of when a stale view shows "Checking payment…" (or holds back
// "Nothing was charged"): only once the rail has an issued address or money seen. A rail the customer never used says
// nothing about a watcher it does not depend on.
func staleChecking(stale, issued bool, received int64) bool {
	return stale && (issued || received > 0)
}

// fillSeen sets the lines of an on-chain payment that was seen: the confirmations (or "Checking payment…" when the
// watcher is stale and the rail was used, staleChecking) and, below the amount, the sats seen so far.
func (h *handler) fillSeen(v *view, so status.Order) {
	oc := so.Onchain
	if oc == nil {
		return
	}
	if oc.ReceivedSat > 0 {
		v.Seen, v.Confs, v.Required = true, oc.Confirmations, oc.RequiredConfirmations
		if oc.ReceivedSat < so.Order.AmountSat {
			v.SeenSat = thousands(oc.ReceivedSat)
		}
	}
	if staleChecking(oc.Stale, oc.Issued, oc.ReceivedSat) {
		v.Seen, v.Checking = false, true
	}
}

// fillMoneroSeen is fillSeen for the Monero rail: the confirmations (or "Checking payment…" when the view is stale
// and the rail was used, staleChecking) and, below the amount, the XMR seen so far.
func (h *handler) fillMoneroSeen(v *view, so status.Order) {
	xm := so.Monero
	if xm == nil {
		return
	}
	if xm.Received > 0 {
		v.MoneroSeen, v.MoneroConfs, v.MoneroRequired = true, xm.Confirmations, xm.RequiredConfirmations
		if xm.Received < so.Order.AmountPiconero {
			v.SeenXMR = xmrrpc.FormatXMR(xm.Received)
		}
	}
	if staleChecking(xm.Stale, xm.Issued, xm.Received) {
		v.MoneroSeen, v.Checking = false, true
	}
}

func (h *handler) pay(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !validToken(token) {
		notFound(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPayBody)
	to := "/c/" + token
	ctx, cancel := context.WithTimeout(r.Context(), h.payBudget)
	defer cancel()
	// The order first, so an unknown token is always the 404, whatever the body.
	so, err := h.res.ByToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	}
	// The rail field: absent, or exactly once "bitcoin" or "monero". A form that does not parse (too large, or
	// malformed) is a 400, so the press never guesses a rail; a body that is not a form has no field.
	if r.ParseForm() != nil {
		badRequest(w)
		return
	}
	choice, named := "", false
	if vals, ok := r.PostForm["rail"]; ok {
		if len(vals) != 1 || vals[0] != railBitcoin && vals[0] != railMonero {
			badRequest(w)
			return
		}
		choice, named = vals[0], true
	}
	switch {
	case err == nil:
	case errors.Is(err, status.ErrUnavailable), busy(err):
		http.Redirect(w, r, to+"?busy=1", http.StatusSeeOther)
		return
	default:
		internal(w)
		return
	}
	o := so.Order
	hasBTC := o.AmountMsat > 0 || o.AmountSat > 0
	switch {
	case !named && hasBTC:
		choice = railBitcoin
	case !named:
		choice = railMonero
	case choice == railBitcoin && !hasBTC, choice == railMonero && o.AmountPiconero == 0:
		badRequest(w) // a rail the order does not have
		return
	}
	// An order that reads paid on any rail, or is past paying, makes nothing: a mixed order paid on-chain never
	// gets a fresh invoice.
	if so.State != status.StateNew {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	if choice == railMonero {
		h.payMonero(ctx, w, r, so, to)
		return
	}
	// Once Monero money was seen the bitcoin rails are not offered (shown): a press from an older page makes nothing.
	if so.Monero != nil && so.Monero.Received > 0 {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	var ocErr, lnErr error
	var ocRan, lnRan bool
	var wg sync.WaitGroup
	if o.AmountSat > 0 && h.oc != nil {
		ocRan = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ocErr = h.oc.Address(ctx, o.ID)
		}()
	}
	if o.AmountMsat > 0 && so.Lightning != nil && so.Lightning.Invoice == nil && (so.Onchain == nil || so.Onchain.ReceivedSat == 0) {
		lnRan = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, lnErr = h.ln.Pay(ctx, token)
		}()
	}
	wg.Wait()
	isBusy := false
	made := ocRan && ocErr == nil || lnRan && lnErr == nil // a step made its address or invoice
	for _, e := range []struct {
		err   error
		fine  []error
		retry bool
	}{{ocErr, []error{onchain.ErrPaid, onchain.ErrExpired}, onchainBusy(ocErr)},
		{lnErr, []error{lnpay.ErrPaid, lnpay.ErrExpired}, busy(lnErr)}} {
		switch {
		case e.err == nil || matches(e.err, e.fine):
		case errors.Is(e.err, store.ErrNotFound):
			notFound(w)
			return
		case e.retry:
			isBusy = true
		case made:
			h.log.Error(logPartialPress)
			isBusy = true
		default:
			internal(w)
			return
		}
	}
	if isBusy {
		to += "?busy=1"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// payMonero is the press with rail=monero: it asks only the Monero rail for the order's subaddress, under the same
// budget, and never touches Lightning or the on-chain rail. As the on-chain press (ruling 8-T8a): the refusals the
// customer may retry (moneroBusy: busy, wallet-rpc unavailable or not yet checked, the wallet behind, a failed
// subaddress check) are ?busy=1; expired and paid are the page; an order without a Monero amount is a 400.
func (h *handler) payMonero(ctx context.Context, w http.ResponseWriter, r *http.Request, so status.Order, to string) {
	// Once bitcoin money was seen the Monero rail is not offered (shown): a press from an older page makes nothing.
	if so.Onchain != nil && so.Onchain.ReceivedSat > 0 {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	if h.xm == nil { // the resolver answers status.ErrUnavailable first; kept so a nil rail is never called
		http.Redirect(w, r, to+"?busy=1", http.StatusSeeOther)
		return
	}
	_, err := h.xm.Address(ctx, so.Order.ID)
	switch {
	case err == nil, errors.Is(err, monero.ErrExpired), errors.Is(err, monero.ErrPaid):
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
		return
	case errors.Is(err, monero.ErrNoMonero):
		badRequest(w)
		return
	case moneroBusy(err):
		to += "?busy=1"
	default:
		internal(w)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func matches(err error, list []error) bool {
	for _, e := range list {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// onchainBusy reports the on-chain refusals that count as busy; none of them stops the Lightning step.
func onchainBusy(err error) bool {
	return matches(err, []error{onchain.ErrAddress, onchain.ErrIndexBehind, onchain.ErrBusy, onchain.ErrUnavailable}) || busy(err)
}

// moneroBusy is onchainBusy for the Monero rail: the refusals of Address that count as busy (ErrUnavailable includes
// the not-yet-checked error).
func moneroBusy(err error) bool {
	return matches(err, []error{monero.ErrSubaddress, monero.ErrIndexBehind, monero.ErrBusy, monero.ErrUnavailable}) || busy(err)
}

// busy reports the refusals the customer may retry: a limit (the per-account cap included), an
// invoice that was not made, an unknown outcome, and a request whose own context ended (the browser
// aborted it, for example by a second press, or a deadline passed). ErrAmount is an operator's
// misconfiguration (an order made under other limits); lnpay logs it, and the customer sees busy.
func busy(err error) bool {
	return matches(err, []error{lnpay.ErrNotCreated, lnpay.ErrUnknown, limits.ErrRate, limits.ErrOpenCap,
		limits.ErrClassShare, limits.ErrAccountCap, limits.ErrDailyCount, limits.ErrDailyAmount, limits.ErrAmount,
		context.Canceled, context.DeadlineExceeded})
}

func (h *handler) qr(w http.ResponseWriter, r *http.Request) {
	so, unavailable, ok := h.load(w, r)
	if !ok {
		return
	}
	var text string
	var has bool
	// No rail is today's QR; exactly rail=monero the Monero one; any other rail value (empty included) is the 404.
	switch rails, named := r.URL.Query()["rail"]; {
	case !named:
		text, has = qrText(shown(so, h.now()))
	case len(rails) == 1 && rails[0] == railMonero:
		text = moneroText(shown(so, h.now()))
		has = text != ""
	}
	if unavailable || !has || len(text) > qr.MaxInput {
		notFound(w)
		return
	}
	svg, err := qr.SVG(text)
	if err != nil {
		internal(w)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "image/svg+xml") // Cache-Control stays no-store: the URL serves each new invoice of the order
	hd.Set("Content-Length", strconv.Itoa(len(svg)))
	w.Write(svg)
}

func (h *handler) style(w http.ResponseWriter, _ *http.Request) {
	hd := w.Header()
	hd.Set("Content-Type", "text/css; charset=utf-8")
	hd.Set("Cache-Control", "public, max-age=3600")
	w.Write(styleCSS)
}

func (h *handler) logo(w http.ResponseWriter, _ *http.Request) {
	hd := w.Header()
	hd.Set("Content-Type", "image/svg+xml")
	hd.Set("Cache-Control", "public, max-age=3600")
	w.Write(logoSVG)
}
