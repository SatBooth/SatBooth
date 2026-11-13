// Package api is the merchant's private API: create an order, read its status. It runs on the
// private listener, never on the internet, and every request needs an API key (Bearer) that holds
// the route's capability. Responses are built from an allowlist of fields; error bodies are fixed
// strings that never echo the request.
//
// Routes, and nothing else:
//
//	POST /v1/orders                     orders:write
//	GET  /v1/orders/{id}                orders:read
//	POST /v1/refunds                    refunds:write
//	GET  /v1/refunds/{refund_id}        refunds:read
//	POST /v1/refunds/{refund_id}/void   refunds:write
//
// Every other path, and every other method on these paths, answers 404 with the fixed not_found
// body, whatever the credentials: the API does not say which routes exist beyond the five it has. A refund id is
// 32 lowercase hex characters, else the path is not a route.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/refund"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
)

// Limits of the request.
const (
	MaxBodyBytes     = 16 << 10
	MinExpiresIn     = 300   // seconds
	MaxExpiresIn     = 86400 // seconds
	DefaultExpiresIn = 3600  // seconds
	maxAmountMsat    = 1 << 53
	maxBearer        = 512
)

// The fixed error codes. The body is always {"error":"<code>"}.
const (
	codeInvalidJSON      = "invalid_json"
	codeBodyTooLarge     = "body_too_large"
	codeInvalidAccountID = "invalid_account_id"
	codeInvalidRef       = "invalid_merchant_ref"
	codeInvalidAmount    = "invalid_lightning_msat"
	codeInvalidSat       = "invalid_onchain_sat"
	codeInvalidPiconero  = "invalid_monero_piconero"
	codeMissingAmount    = "missing_amount"
	codeOnchainDisabled  = "onchain_disabled"
	codeOnchainDown      = "onchain_unavailable"
	codeMoneroDisabled   = "monero_disabled"
	codeMoneroDown       = "monero_unavailable"
	codeInvalidExpiresIn = "invalid_expires_in"
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeNotFound         = "not_found"
	codeConflict         = "conflict"
	codeInternal         = "internal"
	codeInvalidPrice     = "invalid_price"
	codeInvalidCurrency  = "invalid_currency"
	codeInvalidRails     = "invalid_rails"
	codeMixedPricing     = "mixed_pricing"
	codeFiatDisabled     = "fiat_disabled"
	codeRateUnavailable  = "rate_unavailable"

	codeRefundsDisabled   = "refunds_disabled"
	codeInvalidApproval   = "invalid_approval"
	codeApprovalExpired   = "approval_expired"
	codeRefundExists      = "refund_exists"
	codeUnknownOrder      = "unknown_order"
	codeInvalidInvoice    = "invalid_invoice"
	codeOwnInvoice        = "own_invoice"
	codeExceedsRefundable = "exceeds_refundable"
	codeDailyCap          = "daily_cap"
	codeTooManyOpen       = "too_many_open"
	codeRefundInFlight    = "refund_in_flight"
	codeHubUnavailable    = "hub_unavailable"
	codeInvalidState      = "invalid_state"
)

// The rail names of a priced request's "rails".
const (
	railLightning = "lightning"
	railOnchain   = "onchain"
	railMonero    = "monero"
)

// rateRetryAfter is the Retry-After of a rate_unavailable answer, in seconds.
const rateRetryAfter = "10"

// Pricer converts a fiat price to an asset's base units at the current rate (*rates.Service). Currencies lists the
// currencies it is configured for; Reachable reports whether a configured set of providers quotes the asset in the
// currency at all (a permanent fact of the configuration, not an outage).
type Pricer interface {
	Price(asset string, price rates.Decimal, currency string) (rates.Priced, error)
	Currencies() []string
	Reachable(asset, currency string) bool
}

// Refunder is the refund service (*refund.Service): claim a refund, read it, void it. Get reports false for a refund
// that does not exist. Pass a plain nil when refunds are not configured (every refund route then answers 404
// refunds_disabled), never a typed nil pointer.
type Refunder interface {
	Claim(ctx context.Context, approvalHex, invoice string) (refund.View, error)
	Get(ctx context.Context, id string) (refund.View, bool, error)
	Void(ctx context.Context, id, approvalHex string) (refund.View, error)
}

