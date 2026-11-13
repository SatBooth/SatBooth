package checkout

import (
	"errors"
	"strings"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// ErrURI is returned by PaymentURI and MoneroURI for an address, amount or invoice they refuse. It carries no part
// of the input.
var ErrURI = errors.New("checkout: invalid payment URI input")

// maxInvoice is the longest invoice PaymentURI accepts, in characters.
const maxInvoice = 4096

// PaymentURI builds the BIP321 URI for the unified QR and the "open in wallet" link:
// bitcoin:ADDRESS?amount=BTC[&lightning=INVOICE]. The address and invoice are uppercase (BIP173 and
// BOLT 11 advise it for QR codes); the keys stay lowercase because BIP21-era parsers, Bitcoin Core's
// GUI among them, match keys case-sensitively. bolt11 may be empty: the URI then has
// no lightning key.
//
// The invoice must be 1 to 4096 ASCII letters and digits starting with "ln" (bech32), the only strings that survive
// uppercasing and sit in a URI without escaping, so the result is safe in an href and a QR payload.
func PaymentURI(address string, amountSat int64, bolt11 string) (string, error) {
	if !store.ValidAddress(address) || amountSat < 1 || amountSat > store.MaxSat {
		return "", ErrURI
	}
	if bolt11 != "" && !validInvoice(bolt11) {
		return "", ErrURI
	}
	var b strings.Builder
	b.WriteString("bitcoin:")
	b.WriteString(strings.ToUpper(address))
	b.WriteString("?amount=")
	b.WriteString(btcrpc.FormatBTC(amountSat))
	if bolt11 != "" {
		b.WriteString("&lightning=")
		b.WriteString(strings.ToUpper(bolt11))
	}
	return b.String(), nil
}

// validInvoice reports whether s is 1 to maxInvoice ASCII letters and digits starting with "ln"
// in either case (every BOLT 11 invoice does).
func validInvoice(s string) bool {
	if len(s) < 1 || len(s) > maxInvoice || !strings.HasPrefix(strings.ToLower(s), "ln") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// MoneroURI builds the Monero URI for the Monero QR and its "open in wallet" link: monero:ADDRESS?tx_amount=XMR, and
// nothing else in the query (decision 8). The address must have the shape of a subaddress of any network
// (xmrrpc.AnySubaddress: 95 base58 characters, so it sits in a URI without escaping and keeps its case, which base58
// needs); the amount is 1 to store.MaxPiconero piconero, written in the exact 12-decimal form of xmrrpc.FormatXMR.
// Monero's own parse_amount (src/cryptonote_basic/cryptonote_format_utils.cpp) reads that decimal exactly, as a string
// of digits. A wallet that parses it as a double and multiplies by 10^12 does not always: sampling shows it recovers
// every amount up to about 4 * 10^15 piconero (about 4,000 XMR) and can miss by one piconero above that, below the
// 2^53 − 1 bound. The result holds only base58 characters, digits and ".:?=" after "monero:", so it is safe in an href
// and a QR payload.
func MoneroURI(address string, piconero int64) (string, error) {
	if !xmrrpc.AnySubaddress(address) || piconero < 1 || piconero > store.MaxPiconero {
		return "", ErrURI
	}
	return "monero:" + address + "?tx_amount=" + xmrrpc.FormatXMR(piconero), nil
}
