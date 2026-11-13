package nwc

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

var fuzzMethods = []string{
	"make_invoice", "lookup_invoice", "list_transactions", "pay_invoice", "get_budget", "get_info",
	"make_hold_invoice", "settle_hold_invoice", "cancel_hold_invoice", "bogus", "",
}

// numberSeeds cross the integer boundaries.
var numberSeeds = []string{
	"0", "-0", "1", "-1", "1e3", "1E3", "1.0", "1.5", "0.1", "01", "+1", "9007199254740991", "9007199254740992",
	"9007199254740993", "-9007199254740992", "99999999999999999999999", "1e400", "NaN", "Infinity", `"5"`, "null", "true", "[]", "{}",
}

func seedDocs() []string {
	docs := []string{
		"", " ", "{}", "[]", "null", "0", `""`, "{", "}", `{"result_type"`, `{"result_type":"pay_invoice"}`,
		hubOK("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":3}`),
		hubOK("get_budget", `{}`),
		hubOK("get_budget", `{"used_budget":2000,"total_budget":100000,"renews_at":1800086400,"renewal_period":"daily"}`),
		hubOK("get_info", `{"alias":null,"network":"regtest","block_height":101,"methods":["a"],"notifications":null}`),
		hubOK("make_invoice", hubTx("incoming", "pending")),
		hubOK("lookup_invoice", hubTx("outgoing", "settled")),
		hubOK("list_transactions", `{"transactions":[`+hubTx("incoming", "settled")+`],"total_count":1}`),
		hubOK("settle_hold_invoice", `{}`),
		`{"result_type":"","error":{"code":"BAD_REQUEST","message":"failed to decrypt"}}`,
		`{"result_type":"pay_invoice","error":{"code":"INTERNAL","message":"this invoice has already been paid"}}`,
		`{"result_type":"pay_invoice","result_type":"pay_invoice","result":{}}`,
		`{"result_type":"pay_invoice","result":{"fees_paid":1,"fees_paid":2,"preimage":"` + preimageA + `"}}`,
		`{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0},"x":1}`,
		`{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0,"s":" "}}`,
		"{\"result_type\":\"pay_invoice\",\"result\":{\"preimage\":\"" + preimageA + "\",\"fees_paid\":0,\"s\":\" \"}}",
		`{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0,"s":"` + strings.Repeat("a", 70000) + `"}}`,
		"{\"result_type\":\"pay_invoice\",\"result\":{\"s\":\"\xff\xfe\"}}",
		`{"result_type":"pay_invoice","result":{"x":` + strings.Repeat("[", 100000) + strings.Repeat("]", 100000) + `}}`,
		strings.Repeat(`{"a":`, 100000) + "1" + strings.Repeat("}", 100000),
		strings.Repeat("[", 100000),
	}
	for _, n := range numberSeeds {
		docs = append(docs,
			hubOK("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":`+n+`}`),
			hubOK("get_budget", `{"used_budget":`+n+`,"total_budget":`+n+`}`),
			hubOK("get_info", `{"methods":[],"block_height":`+n+`}`),
			hubOK("lookup_invoice", strings.Replace(hubTx("incoming", "settled"), `"amount":10000`, `"amount":`+n, 1)),
			hubOK("lookup_invoice", strings.Replace(hubTx("incoming", "settled"), `"settled_at":null`, `"settled_at":`+n, 1)),
		)
	}
	return docs
}

func hubOK(method, result string) string {
	return `{"result_type":"` + method + `","result":` + result + `}`
}

func newOut(method string) any {
	switch method {
	case "make_invoice", "lookup_invoice", "make_hold_invoice":
		return &Transaction{}
	case "list_transactions":
		return &TransactionList{}
	case "pay_invoice":
		return &PayResult{}
	case "get_budget":
		return &Budget{}
	case "get_info":
		return &Info{}
	}
	return nil
}

func inRange(n int64) bool { return n >= 0 && n <= maxSafeInt }

