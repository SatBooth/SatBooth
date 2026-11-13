package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/satbooth/satbooth/alert"
	"github.com/satbooth/satbooth/api"
	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/checkout"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/limits"
	"github.com/satbooth/satbooth/lninvoice"
	"github.com/satbooth/satbooth/lnpay"
	"github.com/satbooth/satbooth/monero"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/nwc"
	"github.com/satbooth/satbooth/onchain"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/rates"
	"github.com/satbooth/satbooth/refund"
	"github.com/satbooth/satbooth/status"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
	"github.com/satbooth/satbooth/xmrrpc"
)

// Server limits, the same on both listeners.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 10 * time.Second
	WriteTimeout      = 15 * time.Second
	IdleTimeout       = 60 * time.Second
	MaxHeaderBytes    = 8 << 10
	MaxBodyBytes      = 16 << 10
	MaxConcurrent     = 256
	ShutdownGrace     = 10 * time.Second
	dialBound         = 30 * time.Second
	// workerCount is the most workers Run starts: eight fixed (Lightning, two listeners, the on-chain watcher, the Monero
	// watcher, the rate fetcher, the refund service, and with the alerts block the alert deliverer; the alert drain is
	// not a worker: it starts before them, see Run) and one webhook deliverer per endpoint, MaxWebhookEndpoints at most.
	// The fatal channel holds one error from each, so a worker that fails last never blocks the shutdown.
	workerCount = 8 + MaxWebhookEndpoints

	busyBody    = "busy\n"
	headersBody = "request headers too large\n"
)

// Fixed log lines. Logs carry no key, secret, invoice, payment hash or request content.
const (
	logStarted          = "satbooth started"
	logStopping         = "satbooth stopping"
	logStopped          = "satbooth stopped"
	logReloaded         = "api keys reloaded"
	logReloadFailed     = "api key reload failed; the old keys stay in force"
	logServeFailed      = "listener failed"
	logHTTPError        = "http server or client error"
	logStartFailed      = "startup failed"
	logWorkerEnded      = "a background worker ended unexpectedly"
	logForcedClose      = "in-flight requests did not finish within the grace period; connections closed"
	logRailFailed       = "on-chain status could not be read"
	logMoneroRailFailed = "Monero status could not be read"
	logFlightsAbandoned = "invoice requests to the wallet did not finish within the grace period; abandoned"
	logCloseFailed      = "a database did not close cleanly"
)

type options struct {
	now      func() time.Time
	log      *slog.Logger
	wallet   lnpay.Wallet
	payout   refund.Wallet
	rpc      onchain.RPC
	xmr      monero.RPC
	rateRT   http.RoundTripper
	reload   <-chan struct{}
	pub, prv net.Listener
	grace    time.Duration
	// fallback is the deliverers' fallback interval (zero: the production one), see withDeliveryFallback.
	fallback time.Duration
}

// Option configures Run. All of them exist for tests and for the thin wrapper in cmd/satbooth.
type Option func(*options)

// WithClock sets the clock of the stores, the limiter, the services and the deliverer.
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithLogger sets the logger (default: JSON on standard error).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithWallet replaces the NWC connection (and the dial) with w. Tests only.
func WithWallet(w lnpay.Wallet) Option { return func(o *options) { o.wallet = w } }

// WithPayoutWallet replaces the payout NWC connection (and its dial) with w. Tests only. As for WithWallet's wallet, the
// start-up checks need get_info of it (see infoWallet): a w without GetInfo is refused.
func WithPayoutWallet(w refund.Wallet) Option { return func(o *options) { o.payout = w } }

// WithRPC replaces the bitcoind client (and the password file) with r. Tests only.
func WithRPC(r onchain.RPC) Option { return func(o *options) { o.rpc = r } }

// WithWalletRPC replaces the wallet-rpc client (and the password file) with r. Tests only.
func WithWalletRPC(r monero.RPC) Option { return func(o *options) { o.xmr = r } }

// WithRateTransport sends every rate provider request through rt instead of the real transport (the fake rate
// server's re-routing round tripper). Tests only.
func WithRateTransport(rt http.RoundTripper) Option { return func(o *options) { o.rateRT = rt } }

// WithReload makes a receive on ch reload the API keys (the SIGHUP seam).
func WithReload(ch <-chan struct{}) Option { return func(o *options) { o.reload = ch } }

// WithListeners serves on the given listeners instead of binding the configured addresses. Tests only.
func WithListeners(public, private net.Listener) Option {
	return func(o *options) { o.pub, o.prv = public, private }
}

func withGrace(d time.Duration) Option { return func(o *options) { o.grace = d } }

// withDeliveryFallback shortens the longest wait between two passes of every webhook and alert deliverer. A test that
// moves its WithClock clock while the service runs needs it: a deliverer notices a moved clock only at its next timer,
// which is the fallback at most. Production never sets it.
func withDeliveryFallback(d time.Duration) Option { return func(o *options) { o.fallback = d } }

// Supervise turns process signals into the service's controls: SIGHUP sends on reload (dropped if one
// is already pending), the first SIGINT or SIGTERM calls cancel, and the second calls exit(1) at once.
// It returns when sigs is closed.
func Supervise(sigs <-chan os.Signal, cancel context.CancelFunc, reload chan<- struct{}, exit func(int)) {
	stopped := false
	for s := range sigs {
		switch s {
		case syscall.SIGHUP:
			select {
			case reload <- struct{}{}:
			default:
			}
		case syscall.SIGINT, syscall.SIGTERM:
			if stopped {
				exit(1)
				return
			}
			stopped = true
			cancel()
		}
	}
}

// Notify subscribes ch to the signals Supervise handles.
func Notify(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
}

