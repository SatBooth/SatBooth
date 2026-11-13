package xmrrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	tPrimary = "4" + strings.Repeat("A", 94)
	tSub1    = "8" + strings.Repeat("B", 94)
	tSub2    = "8" + strings.Repeat("C", 94)
)

func txid(i int) string { return strings.Repeat("0", 60) + strconv.FormatInt(int64(0xa000+i), 16) }

// entry is a transfer_entry as wallet-rpc v0.18.5.4 writes it (src/wallet/wallet_rpc_server_commands_defs.h,
// struct transfer_entry), with the fields in over replaced (a value "" removes the field).
func entry(over map[string]string) string {
	f := map[string]string{
		"txid": `"` + txid(1) + `"`, "payment_id": `"0000000000000000"`, "height": "1200", "timestamp": "1730000000",
		"amount": "1000000000000", "amounts": "[1000000000000]", "fee": "30000000", "note": `""`, "type": `"in"`,
		"unlock_time": "0", "locked": "true", "subaddr_index": `{"major":0,"minor":1}`,
		"subaddr_indices": `[{"major":0,"minor":1}]`, "address": `"` + tSub1 + `"`, "double_spend_seen": "false",
		"confirmations": "3", "suggested_confirmations_threshold": "1",
	}
	for k, v := range over {
		if v == "" {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + k + `":` + f[k])
	}
	b.WriteByte('}')
	return b.String()
}

// paramsOf returns the params the fake last received, as a map of raw values.
func paramsOf(t *testing.T, f *fake) map[string]string {
	t.Helper()
	f.last.Lock()
	defer f.last.Unlock()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(f.last.body["params"], &m); err != nil {
		t.Fatalf("params %s: %v", f.last.body["params"], err)
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}

func TestTransfersDecode(t *testing.T) {
	good := []string{
		entry(map[string]string{"txid": `"` + txid(1) + `"`, "amount": "1", "subaddr_index": `{"major":0,"minor":1}`}),
		entry(map[string]string{"txid": `"` + txid(2) + `"`, "amount": "9007199254740991", "subaddr_index": `{"major":0,"minor":2}`}),
		entry(map[string]string{"txid": `"` + txid(3) + `"`, "amount": "9223372036854775807", "subaddr_index": `{"major":2,"minor":3}`}),
		entry(map[string]string{"txid": `"` + txid(4) + `"`, "unlock_time": "1500000000", "subaddr_index": `{"major":0,"minor":4}`}),
		entry(map[string]string{"txid": `"` + txid(5) + `"`, "locked": "false", "height": "0", "subaddr_index": `{"major":0,"minor":5}`}),
		entry(map[string]string{"txid": `"` + txid(6) + `"`, "address": `"` + tSub2 + `"`, "subaddr_index": `{"major":0,"minor":4294967295}`}),
	}
	bad := []string{
		entry(map[string]string{"amount": "0"}),
		entry(map[string]string{"amount": "-1"}),
		entry(map[string]string{"amount": "9223372036854775808"}),
		entry(map[string]string{"amount": "1.5"}),
		entry(map[string]string{"txid": `"` + txid(1)[1:] + `"`}),
		entry(map[string]string{"txid": `"` + strings.ToUpper(txid(7)) + `"`}),
		entry(map[string]string{"type": `"out"`}),
	}
	pool := entry(map[string]string{"txid": `"` + txid(9) + `"`, "type": `"pool"`, "height": "0", "double_spend_seen": "true",
		"subaddr_index": `{"major":0,"minor":9}`})
	body := `{"in":[` + strings.Join(append(append([]string{}, good[:3]...), append(bad, good[3:]...)...), ",") + `],"pool":[` + pool + `]}`
	got, err := decodeTransfers(json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.In) != 6 || got.Dropped != 7 || len(got.Pool) != 1 {
		t.Fatalf("in %d pool %d dropped %d, want 6 1 7", len(got.In), len(got.Pool), got.Dropped)
	}
	want := Transfer{TxID: txid(1), Address: tSub1, Account: 0, Index: 1, Amount: 1, Height: 1200, Locked: true, Type: "in", Confirmations: 3}
	if got.In[0] != want {
		t.Errorf("first entry %+v, want %+v", got.In[0], want)
	}
	if got.In[1].Amount != 9007199254740991 || got.In[2].Amount != math.MaxInt64 || got.In[2].Account != 2 || got.In[2].Index != 3 {
		t.Errorf("entries 2 and 3: %+v %+v", got.In[1], got.In[2])
	}
	if got.In[3].UnlockTime != 1500000000 || got.In[4].Locked || got.In[4].Height != 0 || got.In[5].Index != math.MaxUint32 || got.In[5].Address != tSub2 {
		t.Errorf("entries 4 to 6: %+v %+v %+v", got.In[3], got.In[4], got.In[5])
	}
	p := got.Pool[0]
	if p.TxID != txid(9) || p.Type != "pool" || !p.DoubleSpendSeen || p.Height != 0 || p.Index != 9 {
		t.Errorf("pool entry %+v", p)
	}
	// wallet-rpc leaves out an empty list (epee writes no key for an empty container): no entries, no error
	for _, b := range []string{`{}`, `{"in":[]}`, `{"pool":null}`, `{"out":[` + entry(map[string]string{"type": `"out"`}) + `]}`} {
		got, err := decodeTransfers(json.RawMessage(b))
		if err != nil || len(got.In) != 0 || len(got.Pool) != 0 || got.Dropped != 0 {
			t.Errorf("%s: %+v %v", b, got, err)
		}
	}
	for _, b := range []string{`[]`, `null`, `"x"`, `{"in":{}}`, `{"in":5}`, `{"pool":"x"}`, `{"in":[1,2],"pool":{}}`} {
		if _, err := decodeTransfers(json.RawMessage(b)); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%s: err %v, want ErrBadAnswer", b, err)
		}
	}
}

