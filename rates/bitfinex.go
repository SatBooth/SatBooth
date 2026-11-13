package rates

import (
	"bytes"
	"context"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const bitfinexBase = "https://api-pub.bitfinex.com/v2/tickers"

// ProviderBitfinex is Bitfinex's provider name, on its rates and in a quote's sources.
const ProviderBitfinex = "bitfinex"

// bitfinexPairs are Bitfinex's fiat quotes per asset that SatBooth asks for (its trading pairs, observed
// 2026-11-06 by TestLiveRecord: tXMRUSD, tBTCUSD, tBTCEUR and tBTCGBP answered; tBTCJPY was absent from the
// answer). Bitfinex is asked only for these, and Quotes reads the same table.
var bitfinexPairs = map[string][]string{
	AssetXMR: {"USD"},
	AssetBTC: {"USD", "EUR", "GBP"},
}

// BitfinexFiats returns the only currencies Bitfinex is asked for BTC, in the order of its request, as a copy (the
// service's startup check names them when too few providers can quote a currency).
func BitfinexFiats() []string { return append([]string(nil), bitfinexPairs[AssetBTC]...) }

// bitfinexSymbol is Bitfinex's trading symbol of asset in code: t, then the asset, then the code.
func bitfinexSymbol(asset, code string) string { return "t" + asset + code }

// bitfinexTickersURL is the one request for asset in codes.
func bitfinexTickersURL(asset string, codes []string) string {
	syms := make([]string, len(codes))
	for i, c := range codes {
		syms[i] = bitfinexSymbol(asset, c)
	}
	return bitfinexBase + "?symbols=" + strings.Join(syms, ",")
}

type bitfinex struct{ c *http.Client }

// Bitfinex is the Bitfinex exchange's public tickers: the mid of the best bid and ask, for the pairs of
// bitfinexPairs, in one request. The answer has no timestamp, so a rate is stamped with the fetch time.
func Bitfinex(c *http.Client) Provider { return bitfinex{c} }

func (bitfinex) Name() string { return ProviderBitfinex }

func (p bitfinex) Fetch(ctx context.Context, asset string, currencies []string, at time.Time) ([]Rate, error) {
	codes, err := tableCodes(bitfinexPairs, asset, currencies)
	if err != nil || len(codes) == 0 {
		return nil, err
	}
	b, err := get(ctx, p.c, bitfinexTickersURL(asset, codes))
	if err != nil {
		return nil, err
	}
	return parseBitfinex(b, asset, codes, at)
}

// Bitfinex's trading ticker, as documented: [SYMBOL, BID, BID_SIZE, ASK, ASK_SIZE, DAILY_CHANGE,
// DAILY_CHANGE_RELATIVE, LAST_PRICE, VOLUME, HIGH, LOW]. The recorded answer (2026-11-06) has a twelfth element,
// undocumented, which is not read.
const (
	bitfinexBid     = 1
	bitfinexAsk     = 3
	bitfinexTickers = 11
)

// parseBitfinex reads a tickers answer for asset: a JSON array of tickers, each an array. A top-level array whose
// first element is the string "error" is ErrVendor (["error", code, message]); anything other than an array, or an
// element that is not an array, is ErrShape. A ticker whose symbol is bitfinexSymbol(asset, code) for a requested
// code gives that code's rate: BID and ASK are JSON numbers, read from their literal text by ParseDecimal (so an
// exponent, a sign or a zero skips the ticker), and the rate is floor((bid + ask) / 2) in units, kept only when
// 0 < bid ≤ ask. A ticker with fewer than 11 elements, or of a symbol not requested, is skipped (elements after the
// eleventh are not read); a requested symbol twice is ErrShape (the answer could be read two ways).
func parseBitfinex(b []byte, asset string, currencies []string, at time.Time) ([]Rate, error) {
	if !CheckJSON(b, maxDepth) {
		return nil, ErrShape
	}
	top, ok := rawArray(bytes.TrimSpace(b))
	if !ok {
		return nil, ErrShape
	}
	if len(top) > 0 {
		if s, ok := rawString(top[0]); ok && s == "error" {
			return nil, ErrVendor
		}
	}
	want := make(map[string]string, len(currencies))
	for _, c := range wanted(currencies) {
		want[bitfinexSymbol(asset, c)] = c
	}
	seen := make(map[string]bool, len(want))
	var out []Rate
	for _, el := range top {
		t, ok := rawArray(el)
		if !ok {
			return nil, ErrShape
		}
		if len(t) < bitfinexTickers {
			continue
		}
		sym, ok := rawString(t[0])
		if !ok {
			continue
		}
		code, ok := want[sym]
		if !ok {
			continue
		}
		if seen[code] {
			return nil, ErrShape
		}
		seen[code] = true
		bidText, okB := rawNumber(t[bitfinexBid])
		askText, okA := rawNumber(t[bitfinexAsk])
		if !okB || !okA {
			continue
		}
		bid, okB := positive(bidText)
		ask, okA := positive(askText)
		if !okB || !okA || bid.Cmp(ask) > 0 {
			continue
		}
		mid := new(big.Int).Add(bid.units, ask.units)
		mid.Quo(mid, bigTwo)
		out = append(out, Rate{Provider: ProviderBitfinex, Currency: code, Value: Decimal{units: mid}, At: at})
	}
	return out, nil
}
