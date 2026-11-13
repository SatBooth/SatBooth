package nwc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/satbooth/satbooth/nip44"
	"github.com/satbooth/satbooth/nostr"
)

// maxDepth bounds the nesting of a wallet document. The hub's deepest message (a list_transactions
// result with metadata) is under 8 levels.
const maxDepth = 32

func malformed(why string) error { return fmt.Errorf("%w: %s", ErrMalformed, why) }

// BuildRequest builds a signed kind 23194 request for method with params (nil means {}; anything else
// must marshal to a JSON object), valid for ttl (at least one second) from now. It is diagnostic: the
// qualification suite uses it to publish raw requests.
func BuildRequest(k *nostr.SecretKey, wallet, method string, params any, now time.Time, ttl time.Duration) (*nostr.Event, error) {
	conv, err := nip44.ConversationKey(k, wallet)
	if err != nil {
		return nil, err
	}
	if ttl < time.Second {
		return nil, errors.New("nwc: request lifetime must be at least one second")
	}
	return buildRequest(k, conv, wallet, method, params, now, now.Add(ttl))
}

func buildRequest(k *nostr.SecretKey, conv [32]byte, wallet, method string, params any,
	now, expires time.Time) (*nostr.Event, error) {
	if method == "" {
		return nil, errors.New("nwc: empty method")
	}
	if err := nostr.ValidatePubKey(wallet); err != nil {
		return nil, fmt.Errorf("nwc: wallet key: %w", err)
	}
	if expires.Unix() <= now.Unix() {
		return nil, errors.New("nwc: request lifetime must be at least one second")
	}
	raw := json.RawMessage("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("nwc: params: %w", err)
		}
		if len(b) == 0 || b[0] != '{' {
			return nil, errors.New("nwc: params must be a JSON object")
		}
		raw = b
	}
	plain, err := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{method, raw})
	if err != nil {
		return nil, fmt.Errorf("nwc: request: %w", err)
	}
	payload, err := nip44.Encrypt(conv, string(plain))
	if err != nil {
		return nil, fmt.Errorf("nwc: encrypt: %w", err)
	}
	e := &nostr.Event{
		CreatedAt: now.Unix(),
		Kind:      nostr.KindNWCRequest,
		Tags: [][]string{
			{"p", wallet},
			{"encryption", "nip44_v2"},
			{"expiration", strconv.FormatInt(expires.Unix(), 10)},
		},
		Content: payload,
	}
	if err := nostr.Sign(e, k); err != nil {
		return nil, fmt.Errorf("nwc: sign: %w", err)
	}
	if err := nostr.ValidateNWC(e); err != nil {
		return nil, fmt.Errorf("nwc: built an invalid request: %w", err)
	}
	return e, nil
}

// OpenResponse checks a kind 23195 event from wallet addressed to k and returns the id of the request
// it answers and the decrypted content. It is diagnostic, like BuildRequest.
func OpenResponse(e *nostr.Event, k *nostr.SecretKey, wallet string) (reqID string, plaintext string, err error) {
	conv, err := nip44.ConversationKey(k, wallet)
	if err != nil {
		return "", "", err
	}
	return openResponse(e, conv, wallet, k.PublicKeyHex())
}

// openEvent runs the checks shared by responses and notifications: kind, grammar, signature, author
// and addressee, then decrypts. It returns the event's tags for the caller's own lookups.
func openEvent(e *nostr.Event, kind int, conv [32]byte, wallet, client string) (string, error) {
	if e == nil {
		return "", errors.New("nwc: nil event")
	}
	if e.Kind != kind {
		return "", fmt.Errorf("nwc: event kind %d, want %d", e.Kind, kind)
	}
	if err := nostr.ValidateNWC(e); err != nil {
		return "", fmt.Errorf("nwc: %w", err)
	}
	if err := nostr.Verify(e); err != nil {
		return "", fmt.Errorf("nwc: %w", err)
	}
	if e.PubKey != wallet {
		return "", errors.New("nwc: event not signed by the wallet")
	}
	if tagValue(e, "p") != client {
		return "", errors.New("nwc: event not addressed to this client")
	}
	plain, err := nip44.Decrypt(conv, e.Content)
	if err != nil {
		return "", fmt.Errorf("nwc: decrypt: %w", err)
	}
	return plain, nil
}

