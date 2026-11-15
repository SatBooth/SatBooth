//go:build qualify

package qualify

import "testing"

// TestHubAddressLookup: with the hub's Electrum server set to an address nothing listens on (dev/regtest/compose.yml,
// as the hub bundle's default), the hub's deposit-address endpoint still answers. The first call makes an address; the
// next ones look it up for use, the lookup fails, and the hub returns the same address (getAlby/hub v1.24.6
// api/api.go GetUnusedOnchainAddress logs the failure and returns the current address). scripts/qualify.sh then checks
// the hub's log for the local failure, and for no call to any other server.
func TestHubAddressLookup(t *testing.T) {
	w := newWorld(t, false, false)
	h := w.sat
	var first string
	if err := h.do("GET", "/api/wallet/address", nil, &first, false); err != nil || first == "" {
		t.Fatalf("the hub's first deposit address: %v", shownErr(err))
	}
	for i := 0; i < 2; i++ {
		var again string
		if err := h.do("GET", "/api/wallet/address", nil, &again, false); err != nil {
			t.Fatalf("the hub's deposit address, call %d: %v", i+2, shownErr(err))
		}
		if again != first {
			t.Fatalf("call %d returned another address: the lookup was not failed softly", i+2)
		}
	}
}
