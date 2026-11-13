package monero

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/xmrrpc"
)

func TestCheckPasses(t *testing.T) {
	e := newEnv(t)
	e.ready()
	if !e.logHas(`msg="wallet-rpc version" major=1 minor=32`) {
		t.Fatalf("log: %s", e.log.String())
	}
	if e.logHas("ALERT") {
		t.Fatalf("ALERT: %s", e.log.String())
	}
	if e.svc.baseAddress() != e.w.Primary() {
		t.Fatal("base address")
	}
	e.ready()
	if n := strings.Count(e.log.String(), "wallet-rpc version"); n != 1 {
		t.Fatalf("the version was logged %d times", n)
	}
	for _, m := range []string{"get_version", "get_address", "sign_transfer", "get_height"} {
		if e.w.Calls(m) == 0 {
			t.Fatalf("%s not called", m)
		}
	}
	if e.w.Calls("create_address") != 0 || e.w.Calls("store") != 0 {
		t.Fatal("a Check with no record changed the wallet")
	}
}

func u32(v uint32) *uint32 { return &v }
func i(v int) *int         { return &v }

func TestCheckFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(e *env)
		want error
	}{
		{"major 2", func(e *env) { e.rpc.set(func(o *override) { o.version = u32(2<<16 | 0) }) }, ErrVersion},
		{"major 0", func(e *env) { e.rpc.set(func(o *override) { o.version = u32(30) }) }, ErrVersion},
		{"another primary address", func(e *env) {
			e.mod = func(c *Config) { c.Address = monerotest.New(8).Primary() }
			e.svc = e.newService()
		}, ErrAddress},
		{"the probe answers 0", func(e *env) { e.rpc.set(func(o *override) { o.probe = i(0) }) }, ErrNotViewOnly},
		{"the probe answers -1", func(e *env) { e.w.Full(true) }, ErrNotViewOnly},
		{"the probe answers -38", func(e *env) { e.rpc.set(func(o *override) { o.probe = i(-38) }) }, ErrNotViewOnly},
		{"height 0", func(e *env) { e.w.SetHeight(0) }, ErrHeight},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.set(e)
			if err := e.svc.Check(bg); !errors.Is(err, c.want) {
				t.Fatalf("Check = %v, want %v", err, c.want)
			}
			if e.svc.behind.Load() {
				t.Fatal("behind set")
			}
			if _, err := e.svc.Address(bg, e.order(1000).ID); err == nil || err.Error() != errNotChecked.Error() {
				t.Fatalf("Address after a failed Check: %v", err)
			}
		})
	}
}

// Ruling 8-T6a: -13 (no wallet open) at any step is ErrNoWallet; another failure is ErrUnavailable with the step.
func TestCheckNoWalletAndUnavailableAtEachStep(t *testing.T) {
	steps := []struct {
		method string
		step   int
	}{{"get_version", 1}, {"get_address", 2}, {"sign_transfer", 3}, {"get_height", 4}}
	for _, s := range steps {
		t.Run(s.method, func(t *testing.T) {
			e := newEnv(t)
			e.w.Fail(s.method, &xmrrpc.Error{Code: -13, Method: s.method})
			err := e.svc.Check(bg)
			if !errors.Is(err, ErrNoWallet) || err.Error() != "monero: wallet-rpc has no wallet open; start it with --wallet-file" {
				t.Fatalf("Check = %v", err)
			}
			e.w.Fail(s.method, fmt.Errorf("%w: down", xmrrpc.ErrTransport))
			err = e.svc.Check(bg)
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, xmrrpc.ErrTransport) || !strings.Contains(err.Error(), fmt.Sprintf("step %d", s.step)) {
				t.Fatalf("Check = %v", err)
			}
			if s.method != "sign_transfer" { // the probe gives an RPC error as its code
				e.w.Fail(s.method, &xmrrpc.Error{Code: -1, Method: s.method})
				if err := e.svc.Check(bg); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("Check = %v", err)
				}
			}
			e.ready()
		})
	}
	// -13 at the record's step.
	e := newEnv(t)
	e.ready()
	if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
		t.Fatal(err)
	}
	e.rpc.set(func(o *override) {
		o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if len(ix) == 1 && ix[0] > 0 { // the record's index, not step 2's 0
				return xmrrpc.Addresses{}, &xmrrpc.Error{Code: -13, Method: "get_address"}
			}
			return a, err
		}
	})
	if err := e.svc.Check(bg); !errors.Is(err, ErrNoWallet) {
		t.Fatalf("Check = %v", err)
	}
	// Another code at the record's step is an error, never the forgotten case.
	e.rpc.set(func(o *override) {
		o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if len(ix) == 1 && ix[0] > 0 { // the record's index, not step 2's 0
				return xmrrpc.Addresses{}, &xmrrpc.Error{Code: -14, Method: "get_address"}
			}
			return a, err
		}
	})
	if err := e.svc.Check(bg); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "step 5") {
		t.Fatalf("Check = %v", err)
	}
	if e.w.Calls("create_address") != 1 {
		t.Fatal("re-created on another code")
	}
}

