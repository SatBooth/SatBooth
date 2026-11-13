//go:build qualify

package qualify

import (
	"fmt"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/nwc"
)

// The classifiers print a known value or a length, never the text of an answer, also through a wrapping error.
func TestShownClassifiesAnswers(t *testing.T) {
	const secret = "SECRET-TEXT-FROM-AN-ANSWER"
	if got := shown(secret, "a", "b"); got != "other(26 bytes)" {
		t.Fatalf("%q", got)
	}
	if got := shown("a", "a", "b"); got != "a" {
		t.Fatalf("%q", got)
	}
	for name, err := range map[string]error{
		"wallet":   fmt.Errorf("wrapped: %w", &nwc.WalletError{Code: "INTERNAL", Message: secret}),
		"hub":      fmt.Errorf("wrapped: %w", &apiError{Status: 500, Message: secret}),
		"bitcoind": fmt.Errorf("wrapped: %w", &rpcError{Code: -4, Message: secret}),
		"monero":   fmt.Errorf("wrapped: %w", &xmrError{Method: "m", Code: -1, Message: secret}),
	} {
		if s := shownErr(err); strings.Contains(s, secret) || strings.Contains(s, "wrapped") || s == "" {
			t.Errorf("%s: %q", name, s)
		}
	}
	if s := shownErr(&nwc.WalletError{Code: "weird " + secret, Message: "x"}); strings.Contains(s, secret) {
		t.Errorf("%q", s)
	}
}
