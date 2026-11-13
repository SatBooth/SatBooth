package rates

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type custom struct {
	c   *http.Client
	url string
}

// Custom is a rate API the operator runs, at the configured URL, in SatBooth's format: a JSON object
// with exactly the keys
//
//	asset      a string, the asset the rates are for (it must be the one asked)
//	rates      an object of currency code to decimal string, the currency per one unit of the asset
//	timestamp  an integer, Unix seconds, the time the rates were computed
//
// for example {"asset":"BTC","rates":{"USD":"76022.31","EUR":"67979.91"},"timestamp":1793902508}. The
// asset is asked for in the query parameter asset (customAssetURL), and the body's asset must be the one asked.
func Custom(c *http.Client, url string) Provider { return custom{c, url} }

func (custom) Name() string { return "custom" }

func (p custom) Fetch(ctx context.Context, asset string, currencies []string, _ time.Time) ([]Rate, error) {
	b, err := get(ctx, p.c, customAssetURL(p.url, asset))
	if err != nil {
		return nil, err
	}
	return parseCustom(b, asset, currencies)
}

// customAssetURL is the configured URL with asset=<asset> appended to its query: after a '?' when it has none,
// else after a '&' (nothing is added between when the URL already ends in '?' or '&'). The rest of the URL is
// kept byte for byte.
func customAssetURL(base, asset string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	if strings.HasSuffix(base, "?") || strings.HasSuffix(base, "&") {
		sep = ""
	}
	return base + sep + "asset=" + url.QueryEscape(asset)
}

// parseCustom reads SatBooth's format strictly: an unknown or missing key, a wrong type or another
// asset is ErrShape. A code not requested is ignored; a value that is not a string, that ParseDecimal
// refuses, or that is zero is skipped. Every rate is stamped with the body's timestamp.
func parseCustom(b []byte, asset string, currencies []string) ([]Rate, error) {
	top, ok := body(b)
	if !ok || len(top) != 3 {
		return nil, ErrShape
	}
	got, ok := rawString(top["asset"])
	if !ok || got != asset {
		return nil, ErrShape
	}
	rates, ok := rawObject(top["rates"])
	if !ok {
		return nil, ErrShape
	}
	secs, ok := rawUint(top["timestamp"])
	if !ok {
		return nil, ErrShape
	}
	stamp := time.Unix(secs, 0)
	var out []Rate
	for _, code := range wanted(currencies) {
		s, ok := rawString(rates[code])
		if !ok {
			continue
		}
		if v, ok := positive(s); ok {
			out = append(out, Rate{Provider: "custom", Currency: code, Value: v, At: stamp})
		}
	}
	return out, nil
}
