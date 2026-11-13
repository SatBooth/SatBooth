package onchain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

func TestAddressFiftyConcurrentCallsShareOneAllocation(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(50_000)
	var wg sync.WaitGroup
	res := make([]store.Address, 50)
	errs := make([]error, 50)
	for i := range res {
		wg.Add(1)
		go func() { defer wg.Done(); res[i], errs[i] = e.svc.Address(bg, o.ID) }()
	}
	wg.Wait()
	for i := range res {
		if errs[i] != nil || res[i] != res[0] {
			t.Fatalf("call %d: %+v, %v", i, res[i], errs[i])
		}
	}
	if n := e.node.Calls("getnewaddress"); n != 1 {
		t.Fatalf("getnewaddress called %d times", n)
	}
	if m, ok, _ := e.st.MaxAddressIndex(bg); !ok || m != 0 {
		t.Fatalf("max index %d %v", m, ok)
	}
	// A later call returns the stored row without bitcoind.
	again, err := e.svc.Address(bg, o.ID)
	if err != nil || again != res[0] || e.node.Calls("getnewaddress") != 1 {
		t.Fatalf("second round: %+v %v", again, err)
	}
}

func TestAddressTwentyOrdersRiseStrictly(t *testing.T) {
	e := newEnv(t)
	e.ready()
	orders := make([]store.Order, 20)
	for i := range orders {
		orders[i] = e.order(1000)
	}
	var wg sync.WaitGroup
	got := make([]store.Address, 20)
	errs := make([]error, 20)
	for i := range orders {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], errs[i] = e.svc.Address(bg, orders[i].ID) }()
	}
	wg.Wait()
	var idx []int64
	seen := map[string]bool{}
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("order %d: %v", i, errs[i])
		}
		idx = append(idx, got[i].Index)
		if seen[got[i].Address] {
			t.Fatal("duplicate address")
		}
		seen[got[i].Address] = true
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	for i, v := range idx {
		if v != int64(i) {
			t.Fatalf("indexes %v", idx)
		}
	}
	if n := e.node.Calls("getnewaddress"); n != 20 {
		t.Fatalf("getnewaddress called %d times", n)
	}
	if e.logHas("ALERT") {
		t.Fatalf("ALERT: %s", e.log.String())
	}
}

func TestAddressSecondOrderIsIndexPlusOne(t *testing.T) {
	e := newEnv(t)
	e.ready()
	a, err := e.svc.Address(bg, e.order(1000).ID)
	if err != nil || a.Index != 0 {
		t.Fatal(a, err)
	}
	b, err := e.svc.Address(bg, e.order(1000).ID)
	if err != nil || b.Index != 1 || b.Address == a.Address {
		t.Fatal(b, err)
	}
	if !store.ValidAddress(a.Address) || !strings.HasPrefix(a.Address, "bcrt1") {
		t.Fatalf("address %q", a.Address)
	}
}

func TestAddressRefusalsBeforeStepOne(t *testing.T) {
	e := newEnv(t)
	e.ready()
	if _, err := e.svc.Address(bg, strings.Repeat("a", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	ln, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: "ln-only", AmountMsat: 1_000_000, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Address(bg, ln.ID); !errors.Is(err, ErrNoOnchain) {
		t.Fatalf("lightning-only order: %v", err)
	}
	o := e.order(1000)
	e.clk.Advance(time.Hour)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired order: %v", err)
	}
	e.clk.Advance(-time.Hour)
	e.ln.paid = true
	both, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: "both", AmountMsat: 1_000_000, AmountSat: 1000, ExpiresAt: e.clk.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Address(bg, both.ID); !errors.Is(err, ErrPaid) {
		t.Fatalf("paid on Lightning: %v", err)
	}
	if n := e.node.Calls("getnewaddress"); n != 0 {
		t.Fatalf("getnewaddress called %d times for refused orders", n)
	}
}

func TestAddressPaidOnchainIsRefused(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.recordPaid(o, a, 1000)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrPaid) {
		t.Fatalf("paid on-chain: %v", err)
	}
}

func TestAddressCheckFailuresStoreNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(s *onchaintest.State)
	}{
		{"not mine", func(s *onchaintest.State) { s.InfoHook = func(i *btcrpc.AddressInfo) { i.IsMine = false } }},
		{"parent_desc off", func(s *onchaintest.State) { s.InfoHook = func(i *btcrpc.AddressInfo) { i.ParentDesc += "x" } }},
		{"hdkeypath m/1/5", func(s *onchaintest.State) { s.InfoHook = func(i *btcrpc.AddressInfo) { i.HDKeyPath = "m/1/5" } }},
		{"derive answers another address", func(s *onchaintest.State) {
			s.DeriveHook = func(_ int64, l []string) []string { return []string{l[0][:len(l[0])-1] + "q"} }
		}},
		{"derive answers two", func(s *onchaintest.State) {
			s.DeriveHook = func(_ int64, l []string) []string { return append(l, l[0]) }
		}},
		{"address with another network's prefix", func(s *onchaintest.State) { s.HRP = "bc1" }},
		{"hdkeypath index differs from derivation", func(s *onchaintest.State) {
			s.InfoHook = func(i *btcrpc.AddressInfo) { i.HDKeyPath = "m/0/99" }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			o := e.order(1000)
			e.node.Update(c.set)
			a, err := e.svc.Address(bg, o.ID)
			if !errors.Is(err, ErrAddress) {
				t.Fatalf("Address = %+v, %v", a, err)
			}
			if _, err := e.st.AddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored: %v", err)
			}
			if !e.logHas("ALERT: a new address failed its checks; not shown") {
				t.Fatalf("no ALERT: %s", e.log.String())
			}
			if strings.Contains(e.log.String(), "bcrt1") {
				t.Fatal("an address was logged")
			}
			e.node.Update(func(s *onchaintest.State) { s.InfoHook, s.DeriveHook, s.HRP = nil, nil, "bcrt1" })
			got, err := e.svc.Address(bg, o.ID)
			if err != nil || got.OrderID != o.ID {
				t.Fatalf("next call: %+v, %v", got, err)
			}
			if got.Index != 1 {
				t.Fatalf("index %d: the failed one is burned, the next is 1", got.Index)
			}
		})
	}
}

func TestAddressIndexNotAboveMaximumSetsBehind(t *testing.T) {
	e := newEnv(t)
	e.ready()
	for i := 0; i < 2; i++ {
		if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 1 }) // the next index equals the maximum
	o := e.order(1000)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) || !errors.Is(err, ErrAddress) {
		t.Fatalf("Address = %v", err)
	}
	if !e.svc.behind.Load() {
		t.Fatal("behind not set")
	}
	if !e.logHas("ALERT: a new address failed its checks; not shown") || !e.logHas("step=4") ||
		!e.logHas("ALERT: wallet next_index is behind SatBooth's record") {
		t.Fatalf("log: %s", e.log.String())
	}
	calls := e.node.Calls("getnewaddress")
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) || e.node.Calls("getnewaddress") != calls {
		t.Fatalf("a behind service called bitcoind: %v", err)
	}
	if _, err := e.st.AddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stored")
	}
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 2 })
	if err := e.svc.Check(bg); err != nil {
		t.Fatal(err)
	}
	if e.svc.behind.Load() {
		t.Fatal("Check did not clear behind")
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 2 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressOriginPathForm(t *testing.T) {
	e := newEnv(t)
	e.ready()
	e.node.Update(func(s *onchaintest.State) { s.HDPrefix = "m/84h/1h/0h/0/" })
	a, err := e.svc.Address(bg, e.order(1000).ID)
	if err != nil || a.Index != 0 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressCrashAtEachHookPoint(t *testing.T) {
	for _, point := range []string{"address:after-getnewaddress", "address:after-checks", "address:after-store"} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			o := e.order(1000)
			boom := errors.New("crash")
			var once sync.Once
			e.svc.hook = func(p string) error {
				if p != point {
					return nil
				}
				var err error
				once.Do(func() { err = boom })
				return err
			}
			shown, err := e.svc.Address(bg, o.ID)
			if !errors.Is(err, boom) || shown.Address != "" {
				t.Fatalf("crashed call returned %+v, %v", shown, err)
			}
			row, rowErr := e.st.AddressForOrder(bg, o.ID)
			again, err := e.svc.Address(bg, o.ID)
			if err != nil {
				t.Fatal(err)
			}
			if point == "address:after-store" {
				if rowErr != nil || again != row || again.Index != 0 {
					t.Fatalf("the stored address was not returned on retry: %+v %v", again, rowErr)
				}
				if e.node.Calls("getnewaddress") != 1 {
					t.Fatal("retry allocated again")
				}
			} else {
				if !errors.Is(rowErr, store.ErrNotFound) || again.Index != 1 {
					t.Fatalf("row %v; retry %+v", rowErr, again)
				}
			}
		})
	}
}

