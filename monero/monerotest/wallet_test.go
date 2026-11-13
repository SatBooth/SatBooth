package monerotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/satbooth/satbooth/xmrrpc"
)

var bg = context.Background()

func rpcCode(err error) int {
	var e *xmrrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestAddressesAreDeterministicAndValid(t *testing.T) {
	a, b := New(1), New(1)
	if a.Primary() != b.Primary() || a.SubaddressAt(5) != b.SubaddressAt(5) || a.SubaddressAt(5) == a.SubaddressAt(6) {
		t.Fatal("not deterministic")
	}
	if New(2).Primary() == a.Primary() || New(2).SubaddressAt(5) == a.SubaddressAt(5) {
		t.Fatal("the seed does not change the addresses")
	}
	if !xmrrpc.ValidAddress(a.Primary(), xmrrpc.Mainnet, false) || a.Primary()[0] != '4' {
		t.Fatalf("primary %q", a.Primary())
	}
	if a.SubaddressAt(0) != a.Primary() {
		t.Fatal("index 0 is the primary address")
	}
	for i := uint32(1); i < 200; i++ {
		s := a.SubaddressAt(i)
		if !xmrrpc.ValidAddress(s, xmrrpc.Mainnet, true) || !xmrrpc.ValidAddress(s, xmrrpc.Regtest, true) || s[0] != '8' {
			t.Fatalf("subaddress %d %q", i, s)
		}
	}
}

func TestVersionAndHeight(t *testing.T) {
	w := New(1)
	v, err := w.GetVersion(bg)
	if err != nil || v>>16 != 1 || v&0xffff != 32 {
		t.Fatalf("version %d %v", v, err)
	}
	h, err := w.GetHeight(bg)
	if err != nil || h != 1 || w.Height() != 1 {
		t.Fatalf("height %d %v", h, err)
	}
	w.Mine(3)
	if h, _ := w.GetHeight(bg); h != 4 || w.Height() != 4 {
		t.Fatalf("height after Mine(3): %d", h)
	}
	w.SetHeight(2)
	if h, _ := w.GetHeight(bg); h != 2 {
		t.Fatalf("reported height %d", h)
	}
	if w.Height() != 4 {
		t.Fatal("SetHeight moved the chain")
	}
	w.SetHeight(-1)
	if h, _ := w.GetHeight(bg); h != 4 {
		t.Fatalf("SetHeight(-1) did not restore the chain's height: %d", h)
	}
}

func TestCreateGetForgetAndStore(t *testing.T) {
	w := New(1)
	a, err := w.GetAddress(bg, 0, nil)
	if err != nil || a.Address != w.Primary() || len(a.Addresses) != 1 || a.Addresses[0].Index != 0 {
		t.Fatalf("%+v %v", a, err)
	}
	for i := uint32(1); i <= 3; i++ {
		c, err := w.CreateAddress(bg, 0, "satbooth")
		if err != nil || c.Index != i || c.Address != w.SubaddressAt(i) {
			t.Fatalf("create %d: %+v %v", i, c, err)
		}
	}
	got, err := w.GetAddress(bg, 0, []uint32{3, 1})
	if err != nil || got.Address != w.Primary() || len(got.Addresses) != 2 ||
		got.Addresses[0].Address != w.SubaddressAt(3) || got.Addresses[1].Index != 1 || got.Addresses[0].Label != "satbooth" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := w.GetAddress(bg, 0, []uint32{4}); rpcCode(err) != -15 {
		t.Fatalf("index out of range: %v", err)
	}
	if err := w.Store(bg); err != nil || w.Calls("store") != 1 {
		t.Fatal(err)
	}
	w.Forget(2)
	if _, err := w.GetAddress(bg, 0, []uint32{2}); rpcCode(err) != -15 {
		t.Fatalf("forgotten index: %v", err)
	}
	if all, _ := w.GetAddress(bg, 0, nil); len(all.Addresses) != 2 {
		t.Fatalf("after Forget(2): %+v", all)
	}
	// The wallet derives the same subaddresses again.
	if c, _ := w.CreateAddress(bg, 0, "satbooth"); c.Index != 2 || c.Address != w.SubaddressAt(2) {
		t.Fatalf("re-created %+v", c)
	}
	w.Forget(100) // never below the primary address
	if c, _ := w.CreateAddress(bg, 0, ""); c.Index != 1 {
		t.Fatalf("after Forget(100): %+v", c)
	}
	// Another account: its base address is its index 0, a subaddress.
	b, err := w.GetAddress(bg, 1, nil)
	if err != nil || b.Address == w.Primary() || !xmrrpc.ValidAddress(b.Address, xmrrpc.Mainnet, true) {
		t.Fatalf("account 1: %+v %v", b, err)
	}
	if c, _ := w.CreateAddress(bg, 1, ""); c.Index != 1 || c.Address == w.SubaddressAt(1) {
		t.Fatalf("account 1 create: %+v", c)
	}
	if w.Calls("get_address") != 6 || w.Calls("create_address") != 6 {
		t.Fatalf("calls %d %d", w.Calls("get_address"), w.Calls("create_address"))
	}
}

func TestProbe(t *testing.T) {
	w := New(1)
	if c, err := w.SignTransferProbe(bg); err != nil || c != -29 {
		t.Fatalf("%d %v", c, err)
	}
	w.Full(true)
	if c, err := w.SignTransferProbe(bg); err != nil || c != -1 {
		t.Fatalf("full: %d %v", c, err)
	}
	w.Full(false)
	w.Fail("sign_transfer", &xmrrpc.Error{Code: -13, Method: "sign_transfer"})
	if c, err := w.SignTransferProbe(bg); err != nil || c != -13 {
		t.Fatalf("an RPC error is the code, as the client gives it: %d %v", c, err)
	}
	w.Fail("sign_transfer", xmrrpc.ErrTransport)
	if _, err := w.SignTransferProbe(bg); !errors.Is(err, xmrrpc.ErrTransport) {
		t.Fatal(err)
	}
	if w.Calls("sign_transfer") != 4 {
		t.Fatal(w.Calls("sign_transfer"))
	}
}

func TestFailIsOnceAndHangEndsWithTheContext(t *testing.T) {
	w := New(1)
	boom := errors.New("boom")
	for _, m := range []string{"get_version", "get_height", "get_address", "create_address", "store", "get_transfers", "get_transfer_by_txid"} {
		w.Fail(m, boom)
	}
	_, e1 := w.GetVersion(bg)
	_, e2 := w.GetHeight(bg)
	_, e3 := w.GetAddress(bg, 0, nil)
	_, e4 := w.CreateAddress(bg, 0, "")
	e5 := w.Store(bg)
	_, e6 := w.GetTransfers(bg, xmrrpc.TransfersQuery{In: true})
	_, e7 := w.GetTransferByTxID(bg, w.Pay(w.SubaddressAt(1), 1), 0)
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if !errors.Is(err, boom) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := w.GetVersion(bg); err != nil {
		t.Fatal("the failure is not once")
	}
	w.Hang("create_address")
	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	if _, err := w.CreateAddress(ctx, 0, ""); !errors.Is(err, xmrrpc.ErrTransport) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang: %v", err)
	}
	w.Unhang("create_address")
	if c, err := w.CreateAddress(bg, 0, ""); err != nil || c.Index != 1 {
		t.Fatalf("a hung call advanced the index: %+v %v", c, err)
	}
}

