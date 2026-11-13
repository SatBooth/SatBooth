package setup

import (
	"math"
	"net/url"
	"strings"
	"testing"
)

const testPub = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The expected strings are the golden links of cmd/satbooth-pair's TestGoldenLinks, for the same inputs.
func TestLinksMatchPairTool(t *testing.T) {
	got, err := InvoiceLink("http://127.0.0.1:8080", testPub, "SatBooth invoice")
	want := "http://127.0.0.1:8080/apps/new?name=SatBooth+invoice&notification_types=payment_received" +
		"&pubkey=" + testPub + "&request_methods=make_invoice+lookup_invoice+list_transactions+get_info"
	if err != nil || got != want {
		t.Errorf("invoice link\n got %s (%v)\nwant %s", got, err, want)
	}
	got, err = PayoutLink("http://127.0.0.1:8080", testPub, "SatBooth payout", 100000)
	want = "http://127.0.0.1:8080/apps/new?budget_renewal=never&max_amount=100000000&name=SatBooth+payout" +
		"&pubkey=" + testPub + "&request_methods=pay_invoice+lookup_invoice+get_info"
	if err != nil || got != want {
		t.Errorf("payout link\n got %s (%v)\nwant %s", got, err, want)
	}
	got, err = PayoutLink("https://hub.example/", testPub, "SatBooth payout", 2500)
	want = "https://hub.example/apps/new?budget_renewal=never&max_amount=2500000&name=SatBooth+payout" +
		"&pubkey=" + testPub + "&request_methods=pay_invoice+lookup_invoice+get_info"
	if err != nil || got != want {
		t.Errorf("payout link\n got %s (%v)\nwant %s", got, err, want)
	}
	// The largest budget is accepted, and its msat value is exact.
	got, err = PayoutLink("http://127.0.0.1:8080", testPub, "n", math.MaxInt64/1000)
	if err != nil || !strings.Contains(got, "max_amount=9223372036854775000&") {
		t.Errorf("max budget: %s (%v)", got, err)
	}
	// A name with separators round-trips.
	got, err = InvoiceLink("http://127.0.0.1:1", testPub, "a b&c=d")
	if u, perr := url.Parse(got); err != nil || perr != nil || u.Query().Get("name") != "a b&c=d" {
		t.Errorf("name did not round-trip: %s", got)
	}
}

func TestLinksRefuse(t *testing.T) {
	for _, n := range []uint64{0, math.MaxInt64/1000 + 1, math.MaxUint64} {
		if got, err := PayoutLink("http://127.0.0.1:8080", testPub, "n", n); err == nil || got != "" {
			t.Errorf("maxSat %d accepted: %q", n, got)
		}
	}
	badKeys := []string{"", testPub[:63], testPub + "0", strings.ToUpper(testPub), "g" + testPub[1:], testPub[:63] + " "}
	for _, k := range badKeys {
		if _, err := InvoiceLink("http://127.0.0.1:8080", k, "n"); err == nil {
			t.Errorf("invoice key %q accepted", k)
		}
		if _, err := PayoutLink("http://127.0.0.1:8080", k, "n", 1); err == nil {
			t.Errorf("payout key %q accepted", k)
		}
	}
	for _, base := range []string{"", "ftp://h", "http://", "http://h?x=1", "http://h#f", "http://u@h", "http://h//", "http://h x"} {
		if _, err := InvoiceLink(base, testPub, "n"); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
}