func TestAddressHangGivesBusyThenSucceeds(t *testing.T) {
	e := newEnv(t)
	e.ready()
	e.svc.pressWait = 50 * time.Millisecond
	e.svc.callTimeout = 400 * time.Millisecond
	o := e.order(1000)
	e.node.Hang("getnewaddress")
	start := time.Now()
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("Address = %v, want ErrBusy", err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("ErrBusy after %v", time.Since(start))
	}
	// The flight ends at the call timeout by itself.
	// Its bound is 5 x CallTimeout; the hung call itself ends at CallTimeout.
	bound := 2 * 5 * e.svc.callTimeout
	for e.svc.inFlight(o.ID) {
		if time.Since(start) > bound {
			t.Fatalf("the flight did not end within %v", bound)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if took := time.Since(start); took < e.svc.callTimeout/2 {
		t.Fatalf("the flight ended after %v, before the call timeout", took)
	}
	e.node.Unhang("getnewaddress")
	e.svc.pressWait = 10 * time.Second
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 0 {
		t.Fatalf("later call: %+v %v", a, err)
	}
}

func TestAddressFlightSurvivesTheCallersContext(t *testing.T) {
	e := newEnv(t)
	e.ready()
	e.svc.pressWait = time.Minute
	o := e.order(1000)
	gate := make(chan struct{})
	e.node.Update(func(s *onchaintest.State) { s.AddressAfterHook = func(int64) { <-gate } })
	ctx, cancel := contextWithCancel()
	done := make(chan error, 1)
	go func() { _, err := e.svc.Address(ctx, o.ID); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for e.node.Calls("getnewaddress") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no call")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled caller got an address")
	}
	close(gate)
	deadline = time.Now().Add(5 * time.Second)
	for e.svc.inFlight(o.ID) {
		if time.Now().After(deadline) {
			t.Fatal("flight did not end")
		}
		time.Sleep(time.Millisecond)
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 0 || e.node.Calls("getnewaddress") != 1 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestAddressTransportErrorIsUnavailable(t *testing.T) {
	for _, m := range []string{"getnewaddress", "getaddressinfo", "deriveaddresses"} {
		e := newEnv(t)
		e.ready()
		o := e.order(1000)
		e.node.Fail(m, fmt.Errorf("%w: down", btcrpc.ErrTransport))
		if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: %v", m, err)
		}
		if _, err := e.svc.Address(bg, o.ID); err != nil {
			t.Fatalf("%s: retry: %v", m, err)
		}
	}
}

func TestAddressBeforeCheckIsRefused(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Address(bg, e.order(1000).ID); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("%v", err)
	}
	if e.node.Calls("getnewaddress") != 0 {
		t.Fatal("called bitcoind")
	}
}

func TestAddressRestartKeepsIndexes(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = e.newService() // a restart: the store and the node remain
	e.ready()
	if b, err := e.svc.Address(bg, o.ID); err != nil || b != a {
		t.Fatalf("%+v %v", b, err)
	}
	if c, err := e.svc.Address(bg, e.order(1000).ID); err != nil || c.Index != 1 {
		t.Fatalf("%+v %v", c, err)
	}
}

// recordPaid writes the forward transfer of an output, as the watcher does, and the TXO row.
func (e *env) recordPaid(o store.Order, a store.Address, sat int64) {
	e.t.Helper()
	txid := strings.Repeat("ab", 32)
	if _, err := e.st.RecordScan(bg, []store.TXO{{TxID: txid, Vout: 0, Address: a.Address, AmountSat: sat}},
		store.Cursor{BlockHash: strings.Repeat("cd", 32), Height: 1, At: e.clk.Now()}); err != nil {
		e.t.Fatal(err)
	}
	ud, _ := orderUserData(o.ID)
	r, err := e.lg.CreateTransfer(bg, ledger.Transfer{ID: PaidTransferID(txid, 0, 0), DebitAccountID: OnchainWallet, CreditAccountID: MerchantOnchain,
		Amount: ledger.U128From64(uint64(sat)), UserData128: ud, Ledger: ledgerID, Code: codePaid})
	if err != nil || r != ledger.TransferCreated {
		e.t.Fatal(r, err)
	}
}

// An order that expires while its flight waits for the allocation lock gets no address and no getnewaddress call.
func TestAddressFlightRechecksExpiryAndPaymentUnderTheLock(t *testing.T) {
	for _, how := range []string{"expired", "paid on Lightning"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			e.ready()
			e.svc.pressWait = 20 * time.Millisecond
			o, _, err := e.st.CreateOrder(bg, store.Order{AccountID: "acct-1", MerchantRef: "recheck", AmountMsat: 1_000_000, AmountSat: 1000,
				ExpiresAt: e.clk.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			e.svc.alloc <- struct{}{} // another allocation holds the lock
			if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrBusy) {
				t.Fatalf("Address = %v", err)
			}
			if how == "expired" {
				e.clk.Advance(2 * time.Hour)
			} else {
				e.ln.mu.Lock()
				e.ln.paid = true
				e.ln.mu.Unlock()
			}
			<-e.svc.alloc
			deadline := time.Now().Add(5 * time.Second)
			for e.svc.inFlight(o.ID) {
				if time.Now().After(deadline) {
					t.Fatal("flight did not end")
				}
				time.Sleep(time.Millisecond)
			}
			if _, err := e.st.AddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a row was stored: %v", err)
			}
			if e.node.Calls("getnewaddress") != 0 {
				t.Fatal("getnewaddress was called")
			}
		})
	}
}

