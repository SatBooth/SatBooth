// Package refundtest is a scripted fake payout wallet for the refund package's tests. It has the three methods of
// refund.Wallet (the package does not import refund, so refund's own tests can use it).
//
// What a test scripts, per invoice:
//   - Lookup answers: Lookup queues them; each LookupInvoiceByString call takes the next, and the last one stays
//     and answers every later call. An invoice with no script answers a *nwc.WalletError with code NOT_FOUND, like a
//     hub that has never seen it.
//   - Lookup answers after a payment: AfterPay queues them the same way, and they take over from the Lookup script
//     once the invoice has had a pay_invoice call that reached the wallet (its context had not ended): a hub that
//     knows the invoice only after it was asked to pay it, however often the caller looked it up before.
//   - The pay result: Pay sets it (a result or an error); the default is a success with a fixed preimage.
//
// A scripted Answer must hold exactly one of a transaction and an error: Lookup and AfterPay panic on anything else,
// so a mistake in a test shows where it was made.
//
// And for the whole wallet: Budget sets the get_budget answer (default: no budget set), FailOnce makes the next
// call of one method answer an error instead of its script, and BeforePay sets a hook every pay_invoice call runs
// before it answers. A failed call is counted but consumes no scripted lookup answer.
//
// The wallet counts every pay_invoice attempt per invoice (PayCount, Pays), and every lookup per invoice
// (LookupCount), including the calls it fails and the calls made with a context that had already ended: the count
// is of attempts, so a test that asserts "at most one pay_invoice" sees a second attempt whatever its context.
// MaxPaysInFlight is the most pay_invoice calls that were ever in progress at once. The wallet is safe for
// concurrent use; the maps and queues are copied in and out.
package refundtest

import (
	"context"
	"crypto/sha256"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satbooth/satbooth/approval"
	"github.com/satbooth/satbooth/nwc"
)

// The method names FailOnce takes.
const (
	CallPay    = "pay_invoice"
	CallLookup = "lookup_invoice"
	CallBudget = "get_budget"
)

// Answer is one scripted lookup answer: a transaction (copied on each use) or an error.
type Answer struct {
	Tx  *nwc.Transaction
	Err error
}

// Tx is the answer that returns a copy of tx.
func Tx(tx nwc.Transaction) Answer { return Answer{Tx: &tx} }

// Err is the answer that returns err.
func Err(err error) Answer { return Answer{Err: err} }

// NotFound is the answer of a hub that has no record of the invoice.
func NotFound() Answer { return Err(&nwc.WalletError{Code: "NOT_FOUND", Message: "not found"}) }

// PayCall is one pay_invoice call.
type PayCall struct {
	Invoice    string
	AmountMsat int64
	ExpiresAt  time.Time // the request's expiration
}

// Wallet is the scripted fake. The zero value is not ready: use New.
type Wallet struct {
	mu      sync.Mutex
	lookups map[string][]Answer
	after   map[string][]Answer
	pays    map[string]payAnswer
	budget  *nwc.Budget
	budErr  error
	failing map[string][]error
	before  func(ctx context.Context, invoice string)

	total       int
	payCalls    []PayCall
	payCount    map[string]int
	delivered   map[string]int
	lookupCount map[string]int
	inFlight    int
	maxInFlight int
}

type payAnswer struct {
	res *nwc.PayResult
	err error
}

// New returns a wallet with nothing scripted.
func New() *Wallet {
	return &Wallet{lookups: map[string][]Answer{}, after: map[string][]Answer{}, pays: map[string]payAnswer{},
		failing: map[string][]error{}, payCount: map[string]int{}, delivered: map[string]int{}, lookupCount: map[string]int{},
		budget: &nwc.Budget{}}
}

// mustBeOne panics on an answer that is not exactly one of a transaction and an error.
func mustBeOne(answers []Answer) {
	for _, a := range answers {
		if (a.Tx == nil) == (a.Err == nil) {
			panic("refundtest: a scripted Answer needs exactly one of Tx and Err")
		}
	}
}