// TestTransfersSkipCoinbase: wallet-rpc lists a coinbase to the account in "in" with type "block"
// (fill_transfer_entry, src/wallet/wallet_rpc_server.cpp:520, tag v0.18.5.4). It is not a payment: a well-formed one
// is skipped without being counted in Dropped; a malformed one is still dropped and counted, as any other.
func TestTransfersSkipCoinbase(t *testing.T) {
	block := entry(map[string]string{"txid": `"` + txid(2) + `"`, "type": `"block"`, "subaddr_index": `{"major":0,"minor":0}`})
	badBlock := entry(map[string]string{"txid": `"` + txid(3) + `"`, "type": `"block"`, "amount": "0"})
	for _, c := range []struct {
		name    string
		in      []string
		kept    int
		dropped int
	}{
		{"coinbase beside a payment", []string{block, entry(nil)}, 1, 0},
		{"coinbase alone", []string{block}, 0, 0},
		{"two coinbases", []string{block, block}, 0, 0},
		{"malformed coinbase", []string{badBlock, entry(nil)}, 1, 1},
		{"coinbase and a bad entry", []string{block, entry(map[string]string{"type": `"out"`})}, 0, 1},
	} {
		got, err := decodeTransfers(json.RawMessage(`{"in":[` + strings.Join(c.in, ",") + `]}`))
		if err != nil || len(got.In) != c.kept || got.Dropped != c.dropped || len(got.Pool) != 0 {
			t.Errorf("%s: in %d dropped %d err %v, want in %d dropped %d", c.name, len(got.In), got.Dropped, err, c.kept, c.dropped)
		}
		for _, tr := range got.In {
			if tr.Type != "in" {
				t.Errorf("%s: kept an entry of type %q", c.name, tr.Type)
			}
		}
	}
	// "block" belongs to the in list only: in the pool it is an entry SatBooth could not read.
	got, err := decodeTransfers(json.RawMessage(`{"pool":[` + block + `]}`))
	if err != nil || len(got.Pool) != 0 || got.Dropped != 1 {
		t.Errorf("a block entry in the pool: %+v %v, want dropped", got, err)
	}
}