// Another writer stores a higher index between the maximum's read and the store write: the store's refusal sets behind.
func TestAddressStoreIndexRefusalSetsBehind(t *testing.T) {
	e := newEnv(t)
	e.ready()
	other := e.order(1000)
	o := e.order(1000)
	e.svc.hook = func(p string) error {
		if p != "address:after-checks" {
			return nil
		}
		return e.st.AddAddress(bg, store.Address{OrderID: other.ID, Address: e.node.AddressAt(50), Index: 50})
	}
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) || !errors.Is(err, ErrAddress) {
		t.Fatalf("Address = %v", err)
	}
	if !e.svc.behind.Load() {
		t.Fatal("behind not set")
	}
	if _, err := e.st.AddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stored: %v", err)
	}
}

func TestWaitFlights(t *testing.T) {
	e := newEnv(t)
	e.ready()
	if err := e.svc.WaitFlights(bg); err != nil {
		t.Fatalf("no flights: %v", err)
	}
	e.svc.pressWait = 10 * time.Millisecond
	o := e.order(1000)
	e.svc.alloc <- struct{}{}
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	// A flight is running: a short ctx abandons it.
	short, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	if err := e.svc.WaitFlights(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitFlights = %v", err)
	}
	// Once the flight lands, an already-ended ctx still reports success.
	done := make(chan error, 1)
	go func() { done <- e.svc.WaitFlights(bg) }()
	<-e.svc.alloc
	if err := <-done; err != nil {
		t.Fatalf("WaitFlights = %v", err)
	}
	if e.svc.inFlight(o.ID) {
		t.Fatal("still in flight")
	}
	ended, stop := context.WithCancel(bg)
	stop()
	if err := e.svc.WaitFlights(ended); err != nil {
		t.Fatalf("no flights, ended ctx: %v", err)
	}
	if a, err := e.svc.Address(bg, o.ID); err != nil || a.Index != 0 {
		t.Fatalf("%+v %v", a, err)
	}
}
