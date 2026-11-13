package btcrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

func txWithVin(vin string) string {
	return `{"txid":"` + tTx1 + `","confirmations":0,"decoded":{"vin":` + vin + `,"vout":[]}}`
}

// The vin of gettransaction: each entry is a coinbase (no outpoint) or a txid of 64 lowercase hex and an integer vout >= 0.
func TestDecodeTransactionInputs(t *testing.T) {
	zeros := strings.Repeat("0", 64)
	good := []struct {
		vin  string
		want []Outpoint
	}{
		{`[]`, nil},
		{`[{"coinbase":"03","sequence":1}]`, nil},
		{`[{"coinbase":"03"},{"coinbase":"04"}]`, nil},
		{`[{"txid":"` + tTx1 + `","vout":0}]`, []Outpoint{{tTx1, 0}}},
		{`[{"txid":"` + tTx2 + `","vout":4294967295,"sequence":1}]`, []Outpoint{{tTx2, 4294967295}}},
		{`[{"txid":"` + tTx1 + `","vout":1},{"txid":"` + zeros + `","vout":2}]`, []Outpoint{{tTx1, 1}, {zeros, 2}}},
		{`[{"coinbase":"03","txid":"zz","vout":-1}]`, nil}, // a coinbase key decides, as in Core's TxToUniv
	}
	for _, g := range good {
		tx, err := decodeTransaction(json.RawMessage(txWithVin(g.vin)))
		if err != nil || fmt.Sprint(tx.Inputs) != fmt.Sprint(g.want) {
			t.Errorf("vin %s: %+v, %v, want %v", g.vin, tx.Inputs, err, g.want)
		}
	}
	bad := []string{
		`null`, `{}`, `"x"`, `[null]`, `[1]`, `[[]]`, `[{}]`,
		`[{"txid":"` + tTx1 + `"}]`,
		`[{"vout":0}]`,
		`[{"txid":"` + tTx1 + `","vout":-1}]`,
		`[{"txid":"` + tTx1 + `","vout":4294967296}]`,
		`[{"txid":"` + tTx1 + `","vout":1.5}]`,
		`[{"txid":"` + tTx1 + `","vout":1.0}]`,
		`[{"txid":"` + tTx1 + `","vout":"1"}]`,
		`[{"txid":"` + tTx1 + `","vout":null}]`,
		`[{"txid":"` + strings.ToUpper(tTx1) + `","vout":0}]`,
		`[{"txid":"` + tTx1[:63] + `","vout":0}]`,
		`[{"txid":"` + tTx1 + `a","vout":0}]`,
		`[{"txid":5,"vout":0}]`,
		`[{"txid":null,"vout":0}]`,
		`[{"txid":"` + tTx1 + `","vout":0},{"vout":1}]`,
	}
	for _, b := range bad {
		if _, err := decodeTransaction(json.RawMessage(txWithVin(b))); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("vin %s: err = %v, want ErrBadAnswer", b, err)
		}
	}
	// a missing decoded.vin
	if _, err := decodeTransaction(json.RawMessage(`{"txid":"` + tTx1 + `","confirmations":0,"decoded":{"vout":[]}}`)); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("no vin: %v", err)
	}
	// no mempoolconflicts key (an older Core) is not refused
	if tx, err := decodeTransaction(json.RawMessage(txWithVin(`[]`))); err != nil || tx.MempoolConflicts != nil {
		t.Errorf("no mempoolconflicts: %+v, %v", tx, err)
	}
}

func spendAnswer(entries ...string) string { return `[` + strings.Join(entries, ",") + `]` }

func spendEntry(txid string, vout int, spender string) string {
	if spender == "" {
		return fmt.Sprintf(`{"txid":"%s","vout":%d}`, txid, vout)
	}
	return fmt.Sprintf(`{"txid":"%s","vout":%d,"spendingtxid":"%s"}`, txid, vout, spender)
}