// headerSize is the size of the request line and headers as received (the line, each header as
// "name: value\r\n", and the final blank line), up to the host's own accounting of the Host header.
func headerSize(r *http.Request) int {
	n := len(r.Method) + len(r.RequestURI) + len(r.Proto) + 4 + len("Host: ") + len(r.Host) + 4
	for k, vs := range r.Header {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}

// guard wraps h: at most MaxConcurrent requests at once (the rest get 503 and a fixed body), and no
// request body above MaxBodyBytes.
func guard(h http.Handler) http.Handler {
	sem := make(chan struct{}, MaxConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headerSize(r) > MaxHeaderBytes {
			hd := w.Header()
			hd.Set("Content-Type", "text/plain; charset=utf-8")
			hd.Set("Connection", "close")
			w.WriteHeader(http.StatusRequestHeaderFieldsTooLarge)
			io.WriteString(w, headersBody)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			hd := w.Header()
			hd.Set("Content-Type", "text/plain; charset=utf-8")
			hd.Set("Cache-Control", "no-store")
			hd.Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, busyBody)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		}
		h.ServeHTTP(w, r)
	})
}

// invoiceOfAnotherNetwork reports whether a stored invoice passes the payment path's check (lninvoice.Check) for a
// network other than configured. An invoice that passes for none (one an earlier build stored that the check cannot
// read) does not refuse: it states no network.
func invoiceOfAnotherNetwork(bolt11, configured string) bool {
	for _, name := range networkNames {
		if name == configured {
			continue
		}
		if n, err := approval.ParseNetwork(name); err == nil && lninvoice.Check(bolt11, n) == nil {
			return true
		}
	}
	return false
}

// fixedWriter turns whatever net/http would write to its error log (panic values and stacks, accept
// errors, remote addresses, and what its client writes: the standard logger) into one fixed slog line, true for the
// server and the client alike: nothing from the text reaches the log.
type fixedWriter struct{ log *slog.Logger }

func (w fixedWriter) Write(p []byte) (int, error) {
	w.log.Error(logHTTPError)
	return len(p), nil
}

func newServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           guard(h),
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		// net/http reads headers up to MaxHeaderBytes + 4096 bytes and in 4096-byte chunks, so a field of
		// 8 KiB would let about 12 KiB through. 4 KiB bounds what net/http buffers (nothing above about
		// 12 KiB gets in); guard then refuses anything over MaxHeaderBytes (8 KiB) exactly, with 431.
		MaxHeaderBytes: MaxHeaderBytes - 4096,
		ErrorLog:       errorLog(log),
	}
}

// msgOpenOnchain is Run's refusal without an onchain block while the store holds an on-chain order that is not settled
// yet: nothing would ever settle it.
const msgOpenOnchain = "the store holds on-chain orders that are not settled yet, and the configuration has no onchain block; restore it"

// msgOpenMonero is msgOpenOnchain for the Monero rail: without the monero block nothing settles an order with a Monero
// amount.
const msgOpenMonero = "the store holds Monero orders that are not settled yet, and the configuration has no monero block; restore it"

// moneroCauses are the Monero startup check's sentinels, the most specific first (ErrUnavailable last). Their texts are
// fixed, so the operator is told which one without any of wallet-rpc's answer.
var moneroCauses = []error{monero.ErrNoWallet, monero.ErrLogin, monero.ErrAccount, monero.ErrRestricted, monero.ErrAddress,
	monero.ErrNotViewOnly, monero.ErrVersion, monero.ErrHeight, monero.ErrIndexBehind, monero.ErrSubaddress, monero.ErrUnavailable}

// moneroCause is the fixed text of the sentinel err wraps, never err's own text (ErrUnavailable wraps the failed call's
// error), or a fixed text for any other error. ErrUnavailable from one of Check's steps keeps the step's number
// (monero.CheckStep), which names the call that failed.
func moneroCause(err error) string {
	for _, e := range moneroCauses {
		if errors.Is(err, e) {
			if step, ok := monero.CheckStep(err); ok && e == monero.ErrUnavailable {
				return e.Error() + ": step " + strconv.Itoa(step)
			}
			return e.Error()
		}
	}
	return "monero: the startup check failed for another reason"
}

// msgOpenRefund is Run's refusal without a payout block while the store holds a refund that has no result yet: nothing
// would ever send or resolve it.
const msgOpenRefund = "the store holds refunds that are not resolved yet, and the configuration has no payout block; restore it"

// infoWallet is what the start-up checks ask of a wallet beyond its working interface: get_info. *nwc.Client has it. A
// wallet given with WithWallet or WithPayoutWallet that lacks it is refused, so a test double cannot skip a check.
type infoWallet interface {
	GetInfo(ctx context.Context) (*nwc.Info, error)
}

// payoutMethods are every method the payout connection may hold: pay_invoice and lookup_invoice (both required), and what
// the hub grants with them (qualify/REPORT.md). Anything else is refused.
var payoutMethods = []string{"pay_invoice", "lookup_invoice", "pay_keysend", "multi_pay_invoice", "multi_pay_keysend", "get_info", "get_budget"}

// invoiceMethods are every method the invoice connection may hold: the list the hub reports for SatBooth's own invoice
// pairing (qualify/REPORT.md, "13194 content, invoice connection"), nothing added. make_invoice and lookup_invoice are
// required. Any pay method (pay_invoice, pay_keysend, multi_pay_invoice, multi_pay_keysend) or other method is refused:
// the invoice connection has no budget, so one that can pay is an unbounded loss.
var invoiceMethods = []string{"make_invoice", "make_hold_invoice", "settle_hold_invoice", "cancel_hold_invoice",
	"lookup_invoice", "list_transactions", "get_info", "get_budget", "notifications"}