// Lookup appends answers to the invoice's lookup script. It panics on an answer with neither or both of Tx and Err.
func (w *Wallet) Lookup(invoice string, answers ...Answer) {
	mustBeOne(answers)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lookups[invoice] = append(w.lookups[invoice], answers...)
}

// AfterPay appends answers to the invoice's after-payment lookup script (see the package comment). It panics on an
// answer with neither or both of Tx and Err.
func (w *Wallet) AfterPay(invoice string, answers ...Answer) {
	mustBeOne(answers)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.after[invoice] = append(w.after[invoice], answers...)
}

// BeforePay sets a hook that every pay_invoice call runs, without the wallet's lock, after it is counted and before
// it answers (nil: none). A test uses it to hold a payment in flight.
func (w *Wallet) BeforePay(f func(ctx context.Context, invoice string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.before = f
}

// MaxPaysInFlight is the most pay_invoice calls that were in progress at the same time.
func (w *Wallet) MaxPaysInFlight() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxInFlight
}

// Pay sets what pay_invoice answers for the invoice: err if it is set, else res (nil res: the default success).
func (w *Wallet) Pay(invoice string, res *nwc.PayResult, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pays[invoice] = payAnswer{res, err}
}

// Budget sets the get_budget answer.
func (w *Wallet) Budget(b *nwc.Budget, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.budget, w.budErr = b, err
}

// FailOnce makes the next call of the named method (CallPay, CallLookup or CallBudget) answer err. Several calls
// queue: the second FailOnce for a method fails the call after the first. The queue is per method, not per invoice:
// with one serial caller (the refund sender is one) "the next call" is exactly the next call that caller makes, of
// whichever invoice; with concurrent callers which of them gets the failure is unspecified. A call made with a
// context that has already ended answers the context's error and leaves the queue as it was.
func (w *Wallet) FailOnce(call string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failing[call] = append(w.failing[call], err)
}

// PayCount is how many pay_invoice calls the invoice has had.
func (w *Wallet) PayCount(invoice string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.payCount[invoice]
}

// Pays returns every pay_invoice call, in order.
func (w *Wallet) Pays() []PayCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]PayCall(nil), w.payCalls...)
}

// TotalCalls is the number of calls of every method the wallet has had (a failed or empty-script call counts).
func (w *Wallet) TotalCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// LookupCount is how many lookups the invoice has had.
func (w *Wallet) LookupCount(invoice string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lookupCount[invoice]
}

// failNext pops the queued failure of call, if any. The caller holds w.mu.
func (w *Wallet) failNext(call string) error {
	q := w.failing[call]
	if len(q) == 0 {
		return nil
	}
	w.failing[call] = q[1:]
	return q[0]
}

// PayInvoiceUntil implements refund.Wallet. Every call is counted first, with the expiration it was given; a context
// that has ended then answers its error and changes nothing else.
func (w *Wallet) PayInvoiceUntil(ctx context.Context, invoice string, amountMsat int64,
	expiresAt time.Time) (*nwc.PayResult, error) {
	w.mu.Lock()
	w.total++
	w.payCount[invoice]++
	w.payCalls = append(w.payCalls, PayCall{invoice, amountMsat, expiresAt})
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		return nil, err
	}
	w.delivered[invoice]++
	w.inFlight++
	w.maxInFlight = max(w.maxInFlight, w.inFlight)
	before := w.before
	w.mu.Unlock()
	if before != nil {
		before(ctx, invoice)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight--
	if err := w.failNext(CallPay); err != nil {
		return nil, err
	}
	if a, ok := w.pays[invoice]; ok {
		if a.err != nil {
			return nil, a.err
		}
		if a.res != nil {
			r := *a.res
			return &r, nil
		}
	}
	return &nwc.PayResult{Preimage: strings.Repeat("ab", 32)}, nil
}

