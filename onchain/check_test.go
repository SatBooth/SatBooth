package onchain

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/onchain/onchaintest"
	"github.com/satbooth/satbooth/store"
)

func TestCheckPassesOnAFreshWallet(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Check(bg); err != nil {
		t.Fatal(err)
	}
	if e.logHas("ALERT") {
		t.Fatalf("unexpected ALERT: %s", e.log.String())
	}
}

func TestCheckFailures(t *testing.T) {
	rec := func(e *env, mut func(*btcrpc.DescriptorRecord)) []btcrpc.DescriptorRecord {
		r := btcrpc.DescriptorRecord{Desc: e.norm, Active: true, NextIndex: 0, Range: [2]int64{0, 999}}
		mut(&r)
		return []btcrpc.DescriptorRecord{r}
	}
	for _, c := range []struct {
		name string
		mut  func(e *env, s *onchaintest.State)
		want error
	}{
		{"wrong chain", func(e *env, s *onchaintest.State) { s.Chain = "main" }, ErrNetwork},
		{"testnet4 chain name", func(e *env, s *onchaintest.State) { s.Chain = "testnet4" }, ErrNetwork},
		{"private keys enabled", func(e *env, s *onchaintest.State) { s.PrivateKeysEnabled = true }, ErrWallet},
		{"legacy wallet", func(e *env, s *onchaintest.State) { s.WalletDescriptors = false }, ErrWallet},
		{"descriptor has private keys", func(e *env, s *onchaintest.State) { s.HasPrivateKeys = true }, ErrDescriptor},
		{"descriptor not ranged", func(e *env, s *onchaintest.State) { s.Unranged = true }, ErrDescriptor},
		{"second active descriptor", func(e *env, s *onchaintest.State) {
			s.Records = append(rec(e, func(*btcrpc.DescriptorRecord) {}), btcrpc.DescriptorRecord{Desc: "wpkh(other)#12345678", Active: true})
		}, ErrDescriptor},
		{"no active descriptor", func(e *env, s *onchaintest.State) {
			s.Records = rec(e, func(r *btcrpc.DescriptorRecord) { r.Active = false })
		}, ErrDescriptor},
		{"only an internal descriptor", func(e *env, s *onchaintest.State) {
			s.Records = rec(e, func(r *btcrpc.DescriptorRecord) { r.Internal = true })
		}, ErrDescriptor},
		{"desc differs by one character", func(e *env, s *onchaintest.State) {
			s.Records = rec(e, func(r *btcrpc.DescriptorRecord) {
				c := "q"
				if strings.HasSuffix(r.Desc, c) {
					c = "p"
				}
				r.Desc = r.Desc[:len(r.Desc)-1] + c
			})
		}, ErrDescriptor},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.node.Update(func(s *onchaintest.State) { c.mut(e, s) })
			err := e.svc.Check(bg)
			if !errors.Is(err, c.want) {
				t.Fatalf("Check = %v, want %v", err, c.want)
			}
		})
	}
}

// The wallet holds another descriptor than the configured one: the fake's getdescriptorinfo answers from its input, so
// the configured descriptor's normal form differs from the wallet's record.
func TestCheckWalletHoldsAnotherDescriptor(t *testing.T) {
	e := newEnv(t)
	other := onchaintest.Normalise("wpkh(" + tpub[:len(tpub)-1] + "x/0/*)")
	e.node.Update(func(s *onchaintest.State) { s.Descriptor = other })
	if err := e.svc.Check(bg); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("Check = %v", err)
	}
	if e.svc.descriptor() != "" {
		t.Fatal("a failed Check kept a descriptor")
	}
	if _, err := e.svc.Address(bg, e.order(1000).ID); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("Address = %v", err)
	}
	if e.node.Calls("getnewaddress") != 0 {
		t.Fatal("called getnewaddress")
	}
}

