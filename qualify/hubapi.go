//go:build qualify

package qualify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// limitedSpacing is how far apart the rate-limited admin routes are called. The hub allows one request per second
// with a burst of 2 on /api/start, /api/unlock, /api/backup and POST /api/apps (getAlby/hub 921003b
// http/http_service.go:100-114, 178).
const limitedSpacing = 1100 * time.Millisecond

// hubAPI is a client for one hub's HTTP admin API (getAlby/hub 921003b http/http_service.go:96-208). It sends
// the JWT as Authorization: Bearer once it has one.
type hubAPI struct {
	base  string
	token string
	hc    *http.Client

	mu          sync.Mutex
	lastLimited time.Time
}

func newHubAPI(base, token string) *hubAPI {
	return &hubAPI{base: base, token: token, hc: &http.Client{Timeout: 90 * time.Second}}
}

// apiError is a non-2xx answer: the hub sends {"message": ...}.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("hub answered %d (message of %d bytes)", e.Status, len(e.Message))
}

// do sends one request. in is JSON-encoded when not nil; out receives the JSON answer when not nil. limited
// marks the rate-limited routes: they are spaced at least limitedSpacing apart.
func (h *hubAPI) do(method, path string, in, out any, limited bool) error {
	if limited {
		h.mu.Lock()
		if wait := time.Until(h.lastLimited.Add(limitedSpacing)); wait > 0 {
			time.Sleep(wait) // a rate-limit spacing, not a synchronisation
		}
		h.lastLimited = time.Now()
		h.mu.Unlock()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = fmt.Sprintf("(%d bytes)", len(raw))
		}
		return &apiError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: bad JSON answer (%s)", method, path, jsonReason(err))
		}
	}
	return nil
}

type hubInfo struct {
	SetupCompleted bool   `json:"setupCompleted"`
	Running        bool   `json:"running"`
	Unlocked       bool   `json:"unlocked"`
	Network        string `json:"network"`
	StartupState   string `json:"startupState"`
	StartupError   string `json:"startupError"`
}

// info is GET /api/info, which needs no token.
func (h *hubAPI) info() (*hubInfo, error) {
	var i hubInfo
	if err := h.do("GET", "/api/info", nil, &i, false); err != nil {
		return nil, err
	}
	return &i, nil
}

// setup is POST /api/setup (204). Setup alone does not start the node.
func (h *hubAPI) setup() error {
	return h.do("POST", "/api/setup", map[string]any{"backendType": "LDK", "unlockPassword": unlockPassword}, nil, false)
}

// start is POST /api/start. The node starts asynchronously; the answer carries the token, which is kept.
func (h *hubAPI) start() (string, error) {
	var r struct {
		Token string `json:"token"`
	}
	if err := h.do("POST", "/api/start", map[string]any{"unlockPassword": unlockPassword}, &r, true); err != nil {
		return "", err
	}
	h.token = r.Token
	return r.Token, nil
}

// unlock is POST /api/unlock (400 while the node is not running); the new token is kept.
func (h *hubAPI) unlock() (string, error) {
	var r struct {
		Token string `json:"token"`
	}
	err := h.do("POST", "/api/unlock", map[string]any{"unlockPassword": unlockPassword, "permission": "full"}, &r, true)
	if err != nil {
		return "", err
	}
	h.token = r.Token
	return r.Token, nil
}

func (h *hubAPI) connectionInfo() (nodeID string, err error) {
	var r struct {
		Pubkey string `json:"pubkey"`
	}
	err = h.do("GET", "/api/node/connection-info", nil, &r, false)
	return r.Pubkey, err
}

func (h *hubAPI) newAddress() (string, error) {
	var a string
	err := h.do("POST", "/api/wallet/new-address", nil, &a, false)
	return a, err
}

type balances struct {
	Onchain struct {
		SpendableSat int64 `json:"spendableSat"`
		TotalSat     int64 `json:"totalSat"`
	} `json:"onchain"`
	Lightning struct {
		TotalSpendableMsat  int64 `json:"totalSpendableMsat"`
		TotalReceivableMsat int64 `json:"totalReceivableMsat"`
	} `json:"lightning"`
}

func (h *hubAPI) balances() (*balances, error) {
	var b balances
	if err := h.do("GET", "/api/balances", nil, &b, false); err != nil {
		return nil, err
	}
	return &b, nil
}

func (h *hubAPI) walletSync() error { return h.do("POST", "/api/wallet/sync", nil, nil, false) }

func (h *hubAPI) connectPeer(pubkey, host string, port int) error {
	return h.do("POST", "/api/peers", map[string]any{"pubkey": pubkey, "address": host, "port": port}, nil, false)
}

func (h *hubAPI) openChannel(pubkey string, sats int64) (fundingTxID string, err error) {
	var r struct {
		FundingTxID string `json:"fundingTxId"`
	}
	err = h.do("POST", "/api/channels", map[string]any{"pubkey": pubkey, "amountSats": sats, "public": false}, &r, false)
	return r.FundingTxID, err
}

type channel struct {
	ID                    string  `json:"id"`
	RemotePubkey          string  `json:"remotePubkey"`
	Active                bool    `json:"active"`
	Status                string  `json:"status"`
	Confirmations         *uint32 `json:"confirmations"`
	ConfirmationsRequired *uint32 `json:"confirmationsRequired"`
	LocalBalanceSat       int64   `json:"localBalanceSat"`
	IsOutbound            bool    `json:"isOutbound"`
}

