// Package onchain is SatBooth's on-chain rail: addresses from the merchant's descriptor wallet in bitcoind, the
// startup checks against it, and the watcher that counts payments into the ledger.
package onchain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/satbooth/satbooth/descriptor"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// The errors of the rail. Each is fixed text: none carries bitcoind's text, an address or a key.
var (
	ErrNetwork     = errors.New("onchain: bitcoind is on another network than the configuration")
	ErrWallet      = errors.New("onchain: the wallet must be a descriptor wallet with private keys disabled")
	ErrDescriptor  = errors.New("onchain: the wallet's descriptor is not the configured one")
	ErrIndexBehind = errors.New("onchain: the wallet's next_index is not above the recorded address index; re-import the descriptor with next_index set")
	ErrAddress     = errors.New("onchain: a new address failed its checks")
	ErrUnavailable = errors.New("onchain: bitcoind is unavailable")
	ErrBusy        = errors.New("onchain: an address is being made; try again")
	ErrNoOnchain   = errors.New("onchain: the order has no on-chain amount")
	ErrExpired     = errors.New("onchain: order expired")
	ErrPaid        = errors.New("onchain: order already paid")
)

// PressWait is how long an Address caller waits for the allocation before ErrBusy; it is under service.WriteTimeout.
const PressWait = 10 * time.Second

// Defaults and bounds of Config.
const (
	DefaultGapAlert     = 1000
	MaxGapAlert         = 1_000_000
	DefaultPollInterval = 10 * time.Second
	MinPollInterval     = 2 * time.Second
	MaxPollInterval     = 60 * time.Second
	DefaultPollRate     = 5.0
	MaxPollRate         = 10000.0
	DefaultCallTimeout  = 30 * time.Second
	MinCallTimeout      = time.Second
	MaxCallTimeout      = 10 * time.Minute
	maxConfirmations    = rail.MaxConfirmations
)

// Tier says how many confirmations an order of up to UpTo sat needs (rail.Tier; 0: no upper bound, the last tier
// only). Confirmations is 0 to 100 and never decreases from one tier to the next; 0 pays an order from the mempool.
type Tier = rail.Tier

// DefaultTiers are the confirmation tiers when the configuration has none.
func DefaultTiers() []Tier {
	return []Tier{{UpTo: 1_000_000, Confirmations: 1}, {UpTo: 10_000_000, Confirmations: 3}, {UpTo: 0, Confirmations: 6}}
}

// Config is the on-chain rail's configuration. Network is mainnet, testnet, signet or regtest (testnet4 is not
// supported: its chain name was inferred, not read). Zero fields take the defaults (WithDefaults), except Descriptor
// and Network; a nil Tiers takes DefaultTiers, while a tier's Confirmations of 0 is a real value (0-conf), not a
// default.
type Config struct {
	Descriptor   descriptor.Descriptor
	Network      string
	Tiers        []Tier
	GapAlert     int64
	PollInterval time.Duration
	PollRate     float64
	CallTimeout  time.Duration // the bound on every RPC call, whatever the client's own timeout
}