// Figures returns an order's Lightning figures in msat: what it was paid, what settled refunds took back, and what
// open refunds (claimed or sent) hold. The order view carries them whether or not refunds are on. Nil omits them.
type Figures func(ctx context.Context, orderID string) (paidMsat, refundedMsat, openMsat int64, err error)

// Handler returns the API. checkoutBase is the public origin of the checkout listener (for example
// https://pay.example.com); an order's checkout_url is checkoutBase + "/c/" + the order's token. pr is a plain nil
// when fiat pricing is not configured (a priced request then answers fiat_disabled): pass it only inside the
// configured branch, never a typed nil pointer (a nil *rates.Service in the interface is not == nil). The same holds
// for rf; fig may be nil.
func Handler(keys *Keys, st *store.Store, res *status.Resolver, checkoutBase string, pr Pricer, rf Refunder, fig Figures) http.Handler {
	h := newHandler(keys, st, res, checkoutBase, pr, time.Now).(*handler)
	h.rf, h.fig = rf, fig
	return h
}

type handler struct {
	keys *Keys
	st   *store.Store
	res  *status.Resolver
	base string
	pr   Pricer
	rf   Refunder
	fig  Figures
	now  func() time.Time
}

func newHandler(keys *Keys, st *store.Store, res *status.Resolver, checkoutBase string, pr Pricer, now func() time.Time) http.Handler {
	if keys == nil || st == nil || res == nil || res.Ln == nil || checkoutBase == "" || now == nil {
		panic("api: Handler needs keys, store, resolver (with its Lightning rail) and checkout base")
	}
	return &handler{keys: keys, st: st, res: res, base: strings.TrimSuffix(checkoutBase, "/"), pr: pr, now: now}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var need string
	var run func(http.ResponseWriter, *http.Request)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/orders":
		need, run = CapWrite, h.create
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/orders/") &&
		len(r.URL.Path) > len("/v1/orders/") && !strings.Contains(r.URL.Path[len("/v1/orders/"):], "/"):
		need, run = CapRead, h.get
	default:
		need, run = h.refundRoute(r)
		if run == nil {
			writeError(w, http.StatusNotFound, codeNotFound)
			return
		}
	}
	key, ok := bearer(r)
	bits := uint8(0)
	if ok {
		bits = h.keys.identify(key)
	}
	switch {
	case bits == 0:
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, codeUnauthorized)
	case bits&capBit(need) == 0:
		writeError(w, http.StatusForbidden, codeForbidden)
	default:
		run(w, r)
	}
}

