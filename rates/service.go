package rates

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The fixed log lines of the Service. They carry provider names, an error class, an HTTP status, an
// asset and a currency: never a rate, a provider's body or a URL.
const (
	logFetchFailed = "rate fetch failed"
	logUnavailable = "ALERT: fiat pricing is unavailable for a currency"
	logAvailable   = "fiat pricing is available for a currency"
)

// fetchBound bounds one provider call should the provider not honour its client's timeout; a variable
// so a test can shorten it.
var fetchBound = FetchTimeout + time.Second

// ErrCurrency is a currency the Service is not configured for (ErrAsset, likewise, an asset). ErrUnreachable is a
// configured pair that fewer than MinSources of the configured providers can ever quote (Reachable): a lasting
// refusal, unlike ErrTooFew, which an outage causes and its end clears.
var (
	ErrCurrency    = errors.New("rates: currency not configured")
	ErrUnreachable = errors.New("rates: no configured set of providers quotes this pair")
)

// The states of one asset and currency, as the log names them; stateUnknown is before the first pass.
const (
	stateUnknown   = ""
	stateAvailable = "available"
	stateTooFew    = "too_few"
	stateDisagree  = "disagree"
)

// Config is the Service's configuration. The service package validates the operator's configuration
// and names its keys; New's errors are for programmers.
type Config struct {
	Providers  []Provider
	Assets     []string
	Currencies []string
	Interval   time.Duration // between the starts of two passes; at most Policy.MaxAge / 2
	Policy     Policy
	MarginBps  int64
	Now        func() time.Time
	Log        *slog.Logger // nil: slog.Default()
}

// Service fetches the rates in the background, once per provider per asset every Interval, and keeps
// them in a cache. Quote and Price read only the cache: no order ever waits on, or causes, a request.
type Service struct {
	cfg        Config
	log        *slog.Logger
	assets     map[string]bool
	currencies map[string]bool
	reachable  map[string]bool     // asset + " " + currency → Reachable; fixed by New
	asked      map[string][]string // asset → its reachable currencies, in the configured order; fixed by New

	pass    sync.Mutex        // one pass at a time
	states  map[string]string // asset + " " + currency → state; read and written under pass
	failing map[string]bool   // provider name + " " + asset → that fetch failed in the last finished pass; under pass

	mu    sync.Mutex
	cache map[string]map[string][]Rate // asset → provider name → the provider's last good rates
}

// New checks cfg and returns a Service with an empty cache: every quote of a reachable pair is ErrTooFew
// until a pass has fetched enough rates.
func New(cfg Config) (*Service, error) {
	switch {
	case len(cfg.Providers) == 0:
		return nil, errors.New("rates: no providers")
	case len(cfg.Assets) == 0:
		return nil, errors.New("rates: no assets")
	case len(cfg.Currencies) == 0:
		return nil, errors.New("rates: no currencies")
	case cfg.Policy.MinSources < 1:
		return nil, errors.New("rates: MinSources below 1")
	case cfg.Policy.MaxSpreadBps < 1:
		return nil, errors.New("rates: MaxSpreadBps below 1")
	case cfg.Policy.MaxAge < time.Second:
		return nil, errors.New("rates: MaxAge below 1 s")
	case cfg.Interval <= 0 || cfg.Interval > cfg.Policy.MaxAge/2:
		return nil, errors.New("rates: Interval not positive or above MaxAge / 2")
	case cfg.MarginBps < MinMarginBps || cfg.MarginBps > MaxMarginBps:
		return nil, ErrMargin
	case cfg.Now == nil:
		return nil, errors.New("rates: no clock")
	}
	seen := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if seen[p.Name()] {
			return nil, errors.New("rates: two providers with one name")
		}
		seen[p.Name()] = true
	}
	cfg.Providers = append([]Provider(nil), cfg.Providers...)
	cfg.Assets = append([]string(nil), cfg.Assets...)
	cfg.Currencies = append([]string(nil), cfg.Currencies...)
	s := &Service{
		cfg:        cfg,
		log:        cfg.Log,
		assets:     make(map[string]bool, len(cfg.Assets)),
		currencies: make(map[string]bool, len(cfg.Currencies)),
		reachable:  make(map[string]bool, len(cfg.Assets)*len(cfg.Currencies)),
		asked:      make(map[string][]string, len(cfg.Assets)),
		states:     make(map[string]string),
		failing:    make(map[string]bool, len(cfg.Providers)),
		cache:      make(map[string]map[string][]Rate, len(cfg.Assets)),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, a := range cfg.Assets {
		s.assets[a] = true
		s.cache[a] = make(map[string][]Rate, len(cfg.Providers))
	}
	for _, c := range cfg.Currencies {
		s.currencies[c] = true
	}
	for _, a := range cfg.Assets {
		for _, c := range cfg.Currencies {
			n := 0
			for _, p := range cfg.Providers {
				if Quotes(p.Name(), a, c) {
					n++
				}
			}
			s.reachable[a+" "+c] = n >= cfg.Policy.MinSources
			if n >= cfg.Policy.MinSources {
				s.asked[a] = append(s.asked[a], c)
			}
		}
	}
	return s, nil
}