// knownMethod reports whether m is one of SatBooth's own method names (invoiceMethods or payoutMethods): only those
// are ever named in an error. Any other string the hub sends is never echoed, as it could hold anything.
func knownMethod(m string) bool {
	return slices.Contains(invoiceMethods, m) || slices.Contains(payoutMethods, m)
}

// checkInvoiceInfo refuses an invoice connection whose get_info does not answer, lists no methods, lacks make_invoice
// or lookup_invoice, or lists a method outside invoiceMethods (named in the error only if knownMethod), or does not
// state the network (checkNetwork, last).
func checkInvoiceInfo(ctx context.Context, w infoWallet, network string) error {
	cctx, cancel := context.WithTimeout(ctx, dialBound)
	defer cancel()
	in, err := w.GetInfo(cctx)
	if err != nil || in == nil {
		return errors.New("cannot read get_info of the invoice connection")
	}
	if len(in.Methods) == 0 {
		return errors.New("the invoice connection's get_info lists no methods")
	}
	for _, m := range in.Methods {
		switch {
		case slices.Contains(invoiceMethods, m):
		case knownMethod(m):
			return errors.New("the invoice connection holds the method " + strconv.Quote(m) +
				", which it must not; pair it with satbooth-pair -connection invoice")
		default:
			return errors.New("the invoice connection holds a method SatBooth does not know")
		}
	}
	if !slices.Contains(in.Methods, "make_invoice") || !slices.Contains(in.Methods, "lookup_invoice") {
		return errors.New("the invoice connection must hold make_invoice and lookup_invoice")
	}
	return checkNetwork(in, "invoice", network)
}

// checkNetwork refuses a get_info answer that does not state exactly the network the configuration names (want, one of
// the four words approval.ParseNetwork accepts). The hub gives the network only to a connection that holds the get_info
// scope, so an answer without it is refused too. The hub's own string is never echoed: it is named only if it is one
// of the four words. connection is "invoice" or "payout".
func checkNetwork(in *nwc.Info, connection, want string) error {
	got := in.Network
	switch {
	case got == want:
		return nil
	case got == "":
		return errors.New("the " + connection + " connection's get_info does not state the network; pair it with " +
			"satbooth-pair -connection " + connection)
	case slices.Contains(networkNames, got):
		return errors.New("the " + connection + " connection's hub is on the network " + strconv.Quote(got) +
			", not nwc.network " + strconv.Quote(want))
	default:
		return errors.New("the " + connection + " connection's hub reports a network SatBooth does not know")
	}
}

// networkNames are the four network names of NIP-47's get_info and of nwc.network.
var networkNames = []string{"mainnet", "testnet", "signet", "regtest"}

// checkPayoutInfo refuses a payout connection that lacks pay_invoice or lookup_invoice, or holds a method outside
// payoutMethods (named in the error only if knownMethod), or whose budget is not set or renews, or whose get_info does
// not state the network (checkNetwork, after the methods and before the budget).
func checkPayoutInfo(ctx context.Context, w refund.Wallet, iw infoWallet, network string) error {
	cctx, cancel := context.WithTimeout(ctx, dialBound)
	defer cancel()
	in, err := iw.GetInfo(cctx)
	if err != nil || in == nil {
		return errors.New("cannot read get_info of the payout connection")
	}
	if !slices.Contains(in.Methods, "pay_invoice") || !slices.Contains(in.Methods, "lookup_invoice") {
		return errors.New("the payout connection must hold pay_invoice and lookup_invoice")
	}
	for _, m := range in.Methods {
		switch {
		case slices.Contains(payoutMethods, m):
		case knownMethod(m):
			return errors.New("the payout connection holds the method " + strconv.Quote(m) +
				", which refunds do not allow; pair it with satbooth-pair -connection payout")
		default:
			return errors.New("the payout connection holds a method that refunds do not allow; " +
				"pair it with satbooth-pair -connection payout")
		}
	}
	if err := checkNetwork(in, "payout", network); err != nil {
		return err
	}
	bctx, bcancel := context.WithTimeout(ctx, dialBound)
	defer bcancel()
	b, err := w.GetBudget(bctx)
	if err != nil {
		return errors.New("cannot read get_budget of the payout connection")
	}
	// An absent renewal reads as "" here, and the hub's own "never" as "never": only the latter is accepted.
	if b == nil || !b.Set || b.RenewalPeriod != "never" {
		return errors.New("the payout connection needs a budget that is never renewed")
	}
	return nil
}

