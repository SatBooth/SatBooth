// Package monero is SatBooth's Monero rail, view-only: subaddresses from the merchant's view-only wallet in
// monero-wallet-rpc, the startup checks against it, and the ledger ids of its payments. SatBooth holds no spend key
// and wallet-rpc holds none either (Check refuses a wallet that can sign).
package monero

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

// The errors of the rail. Each is fixed text: none carries wallet-rpc's text, an address or a key.
var (
	ErrVersion     = errors.New("monero: wallet-rpc's version is not supported")
	ErrAddress     = errors.New("monero: the wallet's primary address is not the configured one")
	ErrNotViewOnly = errors.New("monero: the wallet is not view-only")
	ErrHeight      = errors.New("monero: the wallet's height is 0")
	ErrSubaddress  = errors.New("monero: a subaddress failed its checks")
	ErrIndexBehind = errors.New("monero: the wallet's subaddresses are behind SatBooth's record and could not be restored")
	ErrNoWallet    = errors.New("monero: wallet-rpc has no wallet open; start it with --wallet-file")
	ErrLogin       = errors.New("monero: wallet-rpc refused SatBooth's login")
	ErrAccount     = errors.New("monero: the wallet has no account account_index")
	ErrRestricted  = errors.New("monero: wallet-rpc runs with --restricted-rpc; SatBooth needs full RPC on the private link")
	ErrUnavailable = errors.New("monero: wallet-rpc is unavailable")
	ErrBusy        = errors.New("monero: an address is being made; try again")
	ErrNoMonero    = errors.New("monero: the order has no Monero amount")
	ErrExpired     = errors.New("monero: order expired")
	ErrPaid        = errors.New("monero: order already paid")
)

// PressWait is how long an Address caller waits for the allocation before ErrBusy; it is under service.WriteTimeout.
const PressWait = 10 * time.Second

// Defaults and bounds of Config.
const (
	DefaultUnusedAlert  = 100
	MaxUnusedAlert      = 10_000
	DefaultPollInterval = 10 * time.Second
	MinPollInterval     = 2 * time.Second
	MaxPollInterval     = 60 * time.Second
	DefaultPollEvery    = 200 * time.Millisecond
	MinPollEvery        = 100 * time.Microsecond
	MaxPollEvery        = time.Second
	DefaultCallTimeout  = 30 * time.Second
	MinCallTimeout      = time.Second
	MaxCallTimeout      = 10 * time.Minute
)

// DefaultTiers are the confirmation tiers when the configuration has none, in piconero: up to 1 XMR 1
// confirmation, up to 10 XMR 3, above 10.
func DefaultTiers() []rail.Tier {
	return []rail.Tier{{UpTo: 1_000_000_000_000, Confirmations: 1}, {UpTo: 10_000_000_000_000, Confirmations: 3}, {UpTo: 0, Confirmations: 10}}
}

// Config is the Monero rail's configuration. Address is the wallet's primary address (account 0's) on Network
// (mainnet, testnet, stagenet or regtest); AccountIndex is the account whose subaddresses are issued. Zero fields take
// the defaults (WithDefaults), except Address, Network and AccountIndex; a nil Tiers takes DefaultTiers. PollEvery
// is the pause between two tracked-payment calls, which service derives from poll_rate as time.Second / rate (no
// float64 here: the package is in the no-float scan).
type Config struct {
	Address      string
	Network      string
	AccountIndex uint32
	Tiers        []rail.Tier
	UnusedAlert  int64
	PollInterval time.Duration
	PollEvery    time.Duration
	CallTimeout  time.Duration // the bound on every RPC call, whatever the client's own timeout
}