// LookupInvoiceByString implements refund.Wallet. Every call is counted first; a context that has ended then answers
// its error and consumes nothing.
func (w *Wallet) LookupInvoiceByString(ctx context.Context, invoice string) (*nwc.Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total++
	w.lookupCount[invoice]++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.failNext(CallLookup); err != nil {
		return nil, err
	}
	scripts := w.lookups
	if w.delivered[invoice] > 0 && len(w.after[invoice]) > 0 {
		scripts = w.after
	}
	q := scripts[invoice]
	if len(q) == 0 {
		return nil, NotFound().Err
	}
	a := q[0]
	if len(q) > 1 {
		scripts[invoice] = q[1:]
	}
	if a.Err != nil {
		return nil, a.Err
	}
	tx := *a.Tx
	return &tx, nil
}

// GetBudget implements refund.Wallet. Every call is counted first; a context that has ended then answers its error.
func (w *Wallet) GetBudget(ctx context.Context) (*nwc.Budget, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.failNext(CallBudget); err != nil {
		return nil, err
	}
	if w.budErr != nil {
		return nil, w.budErr
	}
	b := *w.budget
	return &b, nil
}

// Invoice returns a syntactically valid BOLT 11 invoice for net (it passes lninvoice.Check; it is not payable):
// the same n gives the same invoice, different n different invoices. The data part is 7+104 filler characters and
// the 6-character checksum.
func Invoice(net approval.Network, n int) string { return InvoiceWithAmount(net, n, "") }

// InvoiceFor is Invoice carrying msat in its human-readable part (in pico-bitcoin: msat × 10, then "p"), the form a
// claim needs: SatBooth admits only an invoice that carries exactly the approved amount.
func InvoiceFor(net approval.Network, n int, msat int64) string {
	return InvoiceWithAmount(net, n, strconv.FormatInt(msat*10, 10)+"p")
}

// InvoiceWithAmount is Invoice with amount (BOLT 11 digits and an optional m, u, n or p, such as "10u") after the
// network code in the human-readable part; the empty amount gives Invoice's invoice.
func InvoiceWithAmount(net approval.Network, n int, amount string) string {
	return InvoiceLong(net, n, amount, 111)
}

// InvoiceLong is InvoiceWithAmount with dataLen 5-bit data characters before the checksum (at least 111: a longer
// invoice, as one with a description and route hints is; lninvoice accepts up to 2048 characters in all).
func InvoiceLong(net approval.Network, n int, amount string, dataLen int) string {
	hrps := [...]string{approval.Mainnet: "lnbc", approval.Testnet: "lntb", approval.Signet: "lntbs",
		approval.Regtest: "lnbcrt"}
	hrp := hrps[net] + amount
	seed := sha256.Sum256([]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)})
	data := make([]byte, max(dataLen, 111))
	for i := range data {
		data[i] = seed[i%32] & 31
		if i >= 32 {
			data[i] = (seed[i%32] + byte(i)) & 31
		}
	}
	return bech32(hrp, data)
}

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if top>>uint(i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// bech32 returns hrp + "1" + data + BIP-173 checksum, data in 5-bit values.
func bech32(hrp string, data []byte) string {
	v := make([]byte, 0, 2*len(hrp)+1+len(data)+6)
	for i := 0; i < len(hrp); i++ {
		v = append(v, hrp[i]>>5)
	}
	v = append(v, 0)
	for i := 0; i < len(hrp); i++ {
		v = append(v, hrp[i]&31)
	}
	v = append(append(v, data...), 0, 0, 0, 0, 0, 0)
	mod := polymod(v) ^ 1
	out := []byte(hrp + "1")
	for _, d := range data {
		out = append(out, charset[d])
	}
	for i := 0; i < 6; i++ {
		out = append(out, charset[(mod>>uint(5*(5-i)))&31])
	}
	return string(out)
}
