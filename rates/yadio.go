package rates

import (
	"context"
	"net/http"
	"time"
)

const yadioURL = "https://api.yadio.io/exrates/BTC"

// ProviderYadio is Yadio's provider name, on its rates and in a quote's sources (the checkout page credits
// Yadio by it).
const ProviderYadio = "yadio"

type yadio struct{ c *http.Client }

// Yadio is yadio.io's exchange rates for BTC: an aggregate rate per currency (not one venue's trade),
// for many currencies, with the time of its last update.
func Yadio(c *http.Client) Provider { return yadio{c} }

func (yadio) Name() string { return ProviderYadio }

func (p yadio) Fetch(ctx context.Context, asset string, currencies []string, at time.Time) ([]Rate, error) {
	if asset != AssetBTC {
		return nil, ErrAsset
	}
	b, err := get(ctx, p.c, yadioURL)
	if err != nil {
		return nil, err
	}
	return parseYadio(b, currencies, at)
}

// parseYadio reads {"BTC":{"<CODE>":<number>,…},"base":"BTC","timestamp":<ms>}. A top-level error key
// is ErrVendor (an unknown currency answers 200 with {"error":"currency not found"}). base must be BTC;
// the rates are in the object under the base's key (the doc example for another base carries a BTC
// number beside its map, so the name is never fixed); timestamp is a non-negative JSON integer in
// milliseconds. Each rate is a JSON number, read from its literal text: a missing code, a string, an
// exponent, a sign or a zero is skipped. Other top-level keys are ignored. at is unused: Yadio's own
// timestamp stamps every rate.
func parseYadio(b []byte, currencies []string, _ time.Time) ([]Rate, error) {
	top, ok := body(b)
	if !ok {
		return nil, ErrShape
	}
	if _, ok := top["error"]; ok {
		return nil, ErrVendor
	}
	base, ok := rawString(top["base"])
	if !ok || base != AssetBTC {
		return nil, ErrShape
	}
	ms, ok := rawUint(top["timestamp"])
	if !ok {
		return nil, ErrShape
	}
	rates, ok := rawObject(top[base])
	if !ok {
		return nil, ErrShape
	}
	stamp := time.UnixMilli(ms)
	var out []Rate
	for _, code := range wanted(currencies) {
		s, ok := rawNumber(rates[code])
		if !ok {
			continue
		}
		if v, ok := positive(s); ok {
			out = append(out, Rate{Provider: ProviderYadio, Currency: code, Value: v, At: stamp})
		}
	}
	return out, nil
}