// TestTransferEntryRules: each rule of an entry at its boundary; a bad entry is dropped, never the answer.
func TestTransferEntryRules(t *testing.T) {
	keep := map[string]map[string]string{
		"amount 1":                 {"amount": "1"},
		"amount 2^63-1":            {"amount": "9223372036854775807"},
		"amount above MaxPiconero": {"amount": "9007199254740992"},
		"height 500000000":         {"height": "500000000"},
		"unlock_time 2^64-1":       {"unlock_time": "18446744073709551615"},
		"unlock_time 2^63":         {"unlock_time": "9223372036854775808"},
		"minor 0":                  {"subaddr_index": `{"major":0,"minor":0}`},
		"unknown fields":           {"zzz": `{"a":[1,2]}`, "amounts": `"odd"`, "destinations": "7"},
		"address empty":            {"address": `""`},
		"no optional fields":       {"payment_id": "", "timestamp": "", "amounts": "", "fee": "", "note": "", "subaddr_indices": "", "suggested_confirmations_threshold": ""},
		"confirmations 0":          {"confirmations": "0"},
		"confirmations 500000000":  {"confirmations": "500000000"},
		"no confirmations":         {"confirmations": ""},
	}
	drop := map[string]map[string]string{
		"not an object":      nil,
		"no txid":            {"txid": ""},
		"txid null":          {"txid": "null"},
		"txid number":        {"txid": "5"},
		"txid 65 hex":        {"txid": `"` + txid(1) + `0"`},
		"txid non-hex":       {"txid": `"` + strings.Repeat("g", 64) + `"`},
		"no amount":          {"amount": ""},
		"amount string":      {"amount": `"5"`},
		"amount 1e3":         {"amount": "1e3"},
		"amount 1.0":         {"amount": "1.0"},
		"amount -0":          {"amount": "-0"},
		"amount 2^64":        {"amount": "18446744073709551616"},
		"amount null":        {"amount": "null"},
		"no type":            {"type": ""},
		"confirmations -1":   {"confirmations": "-1"},
		"confirmations 1.5":  {"confirmations": "1.5"},
		"confirmations 1e3":  {"confirmations": "1e3"},
		"confirmations big":  {"confirmations": "500000001"},
		"confirmations 2^64": {"confirmations": "18446744073709551616"},
		"confirmations str":  {"confirmations": `"3"`},
		"confirmations null": {"confirmations": "null"},
		"type pending":       {"type": `"pending"`},
		"type failed":        {"type": `"failed"`},
		"type IN":            {"type": `"IN"`},
		"type pool in in":    {"type": `"pool"`},
		"no subaddr_index":   {"subaddr_index": ""},
		"no minor":           {"subaddr_index": `{"major":0}`},
		"no major":           {"subaddr_index": `{"minor":1}`},
		"minor negative":     {"subaddr_index": `{"major":0,"minor":-1}`},
		"minor 2^32":         {"subaddr_index": `{"major":0,"minor":4294967296}`},
		"minor 1.5":          {"subaddr_index": `{"major":0,"minor":1.5}`},
		"subaddr_index list": {"subaddr_index": `[0,1]`},
		"no height":          {"height": ""},
		"height negative":    {"height": "-1"},
		"height 500000001":   {"height": "500000001"},
		"no unlock_time":     {"unlock_time": ""},
		"unlock_time -1":     {"unlock_time": "-1"},
		"unlock_time 2^64":   {"unlock_time": "18446744073709551616"},
		"unlock_time string": {"unlock_time": `"0"`},
		"no locked":          {"locked": ""},
		"locked string":      {"locked": `"true"`},
		"no double_spend":    {"double_spend_seen": ""},
		"double_spend 1":     {"double_spend_seen": "1"},
		"no address":         {"address": ""},
		"address number":     {"address": "4"},
	}
	for name, over := range keep {
		got, err := decodeTransfers(json.RawMessage(`{"in":[` + entry(over) + `]}`))
		if err != nil || len(got.In) != 1 || got.Dropped != 0 {
			t.Errorf("%s: %+v %v, want kept", name, got, err)
		}
	}
	for name, over := range drop {
		e := `[1]`
		if over != nil {
			e = entry(over)
		}
		got, err := decodeTransfers(json.RawMessage(`{"in":[` + e + `,` + entry(nil) + `]}`))
		if err != nil || len(got.In) != 1 || got.Dropped != 1 {
			t.Errorf("%s: %+v %v, want dropped", name, got, err)
		}
	}
	got, _ := decodeTransfers(json.RawMessage(`{"in":[` + entry(map[string]string{"unlock_time": "18446744073709551615"}) + `]}`))
	if got.In[0].UnlockTime != math.MaxInt64 {
		t.Errorf("unlock_time 2^64-1 read as %d, want it clamped to 2^63-1 (still locked)", got.In[0].UnlockTime)
	}
}