// bearer returns the key of an "Authorization: Bearer <key>" header.
func bearer(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(v) <= len(p) || v[:len(p)] != p || len(v) > len(p)+maxBearer {
		return "", false
	}
	return v[len(p):], true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":"`+codeInternal+`"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// createReq is a decoded, validated POST /v1/orders body. It is either explicit (an amount set, no Price) or priced
// (Price and Currency set, every amount 0).
type createReq struct {
	AccountID      string
	MerchantRef    string
	AmountMsat     int64    // 0 when the order has no Lightning rail, or is priced
	AmountSat      int64    // 0 when the order has no on-chain rail, or is priced
	AmountPiconero int64    // 0 when the order has no Monero rail, or is priced
	Price          string   // the price as sent (a positive decimal); "" for an explicit order
	Currency       string   // the price's currency; "" for an explicit order
	Rails          []string // the rails a priced order names, distinct; nil when it names none
	ExpiresIn      int64    // seconds
}

// parseCreate decodes the body strictly: one JSON object, no duplicate keys (at any depth), no
// unknown keys (matched exactly, so no case variants), the right types. It returns the request,
// or a fixed error code. It checks everything that needs no service; the Lightning amount's bounds
// are the limiter's, whether the on-chain and Monero rails exist is the resolver's, and whether the currency is
// configured is the pricer's, all checked by the caller.
func parseCreate(b []byte) (createReq, string) {
	m, ok := strictObject(b, []string{"account_id", "merchant_ref"},
		[]string{"lightning_msat", "onchain_sat", "monero_piconero", "expires_in", "price", "currency", "rails"})
	if !ok {
		return createReq{}, codeInvalidJSON
	}
	var req createReq
	var okA, okR bool
	req.AccountID, okA = rawString(m["account_id"])
	req.MerchantRef, okR = rawString(m["merchant_ref"])
	switch {
	case !okA || !validName(req.AccountID):
		return createReq{}, codeInvalidAccountID
	case !okR || !validName(req.MerchantRef):
		return createReq{}, codeInvalidRef
	}
	// The pricing members. Price is a JSON string, never a number: a number would pass through a float somewhere.
	rawPrice, hasPrice := m["price"]
	rawCurrency, hasCurrency := m["currency"]
	rawRails, hasRails := m["rails"]
	if hasPrice {
		p, ok := rawString(rawPrice)
		if !ok {
			return createReq{}, codeInvalidPrice
		}
		if d, err := rates.ParseDecimal(p); err != nil || d.IsZero() {
			return createReq{}, codeInvalidPrice
		}
		req.Price = p // as sent: the store matches a repeat's price text byte for byte
	}
	if hasCurrency {
		c, ok := rawString(rawCurrency)
		if !ok || !rates.ValidCurrency(c) {
			return createReq{}, codeInvalidCurrency
		}
		req.Currency = c
	}
	switch {
	case hasCurrency && !hasPrice:
		return createReq{}, codeInvalidPrice
	case hasPrice && !hasCurrency:
		return createReq{}, codeInvalidCurrency
	}
	if hasRails {
		rails, ok := parseRails(rawRails)
		if !ok || !hasPrice {
			return createReq{}, codeInvalidRails
		}
		req.Rails = rails
	}
	_, hasMsat := m["lightning_msat"]
	_, hasSat := m["onchain_sat"]
	_, hasPico := m["monero_piconero"]
	if hasPrice && (hasMsat || hasSat || hasPico) {
		return createReq{}, codeMixedPricing
	}
	// A member that is present must be valid, null and zero included: only an absent one means "no rail".
	if raw, present := m["lightning_msat"]; present {
		amt, ok := rawInt(raw)
		if !ok || amt < 1 || amt > maxAmountMsat || amt%1000 != 0 {
			return createReq{}, codeInvalidAmount
		}
		req.AmountMsat = amt
	}
	if raw, present := m["onchain_sat"]; present {
		sat, ok := rawInt(raw)
		if !ok || sat < 1 || sat > store.MaxSat {
			return createReq{}, codeInvalidSat
		}
		req.AmountSat = sat
	}
	if raw, present := m["monero_piconero"]; present {
		pico, ok := rawInt(raw)
		if !ok || pico < 1 || pico > store.MaxPiconero {
			return createReq{}, codeInvalidPiconero
		}
		req.AmountPiconero = pico
	}
	if !hasPrice && req.AmountMsat == 0 && req.AmountSat == 0 && req.AmountPiconero == 0 {
		return createReq{}, codeMissingAmount
	}
	req.ExpiresIn = DefaultExpiresIn
	if raw, present := m["expires_in"]; present {
		n, ok := rawInt(raw)
		if !ok || n < MinExpiresIn || n > MaxExpiresIn {
			return createReq{}, codeInvalidExpiresIn
		}
		req.ExpiresIn = n
	}
	return req, ""
}

// parseRails reads a "rails" member: an array of 1 to 3 distinct strings, each "lightning", "onchain" or "monero".
func parseRails(raw json.RawMessage) ([]string, bool) {
	l, ok := rawArray(raw)
	if !ok || len(l) < 1 || len(l) > 3 {
		return nil, false
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, ok := rawString(e)
		if !ok || s != railLightning && s != railOnchain && s != railMonero {
			return nil, false
		}
		for _, prev := range out {
			if prev == s {
				return nil, false
			}
		}
		out = append(out, s)
	}
	return out, true
}

// rawInt returns the value of a JSON member that is an integer literal (no fraction, no exponent).
func rawInt(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(raw), 10, 64)
	return n, err == nil
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusBadRequest, codeBodyTooLarge)
		} else {
			writeError(w, http.StatusBadRequest, codeInvalidJSON)
		}
		return
	}
	req, code := parseCreate(body)
	if code != "" {
		writeError(w, http.StatusBadRequest, code)
		return
	}
	order := store.Order{AccountID: req.AccountID, MerchantRef: req.MerchantRef, AmountMsat: req.AmountMsat, AmountSat: req.AmountSat,
		AmountPiconero: req.AmountPiconero, ExpiresAt: h.now().Add(time.Duration(req.ExpiresIn) * time.Second)}
	var quote *store.Quote
	if req.Price != "" {
		// The reference is looked up before any pricing (store.PricedRepeat, the store's own rule). A repeat returns
		// the existing order as it was first priced: a merchant who lost the first answer recovers the order during a
		// rate outage, after fiat pricing was switched off, or when the moved rate would push a rail out of bounds. Any
		// other order under the reference is the store's 409, whatever pricing would answer. An unused reference is
		// priced; the store's check decides a race.
		ln, oc, xm := h.rails(req)
		existing, err := h.st.PricedRepeat(r.Context(), store.PricedRequest{AccountID: req.AccountID, MerchantRef: req.MerchantRef,
			Price: req.Price, Currency: req.Currency, Lightning: ln, Onchain: oc, Monero: xm})
		switch {
		case err == nil:
			h.respond(r.Context(), w, http.StatusOK, existing)
			return
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, codeConflict)
			return
		case !errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusInternalServerError, codeInternal)
			return
		}
		var httpStatus int
		var retry bool
		quote, httpStatus, code, retry = h.price(req, &order)
		if code != "" {
			if retry {
				w.Header().Set("Retry-After", rateRetryAfter)
			}
			writeError(w, httpStatus, code)
			return
		}
	}
	switch {
	// A repeat POST of an on-chain order while the rail is off also answers onchain_disabled, by design.
	case order.AmountSat > 0 && !h.res.HasOnchain():
		code = codeOnchainDisabled
	// Likewise monero_disabled for the Monero rail.
	case order.AmountPiconero > 0 && !h.res.HasMonero():
		code = codeMoneroDisabled
	case order.AmountMsat > 0 && (order.AmountMsat > maxAmountMsat || h.res.Ln.ValidateAmount(order.AmountMsat) != nil):
		code = codeInvalidAmount
	}
	if code != "" {
		writeError(w, http.StatusBadRequest, code)
		return
	}
	var o store.Order
	var created bool
	if quote != nil {
		o, created, err = h.st.CreateQuotedOrder(r.Context(), order, *quote)
	} else {
		o, created, err = h.st.CreateOrder(r.Context(), order)
	}
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, codeConflict)
		return
	case errors.Is(err, store.ErrInvalid) && quote == nil:
		writeError(w, http.StatusBadRequest, codeInvalidJSON)
		return
	case err != nil:
		// A priced request was validated in full before the store, so an ErrInvalid there is ours, not the client's.
		writeError(w, http.StatusInternalServerError, codeInternal)
		return
	}
	httpStatus := http.StatusOK
	if created {
		httpStatus = http.StatusCreated
	}
	h.respond(r.Context(), w, httpStatus, o)
}

