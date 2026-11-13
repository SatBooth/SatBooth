// Package limits holds the invoice-creation limits of threat model §1: a token bucket, a cap on
// open invoices with a reserved share for the "new" class, a cap on the open invoices of one
// account, a daily count and amount cap, and amount bounds. It is pure: no I/O, no goroutines. The
// caller (the order service) computes the counts from its own records and calls Admit under a
// short mutex of its own, so that the count and the creation are atomic.
//
// Every limit but one is global. None is per IP; the per-account cap (MaxOpenPerAccount) is the one
// per-account limit, keyed by the merchant's account_id. The class share uses the account's class,
// which the caller derives from SatBooth's own payment records.
package limits

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Class is the caller's class. ClassKnown is an account with a past paid order. ClassNew is every other
// account. The zero value and any unrecognised value are ClassNew: the least privileged class.
type Class uint8

const (
	ClassNew   Class = 0
	ClassKnown Class = 1
)

// Refusals returned by Admit.
var (
	ErrRate        = errors.New("limits: creation rate exceeded")
	ErrOpenCap     = errors.New("limits: too many open invoices")
	ErrClassShare  = errors.New("limits: open invoices of the new class are at their share")
	ErrDailyCount  = errors.New("limits: daily invoice count reached")
	ErrDailyAmount = errors.New("limits: daily amount reached")
	ErrAmount      = errors.New("limits: amount out of bounds")
	ErrAccountCap  = errors.New("limits: too many open invoices for the account")
	// ErrBadCount means the caller passed a negative count, or more new-class invoices or more
	// invoices of the account than open ones. It is a caller bug; the request is refused and no
	// token is taken.
	ErrBadCount = errors.New("limits: invalid counts")
	// ErrConfig is wrapped by every error New returns.
	ErrConfig = errors.New("limits: invalid configuration")
)

const (
	maxMsat   = 1 << 53 // Lightning amounts never exceed 2^53 msat (exact in a JSON number)
	maxRate   = 1e6
	maxBurst  = 1 << 20
	maxOpen   = 1 << 30
	tokenUnit = int64(1e9) // credit units per token; one nanosecond at 1 token/s adds 1 unit per rate
	// shareRule is the one statement of the accepted NewShare values, in New's error and in Config's doc.
	shareRule = "NewShare must be 0 (the default, 0.2) or in (0, 1), with floor(NewShare × MaxOpen) from 1 to MaxOpen - 1 so that New has a slot and its cap is below the total; Known requests may use unused New slots, only the total caps them"
)

// DefaultMaxOpenPerAccount is the per-account cap a zero MaxOpenPerAccount takes, or MaxOpen when
// that is smaller.
const DefaultMaxOpenPerAccount = 5

// Config. A zero field takes its default, except MaxMsat and DailyMsat, which the operator must
// set. Negative, NaN or infinite values are refused. Every limit is global except
// MaxOpenPerAccount, the one per-account limit, keyed by the merchant's account_id.
type Config struct {
	RatePerSec float64 // tokens per second; default 1
	Burst      int     // bucket size; default 10
	MaxOpen    int     // open invoices, all classes, including reserved slots; default 1000
	// NewShare is the fraction of MaxOpen that caps the New class (Known may use unused New slots). It must be 0 (the default,
	// 0.2) or in (0, 1), with floor(NewShare × MaxOpen) from 1 to MaxOpen - 1 so that New has a
	// slot and its cap is below the total (shareRule): with no New slot no account could ever become Known, so the
	// service could never bootstrap; with MaxOpen of them the cap on New would be no cap. The floor is
	// computed with a 1e-9 tolerance so that 0.29 × 100 is 29, not 28.
	NewShare   float64
	DailyCount int   // invoice attempts per UTC day; default 5000
	DailyMsat  int64 // required: msat per UTC day
	MinMsat    int64 // default 1000 (1 sat)
	MaxMsat    int64 // required
	// MaxOpenPerAccount is the number of open invoices (and reserved slots) one account may hold, in
	// both classes: default min(DefaultMaxOpenPerAccount, MaxOpen), so a MaxOpen below 5 with no
	// per-account value still starts; otherwise 1 to MaxOpen.
	MaxOpenPerAccount int
}

// Limiter is safe for concurrent use.
type Limiter struct {
	cfg    Config
	newCap int // cap on the New class
	now    func() time.Time

	mu     sync.Mutex
	credit int64 // bucket content in 1e-9 tokens
	last   time.Time
	primed bool
}