// Run starts the service and blocks until ctx ends (it then shuts down and returns nil) or a part of
// it fails (it then shuts down and returns the error). Errors never contain a secret.
func Run(ctx context.Context, cfg *Config, opts ...Option) (err error) {
	o := options{now: time.Now, grace: ShutdownGrace}
	for _, f := range opts {
		f(&o)
	}
	if o.log == nil {
		o.log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	log := o.log
	// The standard logger is the HTTP library's only output besides the servers' ErrorLog (a client logs "Unsolicited
	// response received on idle HTTP channel starting with ..." there, with the bytes a receiver sent, even on a fresh
	// connection). It is routed through the fixed writer so no peer's bytes reach the log. This changes process-wide
	// state, so it is done here, where the service's logger exists, before any client or server of this process runs; it
	// covers every HTTP client (the deliverers, the rate fetcher, the RPC clients). A later Run replaces it.
	stdlog.SetFlags(0)
	stdlog.SetOutput(fixedWriter{log})

	// fail logs one fixed line with a reason class, never a value, and returns the error for the caller.
	fail := func(reason, msg string) error {
		log.Error(logStartFailed, "reason", reason)
		return errors.New("service: " + msg)
	}

	// The network of the hub's invoices, and the one network of every block (before any file is opened).
	lnNet, err := cfg.lightningNetwork()
	if err != nil {
		return fail("nwc", "nwc.network is missing or another block names another network")
	}

	// Secrets, loaded in-process, never logged.
	key, err := nostr.LoadSecretKeyFile(cfg.NWC.ClientKeyFile)
	if err != nil {
		return fail("client_key_file", "cannot load nwc.client_key_file")
	}
	// Deferred first, so it runs last: the key is zeroed after the NWC client, the store and the
	// ledger are closed, and nothing that could still sign with it is left running.
	defer key.Zero()
	// The payout key, loaded the same way and zeroed the same way, only when refunds are configured.
	var payKey *nostr.SecretKey
	var refundCfg refund.Config
	if cfg.Payout != nil {
		if refundCfg, err = cfg.Payout.validate(); err != nil {
			return fail("payout", "invalid payout configuration")
		}
		if payKey, err = nostr.LoadSecretKeyFile(cfg.Payout.ClientKeyFile); err != nil {
			return fail("payout_client_key_file", "cannot load payout.client_key_file")
		}
		defer payKey.Zero()
		// Check 1: the payout connection is not the invoice connection, by wallet key or by client key.
		if strings.EqualFold(cfg.Payout.WalletPubKey, cfg.NWC.WalletPubKey) || payKey.PublicKeyHex() == key.PublicKeyHex() {
			return fail("payout", "the payout connection must differ from the invoice connection")
		}
	}
	keys, err := api.LoadKeys(cfg.APIKeysFile)
	if err != nil {
		return fail("api_keys_file", "cannot load api_keys_file")
	}
	// A Config built by struct literal never went through ParseConfig: the webhooks and alerts rules are applied here,
	// with the same position-naming, value-free texts.
	if err := cfg.validateEndpoints(); err != nil {
		msg := strings.TrimPrefix(err.Error(), "config: ")
		if strings.HasPrefix(msg, "alerts.") {
			return fail("alerts", msg)
		}
		return fail("webhooks", msg)
	}
	endpoints := make([]webhook.Endpoint, len(cfg.Webhooks))
	secrets := make([][]byte, len(cfg.Webhooks))
	defer func() {
		for _, s := range secrets {
			clear(s)
		}
	}()
	for i, w := range cfg.Webhooks {
		s, err := webhook.LoadSecret(w.SecretFile)
		if err != nil {
			return fail("webhook_secret_file", fmt.Sprintf("cannot load webhooks[%d].secret_file", i))
		}
		secrets[i] = s
		endpoints[i] = webhook.Endpoint{Name: w.Name, URL: w.URL, Secret: s}
	}
	var alertSecret []byte
	if cfg.Alerts != nil {
		if alertSecret, err = webhook.LoadSecret(cfg.Alerts.SecretFile); err != nil {
			return fail("alerts_secret_file", "cannot load alerts.secret_file")
		}
		defer clear(alertSecret)
	}
	if reason, msg := distinctSecrets(secrets, alertSecret); msg != "" {
		return fail(reason, msg)
	}

	if err := prepareDataDir(cfg.DataDir); err != nil {
		return fail("data_dir", err.Error())
	}
	// One process per data directory, before anything in it is opened: the admission caps and the transition rules
	// hold only within one process. The lock lasts until Run returns (or the process ends, however it ends).
	lock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return fail("data_dir_lock", err.Error())
	}
	// Deferred first, so it runs last: the lock is released only after both Closes below have returned, and each Close
	// returns only after SQLite has closed its file. So "lock released" means "files closed", unless a Close timed out
	// (a connection still busy after 10 s): that is logged as logCloseFailed, and the lock is released anyway, with
	// SQLite possibly still holding the file until the process exits.
	defer lock.Close()
	if err := checkDBFiles(cfg.DataDir); err != nil {
		return fail("data_dir", err.Error())
	}
	lg, err := ledger.Open(ctx, filepath.Join(cfg.DataDir, "ledger.db"), ledger.WithClock(o.now))
	if err != nil {
		return fail("ledger", "cannot open the ledger")
	}
	defer func() {
		if err := lg.Close(); err != nil {
			log.Error(logCloseFailed, "reason", "ledger")
		}
	}()
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "store.db"), store.WithClock(o.now))
	if err != nil {
		return fail("store", "cannot open the store")
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error(logCloseFailed, "reason", "store")
		}
	}()
	// The Lightning network (plan 13), compared before anything else uses the store: a data directory recorded under
	// another network than nwc.network is refused. A store with no record yet that holds invoices is recorded only
	// under the network of its newest invoice (their text states it, and the payment path's own network check reads
	// it), so one start with a mistaken nwc.network cannot bind it for good. Nothing is recorded yet; bindNetwork does
	// that once every check of this start has passed. A network name is not a secret.
	if have, err := st.Network(ctx); err != nil {
		return fail("store", "cannot read the store")
	} else if have != "" {
		if have != cfg.NWC.Network {
			return fail("network", "this data directory belongs to "+have)
		}
	} else if newest, ok, err := st.NewestInvoice(ctx); err != nil {
		return fail("store", "cannot read the store")
	} else if ok && invoiceOfAnotherNetwork(newest, cfg.NWC.Network) {
		return fail("network", "this data directory holds invoices of another network")
	}
	// bindNetwork records nwc.network (the first start that passed its checks) or refuses the start. BindNetwork is
	// atomic, so a second process that raced to another network gets ErrNetwork here too. A start cancelled meanwhile
	// returns the context's error, as a cancelled start does elsewhere.
	bindNetwork := func() error {
		err := st.BindNetwork(ctx, cfg.NWC.Network)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ne *store.NetworkError
		if errors.As(err, &ne) {
			return fail("network", "this data directory belongs to "+ne.Network)
		}
		return fail("store", "cannot record the store's network")
	}
	// The pair stamp (plan 10), right after both databases open: after the migrations, before anything else but
	// the network comparison reads or writes them. The store and the ledger must be from the same moment. A start
	// refused later (for example because the hub is down) has still advanced the stamp; that is harmless and intended,
	// since both files advanced together.
	if err := checkPair(ctx, st, lg, rand.Reader); err != nil {
		if errors.Is(err, ErrPairOlderStore) || errors.Is(err, ErrPairOlderLedger) || errors.Is(err, ErrPairDifferent) {
			return fail("pair", err.Error())
		}
		// Any other error (a read, a write, the random source) may carry a driver's text, so its message is fixed.
		return fail("pair", "cannot check that the store and the ledger are a pair")
	}
	// Alerts (plan 10), right after the store opens and the pair check passes: from here on, every ALERT: line of
	// Run and of everything it builds is also queued for the operator's webhook. Records logged before this are only
	// logged. The drain starts here, not with the workers, so the ALERTs of a start that is then refused are stored
	// too (a later start delivers them). Its stop is deferred after the store's Close, so it runs first: the drain has
	// written what is queued, and returned, before the store closes.
	var alerts *alert.Handler
	if cfg.Alerts != nil {
		alerts = alert.NewHandler(o.log.Handler(), alert.NewStoreQueue(st), o.now)
		log = slog.New(alerts)
		drainCtx, stopDrain := context.WithCancel(context.Background())
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_ = alerts.Run(drainCtx) // it returns only drainCtx's error
		}()
		defer func() {
			stopDrain()
			<-drained
		}()
	}
	if cfg.Onchain == nil {
		// Without the rail nothing settles an on-chain order (lnpay leaves its final events to onchain), so
		// one still inside its watch window would never get one. Fail closed; the operator restores the block.
		if open, err := st.HasOpenOnchainOrder(ctx, o.now()); err != nil {
			return fail("store", "cannot read the store")
		} else if open {
			return fail("onchain", msgOpenOnchain)
		}
	}
	if cfg.Monero == nil {
		// As for the on-chain rail: nothing else settles an order with a Monero amount.
		if open, err := st.HasOpenMoneroOrder(ctx, o.now()); err != nil {
			return fail("store", "cannot read the store")
		} else if open {
			return fail("monero", msgOpenMonero)
		}
	}

	// Check 5: an open refund needs the payout block to be sent or resolved.
	if cfg.Payout == nil {
		if open, err := st.OpenRefunds(ctx); err != nil {
			return fail("store", "cannot read the store")
		} else if len(open) > 0 {
			return fail("payout", msgOpenRefund)
		}
	}

	wallet := o.wallet
	if wallet == nil {
		dctx, cancel := context.WithTimeout(ctx, dialBound)
		c, derr := nwc.Dial(dctx, nwc.Config{RelayURL: cfg.NWC.RelayURL, WalletPubKey: cfg.NWC.WalletPubKey, Key: key,
			Timeout: time.Duration(cfg.NWC.TimeoutSeconds) * time.Second, Log: log})
		cancel()
		if derr != nil {
			return fail(dialReason(derr), "cannot connect to the wallet relay")
		}
		defer c.Close()
		wallet = c
	}
	// Check 4, whether or not a payout block exists: the invoice connection must not be able to pay.
	iw, ok := wallet.(infoWallet)
	if !ok {
		return fail("invoice_connection", "the invoice wallet cannot be checked")
	}
	if err := checkInvoiceInfo(ctx, iw, cfg.NWC.Network); err != nil {
		return fail("invoice_connection", err.Error())
	}
	var payWallet refund.Wallet
	if cfg.Payout != nil {
		payWallet = o.payout
		if payWallet == nil {
			dctx, cancel := context.WithTimeout(ctx, dialBound)
			c, derr := nwc.Dial(dctx, nwc.Config{RelayURL: cfg.Payout.RelayURL, WalletPubKey: cfg.Payout.WalletPubKey, Key: payKey,
				Timeout: time.Duration(cfg.Payout.TimeoutSeconds) * time.Second, Log: log})
			cancel()
			if derr != nil {
				return fail("payout_"+dialReason(derr), "cannot connect to the payout wallet relay")
			}
			defer c.Close()
			payWallet = c
		}
		// Checks 2 and 3.
		piw, ok := payWallet.(infoWallet)
		if !ok {
			return fail("payout_connection", "the payout wallet cannot be checked")
		}
		if err := checkPayoutInfo(ctx, payWallet, piw, cfg.NWC.Network); err != nil {
			return fail("payout_connection", err.Error())
		}
	}

	lim, err := limits.New(cfg.Limits.limits(), o.now)
	if err != nil {
		return fail("limits", "invalid limits")
	}
	svc, err := lnpay.New(lnpay.Config{InvoiceExpiry: time.Duration(cfg.InvoiceExpiryMinutes) * time.Minute, Network: lnNet}, st, lg, wallet, lim, o.now, log)
	if err != nil {
		return fail("invoice_service", "cannot start the invoice service")
	}
	dels, err := webhook.NewEventDeliverers(endpoints, st, o.now, nil, log, webhook.WithIntervals(o.fallback, 0))
	if err != nil {
		return fail("webhook", "cannot start the webhook deliverer")
	}
	var alertDel *webhook.Deliverer
	if alerts != nil {
		if alertDel, err = webhook.NewAlertDeliverer(cfg.Alerts.URL, alertSecret, st, o.now, nil, log,
			webhook.WithIntervals(o.fallback, 0)); err != nil {
			return fail("alerts", "cannot start the alert deliverer")
		}
	}

	// pr is assigned only when fiat pricing is configured: a nil *rates.Service in the interface would not be == nil,
	// and the API would price against it. Startup does not wait for the first fetch: until it lands, a priced order
	// is 503 rate_unavailable.
	var rs *rates.Service
	var pr api.Pricer
	if cfg.Rates != nil {
		// The block is validated here (ratesConfig does it again): a Config built by struct literal never went through
		// ParseConfig, and no file is opened for a block that is refused.
		if err := cfg.Rates.validate(); err != nil {
			return fail("rates", "cannot start the rate fetcher")
		}
		// After Lock (the caller ran it): the custom URL, which may carry a token, lives in this process only.
		custom, err := cfg.Rates.customURL()
		if err != nil {
			return fail("custom_url_file", "cannot load rates.custom_url_file")
		}
		rc, err := cfg.Rates.ratesConfig(custom, o.rateRT, o.now, log, cfg.Monero != nil)
		if err != nil {
			return fail("rates", "cannot start the rate fetcher")
		}
		if rs, err = rates.New(rc); err != nil {
			return fail("rates", "cannot start the rate fetcher")
		}
		pr = rs
	}

	var oc *onchain.Service
	if cfg.Onchain != nil {
		rc, err := cfg.Onchain.railConfig()
		if err != nil {
			return fail("onchain", "invalid onchain configuration")
		}
		rpc := o.rpc
		if rpc == nil {
			// After Lock (the caller ran it): the password lives in this process only, never in a log or an error.
			pw, err := btcrpc.LoadPassword(cfg.Onchain.RPCPasswordFile)
			if err != nil {
				return fail("rpc_password_file", "cannot load onchain.rpc_password_file")
			}
			client, err := btcrpc.New(btcrpc.Config{URL: cfg.Onchain.RPCURL, User: cfg.Onchain.RPCUser,
				Wallet: cfg.Onchain.Wallet, Timeout: rc.WithDefaults().CallTimeout}, pw)
			clear(pw) // New copied it; the client's copy is cleared by Zero
			if err != nil {
				return fail("onchain", "cannot start the bitcoind client")
			}
			defer client.Zero()
			rpc = client
		}
		if oc, err = onchain.New(rc, st, lg, rpc, svc, o.now, log); err != nil {
			return fail("onchain", "cannot start the on-chain rail")
		}
		// Fail fast, as for the relay (Ruling I): an unreachable or wrongly configured bitcoind is a startup
		// failure and the supervisor retries. The reason is a class, never bitcoind's text.
		cctx, cancel := context.WithTimeout(ctx, dialBound)
		cerr := oc.Check(cctx)
		cancel()
		if cerr != nil {
			return fail("onchain", "the on-chain startup check failed")
		}
	}

	var xm *monero.Service
	if cfg.Monero != nil {
		// Validated here too: a Config built by struct literal never went through ParseConfig, and no file is opened
		// and nothing asked of wallet-rpc for a block that is refused.
		mc, err := cfg.Monero.validate()
		if err != nil {
			return fail("monero", "invalid monero configuration")
		}
		rpc := o.xmr
		if rpc == nil {
			// After Lock (the caller ran it): the password lives in this process only, never in a log or an error.
			pw, err := btcrpc.LoadPassword(cfg.Monero.RPCPasswordFile) // the one password loader
			if err != nil {
				return fail("monero_password_file", "cannot load monero.rpc_password_file")
			}
			client, err := xmrrpc.New(xmrrpc.Config{URL: cfg.Monero.WalletRPCURL, User: cfg.Monero.RPCUser,
				Timeout: mc.WithDefaults().CallTimeout}, pw)
			clear(pw) // New copied it; the client's copy is cleared by Zero
			if err != nil {
				return fail("monero", "cannot start the wallet-rpc client")
			}
			defer client.Zero()
			rpc = client
		}
		if xm, err = monero.New(mc, st, lg, rpc, svc, o.now, log); err != nil {
			return fail("monero", "cannot start the Monero rail")
		}
	}
	// Each rail reads the other beside Lightning, and every watched rail and lnpay share one settle mutex (ruling
	// 8-A1): no rail settles an order while the other's settle, or Lightning's record of a payment, changes its paid
	// state.
	shareSettleLock(svc, oc, xm)
	// Lightning paying second: lnpay's completion tells every watched rail, each of which logs the both-rails ALERT for
	// an order it has a payment of. Every rail is told even when one fails; the first error goes back to lnpay, which
	// completes the payment again (each ALERT is logged once per order per process). Set before svc.Run and the servers
	// start (below), so no Pay or reconciler step can run without it.
	var onPaid []lnpay.OnLightningPaid
	if oc != nil {
		onPaid = append(onPaid, oc.LightningPaid)
	}
	if xm != nil {
		onPaid = append(onPaid, xm.LightningPaid)
	}
	if len(onPaid) > 0 {
		svc.SetOnLightningPaid(fanOut(onPaid))
	}
	if xm != nil {
		// Fail fast, as for bitcoind: a wallet-rpc that is unreachable, or holds a wallet that is not the merchant's
		// view-only one, is a startup failure. The cause is the check's fixed sentinel text, never wallet-rpc's answer.
		cctx, cancel := context.WithTimeout(ctx, dialBound)
		cerr := xm.Check(cctx)
		cancel()
		if cerr != nil {
			cause := moneroCause(cerr)
			log.Error(logStartFailed, "reason", "monero", "cause", cause)
			return errors.New("service: the Monero startup check failed: " + cause)
		}
	}

	// rf is assigned only when refunds are configured: a nil *refund.Service in the interface would not be == nil, and
	// the API would call it instead of answering refunds_disabled.
	var rfs *refund.Service
	var rf api.Refunder
	if cfg.Payout != nil {
		if rfs, err = refund.New(ctx, refundCfg, st, lg, payWallet, svc, o.now, log); err != nil {
			return fail("refund", "cannot start the refund service")
		}
		rf = rfs
	}

	pubLn, prvLn := o.pub, o.prv
	if pubLn == nil || prvLn == nil {
		var lc net.ListenConfig
		if pubLn, err = lc.Listen(ctx, "tcp", cfg.PublicListen); err != nil {
			return fail("public_listen", "cannot listen on public_listen")
		}
		if prvLn, err = lc.Listen(ctx, "tcp", cfg.PrivateListen); err != nil {
			pubLn.Close()
			return fail("private_listen", "cannot listen on private_listen")
		}
	}
	res := newResolver(st, svc, oc, xm, o.now, log)
	pubSrv := newServer(checkout.Handler(st, res, o.now, log), log)
	figures := func(ctx context.Context, orderID string) (paid, refunded, open int64, err error) {
		if paid, err = svc.PaidMsat(ctx, orderID); err != nil {
			return 0, 0, 0, err
		}
		if refunded, open, err = st.RefundFigures(ctx, orderID); err != nil {
			return 0, 0, 0, err
		}
		return paid, refunded, open, nil
	}
	prvSrv := newServer(api.Handler(keys, st, res, cfg.CheckoutBaseURL, pr, rf, figures), log)

	// The last check that can refuse the start has passed: the first start to get here records the network, before
	// the first worker or server runs. A refused start records nothing.
	if err := bindNetwork(); err != nil {
		pubLn.Close()
		prvLn.Close()
		return err
	}
	runCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	workers, worker, fatal := newWorkers(log)
	cancelled := func(e error) bool { return errors.Is(e, context.Canceled) || errors.Is(e, http.ErrServerClosed) }
	worker(logWorkerEnded, func() error { return svc.Run(runCtx) }, cancelled)
	for _, del := range dels {
		worker(logWorkerEnded, func() error { return del.Run(runCtx) }, cancelled)
	}
	if alertDel != nil {
		worker(logWorkerEnded, func() error { return alertDel.Run(runCtx) }, cancelled)
	}
	if rs != nil {
		worker(logWorkerEnded, func() error { return rs.Run(runCtx) }, cancelled)
	}
	worker(logServeFailed, func() error { return pubSrv.Serve(pubLn) }, cancelled)
	worker(logServeFailed, func() error { return prvSrv.Serve(prvLn) }, cancelled)
	if oc != nil {
		worker(logWorkerEnded, func() error { return oc.Run(runCtx) }, cancelled)
	}
	if xm != nil {
		worker(logWorkerEnded, func() error { return xm.Run(runCtx) }, cancelled)
	}
	if rfs != nil {
		worker(logWorkerEnded, func() error { return rfs.Run(runCtx) }, cancelled)
	}

	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-o.reload:
				if e := keys.Reload(); e != nil {
					log.Error(logReloadFailed)
				} else {
					log.Info(logReloaded)
				}
			}
		}
	}()

	log.Info(logStarted)
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-fatal:
	}
	log.Info(logStopping)

	// Stop accepting, give in-flight requests and Pay flights the grace period, then close what is left.
	sctx, cancel := context.WithTimeout(context.Background(), o.grace)
	var sd sync.WaitGroup
	var forced bool
	var fmu sync.Mutex
	for _, s := range []*http.Server{pubSrv, prvSrv} {
		sd.Add(1)
		go func() {
			defer sd.Done()
			if e := s.Shutdown(sctx); e != nil {
				fmu.Lock()
				forced = true
				fmu.Unlock()
				s.Close()
			}
		}()
	}
	sd.Wait()
	// A Pay flight is detached from its presser, so a press that went away leaves it running: wait
	// for it within the same grace before the store and the ledger close (deferred above), then
	// abandon it (it is bounded, and its writes are idempotent and re-derived at the next start).
	abandoned := svc.WaitFlights(sctx) != nil
	if oc != nil && oc.WaitFlights(sctx) != nil {
		abandoned = true
	}
	if xm != nil && xm.WaitFlights(sctx) != nil {
		abandoned = true
	}
	cancel()
	if forced {
		log.Warn(logForcedClose)
	}
	if abandoned {
		log.Warn(logFlightsAbandoned)
	}
	stopWorkers()
	workers.Wait()
	<-reloadDone
	log.Info(logStopped)
	return runErr
}