// WithDefaults fills the zero fields.
func (c Config) WithDefaults() Config {
	if c.Tiers == nil {
		c.Tiers = DefaultTiers()
	}
	if c.GapAlert == 0 {
		c.GapAlert = DefaultGapAlert
	}
	if c.PollInterval == 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.PollRate == 0 {
		c.PollRate = DefaultPollRate
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	return c
}

// Validate applies the defaults and checks every field.
func (c Config) Validate() error {
	c = c.WithDefaults()
	switch c.Network {
	case "mainnet", "testnet", "signet", "regtest":
	default:
		return errors.New("onchain: Network must be mainnet, testnet, signet or regtest")
	}
	if c.Descriptor.Func == "" || c.Descriptor.Canonical == "" {
		return errors.New("onchain: a descriptor is required")
	}
	if c.Descriptor.Testnet != (c.Network != "mainnet") {
		return errors.New("onchain: the descriptor's key does not match the network (xpub for mainnet, tpub otherwise)")
	}
	if err := validTiers(c.Tiers); err != nil {
		return err
	}
	switch {
	case c.GapAlert < 1 || c.GapAlert > MaxGapAlert:
		return errors.New("onchain: GapAlert must be 1 to 1,000,000")
	case c.PollInterval < MinPollInterval || c.PollInterval > MaxPollInterval:
		return errors.New("onchain: PollInterval must be 2 s to 60 s")
	case !(c.PollRate > 0 && c.PollRate <= MaxPollRate) || math.IsNaN(c.PollRate):
		return errors.New("onchain: PollRate must be in (0, 10000]")
	case c.CallTimeout < MinCallTimeout || c.CallTimeout > MaxCallTimeout:
		return errors.New("onchain: CallTimeout must be 1 s to 10 min")
	}
	return nil
}

// validTiers is rail.ValidTiers with the on-chain bound, its texts under the rail's own prefix.
func validTiers(t []Tier) error {
	if err := rail.ValidTiers(t, store.MaxSat); err != nil {
		return errors.New("onchain: " + strings.TrimPrefix(err.Error(), "rail: "))
	}
	return nil
}

// Required is the confirmations an order of amountSat needs (rail.Required).
func Required(tiers []Tier, amountSat int64) int { return rail.Required(tiers, amountSat) }

// SettledDepth is the depth at which an output is final (rail.SettledDepth).
func SettledDepth(tiers []Tier) int { return rail.SettledDepth(tiers) }

// Lightning is the part of lnpay the rail uses (rail.Lightning, one interface for both rails).
type Lightning = rail.Lightning

type flight struct {
	done chan struct{}
	a    store.Address
	err  error
}

// Service is the on-chain rail.
type Service struct {
	cfg Config
	st  *store.Store
	lg  *ledger.Ledger
	rpc RPC
	ln  Lightning
	now func() time.Time
	log *slog.Logger

	callTimeout time.Duration
	pressWait   time.Duration

	mu   sync.Mutex // guards desc, peer, settleMu and flights
	desc string     // the descriptor as bitcoind normalised it, set by Check
	peer rail.Peer  // the Monero rail, or nil
	// settleMu is held for the whole of each settle (ruling 8-A1): the rail's own, or the one it shares with the peer
	// rail (SetPeer), so a peer's paid state cannot change between this settle's read of it and its event writes.
	settleMu *sync.Mutex
	flights  map[string]*flight
	landed   chan struct{} // closed when the last flight in progress ends (WaitFlights)
	alloc    chan struct{} // the service-wide allocation mutex: indexes must rise across all orders
	behind   atomic.Bool

	hook func(point string) error // test hook: a non-nil error aborts the step at that point

	// pastExpiry is st.OnchainOrdersPastExpiry, a seam so a test can fail one store query of the pass (st is a
	// concrete *store.Store).
	pastExpiry func(ctx context.Context, now time.Time) ([]store.Order, error)
	// eventsFor is st.EventsForOrder, the same kind of seam: a test fails the events read of one order.
	eventsFor func(ctx context.Context, orderID string) ([]store.Event, error)

	// The watcher (watch.go). running makes Run single, so settle is never concurrent with itself. lastGoodPass,
	// lastPassDur and staleOrders are read by Rail from other goroutines. gapAlertAt and leftSince belong to the pass.
	running      atomic.Bool
	lastGoodPass atomic.Int64 // s.now() in Unix ns at the end of the last good pass; 0 before one
	lastPassDur  atomic.Int64 // the last good pass's duration on s.now(), in ns
	gapAlertAt   time.Time    // when the pass last logged the gap ALERT; zero while below gap_alert
	leftSince    time.Time    // the pass's now when leftWithoutFinal last ran; zero before it
	// diedInPass holds the outputs that step 3 of the current pass marked dead, after that pass's listsinceblock:
	// a replacement of one (an RBF bump) may not be listed before the next pass, so settle writes no final event for
	// their orders in this pass (diedAfterScan), and on a 0 tier does not reverse such a record when that would turn
	// the order unpaid (diedNow). It belongs to the pass and is lost with the process, which is safe: a
	// restarted pass's listsinceblock comes after every stored mark.
	diedInPass map[store.TXOKey]struct{}
	// heldInPass holds the orders whose final event settle held in the current pass for that reason, and the orders
	// step 3 skipped because the wallet is behind the node's mempool (errBehind: their outputs cannot be judged yet); it
	// is cleared with diedInPass. leftWithoutFinal reports none of them in that pass and carries them in
	// leftHeld to the next pass's check, which reports those still without a final event. Both belong to the pass.
	heldInPass map[string]struct{}
	// behindInPass holds the outputs step 3 left unjudged in the current pass because bitcoind's wallet is behind its
	// node's mempool (errBehind); cleared with diedInPass. The pass's settle counts the unrecorded ones as zero
	// (Service.views). It belongs to the pass: only the pass's goroutine (trackAll, then settle) may read or write it;
	// Rail, View, LightningPaid and the other readers never do (they run on other goroutines).
	behindInPass map[store.TXOKey]struct{}
	// behindSince is when the pass first found each output behind (errBehind would have been returned for it): set
	// once, never reset by a later replacement, a mempool mark or a dead mark, and never removed: it lives as long as
	// the process, so that an output settled as dead and reopened by a confirmation cannot obtain it again. One small
	// entry per output ever found replaced in this process bounds it. After behindGrace the node's word is taken and
	// the output is marked dead (track), whenever it is found behind again: the grace is granted once per output. In
	// memory, lost with the process: each restart grants it once more. The pass's goroutine only.
	behindSince map[store.TXOKey]time.Time
	// heldBefore holds the outputs whose first dead mark has been written in this process, and so has had the one-pass
	// hold of diedInPass (the reversal and the order's final event wait for a replacement the next scan may list). A
	// later dead mark of such an output (it came back into the mempool and left again) is not held: the hold is granted
	// once per output. Never pruned (one small entry per output ever marked dead in this process); lost with the
	// process (each restart grants the hold once more). The pass's goroutine only.
	heldBefore map[store.TXOKey]struct{}
	leftHeld   []store.Order

	// bothAlerted is written by the pass (settle) and by lnpay's hook (LightningPaid) from its own goroutines.
	bothMu      sync.Mutex          // guards bothAlerted
	bothAlerted map[string]struct{} // orders whose both-rails ALERT this process logged (settle.go)

	// seen holds the observations of the last pass whose step 3 completed: for each output refreshed in it, the
	// confirmations of the gettransaction answer that set or confirmed its current mined mark, with that mark's height
	// and block hash. trackAll publishes it whole at its normal return and never writes the map afterwards; orderViews
	// and settled read it through seenNow and apply it with boundOut. seenMu is a leaf lock held only to copy the map
	// pointer (the lock order is the settle mutex, then seenMu).
	seenMu sync.Mutex
	seen   map[store.TXOKey]observed
	// behindSeen is the pass's behind set (behindInPass) as trackAll publishes it with seen, at the same moment: a map
	// nothing writes afterwards, read by the peer's View through behindNow (behindInPass itself stays the pass's).
	behindSeen map[store.TXOKey]struct{}
	// heldSeen is heldInPass as the last good-enough pass left it after its settles (the orders whose final event this
	// rail held), published and read the same way (heldNow). The peer's View reads it, so the other rail carries such an
	// order across its window end instead of reporting it.
	heldSeen map[string]struct{}

	staleMu     sync.Mutex          // guards staleOrders
	staleOrders map[string]struct{} // the orders the last pass that reached its end could not refresh or settle
}

// observed is the confirmations of the gettransaction answer that set or confirmed an output's current mined mark,
// with the height and the block hash of that mark.
type observed struct {
	height        int64
	hash          string
	confirmations int64
}

// New validates cfg, creates the two ledger accounts and starts nothing.
func New(cfg Config, st *store.Store, lg *ledger.Ledger, rpc RPC, ln Lightning, now func() time.Time, log *slog.Logger) (*Service, error) {
	if st == nil || lg == nil || rpc == nil || ln == nil || now == nil {
		return nil, errors.New("onchain: nil dependency")
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
		settleMu: &sync.Mutex{}, bothAlerted: map[string]struct{}{}, pastExpiry: st.OnchainOrdersPastExpiry, eventsFor: st.EventsForOrder,
		diedInPass: map[store.TXOKey]struct{}{}, heldInPass: map[string]struct{}{}, behindSince: map[store.TXOKey]time.Time{},
		heldBefore: map[store.TXOKey]struct{}{}}, nil
}

func (s *Service) crash(point string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(point)
}

// SetPeer sets the Monero rail, which Address asks whether an order is already paid there and settle reads beside
// Lightning (nil when the Monero rail is not configured).
// settle is the mutex both rails hold for the whole of each settle (ruling 8-A1): one shared by the two rails, so a
// settle never reads the peer's paid state while the peer's settle changes it; nil keeps the rail's own. Both are set
// before Run.
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

func (s *Service) descriptor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desc
}

// WaitFlights waits for the Address flights in progress to end (they are detached from their callers, so a shutdown
// that closes the store and the ledger must wait for them first), or for ctx to end: then it returns ctx's error and
// the flights still running are abandoned (each is bounded by 5 x CallTimeout; a burned index is harmless, and the
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

// Fixed log lines. They carry no address, descriptor, key or bitcoind text.
const (
	logIBD        = "ALERT: bitcoind is in initial block download or behind its headers"
	logBehind     = "ALERT: wallet next_index is behind SatBooth's record; addresses are not issued"
	logBadAddress = "ALERT: a new address failed its checks; not shown"
)

var errNotChecked = fmt.Errorf("%w: the startup check has not passed", ErrDescriptor)
