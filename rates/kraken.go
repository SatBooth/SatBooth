package rates

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const krakenBase = "https://api.kraken.com/0/public/Ticker"

// krakenPairs are Kraken's fiat quotes per asset (its AssetPairs: BTC 2026-11-05; XMR 2026-11-07, whose other
// three pairs are not fiat). One unknown pair in a request makes the whole answer an error with no result, so
// Kraken is asked only for these.
var krakenPairs = map[string][]string{
	AssetBTC: {"AUD", "CAD", "CHF", "EUR", "GBP", "JPY", "USD"},
	AssetXMR: {"EUR", "USD"},
}

// krakenAsset is Kraken's name of each asset in a pair it is asked for.
var krakenAsset = map[string]string{AssetBTC: "XBT", AssetXMR: "XMR"}

// KrakenFiats returns the only currencies Kraken quotes for BTC, in the order of its request, as a copy (the
// service's startup check refuses a currency that too few providers can quote).
func KrakenFiats() []string { return append([]string(nil), krakenPairs[AssetBTC]...) }

// ProviderKraken is Kraken's provider name, on its rates and in a quote's sources.
const ProviderKraken = "kraken"

var bigTwo = big.NewInt(2)

// krakenTickerURL is the one request for asset in codes: assetVersion=1 makes each result key the display
// name krakenKey (without it, the keys are Kraken's internal names such as XXBTZUSD and XBTAUD).
func krakenTickerURL(asset string, codes []string) string {
	pairs := make([]string, len(codes))
	for i, c := range codes {
		pairs[i] = krakenAsset[asset] + c
	}
	return krakenBase + "?assetVersion=1&pair=" + strings.Join(pairs, ",")
}

// krakenKey is the result key of asset in code under assetVersion=1: BTC/<code> or XMR/<code>.
func krakenKey(asset, code string) string { return asset + "/" + code }

type kraken struct{ c *http.Client }

// Kraken is the Kraken exchange's public Ticker: the mid of the best bid and ask, for the pairs of
// krakenPairs (seven for BTC, two for XMR). The answer has no timestamp, so a rate is stamped with the
// fetch time.
func Kraken(c *http.Client) Provider { return kraken{c} }

func (kraken) Name() string { return ProviderKraken }

func (k kraken) Fetch(ctx context.Context, asset string, currencies []string, at time.Time) ([]Rate, error) {
	codes, err := tableCodes(krakenPairs, asset, currencies)
	if err != nil || len(codes) == 0 {
		return nil, err
	}
	b, err := get(ctx, k.c, krakenTickerURL(asset, codes))
	if err != nil {
		return nil, err
	}
	return parseKraken(b, asset, codes, at)
}

// parseKraken reads a Ticker answer for asset. Kraken does not use HTTP status for errors: any element in
// the error array is ErrVendor, a W-prefixed warning beside a result included. For each requested code,
// the key krakenKey(asset, code) holds an object whose a and b are arrays of strings, a[0] the ask and b[0]
// the bid; the rate is floor((ask + bid) / 2) in units, kept only when 0 < bid ≤ ask (as BTCPay Server
// keeps a pair only when bid ≤ ask). A missing key or an unparsable price is skipped.
func parseKraken(b []byte, asset string, currencies []string, at time.Time) ([]Rate, error) {
	top, ok := body(b)
	if !ok {
		return nil, ErrShape
	}
	errs, ok := rawArray(top["error"])
	if !ok {
		return nil, ErrShape
	}
	if len(errs) != 0 {
		return nil, ErrVendor
	}
	result, ok := rawObject(top["result"])
	if !ok {
		return nil, ErrShape
	}
	var out []Rate
	for _, code := range wanted(currencies) {
		raw, ok := result[krakenKey(asset, code)]
		if !ok {
			continue
		}
		pair, ok := rawObject(raw)
		if !ok {
			return nil, ErrShape
		}
		ask, ok := firstString(pair["a"])
		if !ok {
			return nil, ErrShape
		}
		bid, ok := firstString(pair["b"])
		if !ok {
			return nil, ErrShape
		}
		a, okA := positive(ask)
		d, okB := positive(bid)
		if !okA || !okB || d.Cmp(a) > 0 {
			continue
		}
		mid := new(big.Int).Add(a.units, d.units)
		mid.Quo(mid, bigTwo)
		out = append(out, Rate{Provider: ProviderKraken, Currency: code, Value: Decimal{units: mid}, At: at})
	}
	return out, nil
}

// firstString returns the first element of a JSON array of which that element is a string.
func firstString(raw []byte) (string, bool) {
	l, ok := rawArray(raw)
	if !ok || len(l) == 0 {
		return "", false
	}
	return rawString(l[0])
}