func tagValue(e *nostr.Event, name string) string {
	for _, t := range e.Tags {
		if len(t) == 2 && t[0] == name {
			return t[1]
		}
	}
	return ""
}

func openResponse(e *nostr.Event, conv [32]byte, wallet, client string) (reqID, plaintext string, err error) {
	plaintext, err = openEvent(e, nostr.KindNWCResponse, conv, wallet, client)
	if err != nil {
		return "", "", err
	}
	return tagValue(e, "e"), plaintext, nil
}

// openNotification checks a kind 23197 event and parses its content. A kind 23196 (NIP-04) event is an
// error: the hub always sends both kinds, and only the NIP-44 one is read (getAlby/hub 921003b
// nip47/notifications/nip47_notifier.go:137-145, 195-197).
func openNotification(e *nostr.Event, conv [32]byte, wallet, client string) (Notification, error) {
	plain, err := openEvent(e, nostr.KindNWCNotification, conv, wallet, client)
	if err != nil {
		return Notification{}, err
	}
	top, err := parseObject([]byte(plain))
	if err != nil {
		return Notification{}, err
	}
	if err := onlyKeys(top, "notification_type", "notification"); err != nil {
		return Notification{}, err
	}
	typ, err := reqString(top, "notification_type")
	if err != nil {
		return Notification{}, err
	}
	rawTx, ok := top["notification"]
	if !ok {
		return Notification{}, malformed("notification missing")
	}
	tx, err := parseTransaction(rawTx)
	if err != nil {
		return Notification{}, err
	}
	// getAlby/hub 921003b nip47/notifications/nip47_notifier.go:44-76: payment_received is an incoming
	// row, payment_sent an outgoing one; a hold invoice is incoming.
	switch typ {
	case NotifPaymentReceived, NotifHoldInvoiceAccepted:
		if tx.Type != TypeIncoming {
			return Notification{}, malformed("notification direction")
		}
	case NotifPaymentSent:
		if tx.Type != TypeOutgoing {
			return Notification{}, malformed("notification direction")
		}
	default:
		return Notification{}, malformed("unknown notification type")
	}
	return Notification{Type: typ, Tx: tx, EventID: e.ID}, nil
}