// in returns the "in" entries of the wallet's get_transfers.
func in(t *testing.T, w *Wallet, q xmrrpc.TransfersQuery) []xmrrpc.Transfer {
	t.Helper()
	q.In = true
	r, err := w.GetTransfers(bg, q)
	if err != nil {
		t.Fatal(err)
	}
	return r.In
}

func pool(t *testing.T, w *Wallet) []xmrrpc.Transfer {
	t.Helper()
	r, err := w.GetTransfers(bg, xmrrpc.TransfersQuery{Pool: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.Pool
}

func TestPayMineAndList(t *testing.T) {
	w := New(1)
	for range 3 {
		w.CreateAddress(bg, 0, "")
	}
	s1, s2 := w.SubaddressAt(1), w.SubaddressAt(2)
	tx := w.Pay(s1, 500)
	if len(tx) != 64 {
		t.Fatal(tx)
	}
	p := pool(t, w)
	if len(p) != 1 || p[0].TxID != tx || p[0].Type != "pool" || p[0].Amount != 500 || p[0].Height != 0 || !p[0].Locked ||
		p[0].Index != 1 || p[0].Account != 0 || p[0].Address != s1 {
		t.Fatalf("pool %+v", p)
	}
	if len(in(t, w, xmrrpc.TransfersQuery{})) != 0 {
		t.Fatal("a pool entry is listed in")
	}
	w.Mine(1) // the transaction is mined at height 1; the chain's height is 2
	l := in(t, w, xmrrpc.TransfersQuery{})
	if len(l) != 1 || l[0].Type != "in" || l[0].Height != 1 || !l[0].Locked || len(pool(t, w)) != 0 {
		t.Fatalf("in %+v", l)
	}
	w.Mine(9) // height 11: 1 + 10 > 11 is false
	if l := in(t, w, xmrrpc.TransfersQuery{}); l[0].Locked {
		t.Fatal("still locked at 10 confirmations")
	}
	// The height filter is exclusive at MinHeight, as wallet2::get_payments.
	if len(in(t, w, xmrrpc.TransfersQuery{FilterByHeight: true, MinHeight: 1})) != 0 ||
		len(in(t, w, xmrrpc.TransfersQuery{FilterByHeight: true, MinHeight: 0})) != 1 {
		t.Fatal("min height")
	}
	// One transaction to two subaddresses: two entries, one per subaddress.
	many := w.PayMany(map[string]int64{s1: 7, s2: 8})
	if got, err := w.GetTransferByTxID(bg, many, 0); err != nil || len(got) != 2 || got[0].Amount+got[1].Amount != 15 ||
		got[0].Address == got[1].Address || got[0].Type != "pool" {
		t.Fatalf("%+v %v", got, err)
	}
	// A payment to a foreign address is not the wallet's.
	foreign := New(9).SubaddressAt(1)
	ftx := w.Pay(foreign, 3)
	if got, _ := w.GetTransferByTxID(bg, ftx, 0); len(got) != 0 {
		t.Fatalf("foreign %+v", got)
	}
	if w.TxCalls(many) != 1 || w.TxCalls(ftx) != 1 || w.Calls("get_transfer_by_txid") != 2 {
		t.Fatal("tx calls")
	}
	// A payment to a subaddress the wallet has not created yet, within its lookahead, is found.
	ahead := w.Pay(w.SubaddressAt(50), 4)
	if got, _ := w.GetTransferByTxID(bg, ahead, 0); len(got) != 1 || got[0].Index != 50 {
		t.Fatalf("lookahead %+v", got)
	}
}

func TestLockedPayment(t *testing.T) {
	w := New(1)
	w.CreateAddress(bg, 0, "")
	tx := w.PayLocked(w.SubaddressAt(1), 9, 100)
	w.Mine(20)
	got, _ := w.GetTransferByTxID(bg, tx, 0)
	if len(got) != 1 || got[0].UnlockTime != 100 || !got[0].Locked {
		t.Fatalf("%+v", got)
	}
	w.Mine(80) // height 101
	if got, _ := w.GetTransferByTxID(bg, tx, 0); got[0].Locked {
		t.Fatal("still locked past its unlock height")
	}
	ts := w.PayLocked(w.SubaddressAt(1), 9, 1_900_000_000) // a timestamp form: the fake keeps it locked
	w.Mine(30)
	if got, _ := w.GetTransferByTxID(bg, ts, 0); !got[0].Locked {
		t.Fatal("a timestamp lock is unlocked")
	}
}

func TestDropDoubleSpendPopRebroadcast(t *testing.T) {
	w := New(1)
	w.CreateAddress(bg, 0, "")
	s := w.SubaddressAt(1)
	a := w.Pay(s, 1)
	w.Drop(a)
	if got, err := w.GetTransferByTxID(bg, a, 0); err != nil || got == nil || len(got) != 0 || len(pool(t, w)) != 0 {
		t.Fatalf("dropped: %+v %v", got, err)
	}
	w.Rebroadcast(a)
	if got, _ := w.GetTransferByTxID(bg, a, 0); len(got) != 1 || got[0].Type != "pool" {
		t.Fatalf("rebroadcast %+v", got)
	}
	w.DoubleSpend(a)
	if p := pool(t, w); len(p) != 1 || !p[0].DoubleSpendSeen {
		t.Fatalf("double spend %+v", p)
	}
	w.Mine(2) // a mined at height 1
	if got, _ := w.GetTransferByTxID(bg, a, 0); got[0].Type != "in" || got[0].Height != 1 || got[0].DoubleSpendSeen {
		t.Fatalf("mined %+v", got)
	}
	w.Pop(2) // back to the pool, height 1
	if got, _ := w.GetTransferByTxID(bg, a, 0); got[0].Type != "pool" || w.Height() != 1 {
		t.Fatalf("popped %+v %d", got, w.Height())
	}
	w.Mine(1)
	b := w.Pay(s, 2)
	w.Mine(1) // b at height 2
	w.PopDrop(1)
	if got, _ := w.GetTransferByTxID(bg, b, 0); len(got) != 0 || w.Height() != 2 {
		t.Fatalf("pop-dropped %+v", got)
	}
	if got, _ := w.GetTransferByTxID(bg, a, 0); len(got) != 1 || got[0].Type != "in" {
		t.Fatal("PopDrop(1) touched an older block")
	}
	if got, _ := w.GetTransferByTxID(bg, "00"+a[2:], 0); got == nil || len(got) != 0 {
		t.Fatal("an unknown txid is not an empty slice")
	}
}

func TestForgetPaymentsAndFailTx(t *testing.T) {
	w := New(1)
	w.CreateAddress(bg, 0, "")
	s := w.SubaddressAt(1)
	a := w.Pay(s, 1)
	w.Mine(1) // a at 1
	b := w.Pay(s, 2)
	w.Mine(1) // b at 2
	w.ForgetPayments(1)
	if l := in(t, w, xmrrpc.TransfersQuery{}); len(l) != 1 || l[0].TxID != a {
		t.Fatalf("%+v", l)
	}
	if got, _ := w.GetTransferByTxID(bg, b, 0); len(got) != 0 {
		t.Fatal("a forgotten payment is known by txid")
	}
	w.ForgetPayments(-1)
	if len(in(t, w, xmrrpc.TransfersQuery{})) != 2 {
		t.Fatal("ForgetPayments(-1) did not restore")
	}
	boom := errors.New("boom")
	w.FailTx(a, boom)
	for range 2 {
		if _, err := w.GetTransferByTxID(bg, a, 0); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	}
	if _, err := w.GetTransferByTxID(bg, b, 0); err != nil {
		t.Fatal("FailTx failed another txid")
	}
	w.FailTx(a, nil)
	if _, err := w.GetTransferByTxID(bg, a, 0); err != nil || w.TxCalls(a) != 3 {
		t.Fatal(err, w.TxCalls(a))
	}
}

func TestOtherAccountIsSeparate(t *testing.T) {
	w := New(1)
	w.CreateAddress(bg, 1, "")
	b, _ := w.GetAddress(bg, 1, []uint32{1})
	tx := w.Pay(b.Addresses[0].Address, 5)
	if got, _ := w.GetTransferByTxID(bg, tx, 0); len(got) != 0 {
		t.Fatal("account 0 sees account 1's payment")
	}
	if got, _ := w.GetTransferByTxID(bg, tx, 1); len(got) != 1 || got[0].Account != 1 || got[0].Index != 1 {
		t.Fatalf("%+v", got)
	}
	if r, _ := w.GetTransfers(bg, xmrrpc.TransfersQuery{Pool: true, Account: 0}); len(r.Pool) != 0 {
		t.Fatal("account filter")
	}
}

// MineEmpty adds blocks without the pool's transactions: with Pop, a reorg that replaces a block at the same height
// by one without the transaction, which goes back to the pool.
func TestMineEmpty(t *testing.T) {
	w := New(1)
	w.CreateAddress(bg, 0, "")
	a := w.Pay(w.SubaddressAt(1), 1)
	w.MineEmpty(2)
	if got, _ := w.GetTransferByTxID(bg, a, 0); len(got) != 1 || got[0].Type != "pool" || w.Height() != 3 {
		t.Fatalf("empty blocks mined the pool: %+v %d", got, w.Height())
	}
	w.Mine(1) // a at height 3
	w.Pop(1)
	w.MineEmpty(1)
	if got, _ := w.GetTransferByTxID(bg, a, 0); len(got) != 1 || got[0].Type != "pool" || w.Height() != 4 {
		t.Fatalf("after the reorg: %+v %d", got, w.Height())
	}
	w.MineEmpty(0)
	if w.Height() != 4 {
		t.Fatal("MineEmpty(0) added a block")
	}
}

// Each entry carries wallet-rpc's confirmations at the call (set_confirmations, wallet_rpc_server.cpp:163-168 at
// v0.18.5.4): 0 in the pool, the wallet's height less the entry's when mined, and lower after the wallet's chain
// shortens.
func TestConfirmationsFollowTheWalletHeight(t *testing.T) {
	w := New(1)
	w.CreateAddress(bg, 0, "")
	w.Mine(10)
	tx := w.Pay(w.SubaddressAt(1), 500)
	conf := func() int64 {
		t.Helper()
		l, err := w.GetTransferByTxID(bg, tx, 0)
		if err != nil || len(l) != 1 {
			t.Fatalf("%+v %v", l, err)
		}
		return l[0].Confirmations
	}
	if c := conf(); c != 0 {
		t.Fatalf("pool: %d confirmations", c)
	}
	w.Mine(1)
	if c := conf(); c != 1 {
		t.Fatalf("mined in the last block: %d confirmations, want 1", c)
	}
	w.Mine(4)
	if c := conf(); c != 5 {
		t.Fatalf("4 blocks later: %d, want 5", c)
	}
	w.SetHeight(w.Height() - 3)
	if c := conf(); c != 2 {
		t.Fatalf("after the wallet's height went down by 3: %d, want 2", c)
	}
}