// New validates cfg, applies the defaults and returns a Limiter whose bucket starts full.
func New(cfg Config, now func() time.Time) (*Limiter, error) {
	if now == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrConfig)
	}
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 1
	}
	if cfg.Burst == 0 {
		cfg.Burst = 10
	}
	if cfg.MaxOpen == 0 {
		cfg.MaxOpen = 1000
	}
	if cfg.NewShare == 0 {
		cfg.NewShare = 0.2
	}
	if cfg.DailyCount == 0 {
		cfg.DailyCount = 5000
	}
	if cfg.MinMsat == 0 {
		cfg.MinMsat = 1000
	}
	if cfg.MaxOpenPerAccount == 0 { // after MaxOpen's own default
		cfg.MaxOpenPerAccount = min(DefaultMaxOpenPerAccount, cfg.MaxOpen)
	}
	bad := func(what string) (*Limiter, error) { return nil, fmt.Errorf("%w: %s", ErrConfig, what) }
	switch {
	case math.IsNaN(cfg.RatePerSec) || cfg.RatePerSec <= 0 || cfg.RatePerSec > maxRate:
		return bad("RatePerSec must be in (0, 1e6]")
	case cfg.Burst < 1 || cfg.Burst > maxBurst:
		return bad("Burst must be in [1, 2^20]")
	case cfg.MaxOpen < 1 || cfg.MaxOpen > maxOpen:
		return bad("MaxOpen must be in [1, 2^30]")
	case cfg.MaxOpenPerAccount < 1 || cfg.MaxOpenPerAccount > cfg.MaxOpen: // after MaxOpen, which bounds it
		return bad("MaxOpenPerAccount must be in [1, MaxOpen]")
	case math.IsNaN(cfg.NewShare) || cfg.NewShare <= 0 || cfg.NewShare >= 1:
		return bad(shareRule) // 0 was replaced by the default above
	case cfg.DailyCount < 1:
		return bad("DailyCount must be positive")
	case cfg.MaxMsat < 1 || cfg.MaxMsat > maxMsat:
		return bad("MaxMsat is required, in [1, 2^53]")
	case cfg.DailyMsat < 1 || cfg.DailyMsat > maxMsat:
		return bad("DailyMsat is required, in [1, 2^53]")
	case cfg.MinMsat < 1 || cfg.MinMsat > cfg.MaxMsat:
		return bad("MinMsat must be in [1, MaxMsat]")
	}
	// The 1e-9 absorbs the representation error of products such as 0.29 × 100 (about 1e-14 at
	// MaxOpen's realistic range). It is below float64's resolution only for products far above
	// that range (beyond about 1e7), where it has no effect; MaxOpen is at most 2^30.
	nc := int(math.Floor(cfg.NewShare*float64(cfg.MaxOpen) + 1e-9))
	if nc < 1 || nc >= cfg.MaxOpen {
		return bad(shareRule)
	}
	return &Limiter{cfg: cfg, newCap: nc, now: now, credit: int64(cfg.Burst) * tokenUnit}, nil
}

// Config returns the configuration with the defaults filled in.
func (l *Limiter) Config() Config { return l.cfg }

// Admit decides whether one more invoice of amountMsat may be created for a request of the given
// class. openNow is the number of open invoices plus reserved slots of all classes, openNew the
// part of it held by the New class, openAcct the part of it held by the requesting account,
// dayCount the attempts so far in the current UTC day, dayMsat the msat attempted so far that day.
// It takes one token only when it returns nil.
//
// The checks run in this fixed order, and the first failure is returned:
//
//  1. counts valid (ErrBadCount): none negative, openNew and openAcct at most openNow
//  2. amount bounds (ErrAmount)
//  3. daily count (ErrDailyCount)
//  4. daily amount (ErrDailyAmount)
//  5. open cap (ErrOpenCap): the total is below MaxOpen, for every class. A Known request has no
//     further cap: it may use New slots that are unused, and only the total limits it
//  6. class share (ErrClassShare), for a New request only: it fails at openNew >= the New share
//  7. per-account cap (ErrAccountCap), for both classes: it fails at openAcct >= MaxOpenPerAccount
//  8. the rate token (ErrRate), last, so a refused request never burns a token
func (l *Limiter) Admit(class Class, amountMsat int64, openNow, openNew, openAcct, dayCount int, dayMsat int64) error {
	if openNow < 0 || openNew < 0 || openAcct < 0 || dayCount < 0 || dayMsat < 0 || openNew > openNow || openAcct > openNow {
		return ErrBadCount
	}
	if amountMsat < l.cfg.MinMsat || amountMsat > l.cfg.MaxMsat {
		return ErrAmount
	}
	if dayCount >= l.cfg.DailyCount {
		return ErrDailyCount
	}
	// dayMsat + amountMsat > DailyMsat without overflow: amountMsat is within [1, 2^53] here.
	if dayMsat > l.cfg.DailyMsat-amountMsat {
		return ErrDailyAmount
	}
	if openNow >= l.cfg.MaxOpen {
		return ErrOpenCap
	}
	if class != ClassKnown && openNew >= l.newCap {
		return ErrClassShare
	}
	if openAcct >= l.cfg.MaxOpenPerAccount {
		return ErrAccountCap
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refill()
	if l.credit < tokenUnit {
		return ErrRate
	}
	l.credit -= tokenUnit
	return nil
}

// refill adds the credit earned since the last refill. l.mu is held.
func (l *Limiter) refill() {
	now := l.now()
	if !l.primed || now.Before(l.last) { // first call, or the clock went back: no refill
		l.last, l.primed = now, true
		return
	}
	full := int64(l.cfg.Burst) * tokenUnit
	// At rate r tokens/s, one nanosecond earns r credit units.
	add := float64(now.Sub(l.last)) * l.cfg.RatePerSec
	if add >= float64(full-l.credit) {
		l.credit, l.last = full, now
		return
	}
	if n := int64(add); n > 0 { // a sub-unit gain keeps the old stamp, so the fraction accrues
		l.credit += n
		l.last = now
	}
}

// DayStart returns 00:00 UTC of t's UTC day.
func DayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