// decodeResponse parses the plaintext of a response to method into out, which must be a *Transaction
// (make_invoice, lookup_invoice, make_hold_invoice), *TransactionList (list_transactions), *PayResult
// (pay_invoice), *Budget (get_budget) or *Info (get_info); settle_hold_invoice and cancel_hold_invoice
// take nil. A wallet-reported error is returned as *WalletError; anything that breaks the format wraps
// ErrMalformed. out is written only on success.
//
// Envelope: getAlby/hub 921003b nip47/models/models.go:93-102. result_type is always present, "" for
// UNSUPPORTED_ENCRYPTION and decrypt failures (nip47/event_handler.go:151-156, 240-245).
func decodeResponse(plaintext, method string, out any) error {
	switch method {
	case "make_invoice", "lookup_invoice", "make_hold_invoice":
		if _, ok := out.(*Transaction); !ok {
			return fmt.Errorf("nwc: %s needs a *Transaction", method)
		}
	case "list_transactions":
		if _, ok := out.(*TransactionList); !ok {
			return errors.New("nwc: list_transactions needs a *TransactionList")
		}
	case "pay_invoice":
		if _, ok := out.(*PayResult); !ok {
			return errors.New("nwc: pay_invoice needs a *PayResult")
		}
	case "get_budget":
		if _, ok := out.(*Budget); !ok {
			return errors.New("nwc: get_budget needs a *Budget")
		}
	case "get_info":
		if _, ok := out.(*Info); !ok {
			return errors.New("nwc: get_info needs a *Info")
		}
	case "settle_hold_invoice", "cancel_hold_invoice":
		// Result is an empty object: getAlby/hub 921003b nip47/controllers/settle_hold_invoice_controller.go:15,46-48
		// and cancel_hold_invoice_controller.go:15,46-48.
	default:
		return fmt.Errorf("nwc: unsupported method %q", method)
	}
	if out == nil && method != "settle_hold_invoice" && method != "cancel_hold_invoice" {
		return errors.New("nwc: nil out")
	}
	// A typed nil pointer passes the type checks above and would panic on the write below.
	if rv := reflect.ValueOf(out); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return errors.New("nwc: nil out")
	}

	top, err := parseObject([]byte(plaintext))
	if err != nil {
		return err
	}
	if err := onlyKeys(top, "result_type", "result", "error"); err != nil {
		return err
	}
	rt, err := reqString(top, "result_type")
	if err != nil {
		return err
	}
	rawRes, hasRes := top["result"]
	rawErr, hasErr := top["error"]
	if hasRes == hasErr {
		return malformed("need exactly one of result and error")
	}
	if rt != method && !(rt == "" && hasErr) {
		return malformed("result_type does not match the request")
	}
	if hasErr {
		return parseWalletError(rawErr)
	}
	res, err := parseObject(rawRes)
	if err != nil {
		return err
	}

	switch o := out.(type) {
	case *Transaction:
		// make_invoice_controller.go:61-69; lookup_invoice_controller.go; make_hold_invoice_controller.go:22-24,97-103
		// (the result is the embedded Transaction, so its fields sit at the top of result).
		tx, err := txFromFields(res)
		if err != nil {
			return err
		}
		*o = tx
	case *TransactionList:
		// list_transactions_controller.go:24-27
		rawTxs, ok := res["transactions"]
		if !ok {
			return malformed("transactions missing")
		}
		items, err := parseArray(rawTxs)
		if err != nil {
			return err
		}
		total, err := reqInt(res, "total_count")
		if err != nil {
			return err
		}
		list := TransactionList{Transactions: make([]Transaction, 0, len(items)), TotalCount: total}
		for _, it := range items {
			tx, err := parseTransaction(it)
			if err != nil {
				return err
			}
			list.Transactions = append(list.Transactions, tx)
		}
		*o = list
	case *PayResult:
		// pay_invoice_controller.go:76-82
		pre, err := reqString(res, "preimage")
		if err != nil {
			return err
		}
		if !isHex64(pre) {
			return malformed("preimage is not 64 lowercase hex")
		}
		fees, err := reqInt(res, "fees_paid")
		if err != nil {
			return err
		}
		*o = PayResult{Preimage: pre, FeesPaidMsat: fees}
	case *Budget:
		// get_budget_controller.go:18-79: {} when there is no budget.
		if len(res) == 0 {
			*o = Budget{}
			break
		}
		var b Budget
		b.Set = true
		if b.UsedMsat, err = reqInt(res, "used_budget"); err != nil {
			return err
		}
		if b.TotalMsat, err = reqInt(res, "total_budget"); err != nil {
			return err
		}
		if b.RenewsAt, err = optInt(res, "renews_at"); err != nil {
			return err
		}
		if b.RenewalPeriod, err = optString(res, "renewal_period"); err != nil {
			return err
		}
		*o = b
	case *Info:
		// get_info_controller.go:15-27: node fields are null without the get_info scope.
		var in Info
		if in.Network, err = optString(res, "network"); err != nil {
			return err
		}
		if in.Methods, err = optStrings(res, "methods", true); err != nil {
			return err
		}
		if in.Notifications, err = optStrings(res, "notifications", false); err != nil {
			return err
		}
		bh, err := optInt(res, "block_height")
		if err != nil {
			return err
		}
		if bh != nil {
			in.BlockHeight = *bh
		}
		*o = in
	default: // settle_hold_invoice, cancel_hold_invoice
		if len(res) != 0 {
			return malformed("result is not empty")
		}
	}
	return nil
}