// WithDefaults fills the zero fields.
func (c Config) WithDefaults() Config {
	if c.Tiers == nil {
		c.Tiers = DefaultTiers()
	}
	if c.UnusedAlert == 0 {
		c.UnusedAlert = DefaultUnusedAlert
	}
	if c.PollInterval == 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.PollEvery == 0 {
		c.PollEvery = DefaultPollEvery
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	return c
}

// Validate applies the defaults and checks every field. Network is checked before Address, so a bad network is
// named as such rather than as an address that does not fit it.
func (c Config) Validate() error {
	c = c.WithDefaults()
	if !xmrrpc.ValidNetwork(c.Network) {
		return errors.New("monero: Network must be mainnet, testnet, stagenet or regtest")
	}
	if !xmrrpc.ValidAddress(c.Address, c.Network, false) {
		return errors.New("monero: Address must be the wallet's primary address for Network")
	}
	if err := rail.ValidTiers(c.Tiers, store.MaxPiconero); err != nil {
		return errors.New("monero: " + strings.TrimPrefix(err.Error(), "rail: "))
	}
	switch {
	case c.UnusedAlert < 1 || c.UnusedAlert > MaxUnusedAlert:
		return errors.New("monero: UnusedAlert must be 1 to 10000")
	case c.PollInterval < MinPollInterval || c.PollInterval > MaxPollInterval:
		return errors.New("monero: PollInterval must be 2 s to 60 s")
	case c.PollEvery < MinPollEvery || c.PollEvery > MaxPollEvery:
		return errors.New("monero: PollEvery must be 100 µs to 1 s")
	case c.CallTimeout < MinCallTimeout || c.CallTimeout > MaxCallTimeout:
		return errors.New("monero: CallTimeout must be 1 s to 10 min")
	}
	return nil
}

type flight struct {
	done chan struct{}
	a    store.Subaddress
	err  error
}

// Service is the Monero rail.
type Service struct {
	cfg Config
	st  *store.Store
	lg  *ledger.Ledger
	rpc RPC
	ln  rail.Lightning
	now func() time.Time
	log *slog.Logger

	callTimeout time.Duration
	pressWait   time.Duration

	mu   sync.Mutex // guards base, checked, peer, settleMu, flights and landed
	base string     // the account's base address from a passed Check; "" before one, after a failed one or a changed one
	// checked is the base address the last Check found at its step 2 (the wallet's identity), kept even when its step 5
	// fails behind: the pass's end compares the wallet with it (ruling 8-A6).
	checked string
	// gen is the wallet generation: walletChanged adds 1. A pass reads it before its step 0 and commits nothing
	// if it moved (ruling 8-A6).
	gen  atomic.Uint64
	peer rail.Peer // the on-chain rail, or nil
	// settleMu is held for the whole of each settle (ruling 8-A1): the rail's own, or the one it shares with the peer
	// rail (SetPeer), so a peer's paid state cannot change between this settle's read of it and its event writes.
	settleMu *sync.Mutex
	flights  map[string]*flight
	landed   chan struct{} // closed when the last flight in progress ends (WaitFlights)
	alloc    chan struct{} // the service-wide allocation mutex: indexes must rise across all orders
	behind   atomic.Bool
	// watchable is whether the last Check passed or failed with ErrIndexBehind or ErrSubaddress: the watcher discovers,
	// tracks and settles only then (ruling 8-T7c). False before the first Check.
	watchable atomic.Bool
	// heightLogged is set by Check when it logs logHeightBehind; the pass clears it first, so a pass logs the line once.
	heightLogged atomic.Bool

	versionLogged atomic.Bool

	hook func(point string) error // test hook: a non-nil error aborts the step at that point

	// The watcher (watch.go). running makes Run single, so settle is never concurrent with itself. lastGoodPass,
	// lastPassDur and staleOrders are read by Rail from other goroutines; the rest belongs to the pass.
	running      atomic.Bool
	lastGoodPass atomic.Int64  // s.now() in Unix ns at the end of the last good pass; 0 before one
	lastPassDur  atomic.Int64  // the last good pass's duration on s.now(), in ns
	unusedAt     time.Time     // when the pass last logged the unused-run ALERT; zero while below unused_alert
	leftSince    time.Time     // the pass's now when leftWithoutFinal last ran; zero before it
	leftHeld     []store.Order // the orders that left their window while the peer held their final event (carried)
	heightSeen   int64         // the wallet's height at the last pass that read it
	heightSince  time.Time     // when the pass first read heightSeen; zero before a pass
	frozenAt     time.Time     // when the pass last logged the frozen-height ALERT; zero since the height moved
	discards     int           // the passes discarded in a row (ruling 8-A6); 0 after a commit
	// dssAlert is the double-spend ALERT (track), badAlert the bad-transfer ALERT (keep, track), bigAlert the
	// oversized ALERT (keep) and unobsAlert the ALERT of a payment that left its window unobserved (leftWithoutFinal):
	// each once per payment per process and bounded per pass (boundedAlert, ruling R8).
	dssAlert   *boundedAlert
	badAlert   *boundedAlert
	bigAlert   *boundedAlert
	unobsAlert *boundedAlert

	// bothAlerted is written by the pass (settle) and by lnpay's hook (LightningPaid) from its own goroutines.
	bothMu      sync.Mutex          // guards bothAlerted
	bothAlerted map[string]struct{} // orders whose both-rails ALERT this process logged (settle.go)
	staleMu     sync.Mutex          // guards staleOrders
	staleOrders map[string]struct{} // the orders the last pass that reached its end could not refresh or settle
}

// New validates cfg, creates the two ledger accounts and starts nothing.
func New(cfg Config, st *store.Store, lg *ledger.Ledger, rpc RPC, ln rail.Lightning, now func() time.Time, log *slog.Logger) (*Service, error) {
	if st == nil || lg == nil || rpc == nil || ln == nil || now == nil {
		return nil, errors.New("monero: nil dependency")
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := ensureAccounts(context.Background(), lg); err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, st: st, lg: lg, rpc: rpc, ln: ln, now: now, log: log,
		callTimeout: cfg.CallTimeout, pressWait: PressWait, flights: map[string]*flight{}, alloc: make(chan struct{}, 1),
		settleMu: &sync.Mutex{}, dssAlert: newBoundedAlert(logDoubleSpend, logDoubleMore),
		badAlert: newBoundedAlert(logBadTransfer, logBadMore), bigAlert: newBoundedAlert(logOversized, logOversizedMore),
		unobsAlert: newBoundedAlert(logUnobserved, logUnobservedMore), bothAlerted: map[string]struct{}{}}, nil
}

// SetPeer sets the on-chain rail, which Address asks whether an order is already paid there and settle reads beside
// Lightning (nil when the on-chain rail is not configured).
// settle is the mutex the rail holds for the whole of each settle (ruling 8-A1): one shared by the watched rails and
// lnpay, so a settle never reads the peer's paid state, or Lightning's, while it changes. The service passes it
// whenever a watched rail exists, with a nil peer when the other rail is absent; a nil settle keeps the rail's own,
// which only tests use. Both are set before Run.
func (s *Service) SetPeer(p rail.Peer, settle *sync.Mutex) {
	s.mu.Lock()
	s.peer = p
	if settle != nil {
		s.settleMu = settle
	}
	s.mu.Unlock()
}

// settleLock is the mutex settle holds (SetPeer).
func (s *Service) settleLock() *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settleMu
}