// fanOut is one lnpay hook that calls every rail's hook in turn, each even when an earlier one failed, and returns the
// first error.
func fanOut(hooks []lnpay.OnLightningPaid) lnpay.OnLightningPaid {
	return func(ctx context.Context, o store.Order) error {
		var first error
		for _, f := range hooks {
			if err := f(ctx, o); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
}

// newWorkers returns a worker group, the function that starts a worker in it, and the channel of fatal errors. A
// worker whose function returns an error that expected does not accept ends the service: it logs name and sends one
// fixed error. The channel holds workerCount errors, so no worker blocks on it, however many fail together and
// whether or not anyone is still reading.
func newWorkers(log *slog.Logger) (*sync.WaitGroup, func(name string, f func() error, expected func(error) bool), chan error) {
	var workers sync.WaitGroup
	fatal := make(chan error, workerCount)
	started := 0
	worker := func(name string, f func() error, expected func(error) bool) {
		if started++; started > workerCount {
			panic("service: more workers than workerCount: the fatal channel could block") // a programming error, caught by any start
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			if e := f(); e != nil && !expected(e) {
				log.Error(name)
				fatal <- errors.New("service: " + name)
			}
		}()
	}
	return &workers, worker, fatal
}

// newResolver builds the resolver. Oc and Xm are assigned only when their rail exists: a nil *onchain.Service or
// *monero.Service in the interface would not be == nil, the resolver would think the rail is configured, and the
// first order with an amount on it would call a method on a nil pointer.
func newResolver(st *store.Store, ln status.Lightning, oc *onchain.Service, xm *monero.Service, now func() time.Time, log *slog.Logger) *status.Resolver {
	res := &status.Resolver{St: st, Ln: ln, Now: now}
	if oc != nil {
		res.Oc = loggedRail{oc, log}
	}
	if xm != nil {
		res.Xm = loggedMoneroRail{xm, log}
	}
	return res
}

// loggedRail logs the error of the on-chain Rail, one fixed line per failed request, where the API and the checkout page
// turn it into a bare 500 or an error page. It is the narrowest place: neither package has a logger, the resolver
// is the one thing both share, and wrapping here keeps the log line out of their code. The line carries no order,
// address or error text.
type loggedRail struct {
	status.Onchain
	log *slog.Logger
}

func (l loggedRail) Rail(ctx context.Context, o store.Order) (onchain.Rail, error) {
	r, err := l.Onchain.Rail(ctx, o)
	if err != nil && !errors.Is(err, context.Canceled) {
		l.log.Error(logRailFailed)
	}
	return r, err
}

// loggedMoneroRail is loggedRail for the Monero rail: one fixed line per failed Rail read, which
// the API and the checkout page turn into a bare 500 or an error page. The line carries no order, address or error text.
type loggedMoneroRail struct {
	status.Monero
	log *slog.Logger
}

func (l loggedMoneroRail) Rail(ctx context.Context, o store.Order) (rail.View, error) {
	v, err := l.Monero.Rail(ctx, o)
	if err != nil && !errors.Is(err, context.Canceled) {
		l.log.Error(logMoneroRailFailed)
	}
	return v, err
}

func errorLog(log *slog.Logger) *stdlog.Logger { return stdlog.New(fixedWriter{log}, "", 0) }

// prepareDataDir creates dir with mode 0700 if it is missing. An existing directory must have no group
// or other permission bits: the ledger and the store hold the money records.
func prepareDataDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return errors.New("data_dir is not a directory")
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return errors.New("data_dir must not be accessible to group or others (mode 0700)")
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if os.MkdirAll(dir, 0o700) != nil {
			return errors.New("cannot create data_dir")
		}
		return nil
	}
	return errors.New("cannot read data_dir")
}