// parseWalletError reads {"code","message"}. Unknown keys are ignored.
func parseWalletError(raw json.RawMessage) error {
	obj, err := parseObject(raw)
	if err != nil {
		return err
	}
	code, err := reqString(obj, "code")
	if err != nil {
		return err
	}
	if code == "" || len(code) > 64 {
		return malformed("error code length")
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return malformed("error code alphabet")
		}
	}
	msg, err := optString(obj, "message")
	if err != nil {
		return err
	}
	return &WalletError{Code: code, Message: msg}
}

// parseTransaction validates one transaction object.
func parseTransaction(raw json.RawMessage) (Transaction, error) {
	obj, err := parseObject(raw)
	if err != nil {
		return Transaction{}, err
	}
	return txFromFields(obj)
}

func txFromFields(obj map[string]json.RawMessage) (Transaction, error) {
	var tx Transaction
	var err error
	if tx.Type, err = reqString(obj, "type"); err != nil {
		return Transaction{}, err
	}
	if tx.Type != TypeIncoming && tx.Type != TypeOutgoing {
		return Transaction{}, malformed("transaction type")
	}
	if tx.State, err = reqString(obj, "state"); err != nil {
		return Transaction{}, err
	}
	switch tx.State {
	case StatePending, StateSettled, StateFailed, StateAccepted, StateExpired:
	default:
		return Transaction{}, malformed("transaction state")
	}
	if tx.PaymentHash, err = reqString(obj, "payment_hash"); err != nil {
		return Transaction{}, err
	}
	if !isHex64(tx.PaymentHash) {
		return Transaction{}, malformed("payment_hash is not 64 lowercase hex")
	}
	if tx.Invoice, err = optStringStrict(obj, "invoice"); err != nil {
		return Transaction{}, err
	}
	if tx.Description, err = optStringStrict(obj, "description"); err != nil {
		return Transaction{}, err
	}
	if tx.DescriptionHash, err = optStringStrict(obj, "description_hash"); err != nil {
		return Transaction{}, err
	}
	if tx.Preimage, err = optStringStrict(obj, "preimage"); err != nil {
		return Transaction{}, err
	}
	if tx.Preimage != "" && !isHex64(tx.Preimage) {
		return Transaction{}, malformed("preimage is not 64 lowercase hex")
	}
	if tx.AmountMsat, err = reqInt(obj, "amount"); err != nil {
		return Transaction{}, err
	}
	fees, err := optIntStrict(obj, "fees_paid")
	if err != nil {
		return Transaction{}, err
	}
	tx.FeesPaidMsat = fees
	if tx.CreatedAt, err = reqInt(obj, "created_at"); err != nil {
		return Transaction{}, err
	}
	if tx.ExpiresAt, err = optInt(obj, "expires_at"); err != nil {
		return Transaction{}, err
	}
	if tx.SettledAt, err = optInt(obj, "settled_at"); err != nil {
		return Transaction{}, err
	}
	if m, ok := obj["metadata"]; ok && !isNull(m) {
		if _, err := parseObject(m); err != nil {
			return Transaction{}, err
		}
		tx.Metadata = append(json.RawMessage(nil), m...)
	}
	return tx, nil
}

// --- strict JSON helpers -------------------------------------------------------------------------

func isNull(raw json.RawMessage) bool { return string(raw) == "null" }