func TestCheckIgnoresAnExtraInactiveOrInternalRecord(t *testing.T) {
	e := newEnv(t)
	e.node.Update(func(s *onchaintest.State) {
		s.Records = []btcrpc.DescriptorRecord{
			{Desc: e.norm, Active: true, Range: [2]int64{0, 999}},
			{Desc: "wpkh(other)#12345678", Active: false},
			{Desc: "wpkh(change)#12345678", Active: true, Internal: true},
		}
	})
	if err := e.svc.Check(bg); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNextIndexEqualToRecordedMaximum(t *testing.T) {
	e := newEnv(t)
	e.ready()
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Address(bg, e.order(1000).ID); err != nil {
			t.Fatal(err)
		}
	}
	// next_index 2 is the recorded maximum (index 2): equal is behind.
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 2 })
	err := e.svc.Check(bg)
	if !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Check = %v, want ErrIndexBehind", err)
	}
	want := "onchain: the wallet's next_index is not above the recorded address index; re-import the descriptor with next_index set"
	if err.Error() != want {
		t.Fatalf("message = %q", err.Error())
	}
	if !e.logHas("ALERT: wallet next_index is behind SatBooth's record; addresses are not issued") {
		t.Fatalf("no ALERT: %s", e.log.String())
	}
}

func TestCheckTransportErrorIsUnavailableAndDoesNotSetBehind(t *testing.T) {
	e := newEnv(t)
	e.node.Fail("getblockchaininfo", fmt.Errorf("%w: down", btcrpc.ErrTransport))
	if err := e.svc.Check(bg); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Check = %v", err)
	}
	if e.svc.behind.Load() {
		t.Fatal("behind set by a transport error")
	}
}

func TestCheckIBDAlert(t *testing.T) {
	e := newEnv(t)
	e.node.Update(func(s *onchaintest.State) { s.IBD = true })
	if err := e.svc.Check(bg); err != nil {
		t.Fatal(err)
	}
	if !e.logHas("ALERT: bitcoind is in initial block download or behind its headers") {
		t.Fatalf("no IBD ALERT: %s", e.log.String())
	}
}

func TestCheckHeadersBehind(t *testing.T) {
	for _, c := range []struct {
		ahead int64
		alert bool
	}{{3, false}, {4, true}} {
		e := newEnv(t)
		e.node.Update(func(s *onchaintest.State) { s.HeadersAhead = c.ahead })
		if err := e.svc.Check(bg); err != nil {
			t.Fatal(err)
		}
		if e.logHas("ALERT: bitcoind is in initial block download") != c.alert {
			t.Fatalf("headers ahead %d: alert = %v", c.ahead, !c.alert)
		}
	}
}

func TestChainNamesPerNetwork(t *testing.T) {
	for network, chain := range map[string]string{"mainnet": "main", "testnet": "test", "signet": "signet", "regtest": "regtest"} {
		if chainName(network) != chain {
			t.Errorf("chainName(%q) = %q", network, chainName(network))
		}
	}
}

// A wallet restored from an older backup, or a descriptor re-imported without next_index.
func TestReviewFocus1WalletResetRefusesAddresses(t *testing.T) {
	e := newEnv(t)
	e.ready()
	var addrs []store.Address
	for i := 0; i < 3; i++ {
		a, err := e.svc.Address(bg, e.order(1000).ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.Index != int64(i) {
			t.Fatalf("index %d, want %d", a.Index, i)
		}
		addrs = append(addrs, a)
	}
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 1 })
	if err := e.svc.Check(bg); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Check = %v", err)
	}
	o := e.order(1000)
	if _, err := e.svc.Address(bg, o.ID); !errors.Is(err, ErrIndexBehind) {
		t.Fatalf("Address = %v, want ErrIndexBehind", err)
	}
	// An order that already has its address still gets it while the service is behind.
	if got, err := e.svc.Address(bg, addrs[1].OrderID); err != nil || got != addrs[1] {
		t.Fatalf("stored address while behind: %+v, %v", got, err)
	}
	if _, err := e.st.AddressForOrder(bg, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an address was stored: %v", err)
	}
	if got := e.node.Calls("getnewaddress"); got != 3 {
		t.Fatalf("getnewaddress called %d times; a behind service must not call it", got)
	}
	// An order that already has its address still gets it.
	if a, err := e.svc.Address(bg, ""); err == nil {
		t.Fatalf("empty order id accepted: %v", a)
	}
	e.node.Update(func(s *onchaintest.State) { s.NextIndex = 3 })
	if err := e.svc.Check(bg); err != nil {
		t.Fatalf("Check after repair: %v", err)
	}
	a, err := e.svc.Address(bg, o.ID)
	if err != nil || a.Index != 3 {
		t.Fatalf("Address = %+v, %v; want index 3", a, err)
	}
}