// lockDataDir takes an exclusive, non-blocking flock on satbooth.lock in dir (created 0600) and returns the open
// file that holds it; closing it, or the process ending, releases the lock. The file is opened without following a
// symbolic link, and the opened file must be a regular file of the effective user with no group or other bits, so
// every process locks the same file and no other user can hold it.
func lockDataDir(dir string) (*os.File, error) {
	const shape = "the data directory's lock file must be a regular file of this user, mode 0600"
	f, err := os.OpenFile(filepath.Join(dir, "satbooth.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	switch {
	case errors.Is(err, syscall.ELOOP): // a symbolic link
		return nil, errors.New(shape)
	case err != nil:
		return nil, errors.New("cannot open the data directory's lock file")
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, errors.New("cannot open the data directory's lock file")
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 ||
		sys.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New(shape)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another SatBooth process holds the data directory")
		}
		return nil, errors.New("cannot lock the data directory")
	}
	return f, nil
}

// msgDBFiles refuses a database file that is not a regular file of its own in the data directory. A fixed message: no
// path, no file name.
const msgDBFiles = "store.db and ledger.db, and their -wal and -shm files, must be regular files in the data directory, not links"

// checkDBFiles refuses unless each of store.db and ledger.db, and their -wal and -shm files, is missing or a regular
// file with one link, by Lstat in dir: a symbolic link (SQLite follows it), a hard link or any other kind of file
// would let two data directories, each with a lock of its own, share one database. A missing file is left to the
// open, which creates it (a start) or refuses it (a backup). Both Run and Backup call it after the lock and before
// the first open.
func checkDBFiles(dir string) error {
	for _, db := range []string{"store.db", "ledger.db"} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			fi, err := os.Lstat(filepath.Join(dir, db+suffix))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return errors.New("cannot read data_dir")
			}
			sys, ok := fi.Sys().(*syscall.Stat_t)
			if !ok || !fi.Mode().IsRegular() || sys.Nlink != 1 {
				return errors.New(msgDBFiles)
			}
		}
	}
	return nil
}

