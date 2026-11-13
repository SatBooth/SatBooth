package rates

import (
	"context"
	"net/http"
	"time"
)

const coinbaseURL = "https://api.coinbase.com/v2/exchange-rates?currency=BTC"

// ProviderCoinbase is Coinbase's provider name, on its rates and in a quote's sources.
const ProviderCoinbase = "coinbase"

type coinbase struct{ c *http.Client }

// Coinbase is Coinbase's public exchange-rates endpoint: every currency per one BTC, in one request,
// with no key. What kind of rate it is (last trade, mid or index) is not documented, and the answer is
// cached for up to about 60 s (its cache headers). It has no timestamp, so a rate is stamped with the
// fetch time.
func Coinbase(c *http.Client) Provider { return coinbase{c} }

func (coinbase) Name() string { return ProviderCoinbase }

func (p coinbase) Fetch(ctx context.Context, asset string, currencies []string, at time.Time) ([]Rate, error) {
	if asset != AssetBTC {
		return nil, ErrAsset
	}
	b, err := get(ctx, p.c, coinbaseURL)
	if err != nil {
		return nil, err
	}
	return parseCoinbase(b, currencies, at)
}

// parseCoinbase reads {"data":{"currency":"BTC","rates":{"<CODE>":"<decimal>",…}}}. data.currency
// must be BTC. A code that is missing, not a string, not a decimal ParseDecimal reads, or zero is
// skipped. Errors come as a non-200 (their bodies differ from the documented ones and are never read).
// Coinbase prints at most 18 fraction digits today (395 of its 662 codes had exactly 18 on
// 2026-11-05), the most ParseDecimal accepts: a value with 19 would skip that code, not round it.
func parseCoinbase(b []byte, currencies []string, at time.Time) ([]Rate, error) {
	top, ok := body(b)
	if !ok {
		return nil, ErrShape
	}
	data, ok := rawObject(top["data"])
	if !ok {
		return nil, ErrShape
	}
	if cur, ok := rawString(data["currency"]); !ok || cur != AssetBTC {
		return nil, ErrShape
	}
	rates, ok := rawObject(data["rates"])
	if !ok {
		return nil, ErrShape
	}
	var out []Rate
	for _, code := range wanted(currencies) {
		s, ok := rawString(rates[code])
		if !ok {
			continue
		}
		if v, ok := positive(s); ok {
			out = append(out, Rate{Provider: ProviderCoinbase, Currency: code, Value: v, At: at})
		}
	}
	return out, nil
}
