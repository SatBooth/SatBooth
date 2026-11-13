package nwc

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/nip44"
	"github.com/satbooth/satbooth/nostr"
)

const (
	clientKeyHex = "0000000000000000000000000000000000000000000000000000000000000003"
	walletKeyHex = "b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfef" // BIP-340 test vector 1 key
	otherKeyHex  = "0000000000000000000000000000000000000000000000000000000000000005"
	hashA        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	preimageA    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

var testNow = time.Unix(1_800_000_000, 0)

func key(t testing.TB, h string) *nostr.SecretKey {
	t.Helper()
	k, err := nostr.ParseSecretKeyHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type env struct {
	client, wallet, other *nostr.SecretKey
	conv                  [32]byte // client <-> wallet
}

func newEnv(t testing.TB) env {
	t.Helper()
	e := env{client: key(t, clientKeyHex), wallet: key(t, walletKeyHex), other: key(t, otherKeyHex)}
	var err error
	e.conv, err = nip44.ConversationKey(e.client, e.wallet.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// signed builds a signed event of the given kind by signer, encrypting plaintext with conv.
func signed(t testing.TB, signer *nostr.SecretKey, kind int, tags [][]string, conv [32]byte, plaintext string) *nostr.Event {
	t.Helper()
	payload, err := nip44.Encrypt(conv, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	e := &nostr.Event{Kind: kind, CreatedAt: testNow.Unix(), Tags: tags, Content: payload}
	if err := nostr.Sign(e, signer); err != nil {
		t.Fatal(err)
	}
	return e
}

func (v env) response(t testing.TB, plaintext string) *nostr.Event {
	return signed(t, v.wallet, nostr.KindNWCResponse,
		[][]string{{"p", v.client.PublicKeyHex()}, {"e", hashA}}, v.conv, plaintext)
}

func (v env) notification(t testing.TB, plaintext string) *nostr.Event {
	return signed(t, v.wallet, nostr.KindNWCNotification,
		[][]string{{"p", v.client.PublicKeyHex()}}, v.conv, plaintext)
}

// hubTx is a transaction as the hub sends it (getAlby/hub 921003b nip47/models/models.go:67-82).
func hubTx(typ, state string) string {
	return fmt.Sprintf(`{"type":%q,"state":%q,"invoice":"lnbcrt10n1pabc","description":"coffee","description_hash":"",`+
		`"preimage":%q,"payment_hash":%q,"amount":10000,"fees_paid":0,"created_at":1800000000,`+
		`"expires_at":1800086400,"settled_at":null,"settle_deadline":null,"metadata":{"order":"a1"}}`, typ, state, preimageA, hashA)
}

func TestBuildRequestGrammar(t *testing.T) {
	v := newEnv(t)
	methods := []string{"make_invoice", "lookup_invoice", "list_transactions", "pay_invoice", "get_budget", "get_info", "make_hold_invoice"}
	paramSets := []any{
		nil,
		map[string]any{"amount": 10000, "description": "x"},
		struct {
			Invoice string `json:"invoice"`
		}{"lnbcrt1"},
		json.RawMessage(`{"a":[1,2]}`),
	}
	for _, m := range methods {
		for i, p := range paramSets {
			t.Run(fmt.Sprintf("%s/%d", m, i), func(t *testing.T) {
				e, err := BuildRequest(v.client, v.wallet.PublicKeyHex(), m, p, testNow, 60*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if err := nostr.ValidateNWC(e); err != nil {
					t.Fatalf("ValidateNWC: %v", err)
				}
				if err := nostr.Verify(e); err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if e.Kind != 23194 || e.PubKey != v.client.PublicKeyHex() || e.CreatedAt != testNow.Unix() {
					t.Fatalf("kind %d pub %s created %d", e.Kind, e.PubKey, e.CreatedAt)
				}
				want := [][]string{{"p", v.wallet.PublicKeyHex()}, {"encryption", "nip44_v2"}, {"expiration", strconv.FormatInt(testNow.Unix()+60, 10)}}
				if !reflect.DeepEqual(e.Tags, want) {
					t.Fatalf("tags %v, want %v", e.Tags, want)
				}
				// The wallet decrypts it with its side of the conversation key.
				wc, err := nip44.ConversationKey(v.wallet, v.client.PublicKeyHex())
				if err != nil {
					t.Fatal(err)
				}
				plain, err := nip44.Decrypt(wc, e.Content)
				if err != nil {
					t.Fatal(err)
				}
				var got struct {
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal([]byte(plain), &got); err != nil {
					t.Fatal(err)
				}
				if got.Method != m {
					t.Fatalf("method %q", got.Method)
				}
				if len(got.Params) == 0 || got.Params[0] != '{' {
					t.Fatalf("params %s is not an object", got.Params)
				}
			})
		}
	}
}

func TestBuildRequestRefuses(t *testing.T) {
	v := newEnv(t)
	w := v.wallet.PublicKeyHex()
	cases := []struct {
		name   string
		wallet string
		method string
		params any
		ttl    time.Duration
	}{
		{"empty method", w, "", nil, time.Minute},
		{"bad wallet", "zz", "get_info", nil, time.Minute},
		{"params array", w, "get_info", []int{1}, time.Minute},
		{"params string", w, "get_info", "x", time.Minute},
		{"params null", w, "get_info", json.RawMessage("null"), time.Minute},
		{"params unmarshalable", w, "get_info", make(chan int), time.Minute},
		{"zero ttl", w, "get_info", nil, 0},
		{"negative ttl", w, "get_info", nil, -time.Second},
		{"sub-second ttl", w, "get_info", nil, time.Millisecond},
		{"params too large", w, "pay_invoice", map[string]string{"invoice": strings.Repeat("a", 70000)}, time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if e, err := BuildRequest(v.client, c.wallet, c.method, c.params, testNow, c.ttl); err == nil {
				t.Fatalf("accepted: %+v", e)
			}
		})
	}
}

func TestOpenResponse(t *testing.T) {
	v := newEnv(t)
	w := v.wallet.PublicKeyHex()
	c := v.client.PublicKeyHex()
	plain := `{"result_type":"get_info","result":{}}`

	t.Run("accepts", func(t *testing.T) {
		e := v.response(t, plain)
		id, got, err := OpenResponse(e, v.client, w)
		if err != nil {
			t.Fatal(err)
		}
		if id != hashA || got != plain {
			t.Fatalf("id %q plaintext %q", id, got)
		}
	})

	tampered := func(mut func(e *nostr.Event)) *nostr.Event {
		e := v.response(t, plain)
		mut(e)
		return e
	}
	resign := func(e *nostr.Event, k *nostr.SecretKey) {
		e.PubKey = ""
		e.ID, e.Sig = "", ""
		if err := nostr.Sign(e, k); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]*nostr.Event{
		"different author": tampered(func(e *nostr.Event) { resign(e, v.other) }),
		"different p":      signed(t, v.wallet, 23195, [][]string{{"p", v.other.PublicKeyHex()}, {"e", hashA}}, v.conv, plain),
		"missing e":        signed(t, v.wallet, 23195, [][]string{{"p", c}}, v.conv, plain),
		"bad e":            signed(t, v.wallet, 23195, [][]string{{"p", c}, {"e", "abc"}}, v.conv, plain),
		"bad signature": tampered(func(e *nostr.Event) {
			e.Sig = strings.Repeat("0", 128)
		}),
		"tampered content": tampered(func(e *nostr.Event) { e.Content = e.Content[:len(e.Content)-4] + "AAAA" }),
		"kind 23197":       signed(t, v.wallet, 23197, [][]string{{"p", c}}, v.conv, plain),
		"kind 23194":       signed(t, v.wallet, 23194, [][]string{{"p", c}, {"e", hashA}}, v.conv, plain),
		"not nip44": tampered(func(e *nostr.Event) {
			e.Content = strings.Repeat("A", 200)
			resign(e, v.wallet)
		}),
		"encrypted for someone else": func() *nostr.Event {
			oc, _ := nip44.ConversationKey(v.other, w)
			return signed(t, v.wallet, 23195, [][]string{{"p", c}, {"e", hashA}}, oc, plain)
		}(),
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if id, p, err := OpenResponse(e, v.client, w); err == nil {
				t.Fatalf("accepted: %q %q", id, p)
			}
		})
	}
	t.Run("nil event", func(t *testing.T) {
		if _, _, err := OpenResponse(nil, v.client, w); err == nil {
			t.Fatal("accepted nil")
		}
	})
	t.Run("bad wallet key", func(t *testing.T) {
		if _, _, err := OpenResponse(v.response(t, plain), v.client, "nothex"); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestDecodeResponse(t *testing.T) {
	budget := `{"used_budget":2000,"total_budget":100000,"renews_at":1800086400,"renewal_period":"daily"}`
	info := `{"alias":"a","color":"#fff","pubkey":"` + hashA + `","network":"regtest","block_height":101,"block_hash":"` + hashA +
		`","methods":["pay_invoice","get_info"],"notifications":["payment_received"],"extensions":["02"],"lud16":null}`
	wrap := func(method, result string) string {
		return `{"result_type":"` + method + `","result":` + result + `}`
	}
	var tx Transaction
	var list TransactionList
	var pay PayResult
	var bud Budget
	var inf Info

	okCases := []struct {
		name, method, plain string
		out                 any
		check               func(t *testing.T)
	}{
		{"make_invoice", "make_invoice", wrap("make_invoice", hubTx("incoming", "pending")), &tx, func(t *testing.T) {
			if tx.Type != "incoming" || tx.State != "pending" || tx.AmountMsat != 10000 || tx.PaymentHash != hashA ||
				tx.ExpiresAt == nil || *tx.ExpiresAt != 1800086400 || tx.SettledAt != nil || string(tx.Metadata) != `{"order":"a1"}` ||
				tx.Preimage != preimageA || tx.Description != "coffee" {
				t.Fatalf("%+v", tx)
			}
		}},
		{"lookup_invoice", "lookup_invoice", wrap("lookup_invoice", hubTx("outgoing", "settled")), &tx, func(t *testing.T) {
			if tx.Type != "outgoing" || tx.State != "settled" {
				t.Fatalf("%+v", tx)
			}
		}},
		{"make_hold_invoice", "make_hold_invoice", wrap("make_hold_invoice", hubTx("incoming", "pending")), &tx, nil},
		{"list_transactions", "list_transactions",
			wrap("list_transactions", `{"transactions":[`+hubTx("incoming", "settled")+`,`+hubTx("outgoing", "expired")+`],"total_count":2}`), &list, func(t *testing.T) {
				if len(list.Transactions) != 2 || list.TotalCount != 2 || list.Transactions[1].State != "expired" {
					t.Fatalf("%+v", list)
				}
			}},
		{"list_transactions empty", "list_transactions", wrap("list_transactions", `{"transactions":[],"total_count":0}`), &list, func(t *testing.T) {
			if len(list.Transactions) != 0 || list.TotalCount != 0 {
				t.Fatalf("%+v", list)
			}
		}},
		{"pay_invoice", "pay_invoice", wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":3}`), &pay, func(t *testing.T) {
			if pay.Preimage != preimageA || pay.FeesPaidMsat != 3 {
				t.Fatalf("%+v", pay)
			}
		}},
		{"get_budget", "get_budget", wrap("get_budget", budget), &bud, func(t *testing.T) {
			if !bud.Set || bud.UsedMsat != 2000 || bud.TotalMsat != 100000 || bud.RenewsAt == nil || *bud.RenewsAt != 1800086400 || bud.RenewalPeriod != "daily" {
				t.Fatalf("%+v", bud)
			}
		}},
		{"get_budget none", "get_budget", wrap("get_budget", `{}`), &bud, func(t *testing.T) {
			if bud.Set {
				t.Fatalf("%+v", bud)
			}
		}},
		{"get_budget never renews", "get_budget", wrap("get_budget", `{"used_budget":0,"total_budget":5000,"renewal_period":"never"}`), &bud, func(t *testing.T) {
			if !bud.Set || bud.RenewsAt != nil {
				t.Fatalf("%+v", bud)
			}
		}},
		{"get_info", "get_info", wrap("get_info", info), &inf, func(t *testing.T) {
			if inf.Network != "regtest" || inf.BlockHeight != 101 || !reflect.DeepEqual(inf.Methods, []string{"pay_invoice", "get_info"}) ||
				!reflect.DeepEqual(inf.Notifications, []string{"payment_received"}) {
				t.Fatalf("%+v", inf)
			}
		}},
		{"get_info withheld", "get_info", wrap("get_info", `{"alias":null,"color":null,"pubkey":null,"network":null,"block_height":null,"block_hash":null,"methods":["get_info"],"notifications":null,"lud16":null}`), &inf, func(t *testing.T) {
			if inf.Network != "" || inf.BlockHeight != 0 || len(inf.Notifications) != 0 {
				t.Fatalf("%+v", inf)
			}
		}},
		{"settle_hold_invoice", "settle_hold_invoice", wrap("settle_hold_invoice", `{}`), nil, nil},
		{"cancel_hold_invoice", "cancel_hold_invoice", wrap("cancel_hold_invoice", `{}`), nil, nil},
		{"unknown result keys ignored", "pay_invoice", wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":0,"extra":{"a":[1]}}`), &pay, nil},
		{"whitespace", "pay_invoice", " \n" + wrap("pay_invoice", `{ "preimage" : "`+preimageA+`" , "fees_paid" : 0 }`) + "\n", &pay, nil},
	}
	for _, c := range okCases {
		t.Run("ok/"+c.name, func(t *testing.T) {
			if err := decodeResponse(c.plain, c.method, c.out); err != nil {
				t.Fatal(err)
			}
			if c.check != nil {
				c.check(t)
			}
		})
	}

	t.Run("wallet error", func(t *testing.T) {
		var p PayResult
		err := decodeResponse(`{"result_type":"pay_invoice","error":{"code":"INTERNAL","message":"this invoice has already been paid"}}`, "pay_invoice", &p)
		var we *WalletError
		if !errors.As(err, &we) || we.Code != "INTERNAL" || we.Message != "this invoice has already been paid" {
			t.Fatalf("%v", err)
		}
		if errors.Is(err, ErrMalformed) {
			t.Fatal("wallet error reported as malformed")
		}
	})
	t.Run("wallet error empty result_type", func(t *testing.T) {
		for _, code := range []string{"BAD_REQUEST", "UNSUPPORTED_ENCRYPTION"} {
			var p PayResult
			err := decodeResponse(`{"result_type":"","error":{"code":"`+code+`","message":"failed to decrypt"}}`, "pay_invoice", &p)
			var we *WalletError
			if !errors.As(err, &we) || we.Code != code {
				t.Fatalf("%s: %v", code, err)
			}
		}
	})
	t.Run("wallet error without message", func(t *testing.T) {
		err := decodeResponse(`{"result_type":"get_info","error":{"code":"RESTRICTED"}}`, "get_info", &inf)
		var we *WalletError
		if !errors.As(err, &we) || we.Message != "" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("wallet error text is quoted", func(t *testing.T) {
		err := decodeResponse(`{"result_type":"get_info","error":{"code":"OTHER","message":"a\nb\u001b[31m"}}`, "get_info", &inf)
		if err == nil || strings.ContainsAny(err.Error(), "\n\x1b") {
			t.Fatalf("%q", err)
		}
	})

	good := wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":0}`)
	big := strings.Repeat("a", 70000)
	malformed := map[string]string{
		"empty":                    "",
		"not json":                 "nope",
		"array":                    "[]",
		"null":                     "null",
		"trailing garbage":         good + "x",
		"two documents":            good + good,
		"truncated":                good[:len(good)-1],
		"unknown top-level key":    `{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0},"extra":1}`,
		"wrong-case key":           `{"Result_Type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0}}`,
		"duplicate top-level key":  `{"result_type":"pay_invoice","result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0}}`,
		"duplicate result key":     `{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0,"fees_paid":1}}`,
		"duplicate nested key":     `{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0,"x":{"a":1,"a":2}}}`,
		"missing result_type":      `{"result":{"preimage":"` + preimageA + `","fees_paid":0}}`,
		"result_type empty result": `{"result_type":"","result":{"preimage":"` + preimageA + `","fees_paid":0}}`,
		"result_type not string":   `{"result_type":5,"result":{}}`,
		"result_type null":         `{"result_type":null,"error":{"code":"OTHER"}}`,
		"neither":                  `{"result_type":"pay_invoice"}`,
		"both":                     `{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0},"error":{"code":"OTHER","message":"x"}}`,
		"result null":              `{"result_type":"pay_invoice","result":null}`,
		"error null":               `{"result_type":"pay_invoice","error":null}`,
		"result array":             `{"result_type":"pay_invoice","result":[]}`,
		"result string":            `{"result_type":"pay_invoice","result":"x"}`,
		"error empty code":         `{"result_type":"pay_invoice","error":{"code":"","message":"x"}}`,
		"error missing code":       `{"result_type":"pay_invoice","error":{"message":"x"}}`,
		"error code not string":    `{"result_type":"pay_invoice","error":{"code":1}}`,
		"error code lowercase":     `{"result_type":"pay_invoice","error":{"code":"internal"}}`,
		"error code long":          `{"result_type":"pay_invoice","error":{"code":"` + strings.Repeat("A", 65) + `"}}`,
		"error message not string": `{"result_type":"pay_invoice","error":{"code":"OTHER","message":5}}`,
		"error string":             `{"result_type":"pay_invoice","error":"boom"}`,
		"invalid utf-8":            `{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0,"x":"` + "\xff\xfe" + `"}}`,
		"deep nesting":             `{"result_type":"pay_invoice","result":{"preimage":"` + preimageA + `","fees_paid":0,"x":` + strings.Repeat("[", 100000) + strings.Repeat("]", 100000) + `}}`,
		"pay preimage short":       wrap("pay_invoice", `{"preimage":"abcd","fees_paid":0}`),
		"pay preimage uppercase":   wrap("pay_invoice", `{"preimage":"`+strings.ToUpper(preimageA)+`","fees_paid":0}`),
		"pay preimage missing":     wrap("pay_invoice", `{"fees_paid":0}`),
		"pay preimage null":        wrap("pay_invoice", `{"preimage":null,"fees_paid":0}`),
		"pay fees missing":         wrap("pay_invoice", `{"preimage":"`+preimageA+`"}`),
		"pay fees negative":        wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":-1}`),
		"pay fees negative zero":   wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":-0}`),
		"pay fees exponent":        wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":1e3}`),
		"pay fees fraction":        wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":1.5}`),
		"pay fees dot zero":        wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":1.0}`),
		"pay fees string":          wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":"3"}`),
		"pay fees leading zero":    wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":03}`),
		"pay fees 2^53+1":          wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":9007199254740993}`),
		"pay fees huge":            wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":99999999999999999999999}`),
		"budget used string":       wrap("get_budget", `{"used_budget":"1","total_budget":5}`),
		"budget total missing":     wrap("get_budget", `{"used_budget":1}`),
		"budget used missing":      wrap("get_budget", `{"total_budget":1}`),
		"budget renews negative":   wrap("get_budget", `{"used_budget":1,"total_budget":5,"renews_at":-1}`),
		"budget period number":     wrap("get_budget", `{"used_budget":1,"total_budget":5,"renewal_period":5}`),
		"info methods not array":   wrap("get_info", `{"methods":"pay_invoice"}`),
		"info methods non-string":  wrap("get_info", `{"methods":[1]}`),
		"info methods missing":     wrap("get_info", `{"network":"regtest"}`),
		"info height negative":     wrap("get_info", `{"methods":[],"block_height":-1}`),
		"info height 2^53+1":       wrap("get_info", `{"methods":[],"block_height":9007199254740993}`),
		"info network number":      wrap("get_info", `{"methods":[],"network":1}`),
		"list not object":          wrap("list_transactions", `[]`),
		"list missing txs":         wrap("list_transactions", `{"total_count":0}`),
		"list tx null":             wrap("list_transactions", `{"transactions":[null],"total_count":1}`),
		"list tx bad":              wrap("list_transactions", `{"transactions":[{}],"total_count":1}`),
		"list total negative":      wrap("list_transactions", `{"transactions":[],"total_count":-1}`),
		"list total missing":       wrap("list_transactions", `{"transactions":[]}`),
		"list txs null":            wrap("list_transactions", `{"transactions":null,"total_count":0}`),
		"tx invalid":               wrap("make_invoice", `{"type":"incoming"}`),
		"hold tx invalid":          wrap("make_hold_invoice", `[]`),
		"settle hold non-empty":    wrap("settle_hold_invoice", `[]`),
		"huge string":              wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":0,"x":"`+big+`"}`) + strings.Repeat(" ", 1),
	}
	// "huge string" is valid except for size handling; it must still decode fine (the cap is NIP-44's).
	delete(malformed, "huge string")
	methodFor := func(plain string) string {
		for _, m := range []string{"get_budget", "get_info", "list_transactions", "make_hold_invoice", "settle_hold_invoice", "make_invoice"} {
			if strings.Contains(plain, `"result_type":"`+m+`"`) {
				return m
			}
		}
		return "pay_invoice"
	}
	for name, plain := range malformed {
		t.Run("malformed/"+name, func(t *testing.T) {
			m := methodFor(plain)
			var out any
			switch m {
			case "get_budget":
				out = &Budget{}
			case "get_info":
				out = &Info{}
			case "list_transactions":
				out = &TransactionList{}
			case "make_hold_invoice", "make_invoice":
				out = &Transaction{}
			case "settle_hold_invoice":
				out = nil
			default:
				out = &PayResult{}
			}
			err := decodeResponse(plain, m, out)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
			var we *WalletError
			if errors.As(err, &we) {
				t.Fatal("malformed reported as wallet error")
			}
		})
	}

	t.Run("huge ignored string is fine", func(t *testing.T) {
		var p PayResult
		if err := decodeResponse(wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":0,"x":"`+big+`"}`), "pay_invoice", &p); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("2^53 is accepted", func(t *testing.T) {
		var p PayResult
		if err := decodeResponse(wrap("pay_invoice", `{"preimage":"`+preimageA+`","fees_paid":9007199254740992}`), "pay_invoice", &p); err != nil || p.FeesPaidMsat != 1<<53 {
			t.Fatalf("%v %+v", err, p)
		}
	})
	t.Run("usage errors", func(t *testing.T) {
		var p PayResult
		if err := decodeResponse(good, "no_such_method", &p); err == nil || errors.Is(err, ErrMalformed) {
			t.Fatalf("unknown method: %v", err)
		}
		var b Budget
		if err := decodeResponse(good, "pay_invoice", &b); err == nil || errors.Is(err, ErrMalformed) {
			t.Fatalf("wrong out type: %v", err)
		}
		if err := decodeResponse(good, "pay_invoice", nil); err == nil || errors.Is(err, ErrMalformed) {
			t.Fatalf("nil out: %v", err)
		}
	})
	t.Run("out untouched on failure", func(t *testing.T) {
		p := PayResult{Preimage: "keep"}
		_ = decodeResponse(wrap("pay_invoice", `{"preimage":"bad","fees_paid":0}`), "pay_invoice", &p)
		if p.Preimage != "keep" {
			t.Fatalf("%+v", p)
		}
	})
}

func TestTransactionValidation(t *testing.T) {
	base := map[string]string{
		"type": `"incoming"`, "state": `"pending"`, "invoice": `"lnbcrt1"`, "description": `"d"`, "description_hash": `""`,
		"preimage": `""`, "payment_hash": `"` + hashA + `"`, "amount": `1000`, "fees_paid": `0`, "created_at": `1800000000`,
		"expires_at": `null`, "settled_at": `null`, "metadata": `{}`,
	}
	build := func(over map[string]string, drop ...string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		for _, k := range drop {
			delete(m, k)
		}
		var parts []string
		for k, v := range m {
			parts = append(parts, strconv.Quote(k)+":"+v)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	decode := func(js string) (Transaction, error) {
		var tx Transaction
		err := decodeResponse(`{"result_type":"lookup_invoice","result":`+js+`}`, "lookup_invoice", &tx)
		return tx, err
	}

	for _, typ := range []string{"incoming", "outgoing"} {
		for _, st := range []string{"pending", "settled", "failed", "accepted", "expired"} {
			if _, err := decode(build(map[string]string{"type": strconv.Quote(typ), "state": strconv.Quote(st)})); err != nil {
				t.Errorf("%s/%s: %v", typ, st, err)
			}
		}
	}
	bad := map[string]map[string]string{
		"type unknown":       {"type": `"sideways"`},
		"type uppercase":     {"type": `"Incoming"`},
		"type empty":         {"type": `""`},
		"type number":        {"type": `1`},
		"state unknown":      {"state": `"PENDING"`},
		"state empty":        {"state": `""`},
		"hash short":         {"payment_hash": `"abcd"`},
		"hash uppercase":     {"payment_hash": `"` + strings.ToUpper(hashA) + `"`},
		"hash 65":            {"payment_hash": `"` + hashA + `0"`},
		"hash non-hex":       {"payment_hash": `"` + strings.Repeat("g", 64) + `"`},
		"hash null":          {"payment_hash": `null`},
		"amount negative":    {"amount": `-1`},
		"amount -0":          {"amount": `-0`},
		"amount exponent":    {"amount": `1e3`},
		"amount fraction":    {"amount": `1.5`},
		"amount 2^53+1":      {"amount": `9007199254740993`},
		"amount string":      {"amount": `"5"`},
		"amount null":        {"amount": `null`},
		"fees negative":      {"fees_paid": `-5`},
		"fees 2^53+1":        {"fees_paid": `9007199254740993`},
		"fees fraction":      {"fees_paid": `0.5`},
		"created negative":   {"created_at": `-1`},
		"created string":     {"created_at": `"1"`},
		"expires negative":   {"expires_at": `-1`},
		"expires string":     {"expires_at": `"x"`},
		"expires fraction":   {"expires_at": `1.5`},
		"settled 2^53+1":     {"settled_at": `9007199254740993`},
		"settled bool":       {"settled_at": `true`},
		"preimage uppercase": {"preimage": `"` + strings.ToUpper(preimageA) + `"`},
		"preimage short":     {"preimage": `"ab"`},
		"preimage number":    {"preimage": `5`},
		"invoice number":     {"invoice": `5`},
		"description array":  {"description": `[]`},
		"description null":   {"description": `null`},
		"metadata string":    {"metadata": `"x"`},
		"metadata array":     {"metadata": `[]`},
		"metadata number":    {"metadata": `1`},
	}
	for name, over := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			if _, err := decode(build(over)); !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, k := range []string{"type", "state", "payment_hash", "amount", "created_at"} {
		t.Run("missing/"+k, func(t *testing.T) {
			if _, err := decode(build(nil, k)); !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("optional fields may be missing", func(t *testing.T) {
		tx, err := decode(build(nil, "invoice", "description", "description_hash", "preimage", "fees_paid", "expires_at", "settled_at", "metadata"))
		if err != nil {
			t.Fatal(err)
		}
		if tx.ExpiresAt != nil || tx.SettledAt != nil || tx.Metadata != nil || tx.FeesPaidMsat != 0 {
			t.Fatalf("%+v", tx)
		}
	})
	t.Run("expires and settled null or set", func(t *testing.T) {
		tx, err := decode(build(map[string]string{"expires_at": `5`, "settled_at": `1800000001`, "state": `"settled"`, "preimage": `"` + preimageA + `"`}))
		if err != nil || tx.ExpiresAt == nil || *tx.ExpiresAt != 5 || tx.SettledAt == nil || *tx.SettledAt != 1800000001 {
			t.Fatalf("%v %+v", err, tx)
		}
		tx, err = decode(build(map[string]string{"metadata": `null`}))
		if err != nil || tx.Metadata != nil {
			t.Fatalf("%v %+v", err, tx)
		}
	})
	t.Run("metadata is copied", func(t *testing.T) {
		js := build(map[string]string{"metadata": `{"k":"v"}`})
		tx, err := decode(js)
		if err != nil || string(tx.Metadata) != `{"k":"v"}` {
			t.Fatalf("%v %s", err, tx.Metadata)
		}
	})
}

func TestOpenNotification(t *testing.T) {
	v := newEnv(t)
	w := v.wallet.PublicKeyHex()
	c := v.client.PublicKeyHex()
	notif := func(typ, tx string) string {
		return `{"notification_type":"` + typ + `","notification":` + tx + `}`
	}
	good := []struct{ typ, tx string }{
		{"payment_received", hubTx("incoming", "settled")},
		{"payment_sent", hubTx("outgoing", "settled")},
		{"hold_invoice_accepted", hubTx("incoming", "accepted")},
	}
	for _, g := range good {
		t.Run("ok/"+g.typ, func(t *testing.T) {
			e := v.notification(t, notif(g.typ, g.tx))
			n, err := openNotification(e, v.conv, w, c)
			if err != nil {
				t.Fatal(err)
			}
			if n.Type != g.typ || n.EventID != e.ID || n.Tx.PaymentHash != hashA {
				t.Fatalf("%+v", n)
			}
		})
	}

	t.Run("exported-conv agreement", func(t *testing.T) {
		wc, _ := nip44.ConversationKey(v.wallet, c)
		if wc != v.conv {
			t.Fatal("conversation keys differ")
		}
	})

	okPlain := notif("payment_received", hubTx("incoming", "settled"))
	resign := func(e *nostr.Event, k *nostr.SecretKey) {
		e.PubKey, e.ID, e.Sig = "", "", ""
		if err := nostr.Sign(e, k); err != nil {
			t.Fatal(err)
		}
	}
	e23196 := &nostr.Event{Kind: 23196, CreatedAt: testNow.Unix(), Tags: [][]string{{"p", c}}, Content: "x?iv=y"}
	if err := nostr.Sign(e23196, v.wallet); err != nil {
		t.Fatal(err)
	}
	badSig := v.notification(t, okPlain)
	badSig.Sig = strings.Repeat("0", 128)
	otherAuthor := v.notification(t, okPlain)
	resign(otherAuthor, v.other)
	refuse := map[string]*nostr.Event{
		"23196 dropped":         e23196,
		"response kind":         signed(t, v.wallet, 23195, [][]string{{"p", c}, {"e", hashA}}, v.conv, okPlain),
		"different author":      otherAuthor,
		"different p":           signed(t, v.wallet, 23197, [][]string{{"p", v.other.PublicKeyHex()}}, v.conv, okPlain),
		"bad signature":         badSig,
		"unknown type":          v.notification(t, notif("payment_failed", hubTx("outgoing", "failed"))),
		"empty type":            v.notification(t, notif("", hubTx("incoming", "settled"))),
		"type number":           v.notification(t, `{"notification_type":1,"notification":`+hubTx("incoming", "settled")+`}`),
		"received but outgoing": v.notification(t, notif("payment_received", hubTx("outgoing", "settled"))),
		"sent but incoming":     v.notification(t, notif("payment_sent", hubTx("incoming", "settled"))),
		"missing notification":  v.notification(t, `{"notification_type":"payment_received"}`),
		"null notification":     v.notification(t, `{"notification_type":"payment_received","notification":null}`),
		"bad tx":                v.notification(t, notif("payment_received", `{"type":"incoming"}`)),
		"extra key":             v.notification(t, `{"notification_type":"payment_received","notification":`+hubTx("incoming", "settled")+`,"x":1}`),
		"duplicate key":         v.notification(t, `{"notification_type":"payment_received","notification_type":"payment_sent","notification":`+hubTx("incoming", "settled")+`}`),
		"not json":              v.notification(t, "hello"),
		"array":                 v.notification(t, "[]"),
	}
	for name, e := range refuse {
		t.Run("refuse/"+name, func(t *testing.T) {
			if n, err := openNotification(e, v.conv, w, c); err == nil {
				t.Fatalf("accepted: %+v", n)
			}
		})
	}
	t.Run("nil event", func(t *testing.T) {
		if _, err := openNotification(nil, v.conv, w, c); err == nil {
			t.Fatal("accepted nil")
		}
	})
	t.Run("content errors are malformed", func(t *testing.T) {
		_, err := openNotification(v.notification(t, `{"notification_type":"payment_failed","notification":{}}`), v.conv, w, c)
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("%v", err)
		}
	})
}

func TestCheckJSON(t *testing.T) {
	ok := []string{`{}`, `[]`, `null`, `0`, `"x"`, ` {"a":{"b":[1,{"c":2}]},"d":{"b":1}} `, `{"a":1,"b":{"a":2}}`, `[{"a":1},{"a":1}]`}
	for _, s := range ok {
		if err := checkJSON([]byte(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{``, ` `, `{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `[{"a":1,"a":1}]`, `{} {}`, `1 2`, `{}x`, `{"a"`, "\"\xff\"",
		strings.Repeat("[", maxDepth+1) + strings.Repeat("]", maxDepth+1), `{"a":1,"a":2}`}
	for _, s := range bad {
		if err := checkJSON([]byte(s)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: %v", s, err)
		}
	}
	if err := checkJSON([]byte(strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth))); err != nil {
		t.Errorf("depth %d: %v", maxDepth, err)
	}
}

// The method is passed explicitly: the document's own result_type must be the only thing that differs,
// and every other part of the document must be valid for the method being decoded.
func TestDecodeResponseResultTypeMismatch(t *testing.T) {
	pay := `{"preimage":"` + preimageA + `","fees_paid":0}`
	wrap := func(rt, result string) string { return `{"result_type":"` + rt + `","result":` + result + `}` }
	cases := []struct {
		name, plain, method string
		out                 any
	}{
		{"get_info result for pay_invoice", wrap("get_info", pay), "pay_invoice", &PayResult{}},
		{"cancel for settle", wrap("cancel_hold_invoice", `{}`), "settle_hold_invoice", nil},
		{"settle for cancel", wrap("settle_hold_invoice", `{}`), "cancel_hold_invoice", nil},
		{"make_invoice tx for lookup_invoice", wrap("make_invoice", hubTx("incoming", "pending")), "lookup_invoice", &Transaction{}},
		{"lookup_invoice tx for make_invoice", wrap("lookup_invoice", hubTx("incoming", "pending")), "make_invoice", &Transaction{}},
		{"empty result_type with a result", wrap("", pay), "pay_invoice", &PayResult{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := decodeResponse(c.plain, c.method, c.out); !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
	// Positive controls: the same documents decode under their own method.
	controls := []struct {
		plain, method string
		out           any
	}{
		{wrap("pay_invoice", pay), "pay_invoice", &PayResult{}},
		{wrap("cancel_hold_invoice", `{}`), "cancel_hold_invoice", nil},
		{wrap("settle_hold_invoice", `{}`), "settle_hold_invoice", nil},
		{wrap("make_invoice", hubTx("incoming", "pending")), "make_invoice", &Transaction{}},
		{wrap("lookup_invoice", hubTx("incoming", "pending")), "lookup_invoice", &Transaction{}},
	}
	for _, c := range controls {
		if err := decodeResponse(c.plain, c.method, c.out); err != nil {
			t.Errorf("control %s: %v", c.method, err)
		}
	}
}

func TestDecodeResponseTypedNilOut(t *testing.T) {
	good := `{"result_type":"lookup_invoice","result":` + hubTx("incoming", "pending") + `}`
	if err := decodeResponse(good, "lookup_invoice", (*Transaction)(nil)); err == nil || errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}