// Reachable reports whether asset and currency are both configured and at least MinSources of the configured
// providers can ever quote that pair (Quotes). An unreachable pair is never logged as unavailable, and its Quote is
// ErrUnreachable: no outage made it so, and none will end it.
func (s *Service) Reachable(asset, currency string) bool { return s.reachable[asset+" "+currency] }

// Currencies returns the configured currencies, in their order, as a copy.
func (s *Service) Currencies() []string { return append([]string(nil), s.cfg.Currencies...) }

// Run fetches at once, then at every tick of Interval until ctx ends, and returns ctx.Err(). Passes never
// overlap: a pass starts at the first tick after the previous one ended, and ticks missed during a pass
// are dropped.
func (s *Service) Run(ctx context.Context) error {
	s.fetch(ctx)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.fetch(ctx)
		select { // drop a tick that came during the pass
		case <-t.C:
		default:
		}
	}
}

type fetched struct {
	asset    string
	provider string
	rates    []Rate
	err      error
}

// fetch is one pass: every provider in parallel, one request per provider per asset that has a reachable
// currency, all stamped with the clock at the start of the pass. An unreachable pair is not fetched: every
// provider is given only the asset's reachable currencies and asks only for those it quotes (Kraken and
// Bitfinex by their tables; Coinbase and Yadio refuse any asset but BTC with no request; the custom API is
// asked for the asset), and an asset with no reachable currency is asked of no provider. A good answer
// replaces the provider's entry for that asset as soon as it arrives (whole, under mu), so a stalled provider
// never holds back the others' rates; a failed one keeps the previous rates, which age out by MaxAge. Then the
// failures are logged (at Warn on the first pass of a run of failures of one provider for one asset, at Debug
// after), each reachable asset and currency is aggregated once, and a change of state is logged; an
// unreachable pair is never logged. A pass cut short by ctx logs no failure and no state.
func (s *Service) fetch(ctx context.Context) {
	s.pass.Lock()
	defer s.pass.Unlock()
	at := s.cfg.Now()
	results := make([][]fetched, len(s.cfg.Providers))
	var wg sync.WaitGroup
	for i, p := range s.cfg.Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, asset := range s.cfg.Assets {
				if len(s.asked[asset]) == 0 {
					continue
				}
				f := s.fetchOne(ctx, p, asset, at)
				if f.err == nil {
					s.mu.Lock()
					s.cache[f.asset][f.provider] = f.rates
					s.mu.Unlock()
				}
				results[i] = append(results[i], f)
			}
		}()
	}
	wg.Wait()

	cancelled := ctx.Err() != nil
	if cancelled {
		return
	}
	// A provider's failures for an asset are logged at Warn on the first pass of a run of failures and at Debug
	// after that, so an outage is one Warn per provider and asset, not one per pass; a pass in which that fetch did
	// not fail ends the run. Each asset has its own run: one asset's lasting failure never quiets another's.
	for _, rs := range results {
		for _, f := range rs {
			key := f.provider + " " + f.asset
			if f.err != nil {
				level := slog.LevelWarn
				if s.failing[key] {
					level = slog.LevelDebug
				}
				class, status := errorClass(f.err)
				s.log.Log(context.Background(), level, logFetchFailed, "provider", f.provider, "asset", f.asset, "class", class,
					"status", status)
			}
			s.failing[key] = f.err != nil
		}
	}
	for _, asset := range s.cfg.Assets {
		for _, currency := range s.cfg.Currencies {
			if s.Reachable(asset, currency) {
				s.logState(asset, currency)
			}
		}
	}
}