func checkTx(t *testing.T, tx Transaction) {
	t.Helper()
	if tx.Type != "incoming" && tx.Type != "outgoing" {
		t.Fatalf("type %q", tx.Type)
	}
	switch tx.State {
	case "pending", "settled", "failed", "accepted", "expired":
	default:
		t.Fatalf("state %q", tx.State)
	}
	if !isHex64(tx.PaymentHash) || (tx.Preimage != "" && !isHex64(tx.Preimage)) {
		t.Fatalf("hash %q preimage %q", tx.PaymentHash, tx.Preimage)
	}
	if !inRange(tx.AmountMsat) || !inRange(tx.FeesPaidMsat) || !inRange(tx.CreatedAt) {
		t.Fatalf("numbers %d %d %d", tx.AmountMsat, tx.FeesPaidMsat, tx.CreatedAt)
	}
	if tx.ExpiresAt != nil && !inRange(*tx.ExpiresAt) || tx.SettledAt != nil && !inRange(*tx.SettledAt) {
		t.Fatalf("times")
	}
	if !utf8.ValidString(tx.Invoice) || !utf8.ValidString(tx.Description) {
		t.Fatalf("invalid UTF-8 in a string field")
	}
	if tx.Metadata != nil {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(tx.Metadata, &m); err != nil || m == nil {
			t.Fatalf("metadata %q", tx.Metadata)
		}
	}
}

func checkOut(t *testing.T, out any) {
	t.Helper()
	switch o := out.(type) {
	case *Transaction:
		checkTx(t, *o)
	case *TransactionList:
		if !inRange(o.TotalCount) {
			t.Fatalf("total %d", o.TotalCount)
		}
		for _, tx := range o.Transactions {
			checkTx(t, tx)
		}
	case *PayResult:
		if !isHex64(o.Preimage) || !inRange(o.FeesPaidMsat) {
			t.Fatalf("%+v", o)
		}
	case *Budget:
		if o.Set && (!inRange(o.UsedMsat) || !inRange(o.TotalMsat)) {
			t.Fatalf("%+v", o)
		}
		if !o.Set && (*o != Budget{}) {
			t.Fatalf("%+v", o)
		}
		if o.RenewsAt != nil && !inRange(*o.RenewsAt) {
			t.Fatalf("%+v", o)
		}
	case *Info:
		if !inRange(o.BlockHeight) {
			t.Fatalf("%+v", o)
		}
	}
}

func checkDecode(t *testing.T, plaintext, method string) {
	t.Helper()
	out := newOut(method)
	err := decodeResponse(plaintext, method, out)
	if err != nil {
		var we *WalletError
		if errors.As(err, &we) {
			if we.Code == "" || len(we.Code) > 64 {
				t.Fatalf("wallet error code %q", we.Code)
			}
			if !utf8.ValidString(err.Error()) || strings.ContainsAny(err.Error(), "\n\r\x1b") {
				t.Fatalf("unsafe error text %q", err.Error())
			}
		}
		// Control characters and invalid UTF-8 never reach a log line through an error.
		for _, r := range err.Error() {
			if r < 0x20 && r != '\t' || r == 0x7f {
				t.Fatalf("control character in error %q", err.Error())
			}
		}
		return
	}
	if method != "settle_hold_invoice" && method != "cancel_hold_invoice" && out == nil {
		t.Fatal("success for a method with no out")
	}
	// A valid document must be valid UTF-8 JSON with unique keys.
	if !utf8.ValidString(plaintext) || !json.Valid([]byte(plaintext)) {
		t.Fatalf("accepted invalid document %q", plaintext)
	}
	if err := checkJSON([]byte(plaintext)); err != nil {
		t.Fatalf("accepted a document checkJSON refuses: %v", err)
	}
	checkOut(t, out)
}

func FuzzDecodeResponse(f *testing.F) {
	for _, d := range seedDocs() {
		for _, m := range fuzzMethods {
			f.Add(d, m)
		}
	}
	f.Fuzz(func(t *testing.T, plaintext, method string) { checkDecode(t, plaintext, method) })
}

func TestDecodeResponseProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	docs := seedDocs()
	for _, d := range docs {
		for _, m := range fuzzMethods {
			checkDecode(t, d, m)
		}
	}
	// Mutations: delete, duplicate, replace, insert odd pieces at random offsets.
	pieces := []string{"\"", "\\", "{", "}", "[", "]", ",", ":", "null", "-0", "1e3", "9007199254740993", " ", "\xff", "\x00", "\\ud800", "\\u0000"}
	for i := 0; i < 20000; i++ {
		d := docs[r.IntN(len(docs))]
		if len(d) > 2000 {
			d = d[:2000]
		}
		b := []byte(d)
		for j := r.IntN(4); j >= 0 && len(b) > 0; j-- {
			p := r.IntN(len(b))
			switch r.IntN(3) {
			case 0:
				b = append(b[:p], b[p+1:]...)
			case 1:
				b = append(b[:p], append([]byte(pieces[r.IntN(len(pieces))]), b[p:]...)...)
			default:
				b[p] = byte(r.IntN(256))
			}
		}
		checkDecode(t, string(b), fuzzMethods[r.IntN(len(fuzzMethods))])
	}
}