// checkJSON verifies that b is exactly one JSON document of valid UTF-8, nested at most maxDepth deep,
// with no duplicate key in any object. encoding/json alone accepts duplicates (the last wins) and
// replaces invalid UTF-8, and a wallet must not get to pick which of two readings we use.
func checkJSON(b []byte) error {
	if !utf8.Valid(b) {
		return malformed("invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	type frame struct {
		isObj   bool
		keys    map[string]struct{}
		wantKey bool
	}
	var stack []frame
	started, done := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return malformed("not valid JSON")
			}
			break
		}
		if err != nil || done {
			return malformed("not valid JSON")
		}
		started = true
		if n := len(stack); n > 0 && stack[n-1].isObj && stack[n-1].wantKey {
			if d, ok := tok.(json.Delim); !ok || d != '}' {
				key, ok := tok.(string)
				if !ok {
					return malformed("object key")
				}
				if _, dup := stack[n-1].keys[key]; dup {
					return malformed("duplicate key")
				}
				stack[n-1].keys[key] = struct{}{}
				stack[n-1].wantKey = false
				continue
			}
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(stack) >= maxDepth {
					return malformed("nesting too deep")
				}
				stack = append(stack, frame{isObj: d == '{', keys: map[string]struct{}{}, wantKey: d == '{'})
				continue
			default: // '}' or ']'
				stack = stack[:len(stack)-1]
			}
		}
		// A value (or a closed container) finished: an enclosing object wants its next key.
		if n := len(stack); n > 0 && stack[n-1].isObj {
			stack[n-1].wantKey = true
		} else if n == 0 {
			done = true // a second top-level value is an error
		}
	}
	if !started {
		return malformed("empty document")
	}
	return nil
}

// parseObject checks b with checkJSON and returns its members. b must be a JSON object.
func parseObject(b []byte) (map[string]json.RawMessage, error) {
	if err := checkJSON(b); err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, malformed("not an object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, malformed("not an object")
	}
	return m, nil
}

func parseArray(b []byte) ([]json.RawMessage, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '[' {
		return nil, malformed("not an array")
	}
	var a []json.RawMessage
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, malformed("not an array")
	}
	return a, nil
}

func onlyKeys(m map[string]json.RawMessage, allowed ...string) error {
outer:
	for k := range m {
		for _, a := range allowed {
			if k == a {
				continue outer
			}
		}
		return malformed("unknown key")
	}
	return nil
}

func rawString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", malformed("not a string")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", malformed("not a string")
	}
	return s, nil
}

func reqString(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", malformed(key + " missing")
	}
	return rawString(raw)
}

// optString: missing or null gives "".
func optString(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok || isNull(raw) {
		return "", nil
	}
	return rawString(raw)
}

// optStringStrict: missing gives ""; null is refused.
func optStringStrict(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", nil
	}
	return rawString(raw)
}

// rawInt accepts only ^(0|[1-9][0-9]*)$ with a value of at most 2^53. That refuses "-0", "1e3",
// "1.0", a leading zero, a sign and a string.
func rawInt(raw json.RawMessage) (int64, error) {
	n := len(raw)
	if n == 0 || n > 16 {
		return 0, malformed("not an integer in range")
	}
	if raw[0] == '0' && n > 1 {
		return 0, malformed("not an integer in range")
	}
	var v int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, malformed("not an integer in range")
		}
		v = v*10 + int64(c-'0')
	}
	if v > maxSafeInt {
		return 0, malformed("integer above 2^53")
	}
	return v, nil
}

func reqInt(m map[string]json.RawMessage, key string) (int64, error) {
	raw, ok := m[key]
	if !ok {
		return 0, malformed(key + " missing")
	}
	return rawInt(raw)
}

// optIntStrict: missing gives 0; null is refused.
func optIntStrict(m map[string]json.RawMessage, key string) (int64, error) {
	raw, ok := m[key]
	if !ok {
		return 0, nil
	}
	return rawInt(raw)
}

// optInt: missing or null gives nil.
func optInt(m map[string]json.RawMessage, key string) (*int64, error) {
	raw, ok := m[key]
	if !ok || isNull(raw) {
		return nil, nil
	}
	v, err := rawInt(raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// optStrings reads an array of strings. null gives nil; a missing key is an error when required.
func optStrings(m map[string]json.RawMessage, key string, required bool) ([]string, error) {
	raw, ok := m[key]
	if !ok {
		if required {
			return nil, malformed(key + " missing")
		}
		return nil, nil
	}
	if isNull(raw) {
		return nil, nil
	}
	items, err := parseArray(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, err := rawString(it)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