// fetchOne makes one provider's request for one asset, for the asset's reachable currencies, bounded by the
// client's own timeout and, should a provider not honour it, by fetchBound (FetchTimeout + 1 s). A good answer
// is kept to the reachable pairs, one positive rate per currency, under the provider's name. ErrAsset (the provider does not quote the
// asset) is an empty answer, not a failure.
func (s *Service) fetchOne(ctx context.Context, p Provider, asset string, at time.Time) fetched {
	ctx, cancel := context.WithTimeout(ctx, fetchBound)
	defer cancel()
	name := p.Name()
	rs, err := p.Fetch(ctx, asset, s.asked[asset], at)
	if errors.Is(err, ErrAsset) {
		return fetched{asset: asset, provider: name}
	}
	if err != nil {
		return fetched{asset: asset, provider: name, err: err}
	}
	seen := make(map[string]bool, len(rs))
	kept := make([]Rate, 0, len(rs))
	for _, r := range rs {
		if !s.Reachable(asset, r.Currency) || seen[r.Currency] || r.Value.IsZero() {
			continue
		}
		seen[r.Currency] = true
		r.Provider = name
		kept = append(kept, r)
	}
	return fetched{asset: asset, provider: name, rates: kept}
}

// errorClass names a fetch error's class and its HTTP status (0 when none). An error outside the
// provider classes, such as a context's, is a transport failure.
func errorClass(err error) (string, int) {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return "status", se.Code
	case errors.Is(err, ErrStatus):
		return "status", 0
	case errors.Is(err, ErrTooLarge):
		return "too_large", 0
	case errors.Is(err, ErrShape):
		return "shape", 0
	case errors.Is(err, ErrVendor):
		return "vendor", 0
	}
	return "transport", 0
}

// rates returns the cached rates of an asset in a currency, one entry per provider name (so a name
// configured twice never counts twice), in no particular order: Aggregate sorts.
func (s *Service) rates(asset, currency string) []Rate {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Rate
	for _, rs := range s.cache[asset] {
		for _, r := range rs {
			if r.Currency == currency {
				out = append(out, r)
			}
		}
	}
	return out
}

// logState aggregates one asset and currency and logs a change of state: the ALERT on entering too_few
// or disagree, or on a change between them (on the first pass too); the recovery on entering available
// from either. Only names are logged, never a rate.
func (s *Service) logState(asset, currency string) {
	_, fresh, err := aggregate(asset, currency, s.rates(asset, currency), s.cfg.Now(), s.cfg.Policy)
	state := stateAvailable
	switch {
	case errors.Is(err, ErrDisagree):
		state = stateDisagree
	case err != nil: // ErrTooFew; ErrDuplicate cannot come from the cache (one entry per provider)
		state = stateTooFew
	}
	key := asset + " " + currency
	prev := s.states[key]
	s.states[key] = state
	if state == prev {
		return
	}
	providers := make([]string, len(fresh))
	for i, r := range fresh {
		providers[i] = r.Provider
	}
	switch state {
	case stateAvailable:
		if prev != stateUnknown {
			s.log.Info(logAvailable, "asset", asset, "currency", currency, "providers", providers)
		}
	case stateTooFew:
		s.log.Error(logUnavailable, "asset", asset, "currency", currency, "reason", state, "providers", providers)
	case stateDisagree:
		low, high := fresh[0], fresh[0]
		for _, r := range fresh[1:] { // fresh is sorted by name: strict comparisons keep the first by name
			if r.Value.Cmp(low.Value) < 0 {
				low = r
			}
			if r.Value.Cmp(high.Value) > 0 {
				high = r
			}
		}
		s.log.Error(logUnavailable, "asset", asset, "currency", currency, "reason", state, "providers", providers,
			"low", low.Provider, "high", high.Provider)
	}
}

// Quote returns the current quote of asset in currency from the cache, at the clock's time: ErrAsset or
// ErrCurrency for one not configured, ErrUnreachable for a pair that is not Reachable, else Aggregate's result.
// It never makes a request.
func (s *Service) Quote(asset, currency string) (Quote, error) {
	if !s.assets[asset] {
		return Quote{}, ErrAsset
	}
	if !s.currencies[currency] {
		return Quote{}, ErrCurrency
	}
	if !s.Reachable(asset, currency) {
		return Quote{}, ErrUnreachable
	}
	return Aggregate(asset, currency, s.rates(asset, currency), s.cfg.Now(), s.cfg.Policy)
}

// Priced is a price converted at a quote with the service's margin. Amount is in Quote.Asset's base unit:
// satoshis for BTC, piconero for XMR.
type Priced struct {
	Amount    int64
	Quote     Quote
	MarginBps int64
}

// Price converts price, in currency, to base units of asset: Quote, then Convert with the service's margin.
// Convert's errors pass through.
func (s *Service) Price(asset string, price Decimal, currency string) (Priced, error) {
	q, err := s.Quote(asset, currency)
	if err != nil {
		return Priced{}, err
	}
	n, err := Convert(asset, price, q.Rate, s.cfg.MarginBps)
	if err != nil {
		return Priced{}, err
	}
	return Priced{Amount: n, Quote: q, MarginBps: s.cfg.MarginBps}, nil
}
