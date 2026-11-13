package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/satbooth/satbooth/refund"
)

const refundsPath = "/v1/refunds"

// refundRoute matches the three refund routes exactly: POST /v1/refunds, GET /v1/refunds/{id} and
// POST /v1/refunds/{id}/void, where the id is 32 lowercase hex characters. Anything else (another method, a
// trailing slash, extra segments, another id shape) is no route: run is nil and the caller answers not_found.
func (h *handler) refundRoute(r *http.Request) (need string, run func(http.ResponseWriter, *http.Request)) {
	p := r.URL.Path
	if p == refundsPath {
		if r.Method == http.MethodPost {
			return CapRefundsWrite, h.refundClaim
		}
		return "", nil
	}
	rest, ok := strings.CutPrefix(p, refundsPath+"/")
	if !ok {
		return "", nil
	}
	id, void := strings.CutSuffix(rest, "/void")
	if !lowerHex(id, 32) {
		return "", nil
	}
	switch {
	case void && r.Method == http.MethodPost:
		return CapRefundsWrite, h.refundVoid
	case !void && r.Method == http.MethodGet:
		return CapRefundsRead, h.refundGet
	}
	return "", nil
}

// refundView is the allowlist of a refund response. Times are RFC 3339 UTC in whole seconds, as in the order view.
// reason is absent unless the refund failed and resolved_at unless it resolved. It has no invoice, payment hash,
// preimage, approval or key id.
type refundView struct {
	ID         string  `json:"id"`
	OrderID    string  `json:"order_id"`
	AmountMsat int64   `json:"amount_msat"`
	State      string  `json:"state"`
	Reason     string  `json:"reason,omitempty"`
	ClaimedAt  string  `json:"claimed_at"`
	ResolvedAt *string `json:"resolved_at,omitempty"`
}

func newRefundView(v refund.View) refundView {
	out := refundView{ID: v.ID, OrderID: v.OrderID, AmountMsat: v.AmountMsat, State: v.State, Reason: v.Reason,
		ClaimedAt: v.ClaimedAt.UTC().Format(time.RFC3339)}
	if !v.ResolvedAt.IsZero() {
		t := v.ResolvedAt.UTC().Format(time.RFC3339)
		out.ResolvedAt = &t
	}
	return out
}

// refundError maps a refund sentinel to its HTTP status and fixed code; anything else is internal.
func refundError(err error) (int, string) {
	switch {
	case errors.Is(err, refund.ErrApproval):
		return http.StatusBadRequest, codeInvalidApproval
	case errors.Is(err, refund.ErrApprovalExpired):
		return http.StatusBadRequest, codeApprovalExpired
	case errors.Is(err, refund.ErrRefundExists):
		return http.StatusConflict, codeRefundExists
	case errors.Is(err, refund.ErrUnknownOrder):
		return http.StatusNotFound, codeUnknownOrder
	case errors.Is(err, refund.ErrInvoice):
		return http.StatusBadRequest, codeInvalidInvoice
	case errors.Is(err, refund.ErrOwnInvoice):
		return http.StatusBadRequest, codeOwnInvoice
	case errors.Is(err, refund.ErrExceedsRefundable):
		return http.StatusConflict, codeExceedsRefundable
	case errors.Is(err, refund.ErrDailyCap):
		return http.StatusTooManyRequests, codeDailyCap
	case errors.Is(err, refund.ErrTooManyOpen):
		return http.StatusTooManyRequests, codeTooManyOpen
	case errors.Is(err, refund.ErrInFlight):
		return http.StatusConflict, codeRefundInFlight
	case errors.Is(err, refund.ErrHubUnavailable):
		return http.StatusServiceUnavailable, codeHubUnavailable
	case errors.Is(err, refund.ErrState):
		return http.StatusConflict, codeInvalidState
	case errors.Is(err, refund.ErrNotFound):
		return http.StatusNotFound, codeNotFound
	}
	return http.StatusInternalServerError, codeInternal
}

// readStrings reads the request body (at most MaxBodyBytes) as an object with exactly the given string members. On
// a refusal it has written the answer and returns false.
func readStrings(w http.ResponseWriter, r *http.Request, keys ...string) (map[string]string, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusBadRequest, codeBodyTooLarge)
		} else {
			writeError(w, http.StatusBadRequest, codeInvalidJSON)
		}
		return nil, false
	}
	m, ok := strictObject(body, keys, nil)
	if !ok {
		writeError(w, http.StatusBadRequest, codeInvalidJSON)
		return nil, false
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		s, ok := rawString(m[k])
		if !ok {
			writeError(w, http.StatusBadRequest, codeInvalidJSON)
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

func (h *handler) refundClaim(w http.ResponseWriter, r *http.Request) {
	if h.rf == nil {
		writeError(w, http.StatusNotFound, codeRefundsDisabled)
		return
	}
	m, ok := readStrings(w, r, "approval", "invoice")
	if !ok {
		return
	}
	v, err := h.rf.Claim(r.Context(), m["approval"], m["invoice"])
	if err != nil {
		st, code := refundError(err)
		writeError(w, st, code)
		return
	}
	writeJSON(w, http.StatusAccepted, newRefundView(v))
}

func (h *handler) refundGet(w http.ResponseWriter, r *http.Request) {
	if h.rf == nil {
		writeError(w, http.StatusNotFound, codeRefundsDisabled)
		return
	}
	v, found, err := h.rf.Get(r.Context(), strings.TrimPrefix(r.URL.Path, refundsPath+"/"))
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal)
	case !found:
		writeError(w, http.StatusNotFound, codeNotFound)
	default:
		writeJSON(w, http.StatusOK, newRefundView(v))
	}
}

// refundVoid passes a signed void to the refund service, which ends any open refund the hub does not report pending:
// a claimed one answers 200, failed with the reason cancelled, or settled when the ledger already shows it paid. Its
// refusals are the existing codes.
func (h *handler) refundVoid(w http.ResponseWriter, r *http.Request) {
	if h.rf == nil {
		writeError(w, http.StatusNotFound, codeRefundsDisabled)
		return
	}
	m, ok := readStrings(w, r, "approval")
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, refundsPath+"/"), "/void")
	v, err := h.rf.Void(r.Context(), id, m["approval"])
	if err != nil {
		st, code := refundError(err)
		writeError(w, st, code)
		return
	}
	writeJSON(w, http.StatusOK, newRefundView(v))
}
