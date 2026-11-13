package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
)

const testAlertSecret = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"

// The alerts block follows the webhook block's rules; its URL must differ from every webhooks[i].url (also when only the case of
// the scheme or host, a default port or an empty path differ); its secret file is loaded with the webhook secret's
// rules, so a file looser than 0600 is refused.
func TestConfigAlerts(t *testing.T) {
	block := func(url, secret string) []byte {
		m := goodConfig()
		m["alerts"] = map[string]any{"url": url, "secret_file": secret}
		return enc(t, m)
	}
	c, err := ParseConfig(block("https://monitor.example.com/satbooth-alert", "/etc/satbooth/alerts.secret"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerts == nil || c.Alerts.URL != "https://monitor.example.com/satbooth-alert" || c.Alerts.SecretFile != "/etc/satbooth/alerts.secret" {
		t.Fatalf("alerts %+v", c.Alerts)
	}
	if c, err := ParseConfig(enc(t, goodConfig())); err != nil || c.Alerts != nil {
		t.Fatalf("no block: %+v %v", c, err)
	}
	for _, url := range []string{
		"https://merchant.example.com/hook", "HTTPS://Merchant.Example.com/hook", "https://merchant.example.com:443/hook",
	} {
		if _, err := ParseConfig(block(url, "/etc/satbooth/alerts.secret")); err == nil || !strings.Contains(err.Error(), "alerts.url must differ from webhooks[0].url") {
			t.Errorf("%s: %v", url, err)
		}
	}
	m := goodConfig()
	m["webhooks"].([]any)[0].(map[string]any)["url"] = "http://192.0.2.7"
	m["alerts"] = map[string]any{"url": "http://192.0.2.7:80/", "secret_file": "/s"}
	if _, err := ParseConfig(enc(t, m)); err == nil || !strings.Contains(err.Error(), "alerts.url must differ") {
		t.Errorf("empty path and default port: %v", err)
	}
	for name, b := range map[string][]byte{
		"ftp":        block("ftp://monitor.example.com/", "/s"),
		"user info":  block("https://u@monitor.example.com/", "/s"),
		"fragment":   block("https://monitor.example.com/#x", "/s"),
		"no secret":  block("https://monitor.example.com/", ""),
		"no url":     block("", "/s"),
		"null field": []byte(strings.Replace(string(block("https://monitor.example.com/", "/s")), `"secret_file":"/s"`, `"secret_file":"/s","extra":1`, 1)),
	} {
		if _, err := ParseConfig(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	sec := filepath.Join(dir, "alerts.secret")
	if err := os.WriteFile(sec, []byte(testAlertSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m = goodConfig()
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"].([]any)[0].(map[string]any)["secret_file"] = whPath
	m["alerts"] = map[string]any{"url": "https://monitor.example.com/a", "secret_file": sec}
	if c, err = ParseConfig(enc(t, m)); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
	// The alerts secret may not be the merchant's: the same bytes in another file are refused, without the value.
	same := filepath.Join(dir, "same.secret")
	if err := os.WriteFile(same, []byte(testWebhook+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m["alerts"] = map[string]any{"url": "https://monitor.example.com/a", "secret_file": same}
	sc, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.CheckFiles(); err == nil || !strings.Contains(err.Error(), "alerts.secret_file must differ from webhooks[0].secret_file") || strings.Contains(err.Error(), testWebhook) {
		t.Fatalf("same secret: %v", err)
	}
	if err, logs := startRefuses(t, sc, "alerts_secret_file", "alerts.secret_file must differ from webhooks[0].secret_file"); strings.Contains(err.Error()+logs, testWebhook) {
		t.Fatal("the secret reached the error or the log")
	}
	// K and K followed by a zero byte are other bytes but one HMAC-SHA256 key (HMAC pads the key with zeros
	// to 64 bytes), so the alert receiver could sign merchant webhooks. Refused the same way.
	padded := filepath.Join(dir, "padded.secret")
	if err := os.WriteFile(padded, []byte(testWebhook+"00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m["alerts"] = map[string]any{"url": "https://monitor.example.com/a", "secret_file": padded}
	pc, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.CheckFiles(); err == nil || !strings.Contains(err.Error(), "alerts.secret_file must differ from webhooks[0].secret_file") {
		t.Fatalf("K||0x00: %v", err)
	}
	startRefuses(t, pc, "alerts_secret_file", "alerts.secret_file must differ from webhooks[0].secret_file")
	if err := os.Chmod(sec, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err == nil || !strings.Contains(err.Error(), "alerts.secret_file") || strings.Contains(err.Error(), testAlertSecret) {
		t.Fatalf("0640 alerts secret: %v", err)
	}
	if err, _ := startRefuses(t, c, "alerts_secret_file", "cannot load alerts.secret_file"); err == nil {
		t.Fatal("Run started with a 0640 alerts secret")
	}
}

// alertsMod adds an alerts block posting to url, with a 0600 secret file in the rig's directory.
func alertsMod(url string) func(dir string, m map[string]any) {
	return func(dir string, m map[string]any) {
		sec := filepath.Join(dir, "alerts.secret")
		if err := os.WriteFile(sec, []byte(testAlertSecret+"\n"), 0o600); err != nil {
			panic(err)
		}
		m["alerts"] = map[string]any{"url": url, "secret_file": sec}
	}
}

// Run wraps the logger once the store is open: the both-rails ALERT (logged by the on-chain rail, a package Run
// builds) is posted to alerts.url, signed with the alerts secret, with the order in its fields; the merchant's
// receiver gets no alert.
func TestRunPostsAlerts(t *testing.T) {
	type got struct {
		header string
		body   []byte
	}
	alerts := make(chan got, 16)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		alerts <- got{req.Header.Get(webhook.HeaderName), b}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(recv.Close)
	node := testNode(t)
	r := startRigWith(t, mods(onchainMod("http://127.0.0.1:18443"), alertsMod(recv.URL)), WithRPC(node))
	body := `{"account_id":"acct1","merchant_ref":"mixed-alert","lightning_msat":21000000,"onchain_sat":50000}`
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var o mixedResp
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	got1 := r.mixed(o.ID)
	if got1.Onchain == nil || got1.Onchain.Address == nil {
		t.Fatalf("no address: %+v", got1)
	}
	node.Pay(*got1.Onchain.Address, 50000)
	node.Mine(1)
	eventually(t, "the order to be paid on-chain", func() bool { return r.mixed(o.ID).Onchain.State == "paid" })
	r.wallet.settleAll()
	var a got
	select {
	case a = <-alerts:
	case <-time.After(20 * time.Second):
		t.Fatalf("no alert posted; logs:\n%s", r.logs)
	}
	var p struct {
		ID      string            `json:"id"`
		Type    string            `json:"type"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
		Dropped int               `json:"dropped"`
		Repeats int               `json:"repeats"`
	}
	if err := json.Unmarshal(a.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "alert" || p.Message != "ALERT: an order was paid on both rails" || len(p.Fields) != 1 || p.Fields["order"] != o.ID || p.Dropped != 0 || p.Repeats < 1 {
		t.Fatalf("alert %s", a.body)
	}
	secret, _ := hex.DecodeString(testAlertSecret)
	if err := webhook.Verify(secret, a.header, a.body, time.Now(), webhook.RecommendedWindow); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for {
		select {
		case h := <-r.hooks:
			if strings.Contains(string(h.body), `"type":"alert"`) {
				t.Fatalf("the merchant got an alert: %s", h.body)
			}
			continue
		default:
		}
		break
	}
}

// An ALERT logged during a start that is then refused is stored: the drain runs from the wrap and stops before the
// store closes. Here the on-chain check finds the wallet's next_index behind SatBooth's record (an address was issued,
// then the wallet forgot it), logs the ALERT and refuses the start; the alert row exists afterwards.
func TestRefusedStartStoresItsAlerts(t *testing.T) {
	node := testNode(t)
	r := startRigWith(t, mods(onchainMod("http://127.0.0.1:18443"), alertsMod("http://127.0.0.1:9/alerts")), WithRPC(node))
	body := `{"account_id":"acct1","merchant_ref":"refused-alert","onchain_sat":50000}`
	code, b := do(t, "POST", r.prvURL+"/v1/orders", testAPIKey, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var o mixedResp
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, "POST", o.CheckoutURL+"/pay", "", nil); code != http.StatusSeeOther {
		t.Fatalf("pay: %d", code)
	}
	if got := r.mixed(o.ID); got.Onchain == nil || got.Onchain.Address == nil {
		t.Fatalf("no address: %+v", got)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	node.Update(func(s *onchaintest.State) { s.NextIndex = 0 })
	_, logs := startRefuses(t, r.cfg, "onchain", "the on-chain startup check failed", WithWallet(newFakeWallet()), WithRPC(node))
	const behind = "ALERT: wallet next_index is behind SatBooth's record; addresses are not issued"
	if !strings.Contains(logs, behind) {
		t.Fatalf("no ALERT before the refusal:\n%s", logs)
	}
	st, err := store.Open(context.Background(), filepath.Join(r.cfg.DataDir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.UndeliveredAlerts(context.Background(), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range p {
		found = found || a.Message == behind
	}
	if !found {
		t.Fatalf("the refused start's ALERT was not stored: %+v", p)
	}
}

// sameHMACKey compares the HMAC-SHA256 key blocks: equal keys, a key and the same key followed by zero bytes, and a
// key over 64 bytes and its SHA-256 are one key, and each pair signs alike; any other byte makes another key.
func TestSameHMACKey(t *testing.T) {
	k := bytes.Repeat([]byte{0x5a}, 32)
	long := bytes.Repeat([]byte{0x6b}, 65)
	sum := sha256.Sum256(long)
	sign := func(key []byte) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte("payload"))
		return h.Sum(nil)
	}
	for _, c := range []struct {
		name string
		a, b []byte
		same bool
	}{
		{"equal", k, slices.Clone(k), true},
		{"K and K||0x00", k, append(slices.Clone(k), 0), true},
		{"K and K padded to 64 bytes", k, append(slices.Clone(k), make([]byte, 32)...), true},
		{"a long key and its hash", long, sum[:], true},
		{"one byte differs", k, append(bytes.Repeat([]byte{0x5a}, 31), 0x5b), false},
		{"K and K||0x01", k, append(slices.Clone(k), 1), false},
		{"64 bytes, last byte differs", append(bytes.Repeat([]byte{1}, 63), 2), bytes.Repeat([]byte{1}, 64), false},
	} {
		if got := sameHMACKey(c.a, c.b); got != c.same {
			t.Errorf("%s: sameHMACKey %v, want %v", c.name, got, c.same)
		}
		if got := sameHMACKey(c.b, c.a); got != c.same {
			t.Errorf("%s, swapped: sameHMACKey %v, want %v", c.name, got, c.same)
		}
		if signs := bytes.Equal(sign(c.a), sign(c.b)); signs != c.same {
			t.Errorf("%s: HMAC signs alike %v, but the case says %v", c.name, signs, c.same)
		}
	}
}