func TestGetTransfers(t *testing.T) {
	f := newFake(t)
	f.reply = result(`{"in":[` + entry(nil) + `]}`)
	c := serveFake(t, f)
	got, err := c.GetTransfers(context.Background(), TransfersQuery{In: true, Pool: true, Account: 3, FilterByHeight: true, MinHeight: 10})
	if err != nil || len(got.In) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	want := map[string]string{"in": "true", "out": "false", "pending": "false", "failed": "false", "pool": "true",
		"filter_by_height": "true", "min_height": "10", "max_height": "500000000", "account_index": "3"}
	if p := paramsOf(t, f); !reflect.DeepEqual(p, want) {
		t.Errorf("params %v\nwant   %v", p, want)
	}
	if _, err := c.GetTransfers(context.Background(), TransfersQuery{In: true, MinHeight: 5, MaxHeight: 7}); err != nil {
		t.Fatal(err)
	}
	if p := paramsOf(t, f); p["max_height"] != "7" || p["pool"] != "false" || p["filter_by_height"] != "false" {
		t.Errorf("params %v", p)
	}
	n := f.requests.Load()
	for _, q := range []TransfersQuery{{MinHeight: -1}, {MaxHeight: -1}, {MaxHeight: 500000001}, {MinHeight: 500000001}} {
		if _, err := c.GetTransfers(context.Background(), q); !errors.Is(err, ErrArgument) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	if f.requests.Load() != n {
		t.Error("a refused query was sent")
	}
}

func TestTransferByTxID(t *testing.T) {
	f := newFake(t)
	first := entry(map[string]string{"txid": `"` + txid(5) + `"`, "subaddr_index": `{"major":0,"minor":1}`})
	second := entry(map[string]string{"txid": `"` + txid(5) + `"`, "subaddr_index": `{"major":0,"minor":2}`, "address": `"` + tSub2 + `"`, "amount": "7"})
	out := entry(map[string]string{"txid": `"` + txid(5) + `"`, "type": `"out"`})
	f.reply = result(`{"transfer":` + first + `,"transfers":[` + first + `,` + second + `,` + out + `]}`)
	c := serveFake(t, f)
	got, err := c.GetTransferByTxID(context.Background(), txid(5), 0)
	if err != nil || len(got) != 2 || got[0].Index != 1 || got[1].Index != 2 || got[1].Amount != 7 || got[1].Address != tSub2 {
		t.Fatalf("%+v %v", got, err)
	}
	if p := paramsOf(t, f); !reflect.DeepEqual(p, map[string]string{"txid": `"` + txid(5) + `"`, "account_index": "0"}) {
		t.Errorf("params %v", p)
	}
	// a success wallet-rpc never sends (it answers success only with a non-empty "transfers",
	// wallet_rpc_server.cpp:3063-3067): an unexpected answer, never "not found"
	bad := entry(map[string]string{"txid": `"` + txid(5) + `"`, "amount": "0"})
	for _, b := range []string{`{}`, `{"transfer":` + first + `}`, `{"transfers":[]}`, `{"transfers":null}`,
		`{"transfers":[` + bad + `]}`, `{"transfers":[` + bad + `,1]}`} {
		f.reply = result(b)
		if got, err := c.GetTransferByTxID(context.Background(), txid(5), 0); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%.60s: %+v %v, want ErrBadAnswer", b, got, err)
		}
	}
	// an outgoing-only transaction keeps no incoming entry: empty, no error
	f.reply = result(`{"transfers":[` + out + `]}`)
	if got, err := c.GetTransferByTxID(context.Background(), txid(5), 0); err != nil || got == nil || len(got) != 0 {
		t.Errorf("out only: %+v %v", got, err)
	}
	// a malformed entry beside a good one is dropped, entry by entry
	f.reply = result(`{"transfers":[` + bad + `,` + second + `]}`)
	if got, err := c.GetTransferByTxID(context.Background(), txid(5), 0); err != nil || len(got) != 1 || got[0].Index != 2 {
		t.Errorf("one bad, one good: %+v %v", got, err)
	}
	f.reply = result(`{"transfers":{}}`)
	if _, err := c.GetTransferByTxID(context.Background(), txid(5), 0); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("transfers not a list: %v", err)
	}
	// the not-found code: an empty slice, no error
	f.reply = rpcError(-8, "Transaction not found.")
	got, err = c.GetTransferByTxID(context.Background(), txid(5), 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("not found: %#v %v", got, err)
	}
	// any other code is an error: never "not found"
	for _, code := range []int{-1, -13, -32601, -3, -14, 0, 8} {
		f.reply = rpcError(code, "x")
		var e *Error
		if _, err := c.GetTransferByTxID(context.Background(), txid(5), 0); !errors.As(err, &e) || e.Code != code {
			t.Errorf("code %d: %v", code, err)
		}
	}
	f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) { w.WriteHeader(500) }
	if _, err := c.GetTransferByTxID(context.Background(), txid(5), 0); !errors.Is(err, ErrHTTP) {
		t.Errorf("500: %v", err)
	}
	n := f.requests.Load()
	for _, id := range []string{"", txid(5)[1:], strings.ToUpper(txid(5)), txid(5) + "0", strings.Repeat("g", 64)} {
		if _, err := c.GetTransferByTxID(context.Background(), id, 0); !errors.Is(err, ErrArgument) {
			t.Errorf("txid %q: %v", id, err)
		}
	}
	if f.requests.Load() != n {
		t.Error("a refused txid was sent")
	}
}