func (s *Service) peerRail() rail.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peer
}

func (s *Service) baseAddress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base
}

func (s *Service) checkedBase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checked
}

func (s *Service) setChecked(b string) {
	s.mu.Lock()
	s.checked = b
	s.mu.Unlock()
}

func (s *Service) setBase(b string) {
	s.mu.Lock()
	s.base = b
	s.mu.Unlock()
}

func (s *Service) crash(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

// WaitFlights waits for the Address flights in progress to end (they are detached from their callers, so a shutdown
// that closes the store and the ledger must wait for them first), or for ctx to end: then it returns ctx's error and
// the flights still running are abandoned (each is bounded by 5 x CallTimeout; a skipped index is harmless, and the
// store row is the record). An already-landed flight wins over an ended ctx.
func (s *Service) WaitFlights(ctx context.Context) error {
	s.mu.Lock()
	if len(s.flights) == 0 {
		s.mu.Unlock()
		return nil
	}
	landed := s.landed
	s.mu.Unlock()
	select {
	case <-landed:
		return nil
	case <-ctx.Done():
		select {
		case <-landed:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (s *Service) inFlight(orderID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flights[orderID] != nil
}

// Fixed log lines. They carry no address, key or wallet-rpc text.
const (
	logVersion       = "wallet-rpc version"
	logHeightBehind  = "ALERT: the wallet's height is below SatBooth's cursor; payments are found again as it catches up"
	logRecreated     = "ALERT: wallet-rpc had forgotten issued subaddresses; re-created up to the record"
	logBadSubaddress = "ALERT: a new subaddress failed its checks; not shown"
)

var errNotChecked = fmt.Errorf("%w: the startup check has not passed", ErrUnavailable)