func TestCheckOtherAccount(t *testing.T) {
	e := newEnv(t)
	e.mod = func(c *Config) { c.AccountIndex = 1 }
	e.svc = e.newService()
	e.ready()
	acct, _ := e.w.GetAddress(bg, 1, nil)
	if e.svc.baseAddress() != acct.Address || acct.Address == e.w.Primary() {
		t.Fatal("the account's base address is not kept")
	}
	a, err := e.svc.Address(bg, e.order(1000).ID)
	if err != nil || a.Index != 1 {
		t.Fatal(a, err)
	}
	if got, _ := e.w.GetAddress(bg, 1, []uint32{1}); got.Addresses[0].Address != a.Address {
		t.Fatal("the subaddress is not the account's")
	}
	if a.Address == e.w.SubaddressAt(1) {
		t.Fatal("account 0's subaddress was issued")
	}
	// The primary address is account 0's, whatever the account.
	e.mod = func(c *Config) { c.AccountIndex, c.Address = 1, monerotest.New(8).Primary() }
	e.svc = e.newService()
	if err := e.svc.Check(bg); !errors.Is(err, ErrAddress) {
		t.Fatalf("Check = %v", err)
	}
	e.rpc.set(func(o *override) {
		o.getAddr = func(acct uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if acct == 1 && len(ix) == 1 && ix[0] == 0 {
				a.Address = "4" + a.Address[1:] // the account's base address must be a subaddress
			}
			return a, err
		}
	})
	e.mod = func(c *Config) { c.AccountIndex = 1 }
	e.svc = e.newService()
	if err := e.svc.Check(bg); !errors.Is(err, ErrAddress) {
		t.Fatalf("Check = %v", err)
	}
}

func TestCheckRecordedSubaddressDiffers(t *testing.T) {
	e := newEnv(t)
	e.ready()
	for range 2 {
		if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	right := e.w
	// Another wallet with the same primary address shape and two subaddresses of its own, over the same store.
	other := monerotest.New(8)
	other.CreateAddress(bg, 0, "")
	other.CreateAddress(bg, 0, "")
	e.setWallet(other)
	e.mod = func(c *Config) { c.Address = other.Primary() }
	e.svc = e.newService()
	if err := e.svc.Check(bg); !errors.Is(err, ErrSubaddress) {
		t.Fatalf("Check = %v", err)
	}
	if !e.svc.behind.Load() {
		t.Fatal("behind not set")
	}
	if _, err := e.svc.Address(bg, e.order(1000).ID); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Address = %v", err)
	}
	if other.Calls("create_address") != 2 {
		t.Fatal("the wallet was changed")
	}
	// The right wallet passes and clears behind.
	e.setWallet(right)
	e.mod = nil
	svc := e.newService()
	svc.behind.Store(true)
	e.svc = svc
	e.ready()
	if e.svc.behind.Load() {
		t.Fatal("behind not cleared")
	}
}