func TestTypedCalls(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	ctx := context.Background()

	// the answer recorded from wallet-rpc v0.18.5.4
	f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) {
		io.WriteString(w, "{\r\n  \"id\": \"0\",\r\n  \"jsonrpc\": \"2.0\",\r\n  \"result\": {\r\n    \"release\": true,\r\n    \"version\": 65568\r\n  }\r\n}")
	}
	if v, err := c.GetVersion(ctx); err != nil || v != 65568 {
		t.Errorf("GetVersion %d %v", v, err)
	}
	f.reply = result(`{"height":1234}`)
	if h, err := c.GetHeight(ctx); err != nil || h != 1234 {
		t.Errorf("GetHeight %d %v", h, err)
	}
	f.reply = result(`{"address":"` + tPrimary + `","addresses":[{"address":"` + tSub1 + `","label":"x","address_index":1,"used":true},{"address":"` + tSub2 + `","label":"","address_index":2,"used":false}]}`)
	a, err := c.GetAddress(ctx, 0, []uint32{1, 2})
	want := Addresses{Address: tPrimary, Addresses: []AddressEntry{{Address: tSub1, Label: "x", Index: 1, Used: true}, {Address: tSub2, Index: 2}}}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Errorf("GetAddress %+v %v", a, err)
	}
	if p := paramsOf(t, f); p["account_index"] != "0" || p["address_index"] != "[1,2]" || len(p) != 2 {
		t.Errorf("GetAddress params %v", p)
	}
	if _, err := c.GetAddress(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	if p := paramsOf(t, f); p["account_index"] != "1" || p["address_index"] != "[]" {
		t.Errorf("GetAddress nil indices sent as %v", p)
	}
	f.reply = result(`{"address":"` + tSub1 + `","address_index":7,"address_indices":[7],"addresses":["` + tSub1 + `"]}`)
	if cr, err := c.CreateAddress(ctx, 0, "order"); err != nil || cr != (Created{Address: tSub1, Index: 7}) {
		t.Errorf("CreateAddress %+v %v", cr, err)
	}
	if p := paramsOf(t, f); !reflect.DeepEqual(p, map[string]string{"account_index": "0", "label": `"order"`}) {
		t.Errorf("CreateAddress params %v", p)
	}
	f.reply = result(`{}`)
	if err := c.Store(ctx); err != nil {
		t.Errorf("Store %v", err)
	}
	f.reply = rpcError(-29, "This wallet is watch-only and cannot sign")
	if code, err := c.SignTransferProbe(ctx); err != nil || code != -29 {
		t.Errorf("probe on a view-only wallet: %d %v", code, err)
	}
	if p := paramsOf(t, f); !reflect.DeepEqual(p, map[string]string{"unsigned_txset": `""`, "export_raw": "false"}) {
		t.Errorf("probe params %v", p)
	}
	f.reply = result(`{"signed_txset":"","tx_hash_list":[]}`)
	if code, err := c.SignTransferProbe(ctx); err != nil || code != 0 {
		t.Errorf("probe success: %d %v", code, err)
	}
	f.reply = func(w http.ResponseWriter, _ string, _ json.RawMessage) { w.WriteHeader(503) }
	if code, err := c.SignTransferProbe(ctx); !errors.Is(err, ErrHTTP) || code != 0 {
		t.Errorf("probe on a failure: %d %v", code, err)
	}
	f.reply = result(`"x"`)
	if code, err := c.SignTransferProbe(ctx); err != nil || code != 0 {
		t.Errorf("probe: any result is a success: %d %v", code, err)
	}
}