// rails is a priced request's rail set. Without "rails" it is the operator's configuration: Lightning, and on-chain
// when that rail is configured; never Monero, which a request names. A retry after the operator changed it therefore
// names another rail set, and the store answers 409, by design.
func (h *handler) rails(req createReq) (ln, oc, xm bool) {
	rails := req.Rails
	if rails == nil {
		rails = []string{railLightning}
		if h.res.HasOnchain() {
			rails = append(rails, railOnchain)
		}
	}
	for _, r := range rails {
		ln = ln || r == railLightning
		oc = oc || r == railOnchain
		xm = xm || r == railMonero
	}
	return ln, oc, xm
}

// price converts a priced request at the current rates: it sets the amount of each of its rails in o and returns the
// quote to record, or an HTTP status, a fixed code and whether the answer carries Retry-After (rate_unavailable
// only; the caller sets the header). Nothing is stored or written here, and no provider text reaches the answer. The
// checks that need no rate (fiat configured, currency configured, the on-chain and Monero rails present, the XMR pair
// reachable) come first, so a request they refuse never reaches the pricer. The BTC quote prices the Lightning and
// on-chain rails, the XMR quote the Monero rail; the quote records one rate per asset priced.
//
// A request under a used reference never gets here: store.PricedRepeat answers it first.
func (h *handler) price(req createReq, o *store.Order) (q *store.Quote, status int, code string, retry bool) {
	if h.pr == nil {
		return nil, http.StatusBadRequest, codeFiatDisabled, false
	}
	configured := false
	for _, c := range h.pr.Currencies() {
		if c == req.Currency {
			configured = true
		}
	}
	if !configured {
		return nil, http.StatusBadRequest, codeInvalidCurrency, false
	}
	ln, oc, xm := h.rails(req)
	switch {
	case oc && !h.res.HasOnchain():
		return nil, http.StatusBadRequest, codeOnchainDisabled, false
	case xm && !h.res.HasMonero():
		return nil, http.StatusBadRequest, codeMoneroDisabled, false
	case xm && !h.pr.Reachable(rates.AssetXMR, req.Currency):
		// Ruling 8-A: no configured set of providers quotes XMR in this currency. Permanent: no Retry-After.
		return nil, http.StatusBadRequest, codeInvalidCurrency, false
	}
	d, err := rates.ParseDecimal(req.Price)
	if err != nil { // parseCreate accepted it
		return nil, http.StatusInternalServerError, codeInternal, false
	}
	out := &store.Quote{Price: req.Price, Currency: req.Currency}
	first := true
	// quote prices one asset, maps its errors (rangeCode for ErrRange), and records its rate in out. The quote time is
	// the oldest of the quotes', and every quote must carry the same margin.
	quote := func(asset, rangeCode string, max int64) (int64, int, string, bool) {
		priced, err := h.pr.Price(asset, d, req.Currency)
		switch {
		case errors.Is(err, rates.ErrTooFew), errors.Is(err, rates.ErrDisagree), errors.Is(err, rates.ErrCurrency),
			errors.Is(err, rates.ErrDuplicate):
			return 0, http.StatusServiceUnavailable, codeRateUnavailable, true
		case errors.Is(err, rates.ErrUnreachable):
			return 0, http.StatusBadRequest, codeInvalidCurrency, false
		case errors.Is(err, rates.ErrRange):
			return 0, http.StatusBadRequest, rangeCode, false
		case err != nil:
			return 0, http.StatusInternalServerError, codeInternal, false
		case priced.Amount < 1 || priced.Amount > max, priced.Quote.Asset != asset, priced.Quote.Currency != req.Currency,
			!first && priced.MarginBps != out.MarginBps:
			// Convert and Aggregate guarantee all of these, and the service has one margin; refuse rather than record a
			// quote of something else.
			return 0, http.StatusInternalServerError, codeInternal, false
		}
		if first || priced.Quote.At.Before(out.QuotedAt) {
			out.QuotedAt = priced.Quote.At
		}
		first, out.MarginBps = false, priced.MarginBps
		r := store.AssetRate{Asset: asset, Rate: priced.Quote.Rate.String()}
		for _, s := range priced.Quote.Sources {
			r.Sources = append(r.Sources, store.QuoteSource{Provider: s.Provider, Rate: s.Value.String(), At: s.At})
		}
		out.Rates = append(out.Rates, r)
		return priced.Amount, 0, "", false
	}
	var sats, pico int64
	if ln || oc {
		rangeCode := codeInvalidAmount
		if oc {
			rangeCode = codeInvalidSat
		}
		if sats, status, code, retry = quote(rates.AssetBTC, rangeCode, store.MaxSat); code != "" {
			return nil, status, code, retry
		}
	}
	if xm {
		if pico, status, code, retry = quote(rates.AssetXMR, codeInvalidPiconero, store.MaxPiconero); code != "" {
			return nil, status, code, retry
		}
	}
	// Each rail gets exactly the converted amount; the caller checks the Lightning bounds and refuses, never clamps.
	if ln {
		o.AmountMsat = sats * 1000 // at most MaxSat × 1000, inside int64
	}
	if oc {
		o.AmountSat = sats
	}
	if xm {
		o.AmountPiconero = pico
	}
	return out, 0, "", false
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/orders/")
	o, err := h.st.OrderByID(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal)
		return
	}
	h.respond(r.Context(), w, http.StatusOK, o)
}