// Wallet-rpc killed after create_address and before store forgets subaddresses SatBooth recorded.
func TestCheckRecreatesForgottenSubaddresses(t *testing.T) {
	e := newEnv(t)
	e.ready()
	var issued []store.Subaddress
	for range 3 {
		a, err := e.svc.Address(bg, e.order(1000).ID)
		if err != nil {
			t.Fatal(err)
		}
		issued = append(issued, a)
	}
	stores := e.w.Calls("store")
	e.w.Forget(2)
	if _, err := e.w.GetAddress(bg, 0, []uint32{3}); err == nil {
		t.Fatal("the wallet still knows index 3")
	}
	e.svc = e.newService() // the restart that follows
	e.ready()
	if !e.logHas("ALERT: wallet-rpc had forgotten issued subaddresses; re-created up to the record") || !e.logHas("count=2") {
		t.Fatalf("log: %s", e.log.String())
	}
	if e.w.Calls("create_address") != 3+2 || e.w.Calls("store") != stores+1 {
		t.Fatalf("create %d store %d", e.w.Calls("create_address"), e.w.Calls("store"))
	}
	got, err := e.w.GetAddress(bg, 0, []uint32{3})
	if err != nil || got.Addresses[0].Address != issued[2].Address {
		t.Fatalf("%+v %v", got, err)
	}
	if e.svc.behind.Load() {
		t.Fatal("behind set")
	}
	if a, err := e.svc.Address(bg, e.order(1000).ID); err != nil || a.Index != 4 {
		t.Fatalf("%+v %v", a, err)
	}
	if strings.Contains(e.log.String(), issued[0].Address[:20]) {
		t.Fatal("a subaddress was logged")
	}

	// A wallet with another seed over the same store re-creates subaddresses that are not the recorded ones.
	other := monerotest.New(8)
	for range 3 {
		other.CreateAddress(bg, 0, "")
	}
	other.Forget(3)
	e.setWallet(other)
	e.mod = func(c *Config) { c.Address = other.Primary() }
	e.svc = e.newService()
	err = e.svc.Check(bg)
	if !errors.Is(err, ErrIndexBehind) || err.Error() != "monero: the wallet's subaddresses are behind SatBooth's record and could not be restored" {
		t.Fatalf("Check = %v", err)
	}
	if !e.svc.behind.Load() {
		t.Fatal("behind not set")
	}
	if other.Calls("store") != 0 {
		t.Fatal("a failed restore stored the wallet")
	}
	if _, err := e.svc.Address(bg, e.order(1000).ID); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Address = %v", err)
	}
}

// The restore stores the wallet after every 100 created indices as well as at the end (no second store
// when the last index is a multiple of 100), so a start that hits its bound keeps its progress. A store that fails in
// the loop is ErrUnavailable at step 5; the next Check creates only the indices still missing.
func TestCheckRestoreStoresEvery100(t *testing.T) {
	// record stores an order's subaddress at index m, the wallet's own: the record's maximum is m, as after m issued
	// subaddresses, while the fresh wallet knows index 0 alone (a wallet file restored from keys).
	record := func(e *env, m int) {
		e.t.Helper()
		a := store.Subaddress{OrderID: e.order(1000).ID, Address: e.w.SubaddressAt(uint32(m)), Index: int64(m), CreatedAt: e.clk.Now()}
		if err := e.st.AddSubaddress(bg, a); err != nil {
			e.t.Fatal(err)
		}
	}
	for _, c := range []struct{ m, stores int }{{250, 3}, {200, 2}, {101, 2}, {100, 1}, {99, 1}, {1, 1}} {
		t.Run(fmt.Sprint(c.m), func(t *testing.T) {
			e := newEnv(t)
			record(e, c.m)
			stores, creates := e.w.Calls("store"), e.w.Calls("create_address")
			e.ready()
			if n := e.w.Calls("store") - stores; n != c.stores {
				t.Fatalf("%d store calls for %d indices, want %d", n, c.m, c.stores)
			}
			if n := e.w.Calls("create_address") - creates; n != c.m || !e.logHas(fmt.Sprintf("count=%d", c.m)) {
				t.Fatalf("%d create_address calls, want %d\n%s", n, c.m, e.log)
			}
		})
	}
	t.Run("store fails in the loop", func(t *testing.T) {
		e := newEnv(t)
		record(e, 250)
		stores, creates := e.w.Calls("store"), e.w.Calls("create_address")
		e.w.Fail("store", &xmrrpc.Error{Code: -1, Method: "store"})
		if err := e.svc.Check(bg); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "step 5") {
			t.Fatalf("Check = %v", err)
		}
		if n := e.w.Calls("create_address") - creates; n != 100 {
			t.Fatalf("%d create_address calls before the failed store, want 100", n)
		}
		e.ready() // the wallet knows 1 to 100: 150 more, stored at the 100th of them (index 200) and at the end
		if n := e.w.Calls("create_address") - creates; n != 250 {
			t.Fatalf("%d create_address calls in all, want 250", n)
		}
		if n := e.w.Calls("store") - stores; n != 1+2 {
			t.Fatalf("%d store calls, want 3 (the failed one, then two)", n)
		}
	})
}