func TestSpentInMempool(t *testing.T) {
	ctx := context.Background()
	outs := []Outpoint{{tTx1, 0}, {tTx2, 3}}
	c, s := serve(t, spendAnswer(spendEntry(tTx1, 0, tTx2), spendEntry(tTx2, 3, "")))
	got, err := c.SpentInMempool(ctx, outs)
	if err != nil || len(got) != 2 || got[0] != tTx2 || got[1] != "" {
		t.Fatalf("SpentInMempool = %q, %v", got, err)
	}
	if s.body["method"] != "gettxspendingprevout" || s.path != "/" {
		t.Errorf("sent %v on %s", s.body, s.path)
	}
	p, _ := s.body["params"].(map[string]any)
	list, _ := p["outputs"].([]any)
	opt, _ := p["options"].(map[string]any)
	if len(list) != 2 || opt["mempool_only"] != true || len(opt) != 1 || len(p) != 2 {
		t.Fatalf("params %v", s.body["params"])
	}
	if o, _ := list[1].(map[string]any); o["txid"] != tTx2 || o["vout"] != float64(3) || len(o) != 2 {
		t.Errorf("second outpoint %v", list[1])
	}
	// extra keys are ignored
	c, _ = serve(t, `[{"txid":"`+tTx1+`","vout":0,"spendingtx":{"x":1},"spendingtxid":"`+tTx2+`","other":true}]`)
	if got, err = c.SpentInMempool(ctx, outs[:1]); err != nil || got[0] != tTx2 {
		t.Errorf("extra keys: %q, %v", got, err)
	}

	// arguments are refused before any request
	c, s = serve(t, `[]`)
	for name, o := range map[string][]Outpoint{
		"nil": nil, "empty": {}, "short txid": {{tTx1[:63], 0}}, "upper": {{strings.ToUpper(tTx1), 0}},
		"negative vout": {{tTx1, -1}}, "one bad of two": {{tTx1, 0}, {"zz", 0}},
	} {
		if _, err := c.SpentInMempool(ctx, o); !errors.Is(err, ErrArgument) {
			t.Errorf("%s: err = %v, want ErrArgument", name, err)
		}
	}
	if s.n.Load() != 0 {
		t.Errorf("%d requests for refused arguments", s.n.Load())
	}

	// a bad answer
	one := outs[:1]
	for _, b := range []string{
		`null`, `{}`, `"x"`, `[]`, `[null]`, `[1]`, `[{}]`,
		spendAnswer(spendEntry(tTx1, 0, ""), spendEntry(tTx1, 0, "")), // too many
		spendAnswer(spendEntry(tTx2, 0, "")),                          // another txid
		spendAnswer(spendEntry(tTx1, 1, "")),                          // another vout
		`[{"vout":0}]`, `[{"txid":"` + tTx1 + `"}]`,
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":"zz"}]`,
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":"` + strings.ToUpper(tTx2) + `"}]`,
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":5}]`,
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":null}]`, // null is not "no spender": that one marks dead
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":["` + tTx2 + `"]}]`,
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":""}]`,
		`[{"txid":"` + tTx1 + `","vout":"0"}]`,
		`[{"txid":"` + tTx1 + `","vout":0.5}]`,
	} {
		c, _ := serve(t, b)
		if _, err := c.SpentInMempool(ctx, one); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("answer %s: err = %v, want ErrBadAnswer", b, err)
		}
	}
	// order: an answer in another order is refused
	c, _ = serve(t, spendAnswer(spendEntry(tTx2, 3, ""), spendEntry(tTx1, 0, "")))
	if _, err := c.SpentInMempool(ctx, outs); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("reordered answer: %v", err)
	}
	// a JSON-RPC error is an *Error, never an answer
	c = rpcErrServer(t, 500, -8, "x")
	var e *Error
	if _, err := c.SpentInMempool(ctx, one); !errors.As(err, &e) || e.Code != -8 || e.Method != "gettxspendingprevout" {
		t.Errorf("error answer: %v", err)
	}
}

// checkSpent runs the answer decoder on raw for the outpoints asked: none may panic, and a success has one result per
// outpoint, each "" or 64 lowercase hex.
func checkSpent(t *testing.T, raw []byte, asked []Outpoint) {
	t.Helper()
	got, err := decodeSpendingPrevout(raw, asked)
	if err != nil {
		if !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("error of unexpected kind: %v", err)
		}
		return
	}
	if len(got) != len(asked) {
		t.Fatalf("%d results for %d outpoints", len(got), len(asked))
	}
	for _, g := range got {
		if g != "" && !isLowerHex(g, 64) {
			t.Fatalf("spender %q", g)
		}
	}
}