func checkTxJSON(t *testing.T, raw string) {
	t.Helper()
	var tx Transaction
	err := decodeResponse(hubOK("lookup_invoice", raw), "lookup_invoice", &tx)
	if err == nil {
		checkTx(t, tx)
	} else if !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-malformed error %v", err)
	}
}

func FuzzTransaction(f *testing.F) {
	for _, n := range numberSeeds {
		for _, field := range []string{"amount", "fees_paid", "created_at", "expires_at", "settled_at"} {
			f.Add(strings.Replace(hubTx("incoming", "settled"), `"`+field+`":`, `"`+field+`":`+n+`,"x":`, 1))
		}
	}
	for _, typ := range []string{"incoming", "outgoing", "", "Incoming"} {
		for _, st := range []string{"pending", "settled", "failed", "accepted", "expired", "", "PENDING"} {
			f.Add(hubTx(typ, st))
		}
	}
	f.Add(`{}`)
	f.Add(`[]`)
	f.Add(strings.Replace(hubTx("incoming", "settled"), `"type":"incoming"`, `"type":"incoming","type":"outgoing"`, 1))
	f.Add(strings.Replace(hubTx("incoming", "settled"), `"coffee"`, `"`+strings.Repeat("a", 70000)+`"`, 1))
	f.Add(strings.Replace(hubTx("incoming", "settled"), `"coffee"`, "\" \xff\"", 1))
	f.Add(strings.Replace(hubTx("incoming", "settled"), `{"order":"a1"}`, strings.Repeat("[", 5000), 1))
	f.Add(strings.Replace(hubTx("incoming", "settled"), hashA, strings.ToUpper(hashA), 1))
	f.Fuzz(func(t *testing.T, raw string) { checkTxJSON(t, raw) })
}

func FuzzNotificationContent(f *testing.F) {
	v := newEnv(f)
	for _, d := range []string{
		`{"notification_type":"payment_received","notification":` + hubTx("incoming", "settled") + `}`,
		`{"notification_type":"payment_sent","notification":` + hubTx("outgoing", "settled") + `}`,
		`{"notification_type":"hold_invoice_accepted","notification":` + hubTx("incoming", "accepted") + `}`,
		`{"notification_type":"payment_received","notification":{}}`,
		`{"notification_type":"payment_received"}`,
		`{"notification_type":"","notification":null}`,
		`{"notification_type":"payment_failed","notification":` + hubTx("incoming", "settled") + `}`,
		`{"notification_type":"payment_received","notification_type":"payment_sent","notification":` + hubTx("incoming", "settled") + `}`,
		`{"notification_type":"payment_received","notification":` + hubTx("incoming", "settled") + `,"x":1}`,
		`{"notification_type":"payment_received","notification":` + strings.Repeat("[", 100000),
		`{"notification_type":"payment_received","notification":"` + strings.Repeat("a", 70000) + `"}`,
		"{\"notification_type\":\"payment_received\",\"notification\":\"\xff \"}",
		"", "[]", "null", "{",
	} {
		f.Add(d)
	}
	w := v.wallet.PublicKeyHex()
	c := v.client.PublicKeyHex()
	f.Fuzz(func(t *testing.T, content string) {
		if content == "" || len(content) > 65535 {
			return // NIP-44 cannot carry it
		}
		e := v.notification(t, content)
		n, err := openNotification(e, v.conv, w, c)
		if err != nil {
			return
		}
		switch n.Type {
		case "payment_received":
			if n.Tx.Type != "incoming" {
				t.Fatalf("%+v", n)
			}
		case "payment_sent":
			if n.Tx.Type != "outgoing" {
				t.Fatalf("%+v", n)
			}
		case "hold_invoice_accepted":
		default:
			t.Fatalf("type %q", n.Type)
		}
		if n.EventID != e.ID {
			t.Fatal("event id")
		}
		checkTx(t, n.Tx)
	})
}