// The re-create loop stops when the wallet's indexes do not rise (each at least 1, strictly above the last).
func TestCheckRestoreNeedsRisingIndexes(t *testing.T) {
	e := newEnv(t)
	e.ready()
	for range 3 {
		if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	e.w.Forget(3)
	e.rpc.set(func(o *override) {
		o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Index = 1; return c }
	})
	before := e.w.Calls("create_address")
	if err := e.svc.Check(bg); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Check = %v", err)
	}
	if n := e.w.Calls("create_address") - before; n != 2 {
		t.Fatalf("%d calls", n)
	}
	e.rpc.set(func(o *override) {
		o.create = func(c xmrrpc.Created) xmrrpc.Created { c.Index = 0; return c }
	})
	before = e.w.Calls("create_address")
	if err := e.svc.Check(bg); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Check = %v", err)
	}
	if n := e.w.Calls("create_address") - before; n != 1 {
		t.Fatalf("%d calls", n)
	}
	// A wallet that reaches m but still does not know it (get_address answers -15) is behind after m calls.
	e2 := newEnv(t)
	e2.ready()
	for range 3 {
		if _, err := e2.svc.Address(bg, e2.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	e2.w.Forget(3)
	e2.rpc.set(func(o *override) {
		o.getAddr = func(_ uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
			if len(ix) == 1 && ix[0] > 0 { // the record's index, not step 2's 0
				return xmrrpc.Addresses{}, &xmrrpc.Error{Code: -15, Method: "get_address"}
			}
			return a, err
		}
		n := uint32(0)
		o.create = func(c xmrrpc.Created) xmrrpc.Created { n++; c.Index = n; return c }
	})
	before = e2.w.Calls("create_address")
	if err := e2.svc.Check(bg); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Check = %v", err)
	}
	if n := e2.w.Calls("create_address") - before; n != 3 {
		t.Fatalf("%d calls", n)
	}
	// A transport failure in the loop is ErrUnavailable, and the next Check finishes the restore.
	e3 := newEnv(t)
	e3.ready()
	for range 3 {
		if _, err := e3.svc.Address(bg, e3.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	e3.w.Forget(2)
	e3.w.Fail("create_address", fmt.Errorf("%w: down", xmrrpc.ErrTransport))
	if err := e3.svc.Check(bg); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "step 5") {
		t.Fatalf("Check = %v", err)
	}
	e3.ready()
	if e3.w.Calls("store") != 3+1 {
		t.Fatal("no store after the restore")
	}
	// A store failure after the restore is ErrUnavailable.
	e3.w.Forget(1)
	e3.w.Fail("store", &xmrrpc.Error{Code: -1, Method: "store"})
	if err := e3.svc.Check(bg); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Check = %v", err)
	}
}