// orderView is the allowlist: the only fields an API response carries. It has no invoice, payment
// hash, preimage or internal state. lightning_msat, onchain_sat and monero_piconero are 0 for a rail the
// order does not have, and that rail's object is null. monero_piconero is at most 2^53 − 1, so a JavaScript
// JSON.parse reads it exactly, as it does onchain_sat.
type orderView struct {
	ID             string       `json:"id"`
	MerchantRef    string       `json:"merchant_ref"`
	AccountID      string       `json:"account_id"`
	AmountMsat     int64        `json:"lightning_msat"`
	AmountSat      int64        `json:"onchain_sat"`
	AmountPiconero int64        `json:"monero_piconero"`
	State          string       `json:"state"`
	CreatedAt      string       `json:"created_at"`
	ExpiresAt      string       `json:"expires_at"`
	PaidAt         *string      `json:"paid_at"`
	CheckoutURL    string       `json:"checkout_url"`
	Lightning      *lightningV  `json:"lightning"`
	Onchain        *onchainView `json:"onchain"`
	Monero         *moneroView  `json:"monero"`
	Pricing        *pricingView `json:"pricing"`
}

// pricingView is the price of a priced order and the rates it was converted at, as recorded when it was created
// (null for an order created with explicit amounts). rates holds one member per asset priced, "BTC" for the
// Lightning and on-chain amounts and "XMR" for the Monero amount. margin_percent is the operator's margin in percent;
// times are RFC 3339 UTC in whole seconds.
type pricingView struct {
	Price         string                   `json:"price"`
	Currency      string                   `json:"currency"`
	MarginPercent string                   `json:"margin_percent"`
	QuotedAt      string                   `json:"quoted_at"`
	Rates         map[string]assetRateView `json:"rates"`
}