func spendSeeds() []string {
	return []string{
		spendAnswer(spendEntry(tTx1, 0, tTx2), spendEntry(tTx2, 3, "")), spendAnswer(spendEntry(tTx1, 0, "")), `[]`, `{}`, `null`, ``,
		spendAnswer(spendEntry(tTx1, 0, "zz")), spendAnswer(spendEntry(tTx2, 0, "")), spendAnswer(spendEntry(tTx1, 1, "")),
		`[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":""}]`, `[{"txid":"` + tTx1 + `","vout":0,"spendingtxid":null}]`, `[null]`, `[{"txid":"` + tTx1 + `","vout":1.5}]`,
	}
}

func FuzzSpendingPrevout(f *testing.F) {
	for _, s := range spendSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkSpent(t, raw, []Outpoint{{tTx1, 0}, {tTx2, 3}})
		checkSpent(t, raw, []Outpoint{{tTx1, 0}})
	})
}

func TestSpendingPrevoutMutationProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(21, 23))
	alphabet := []byte(`{}[]",:0123456789.-eE abcdefABCDEF` + "\x00\xff")
	n := 0
	for _, seed := range spendSeeds() {
		for k := 0; k < 500; k++ {
			b := mutate(r, []byte(seed), alphabet)
			checkSpent(t, b, []Outpoint{{tTx1, 0}, {tTx2, 3}})
			checkSpent(t, b, []Outpoint{{tTx1, 0}})
			n++
		}
	}
	if n < 5000 {
		t.Fatalf("only %d mutations", n)
	}
}

func mutate(r *rand.Rand, b []byte, alphabet []byte) []byte {
	for m := 0; m < 1+r.IntN(4) && len(b) > 0; m++ {
		i := r.IntN(len(b))
		switch r.IntN(3) {
		case 0:
			b[i] = alphabet[r.IntN(len(alphabet))]
		case 1:
			b = append(b[:i], b[i+1:]...)
		default:
			b = append(b[:i], append([]byte{alphabet[r.IntN(len(alphabet))]}, b[i:]...)...)
		}
	}
	return b
}

// checkVin: whatever the vin, a decoded transaction's inputs are 64 lowercase hex with an index in range.
func checkVin(t *testing.T, raw []byte) {
	t.Helper()
	tx, err := decodeTransaction(json.RawMessage(raw))
	if err != nil {
		if !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("error of unexpected kind: %v", err)
		}
		return
	}
	for _, in := range tx.Inputs {
		if !isLowerHex(in.TxID, 64) || in.Vout < 0 || in.Vout > 1<<32-1 {
			t.Fatalf("input %+v", in)
		}
	}
}

func vinSeeds() []string {
	var out []string
	for _, v := range []string{
		`[]`, `[{"coinbase":"03"}]`, `[{"txid":"` + tTx1 + `","vout":0}]`, `[{"txid":"` + tTx1 + `","vout":4294967296}]`,
		`[{"txid":"` + tTx1 + `","vout":-1}]`, `[{"txid":"zz","vout":0}]`, `[{"txid":"` + tTx1 + `","vout":1.5}]`, `[null]`, `{}`, `null`,
		`[{"txid":"` + tTx1 + `"}]`, `[{"vout":0}]`, `[{"coinbase":"03","txid":"` + tTx1 + `","vout":0}]`,
	} {
		out = append(out, txWithVin(v))
	}
	return append(out, `{"txid":"`+tTx1+`","confirmations":0,"decoded":{"vout":[]}}`)
}

func FuzzDecodeVin(f *testing.F) {
	for _, s := range vinSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) { checkVin(t, raw) })
}

func TestDecodeVinMutationProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(31, 37))
	alphabet := []byte(`{}[]",:0123456789.-eE abcdef` + "\x00\xff")
	n := 0
	for _, seed := range vinSeeds() {
		for k := 0; k < 500; k++ {
			checkVin(t, mutate(r, []byte(seed), alphabet))
			n++
		}
	}
	if n < 5000 {
		t.Fatalf("only %d mutations", n)
	}
}