func (h *hubAPI) channels() ([]channel, error) {
	var c []channel
	err := h.do("GET", "/api/channels", nil, &c, false)
	return c, err
}

// createApp is POST /api/apps (rate limited; getAlby/hub 921003b api/models.go:152-165, 294-304). pubkeyHex is the client's public key; the hub then returns
// no secret. maxAmountSat 0 means no budget. It returns the connection's wallet public key.
func (h *hubAPI) createApp(name, pubkeyHex string, scopes []string, maxAmountSat uint64, budgetRenewal string) (walletPubkey string, id uint, err error) {
	var r struct {
		WalletPubkey string `json:"walletPubkey"`
		ID           uint   `json:"id"`
	}
	req := map[string]any{
		"name": name, "pubkey": pubkeyHex, "scopes": scopes,
		"maxAmountSat": maxAmountSat, "budgetRenewal": budgetRenewal,
	}
	if err = h.do("POST", "/api/apps", req, &r, true); err != nil {
		return "", 0, err
	}
	return r.WalletPubkey, r.ID, nil
}

// appBudgetUsage is GET /api/apps/{pubkey} (the connection's client public key): the connection's budget usage in sats (the field is required: a hub that
// renames it fails the check instead of reading as 0) and its budget (maxAmount, in sats).
func (h *hubAPI) appBudgetUsage(clientPubkey string) (used, max uint64, err error) {
	var body json.RawMessage
	if err = h.do("GET", "/api/apps/"+clientPubkey, nil, &body, false); err != nil {
		return 0, 0, err
	}
	return parseBudgetUsage(body)
}

// parseBudgetUsage reads budgetUsage and maxAmount from the hub's app JSON. Both are required numbers: a missing
// field, a JSON null or any other non-number is an error, never 0.
func parseBudgetUsage(body []byte) (used, max uint64, err error) {
	var m map[string]json.RawMessage // the keys are looked up exactly: encoding/json would match a struct field's case
	if err = json.Unmarshal(body, &m); err != nil {
		return 0, 0, fmt.Errorf("the hub's app: %s", jsonReason(err))
	}
	for k, dst := range map[string]*uint64{"budgetUsage": &used, "maxAmount": &max} {
		var n *uint64
		raw, ok := m[k]
		if !ok {
			return 0, 0, fmt.Errorf("the hub's app has no %s field", k)
		}
		if err = json.Unmarshal(raw, &n); err != nil {
			return 0, 0, fmt.Errorf("the hub's app %s: %s", k, jsonReason(err))
		}
		if n == nil {
			return 0, 0, fmt.Errorf("the hub's app %s is null", k)
		}
		*dst = *n
	}
	return used, max, nil
}

// hubTx is the part of the hub's own Transaction JSON (api/models.go) the suite reads.
type hubTx struct {
	Type        string `json:"type"`
	State       string `json:"state"`
	Invoice     string `json:"invoice"`
	PaymentHash string `json:"paymentHash"`
	AmountMsat  uint64 `json:"amountMsat"`
	FeesMsat    uint64 `json:"feesPaidMsat"`
	Failure     string `json:"failureReason"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// rowsFor lists the hub's own rows for one payment hash: GET /api/transactions (getAlby/hub 921003b
// http/http_service.go:765-806, api/transactions.go:92-122). The list holds settled rows of both directions and the
// unsettled and failed outgoing ones; an unsettled incoming row (an accepted hold invoice) is not listed.
func (h *hubAPI) rowsFor(paymentHash string) ([]hubTx, error) {
	var r struct {
		Transactions []hubTx `json:"transactions"`
	}
	if err := h.do("GET", "/api/transactions?limit=1000", nil, &r, false); err != nil {
		return nil, err
	}
	var rows []hubTx
	for _, x := range r.Transactions {
		if x.PaymentHash == paymentHash {
			rows = append(rows, x)
		}
	}
	return rows, nil
}

// makeInvoice is POST /api/invoices: the hub's own invoice, made by no NWC connection.
func (h *hubAPI) makeInvoice(amountSat uint64, description string) (*hubTx, error) {
	var t hubTx
	err := h.do("POST", "/api/invoices", map[string]any{"amountSat": amountSat, "description": description}, &t, false)
	return &t, err
}

// payInvoice is POST /api/payments/:invoice, the customer's role. It returns when the payment settled or failed.
func (h *hubAPI) payInvoice(bolt11 string) (*hubTx, error) {
	var t hubTx
	err := h.do("POST", "/api/payments/"+url.PathEscape(bolt11), map[string]any{}, &t, false)
	return &t, err
}

// transaction is GET /api/transactions/:paymentHash.
func (h *hubAPI) transaction(paymentHash string) (*hubTx, error) {
	var t hubTx
	err := h.do("GET", "/api/transactions/"+paymentHash, nil, &t, false)
	return &t, err
}

// ensureToken makes sure the stored token still works (a restarted hub may have a new JWT secret) and unlocks
// for a new one when it does not.
func (h *hubAPI) ensureToken(t testing.TB) {
	t.Helper()
	if h.token != "" {
		if _, err := h.balances(); err == nil {
			return
		}
	}
	if _, err := h.unlock(); err != nil {
		t.Fatalf("unlock %s: %v", h.base, shownErr(err))
	}
}