// assetRateView is one asset's rate (the currency per coin, before the margin) and the provider rates behind it.
type assetRateView struct {
	Rate    string       `json:"rate"`
	Sources []sourceView `json:"sources"`
}

type sourceView struct {
	Provider string `json:"provider"`
	Rate     string `json:"rate"`
	At       string `json:"at"`
}

func newPricingView(q store.Quote) *pricingView {
	v := &pricingView{Price: q.Price, Currency: q.Currency, MarginPercent: rates.FormatPercent(q.MarginBps),
		QuotedAt: q.QuotedAt.UTC().Format(time.RFC3339), Rates: make(map[string]assetRateView, len(q.Rates))}
	for _, r := range q.Rates {
		a := assetRateView{Rate: r.Rate, Sources: make([]sourceView, len(r.Sources))}
		for i, s := range r.Sources {
			a.Sources[i] = sourceView{Provider: s.Provider, Rate: s.Rate, At: s.At.UTC().Format(time.RFC3339)}
		}
		v.Rates[r.Asset] = a
	}
	return v
}

// lightningV is the Lightning rail as the merchant sees it. The three figures are in msat and are absent when the
// handler has no Figures: paid_msat is what the order was paid, refunded_msat what settled refunds took back and
// refund_open_msat what claimed or sent refunds hold.
type lightningV struct {
	State          string `json:"state"`
	PaidMsat       *int64 `json:"paid_msat,omitempty"`
	RefundedMsat   *int64 `json:"refunded_msat,omitempty"`
	RefundOpenMsat *int64 `json:"refund_open_msat,omitempty"`
}