func TestTypedCallsBadShapes(t *testing.T) {
	f := newFake(t)
	c := serveFake(t, f)
	ctx := context.Background()
	calls := map[string]func() error{
		"get_version": func() error { _, err := c.GetVersion(ctx); return err },
		"get_height":  func() error { _, err := c.GetHeight(ctx); return err },
		"get_address": func() error { _, err := c.GetAddress(ctx, 0, nil); return err },
		"create":      func() error { _, err := c.CreateAddress(ctx, 0, ""); return err },
		"transfers":   func() error { _, err := c.GetTransfers(ctx, TransfersQuery{In: true}); return err },
		"by_txid":     func() error { _, err := c.GetTransferByTxID(ctx, txid(1), 0); return err },
	}
	bad := map[string][]string{
		"get_version": {`[]`, `"x"`, `{}`, `{"version":"1"}`, `{"version":-1}`, `{"version":4294967296}`, `{"version":1.5}`, `{"version":null}`},
		"get_height":  {`[]`, `{}`, `{"height":"1"}`, `{"height":-1}`, `{"height":500000001}`, `{"height":1e3}`},
		"get_address": {`[]`, `{}`, `{"address":5,"addresses":[]}`, `{"address":"a","addresses":{}}`, `{"address":"a","addresses":[1]}`,
			`{"address":"a","addresses":[{"address":"b","address_index":-1,"used":false}]}`,
			`{"address":"a","addresses":[{"address":"b","address_index":1,"used":"no"}]}`,
			`{"address":"a","addresses":[{"address_index":1}]}`, `{"address":"a","addresses":[{"address":"b"}]}`},
		"create":    {`[]`, `{}`, `{"address":"a"}`, `{"address_index":1}`, `{"address":"a","address_index":-1}`, `{"address":"","address_index":1}`},
		"transfers": {`[]`, `"x"`, `{"in":{}}`},
		"by_txid":   {`[]`, `{"transfers":5}`},
	}
	for name, bodies := range bad {
		for _, b := range bodies {
			f.reply = result(b)
			if err := calls[name](); !errors.Is(err, ErrBadAnswer) {
				t.Errorf("%s with %s: %v, want ErrBadAnswer", name, b, err)
			}
		}
	}
	// the boundaries that pass
	f.reply = result(`{"version":4294967295,"release":false}`)
	if v, err := c.GetVersion(ctx); err != nil || v != math.MaxUint32 {
		t.Errorf("version 2^32-1: %d %v", v, err)
	}
	f.reply = result(`{"height":500000000}`)
	if h, err := c.GetHeight(ctx); err != nil || h != 500000000 {
		t.Errorf("height max: %d %v", h, err)
	}
	f.reply = result(`{"address":"a"}`)
	if a, err := c.GetAddress(ctx, 0, nil); err != nil || a.Address != "a" || len(a.Addresses) != 0 {
		t.Errorf("no addresses list (epee leaves out an empty one): %+v %v", a, err)
	}
}

// Ruling 8-A4: an in entry shaped as wallet-rpc v0.18.5.4 serializes transfer_entry (every field
// KV_SERIALIZE except confirmations and suggested_confirmations_threshold, which are KV_SERIALIZE_OPT with default 0;
// wallet_rpc_server_commands_defs.h:1469-1488) is kept; the same entry without double_spend_seen is dropped without
// failing the answer, and counted, so the watcher's ALERT names the range.
func TestTransfersDropAnEntryMissingAField(t *testing.T) {
	full := `{"txid":"` + strings.Repeat("ab", 32) + `","payment_id":"0000000000000000","height":150,"timestamp":1,` +
		`"amount":1000000000000,"amounts":[1000000000000],"fee":0,"note":"","destinations":[],"type":"in","unlock_time":0,` +
		`"locked":true,"subaddr_index":{"major":0,"minor":3},"subaddr_indices":[{"major":0,"minor":3}],"address":"x",` +
		`"double_spend_seen":false,"confirmations":50,"suggested_confirmations_threshold":1}`
	tr, err := decodeTransfers([]byte(`{"in":[` + full + `]}`))
	if err != nil || len(tr.In) != 1 || tr.Dropped != 0 {
		t.Fatalf("the v0.18.5.4 shape: %+v, %v", tr, err)
	}
	missing := strings.Replace(full, `"double_spend_seen":false,`, "", 1)
	tr, err = decodeTransfers([]byte(`{"in":[` + missing + `]}`))
	if err != nil || len(tr.In) != 0 || tr.Dropped != 1 {
		t.Fatalf("got %+v, %v", tr, err)
	}
}