func TestCheckHeightBelowCursorAlerts(t *testing.T) {
	e := newEnv(t)
	if _, err := e.st.RecordMoneroScan(bg, nil, store.MoneroCursor{Height: 50, At: e.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	e.w.Mine(48) // height 49
	e.ready()
	const line = "ALERT: the wallet's height is below SatBooth's cursor; payments are found again as it catches up"
	if !e.logHas(line) {
		t.Fatalf("log: %s", e.log.String())
	}
	e.w.Mine(1) // height 50: not below
	e.ready()
	if n := strings.Count(e.log.String(), line); n != 1 {
		t.Fatalf("%d lines", n)
	}
}

type addrCall struct {
	account uint32
	indices []uint32
}

// Step 2 asks for index 0 alone, never the whole list, whatever the number issued.
func TestCheckAsksForIndexZeroOnly(t *testing.T) {
	for _, acct := range []uint32{0, 1} {
		t.Run(fmt.Sprint("account ", acct), func(t *testing.T) {
			e := newEnv(t)
			e.mod = func(c *Config) { c.AccountIndex = acct }
			e.svc = e.newService()
			e.ready()
			for range 3 {
				if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
					t.Fatal(err)
				}
			}
			var calls []addrCall
			e.rpc.set(func(o *override) {
				o.getAddr = func(a uint32, ix []uint32, r xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
					calls = append(calls, addrCall{a, append([]uint32(nil), ix...)})
					return r, err
				}
			})
			e.ready()
			want := []addrCall{{acct, []uint32{0}}}
			if acct != 0 {
				want = append(want, addrCall{0, []uint32{0}})
			}
			want = append(want, addrCall{acct, []uint32{3}}) // step 5: the record's maximum
			if fmt.Sprint(calls) != fmt.Sprint(want) {
				t.Fatalf("get_address calls %v, want %v", calls, want)
			}
		})
	}
}

// A step 2 answer whose one entry is not index 0 with the answer's address is refused.
func TestCheckRefusesAMalformedBaseAnswer(t *testing.T) {
	cases := []struct {
		name string
		f    func(a *xmrrpc.Addresses)
	}{
		{"no entry", func(a *xmrrpc.Addresses) { a.Addresses = nil }},
		{"two entries", func(a *xmrrpc.Addresses) { a.Addresses = append(a.Addresses, a.Addresses[0]) }},
		{"entry at index 1", func(a *xmrrpc.Addresses) { a.Addresses[0].Index = 1 }},
		{"entry with another address", func(a *xmrrpc.Addresses) { a.Addresses[0].Address = monerotest.New(8).Primary() }},
	}
	// Each read of step 2: account 0's alone, and for account 1 its own base read and then account 0's.
	reads := []struct{ config, read uint32 }{{0, 0}, {1, 1}, {1, 0}}
	for _, r := range reads {
		for _, c := range cases {
			t.Run(fmt.Sprintf("account %d, read of %d, %s", r.config, r.read, c.name), func(t *testing.T) {
				e := newEnv(t)
				e.mod = func(cfg *Config) { cfg.AccountIndex = r.config }
				e.svc = e.newService()
				e.ready() // the healthy wallet passes first
				hit := 0
				e.rpc.set(func(o *override) {
					o.getAddr = func(acct uint32, ix []uint32, a xmrrpc.Addresses, err error) (xmrrpc.Addresses, error) {
						if acct == r.read && len(ix) == 1 && ix[0] == 0 && err == nil {
							hit++
							c.f(&a)
						}
						return a, err
					}
				})
				if err := e.svc.Check(bg); !errors.Is(err, ErrAddress) || hit != 1 {
					t.Fatalf("Check = %v (the malformed read was made %d times)", err, hit)
				}
			})
		}
	}
}

// Ruling 8-T6d: a refused login at any step, a missing account at step 2 and a restricted wallet-rpc at the probe
// each name their cause, with the fixed text only.
func TestCheckNamesItsCause(t *testing.T) {
	auth := func(m string) error { return fmt.Errorf("%w: %s: refused after one retry", xmrrpc.ErrAuth, m) }
	for _, m := range []string{"get_version", "get_address", "sign_transfer", "get_height"} {
		e := newEnv(t)
		e.w.Fail(m, auth(m))
		if err := e.svc.Check(bg); err != ErrLogin || err.Error() != "monero: wallet-rpc refused SatBooth's login" {
			t.Fatalf("%s: Check = %v", m, err)
		}
	}
	// A refused login during the restore (step 5).
	e := newEnv(t)
	e.ready()
	if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
		t.Fatal(err)
	}
	e.w.Forget(1)
	e.w.Fail("create_address", auth("create_address"))
	if err := e.svc.Check(bg); err != ErrLogin {
		t.Fatalf("restore: Check = %v", err)
	}

	for _, acct := range []uint32{0, 1} {
		e := newEnv(t)
		e.mod = func(c *Config) { c.AccountIndex = acct }
		e.svc = e.newService()
		e.w.Fail("get_address", &xmrrpc.Error{Code: -14, Method: "get_address"})
		if err := e.svc.Check(bg); err != ErrAccount || err.Error() != "monero: the wallet has no account account_index" {
			t.Fatalf("account %d: Check = %v", acct, err)
		}
	}

	e = newEnv(t)
	e.rpc.set(func(o *override) { o.probe = i(-7) })
	err := e.svc.Check(bg)
	if err != ErrRestricted || err.Error() != "monero: wallet-rpc runs with --restricted-rpc; SatBooth needs full RPC on the private link" {
		t.Fatalf("Check = %v", err)
	}
	if e.svc.behind.Load() || e.svc.baseAddress() != "" {
		t.Fatal("a failed Check set behind or kept a base address")
	}
	// Only -29 is view-only; every other code but -13 and -7 is ErrNotViewOnly.
	for _, code := range []int{0, -1, -6, -8, -14, -28, -30, 1} {
		e.rpc.set(func(o *override) { o.probe = i(code) })
		if err := e.svc.Check(bg); err != ErrNotViewOnly {
			t.Fatalf("probe %d: Check = %v", code, err)
		}
	}
}

