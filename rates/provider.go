package rates

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

const (
	// AssetBTC and AssetXMR are the assets SatBooth prices: bitcoin (base unit the satoshi) and Monero (the
	// piconero).
	AssetBTC = "BTC"
	AssetXMR = "XMR"
	// UserAgent is the only header that names the client; nothing identifies the deployment.
	UserAgent = "SatBooth"
	// MaxResponse is the most of a body read; one byte more is ErrTooLarge.
	MaxResponse = 256 << 10
	// FetchTimeout bounds one request, from the dial to the last byte of the body.
	FetchTimeout = 10 * time.Second
)

// The error classes of a fetch. Each is returned bare, or as a *StatusError for ErrStatus: never with a
// provider's body, and never with Go's transport error, whose text holds the URL (an operator's custom
// URL may carry a token in its query).
var (
	ErrTransport = errors.New("rates: transport failure")          // dial, TLS, timeout, cancel, a redirect answer
	ErrStatus    = errors.New("rates: HTTP status not 200")        // see StatusError for the code
	ErrTooLarge  = errors.New("rates: response too large")         // more than MaxResponse bytes
	ErrShape     = errors.New("rates: response not as documented") // JSON, UTF-8, duplicates, nesting, types
	ErrVendor    = errors.New("rates: provider signals an error")  // an HTTP 200 whose body is an error
	ErrAsset     = errors.New("rates: asset not quoted")
)

// StatusError is ErrStatus with the HTTP status code.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("rates: HTTP status %d", e.Code) }
func (e *StatusError) Unwrap() error { return ErrStatus }

// Rate is one provider's rate for one currency: Value units of the currency per coin (positive), and
// At the vendor's timestamp when it has one, else the fetch time the caller passed.
type Rate struct {
	Provider string
	Currency string
	Value    Decimal
	At       time.Time
}

// Provider fetches the rates of one asset against a list of currencies. at is the caller's clock at the
// start of the pass; a provider never reads the wall clock. A requested currency the provider does not
// quote is absent from the result, not an error.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, asset string, currencies []string, at time.Time) ([]Rate, error)
}

// NewClient returns the client every provider request goes through. rt nil gives a fresh transport
// that never takes a proxy from the environment and speaks TLS 1.2 or later; a test passes the fake's
// round tripper. Redirects are never followed: the 3xx answer comes back as the response.
func NewClient(rt http.RoundTripper) *http.Client {
	if rt == nil {
		rt = &http.Transport{
			Proxy:                  nil,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:      true,
			MaxResponseHeaderBytes: 8 << 10,
			IdleConnTimeout:        30 * time.Second,
		}
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       FetchTimeout,
	}
}

// get makes one GET and returns the body of a 200, or an error class. No error carries the URL, a
// header or a byte of the body. A URL that is not https is refused before any request.
func get(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil || req.URL.Scheme != "https" {
		return nil, ErrTransport
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, ErrTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused; the bytes are never looked at.
		io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponse))
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, ErrTransport
		}
		return nil, &StatusError{Code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return nil, ErrTransport
	}
	if len(b) > MaxResponse {
		return nil, ErrTooLarge
	}
	return b, nil
}

// Quotes reports whether the provider of that name can ever quote asset in currency: Kraken and Bitfinex by their
// pair tables, Coinbase and Yadio for BTC in any currency, the custom API for every pair (the operator's API is
// taken to answer what it is asked), and no unknown name. It is the one definition of which configured providers
// can reach a pair (Service.Reachable).
func Quotes(provider, asset, currency string) bool {
	switch provider {
	case ProviderKraken:
		return slices.Contains(krakenPairs[asset], currency)
	case ProviderBitfinex:
		return slices.Contains(bitfinexPairs[asset], currency)
	case ProviderCoinbase, ProviderYadio:
		return asset == AssetBTC
	case "custom":
		return true
	}
	return false
}

// tableCodes returns the currencies, without repeats and in their order, that a provider's pair table lists for
// asset; ErrAsset when the table has no such asset. Kraken and Bitfinex ask only for these: one pair they do not
// know would fail the whole request.
func tableCodes(table map[string][]string, asset string, currencies []string) ([]string, error) {
	listed, ok := table[asset]
	if !ok {
		return nil, ErrAsset
	}
	var out []string
	for _, c := range wanted(currencies) {
		if slices.Contains(listed, c) {
			out = append(out, c)
		}
	}
	return out, nil
}