// dialReason classifies a failed first connection to the wallet relay, without any of its text.
func dialReason(err error) string {
	var refused *nwc.RelayRefusedError
	switch {
	case errors.As(err, &refused):
		return "wallet_relay_refused"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nwc.ErrNoResponse):
		return "wallet_timeout"
	case errors.Is(err, nwc.ErrClosed), errors.Is(err, context.Canceled):
		return "wallet_closed"
	}
	return "wallet_other"
}

// shareSettleLock gives one settle mutex to every watched rail present and to lnpay, and makes each rail the other's
// peer when both exist. It returns the mutex, or nil, giving nothing to anyone, when there is no watched rail. It runs
// before lnpay's reconciler and the rails' passes start.
func shareSettleLock(ln *lnpay.Service, oc *onchain.Service, xm *monero.Service) *sync.Mutex {
	if oc == nil && xm == nil {
		return nil
	}
	mu := &sync.Mutex{}
	// A nil *Service in the interface would not be == nil, so the peer is passed only when it exists.
	switch {
	case oc != nil && xm != nil:
		xm.SetPeer(oc, mu)
		oc.SetPeer(xm, mu)
	case oc != nil:
		oc.SetPeer(nil, mu)
	default:
		xm.SetPeer(nil, mu)
	}
	if ln != nil {
		ln.SetSettleLock(mu)
	}
	return mu
}
