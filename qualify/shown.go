//go:build qualify

package qualify

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/satbooth/satbooth/nwc"
)

// The harness's output never carries a free-form string from a hub or wallet answer: it prints a value from a fixed
// list written here, or "other" with the string's length. An assertion keeps comparing the raw string; only what it
// prints is classified.

// shown returns s if it is one of known, and "other(N bytes)" otherwise.
func shown(s string, known ...string) string {
	for _, k := range known {
		if s == k {
			return s
		}
	}
	return fmt.Sprintf("other(%d bytes)", len(s))
}

// shownState is a hub row's or invoice's state as NWC names it.
func shownState(s string) string {
	return shown(s, nwc.StatePending, nwc.StateSettled, nwc.StateFailed, nwc.StateAccepted, nwc.StateExpired)
}

// shownType is a hub row's type as NWC names it.
func shownType(s string) string { return shown(s, nwc.TypeIncoming, nwc.TypeOutgoing) }

// shownCode is the code of an NWC wallet error (NIP-47's list).
func shownCode(s string) string {
	return shown(s, "RATE_LIMITED", "NOT_IMPLEMENTED", "INSUFFICIENT_BALANCE", "QUOTA_EXCEEDED", "RESTRICTED", "UNAUTHORIZED",
		"INTERNAL", "OTHER", "PAYMENT_FAILED", "NOT_FOUND", "UNSUPPORTED_ENCRYPTION")
}

// shownRenewal is a budget's renewal period as the hub names it.
func shownRenewal(s string) string {
	return shown(s, "never", "daily", "weekly", "monthly", "yearly", "")
}

// shownErr is err for a failure message. An error that is, or wraps, an answer's error (an NWC wallet error, a hub's
// HTTP error, a bitcoind or Monero RPC error) is printed as its kind and code only, whatever text its wrapper adds;
// any other error is the harness's own or the standard library's, and is printed as it is.
func shownErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	var we *nwc.WalletError
	var ae *apiError
	var re *rpcError
	var xe *xmrError
	switch {
	case errors.As(err, &we):
		return fmt.Sprintf("wallet error %s (message of %d bytes)", shownCode(we.Code), len(we.Message))
	case errors.As(err, &ae):
		return ae.Error()
	case errors.As(err, &re):
		return re.Error()
	case errors.As(err, &xe):
		return xe.Error()
	}
	return err.Error()
}

// jsonReason is why a JSON answer did not decode, without the text of the answer the decoder quotes.
func jsonReason(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return "syntax error"
	case errors.As(err, &te):
		return "wrong type"
	}
	return "does not decode"
}