// A pool entry as wallet-rpc v0.18.5.4 wrote it in the regtest qualification (its 16 keys, values replaced): no
// confirmations key, because KV_SERIALIZE_OPT(confirmations, (uint64_t)0) (wallet_rpc_server_commands_defs.h:1486)
// leaves the key out when the value is 0 (KV_SERIALIZE_OPT_N, contrib/epee/include/serialization/
// keyvalue_serialization.h:90-96), which set_confirmations gives every pool entry. It is kept, with 0 confirmations;
// dropping it left every payment in the pool unseen. So is an in entry without the key (mined at or above the
// wallet's height).
func TestTransfersEntryWithoutConfirmations(t *testing.T) {
	pool := `{"address":"` + tSub1 + `","amount":1000000000000,"amounts":[1000000000000],"double_spend_seen":false,` +
		`"fee":30000000,"height":0,"locked":true,"note":"","payment_id":"0000000000000000",` +
		`"subaddr_index":{"major":0,"minor":1},"subaddr_indices":[{"major":0,"minor":1}],` +
		`"suggested_confirmations_threshold":1,"timestamp":1730000000,"txid":"` + txid(1) + `","type":"pool","unlock_time":0}`
	tr, err := decodeTransfers(json.RawMessage(`{"pool":[` + pool + `]}`))
	if err != nil || len(tr.Pool) != 1 || tr.Dropped != 0 || tr.Pool[0].Confirmations != 0 || tr.Pool[0].Type != "pool" {
		t.Fatalf("a pool entry without confirmations: %+v, %v; want kept with 0", tr, err)
	}
	in := entry(map[string]string{"confirmations": ""})
	tr, err = decodeTransfers(json.RawMessage(`{"in":[` + in + `]}`))
	if err != nil || len(tr.In) != 1 || tr.Dropped != 0 || tr.In[0].Confirmations != 0 {
		t.Fatalf("an in entry without confirmations: %+v, %v; want kept with 0", tr, err)
	}
	l, err := decodeTransferList(json.RawMessage(`{"transfers":[` + pool + `]}`))
	if err != nil || len(l) != 1 || l[0].Confirmations != 0 {
		t.Fatalf("get_transfer_by_txid's pool entry without confirmations: %+v, %v; want kept with 0", l, err)
	}
}

// Ruling 8-A6: an entry's confirmations (set_confirmations, src/wallet/wallet_rpc_server.cpp:163-168 at v0.18.5.4:
// the wallet's height less the entry's, 0 for a pool entry or one at or above the wallet's height, computed under the
// same refresh as the entry) are kept as sent when they are a plain integer of 0 to 500,000,000, and the entry is
// dropped otherwise. Seeded values cross both bounds, with the forms a JSON number can take.
func TestTransferConfirmationsProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 6))
	for range 5_000 {
		var raw string
		want, keep := int64(0), false
		switch r.IntN(5) { // a leading zero is not JSON: the whole answer fails, as for any field
		case 0:
			v := []int64{0, 1, 2, maxHeight - 1, maxHeight, maxHeight + 1, math.MaxInt64}[r.IntN(7)]
			raw, want, keep = strconv.FormatInt(v, 10), v, v <= maxHeight
		case 1:
			v := r.Int64N(maxHeight + 1)
			raw, want, keep = strconv.FormatInt(v, 10), v, true
		case 2:
			raw = "-" + strconv.FormatInt(r.Int64N(maxHeight)+1, 10)
		case 3:
			raw = strconv.FormatInt(r.Int64N(maxHeight), 10) + []string{".0", "e0", ".5", "E1"}[r.IntN(4)]
		case 4:
			raw = `"` + strconv.FormatInt(r.Int64N(maxHeight), 10) + `"`
		}
		got, err := decodeTransfers(json.RawMessage(`{"in":[` + entry(map[string]string{"confirmations": raw}) + `]}`))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		switch {
		case keep && (len(got.In) != 1 || got.In[0].Confirmations != want):
			t.Fatalf("confirmations %s: %+v, want kept with %d", raw, got, want)
		case !keep && (len(got.In) != 0 || got.Dropped != 1):
			t.Fatalf("confirmations %s: %+v, want dropped", raw, got)
		}
	}
}