// onchainView is the on-chain rail as the merchant sees it. late_sat and overpaid stay visible on a
// paid order, so that the surplus can be refunded. stale is Rail.Stale, the value the checkout page
// uses for "Checking payment…": the watcher has had no good pass for a while, or its last pass could
// not bring this order up to date (a failed settle or output call), so the values may be old
// (re-read until it is false before acting on expired).
type onchainView struct {
	State                 string  `json:"state"`
	Address               *string `json:"address"` // null until issued
	RequiredConfirmations int     `json:"required_confirmations"`
	Confirmations         int     `json:"confirmations"`
	ReceivedSat           int64   `json:"received_sat"`
	ConfirmedSat          int64   `json:"confirmed_sat"`
	LateSat               int64   `json:"late_sat"`
	Overpaid              bool    `json:"overpaid"`
	Stale                 bool    `json:"stale"`
}

// moneroView is the Monero rail as the merchant sees it, in piconero: the members of onchainView, from rail.View.
type moneroView struct {
	State                 string  `json:"state"`
	Address               *string `json:"address"` // null until issued
	RequiredConfirmations int     `json:"required_confirmations"`
	Confirmations         int     `json:"confirmations"`
	ReceivedPiconero      int64   `json:"received_piconero"`
	ConfirmedPiconero     int64   `json:"confirmed_piconero"`
	LatePiconero          int64   `json:"late_piconero"`
	Overpaid              bool    `json:"overpaid"`
	Stale                 bool    `json:"stale"`
}

func newMoneroView(v rail.View) *moneroView {
	m := &moneroView{State: v.State, RequiredConfirmations: v.RequiredConfirmations, Confirmations: v.Confirmations,
		ReceivedPiconero: v.Received, ConfirmedPiconero: v.Confirmed, LatePiconero: v.Late, Overpaid: v.Overpaid, Stale: v.Stale}
	if v.Issued {
		a := v.Address
		m.Address = &a
	}
	return m
}

func (h *handler) respond(ctx context.Context, w http.ResponseWriter, code int, o store.Order) {
	so, err := h.res.ByID(ctx, o.ID)
	switch {
	case errors.Is(err, status.ErrMoneroUnavailable):
		writeError(w, http.StatusServiceUnavailable, codeMoneroDown)
		return
	case errors.Is(err, status.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, codeOnchainDown)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal)
		return
	}
	v := orderView{ID: o.ID, MerchantRef: o.MerchantRef, AccountID: o.AccountID, AmountMsat: o.AmountMsat, AmountSat: o.AmountSat,
		AmountPiconero: o.AmountPiconero, State: so.State, CreatedAt: o.CreatedAt.UTC().Format(time.RFC3339),
		ExpiresAt: o.ExpiresAt.UTC().Format(time.RFC3339), CheckoutURL: h.base + "/c/" + o.Token}
	if so.State == status.StatePaid {
		p := so.PaidAt.UTC().Format(time.RFC3339)
		v.PaidAt = &p
	}
	if so.Lightning != nil {
		v.Lightning = &lightningV{State: string(so.Lightning.State)}
		if h.fig != nil {
			paid, refunded, open, err := h.fig(ctx, o.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal)
				return
			}
			v.Lightning.PaidMsat, v.Lightning.RefundedMsat, v.Lightning.RefundOpenMsat = &paid, &refunded, &open
		}
	}
	if rl := so.Onchain; rl != nil {
		v.Onchain = railView(*rl)
	}
	if xv := so.Monero; xv != nil {
		v.Monero = newMoneroView(*xv)
	}
	if so.Quote != nil {
		v.Pricing = newPricingView(*so.Quote)
	}
	writeJSON(w, code, v)
}

func railView(rl onchain.Rail) *onchainView {
	v := &onchainView{State: rl.State, RequiredConfirmations: rl.RequiredConfirmations, Confirmations: rl.Confirmations,
		ReceivedSat: rl.ReceivedSat, ConfirmedSat: rl.ConfirmedSat, LateSat: rl.LateSat, Overpaid: rl.Overpaid, Stale: rl.Stale}
	if rl.Issued {
		a := rl.Address
		v.Address = &a
	}
	return v
}
