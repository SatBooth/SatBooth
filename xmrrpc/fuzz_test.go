package xmrrpc

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

func FuzzParseChallenge(f *testing.F) {
	for _, l := range recordedChallenges(f) {
		f.Add(l)
	}
	f.Add(`Digest qop="auth",realm="a\"b",nonce="n",opaque="o",stale=TRUE,algorithm=md5`)
	f.Add("Digest qop=\"auth\",realm=\"caf\xc3\xa9\",nonce=\"\x00\"")
	f.Add(`Basic realm="x"`)
	r := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		f.Add(genChallenge(r))
	}
	f.Fuzz(func(t *testing.T, h string) {
		got, ok := parseChallenge(h)
		want, wantOK := refParse(h)
		if ok != wantOK || got != want {
			t.Fatalf("%q: parse %+v %v, reference %+v %v", h, got, ok, want, wantOK)
		}
		if !ok {
			return
		}
		if back, ok := parseChallenge(format(got)); !ok || back != got {
			t.Fatalf("%q: round trip %+v %v", h, back, ok)
		}
		// whatever the server sent, the Authorization value is one header line epee can read
		a := authorize(got, tUser, []byte(tPass), "POST", "/json_rpc", 1, "0123456789abcdef")
		if strings.ContainsAny(a, "\x00\r\n") || strings.Contains(a, tPass) {
			t.Fatalf("authorization %q", a)
		}
	})
}

// checkTransfer asserts the row rules on a kept entry.
func checkTransfer(t *testing.T, tr Transfer, list string) {
	t.Helper()
	if !isTxid(tr.TxID) || tr.Amount < 1 || tr.Height < 0 || tr.Height > maxHeight || tr.UnlockTime < 0 ||
		tr.Confirmations < 0 || tr.Confirmations > maxHeight {
		t.Fatalf("kept %+v", tr)
	}
	if (list != "" && tr.Type != list) || (tr.Type != "in" && tr.Type != "pool") {
		t.Fatalf("kept type %q in %q", tr.Type, list)
	}
}

func FuzzTransfers(f *testing.F) {
	f.Add([]byte(`{"in":[` + entry(nil) + `],"pool":[` + entry(map[string]string{"type": `"pool"`, "height": "0"}) + `]}`))
	f.Add([]byte(`{"in":[` + entry(map[string]string{"amount": "9223372036854775808"}) + `,1,null,{}]}`))
	f.Add([]byte(`{"transfers":[` + entry(nil) + `]}`))
	f.Add([]byte(`{"in":{}}`))
	f.Add([]byte(`{"in":[` + entry(map[string]string{"type": `"block"`}) + `],"pool":[` + entry(map[string]string{"type": `"block"`}) + `]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := decodeTransfers(data)
		if err != nil {
			if !errors.Is(err, ErrBadAnswer) {
				t.Fatalf("err %v", err)
			}
		} else {
			for _, tr := range got.In {
				checkTransfer(t, tr, "in")
			}
			for _, tr := range got.Pool {
				checkTransfer(t, tr, "pool")
			}
			if got.Dropped < 0 {
				t.Fatal("negative Dropped")
			}
		}
		list, err := decodeTransferList(data)
		if err != nil && !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("err %v", err)
		}
		for _, tr := range list {
			checkTransfer(t, tr, "")
		}
	})
}

func FuzzValidAddress(f *testing.F) {
	f.Add(addr('4', 95), Mainnet, false)
	f.Add(addr('A', 95), Testnet, false)
	f.Add(addr('7', 95), Stagenet, true)
	f.Add(addr('8', 94)+"\x00", Regtest, true)
	f.Add("\xc3\xa9"+addr('8', 93), Mainnet, true)
	f.Fuzz(func(t *testing.T, s, network string, sub bool) {
		if got := ValidAddress(s, network, sub); got != refValid(s, network, sub) {
			t.Fatalf("ValidAddress(%q, %q, %v) = %v", s, network, sub, got)
		}
		anySub := refValid(s, Mainnet, true) || refValid(s, Testnet, true) || refValid(s, Stagenet, true)
		if AnySubaddress(s) != anySub {
			t.Fatalf("AnySubaddress(%q)", s)
		}
	})
}

// FuzzEnvelope: decodeEnvelope never panics, and what it accepts is a JSON-RPC 2.0 object with id "0".
func FuzzEnvelope(f *testing.F) {
	f.Add([]byte(`{"id":"0","jsonrpc":"2.0","result":{}}`))
	f.Add([]byte(`{"error":{"code":-8,"message":"x"},"id":"0","jsonrpc":"2.0"}`))
	f.Add([]byte(`{"id":"0","jsonrpc":"2.0","result":null}`))
	f.Add([]byte(`{"id":"0","jsonrpc":"2.0","result":1} {}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		raw, err := decodeEnvelope(data, "get_height")
		var e *Error
		switch {
		case err == nil:
			var env struct {
				ID      string          `json:"id"`
				JSONRPC string          `json:"jsonrpc"`
				Result  json.RawMessage `json:"result"`
			}
			if json.Unmarshal(data, &env) != nil || env.ID != "0" || env.JSONRPC != "2.0" || len(raw) == 0 || string(raw) == "null" {
				t.Fatalf("accepted %q", data)
			}
		case errors.As(err, &e):
			if e.Method != "get_height" {
				t.Fatalf("method %q", e.Method)
			}
		case !errors.Is(err, ErrBadAnswer):
			t.Fatalf("err %v", err)
		}
	})
}