// Giving up on the allocation lock keeps the step number.
func TestCheckLockWaitKeepsTheStep(t *testing.T) {
	e := newEnv(t)
	e.svc.alloc <- struct{}{} // an allocation holds the lock
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	// The context ends only once steps 1 to 4 are done, as Check is about to wait for the held lock.
	e.svc.hook = func(p string) error {
		if p == "check:before-lock" {
			cancel()
		}
		return nil
	}
	err := e.svc.Check(ctx)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "step 5") {
		t.Fatalf("Check = %v", err)
	}
	if e.w.Calls("get_height") != 1 {
		t.Fatal("steps 1 to 4 did not run")
	}
	<-e.svc.alloc
	e.svc.hook = nil
	e.ready()
}

// CheckStep names the step of an ErrUnavailable from Check, whose text and wrapped errors stay as they were; any other
// error has no step.
func TestCheckStep(t *testing.T) {
	down := func(m string) error { return fmt.Errorf("%w: %s: connection refused", xmrrpc.ErrTransport, m) }
	for step, m := range map[int]string{1: "get_version", 2: "get_address", 3: "sign_transfer", 4: "get_height"} {
		e := newEnv(t)
		e.w.Fail(m, down(m))
		err := e.svc.Check(bg)
		n, ok := CheckStep(err)
		if !ok || n != step || !errors.Is(err, ErrUnavailable) || !errors.Is(err, xmrrpc.ErrTransport) ||
			!strings.HasPrefix(err.Error(), fmt.Sprintf("monero: wallet-rpc is unavailable: step %d: ", step)) {
			t.Fatalf("%s: Check = %v, CheckStep = %d %v", m, err, n, ok)
		}
	}
	for _, err := range []error{ErrNoWallet, ErrUnavailable, fmt.Errorf("%w: x", ErrUnavailable), nil} {
		if n, ok := CheckStep(err); ok || n != 0 {
			t.Fatalf("%v: CheckStep = %d %v", err, n, ok)
		}
	}
}
