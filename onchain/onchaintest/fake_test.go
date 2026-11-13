package onchaintest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/btcrpc"
)

var bg = context.Background()

func TestAddressesAreDeterministicAndBech32(t *testing.T) {
	a, b := New("d#x", 1), New("d#x", 1)
	if a.AddressAt(5) != b.AddressAt(5) || a.AddressAt(5) == a.AddressAt(6) || New("d#x", 2).AddressAt(5) == a.AddressAt(5) {
		t.Fatal("not deterministic")
	}
	addr := a.AddressAt(0)
	if !strings.HasPrefix(addr, "bcrt1q") || len(addr) != 42 {
		t.Fatal(addr)
	}
	for _, c := range addr[6:] {
		if !strings.ContainsRune(bech32Charset, c) {
			t.Fatalf("bad character %q", c)
		}
	}
}

func TestGetNewAddressAdvancesAndRefusesTheWrongType(t *testing.T) {
	n := New("d#x", 1)
	if _, err := n.GetNewAddress(bg, "bech32m"); err == nil {
		t.Fatal("wrong type served")
	}
	a, _ := n.GetNewAddress(bg, "bech32")
	b, _ := n.GetNewAddress(bg, "bech32")
	if a != n.AddressAt(0) || b != n.AddressAt(1) {
		t.Fatal("order")
	}
	i, _ := n.GetAddressInfo(bg, b)
	if !i.IsMine || i.HDKeyPath != "m/0/1" || i.ParentDesc != "d#x" {
		t.Fatalf("%+v", i)
	}
	if x, _ := n.GetAddressInfo(bg, "bcrt1qnotmine"); x.IsMine {
		t.Fatal("foreign address is mine")
	}
	if l, err := n.DeriveAddresses(bg, "d#x", 1); err != nil || len(l) != 1 || l[0] != b {
		t.Fatal(l, err)
	}
}

func TestFailHangAndCalls(t *testing.T) {
	n := New("d#x", 1)
	n.Fail("getwalletinfo", errors.New("boom"))
	if _, err := n.GetWalletInfo(bg); err == nil {
		t.Fatal("no failure")
	}
	if _, err := n.GetWalletInfo(bg); err != nil {
		t.Fatal("the failure is not once")
	}
	n.Hang("getblockchaininfo")
	ctx, cancel := context.WithTimeout(bg, 30*time.Millisecond)
	defer cancel()
	if _, err := n.GetBlockchainInfo(ctx); !errors.Is(err, btcrpc.ErrTransport) {
		t.Fatalf("hang: %v", err)
	}
	if n.Calls("getwalletinfo") != 2 || n.Calls("getblockchaininfo") != 1 {
		t.Fatal("calls")
	}
}

func paid(t *testing.T, n *Node) (addr, txid string) {
	t.Helper()
	addr, _ = n.GetNewAddress(bg, "bech32")
	return addr, n.Pay(addr, 5000)
}

func TestMempoolMinedAndListSinceBlock(t *testing.T) {
	n := New("d#x", 1)
	n.Mine(2)
	base := n.Tip()
	addr, txid := paid(t, n)
	sb, _ := n.ListSinceBlock(bg, base, 1)
	if len(sb.Transactions) != 1 || sb.Transactions[0].Confirmations != 0 || sb.Transactions[0].Address != addr {
		t.Fatalf("%+v", sb)
	}
	if ok, _ := n.InMempool(bg, txid); !ok {
		t.Fatal("not in mempool")
	}
	n.Mine(1)
	tx, _ := n.GetTransaction(bg, txid)
	if tx.Confirmations != 1 || tx.BlockHash == "" || tx.BlockHeight != 3 {
		t.Fatalf("%+v", tx)
	}
	if ok, _ := n.InMempool(bg, txid); ok {
		t.Fatal("mined tx in mempool")
	}
	sb, _ = n.ListSinceBlock(bg, base, 1)
	if len(sb.Transactions) != 1 || sb.Transactions[0].Confirmations != 1 {
		t.Fatalf("%+v", sb)
	}
	sb, _ = n.ListSinceBlock(bg, n.Tip(), 1)
	if len(sb.Transactions) != 0 {
		t.Fatalf("above the tip: %+v", sb)
	}
	if _, err := n.ListSinceBlock(bg, strings.Repeat("0", 64), 1); err == nil {
		t.Fatal("unknown block accepted")
	}
}

func TestReorgReturnsToMempoolWithRemoved(t *testing.T) {
	n := New("d#x", 1)
	n.Mine(1)
	_, txid := paid(t, n)
	n.Mine(1)
	old := n.Tip()
	n.Invalidate(1)
	tx, _ := n.GetTransaction(bg, txid)
	if tx.Confirmations != 0 || tx.BlockHash != "" {
		t.Fatalf("%+v", tx)
	}
	if ok, _ := n.InMempool(bg, txid); !ok {
		t.Fatal("not back in the mempool")
	}
	sb, err := n.ListSinceBlock(bg, old, 1)
	if err != nil || len(sb.Removed) != 1 || len(sb.Transactions) != 1 || sb.Transactions[0].Confirmations != 0 {
		t.Fatalf("%+v %v", sb, err)
	}
}

func TestReplaceKeepAndDrop(t *testing.T) {
	for _, keep := range []bool{true, false} {
		n := New("d#x", 1)
		_, a := paid(t, n)
		b := n.Replace(a, keep)
		ta, _ := n.GetTransaction(bg, a)
		if ta.Confirmations != 0 || len(ta.MempoolConflicts) != 1 || ta.MempoolConflicts[0] != b {
			t.Fatalf("keep=%v replaced one: %+v", keep, ta)
		}
		if ok, _ := n.InMempool(bg, a); ok {
			t.Fatal("replaced tx in the mempool")
		}
		n.Mine(1)
		ta, _ = n.GetTransaction(bg, a)
		if ta.Confirmations >= 0 || len(ta.MempoolConflicts) != 0 || ta.BlockHash != "" {
			t.Fatalf("keep=%v conflicted one: %+v", keep, ta)
		}
		sb, _ := n.ListSinceBlock(bg, "", 1)
		want := 0
		if keep {
			want = 1
		}
		if len(sb.Transactions) != want {
			t.Fatalf("keep=%v: %d entries", keep, len(sb.Transactions))
		}
	}
}

func TestEvict(t *testing.T) {
	n := New("d#x", 1)
	_, a := paid(t, n)
	n.Evict(a)
	ta, _ := n.GetTransaction(bg, a)
	if ok, _ := n.InMempool(bg, a); ok || ta.Confirmations != 0 || len(ta.MempoolConflicts) != 0 {
		t.Fatalf("%+v", ta)
	}
}

// listsinceblock's lastblock is target_confirmations - 1 blocks below the tip, at least the genesis,
// and a target below 1 is error -8.
func TestListSinceBlockTargetConfirmations(t *testing.T) {
	n := New("d#x", 1)
	n.Mine(10)
	for _, c := range []struct {
		target int
		height int64
	}{{1, 10}, {2, 9}, {7, 4}, {11, 0}, {12, 0}, {1000, 0}} {
		sb, err := n.ListSinceBlock(bg, "", c.target)
		if err != nil || sb.LastBlock != n.BlockHash(c.height) {
			t.Errorf("target %d: lastblock %s (%v), want height %d", c.target, sb.LastBlock, err, c.height)
		}
	}
	var e *btcrpc.Error
	if _, err := n.ListSinceBlock(bg, "", 0); !errors.As(err, &e) || e.Code != -8 {
		t.Fatalf("target 0: %v", err)
	}
	// Fed back, the lastblock re-lists what has fewer than target confirmations.
	_, txid := paid(t, n)
	n.Mine(3) // 3 confirmations
	sb, _ := n.ListSinceBlock(bg, "", 3)
	if again, _ := n.ListSinceBlock(bg, sb.LastBlock, 3); len(again.Transactions) != 0 {
		t.Fatalf("3 confirmations re-listed with target 3: %+v", again)
	}
	sb, _ = n.ListSinceBlock(bg, "", 4)
	if again, _ := n.ListSinceBlock(bg, sb.LastBlock, 4); len(again.Transactions) != 1 || again.Transactions[0].TxID != txid {
		t.Fatalf("3 confirmations not re-listed with target 4: %+v", again)
	}
}

// Break fails every call of a method until Mend; FailTx and HangTx act on one txid only; TxCalls counts per txid.
func TestBreakFailTxHangTx(t *testing.T) {
	n := New("d#x", 1)
	boom := errors.New("boom")
	n.Break("getblockchaininfo", boom)
	for i := 0; i < 3; i++ {
		if _, err := n.GetBlockchainInfo(bg); !errors.Is(err, boom) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	n.Mend("getblockchaininfo")
	if _, err := n.GetBlockchainInfo(bg); err != nil {
		t.Fatal(err)
	}
	_, a := paid(t, n)
	_, b := paid(t, n)
	n.FailTx(a, &btcrpc.Error{Code: -5, Method: "gettransaction"})
	for i := 0; i < 2; i++ {
		if _, err := n.GetTransaction(bg, a); err == nil {
			t.Fatal("no failure")
		}
		if _, err := n.InMempool(bg, a); err == nil {
			t.Fatal("no failure for getmempoolentry")
		}
	}
	if _, err := n.GetTransaction(bg, b); err != nil {
		t.Fatal(err)
	}
	n.MendTx(a)
	if _, err := n.GetTransaction(bg, a); err != nil {
		t.Fatal(err)
	}
	if n.TxCalls(a) != 3 || n.TxCalls(b) != 1 {
		t.Fatalf("calls %d %d", n.TxCalls(a), n.TxCalls(b))
	}
	n.HangTx(b)
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := n.GetTransaction(ctx, b); !errors.Is(err, btcrpc.ErrTransport) || time.Since(start) < 40*time.Millisecond {
		t.Fatalf("hang: %v after %v", err, time.Since(start))
	}
	if _, err := n.GetTransaction(bg, a); err != nil {
		t.Fatal("a hang on b stopped a")
	}
	n.UnhangTx(b)
	if _, err := n.GetTransaction(bg, b); err != nil {
		t.Fatal(err)
	}
}

// Rebroadcast puts an evicted, unmined, unconflicted transaction back in the mempool, and nothing else.
func TestRebroadcast(t *testing.T) {
	n := New("d#x", 1)
	_, txid := paid(t, n)
	n.Evict(txid)
	if ok, _ := n.InMempool(bg, txid); ok {
		t.Fatal("evicted")
	}
	n.Rebroadcast(txid)
	if ok, _ := n.InMempool(bg, txid); !ok {
		t.Fatal("not rebroadcast")
	}
	n.Mine(1)
	n.Rebroadcast(txid) // mined: no change
	if ok, _ := n.InMempool(bg, txid); ok {
		t.Fatal("a mined transaction went back to the mempool")
	}
	_, a := paid(t, n)
	n.Replace(a, false)
	n.Mine(1) // a is conflicted
	n.Rebroadcast(a)
	if ok, _ := n.InMempool(bg, a); ok {
		t.Fatal("a conflicted transaction went back to the mempool")
	}
}

// While the mempool is loading (MempoolLoading), getmempoolinfo answers loaded false and getmempoolentry finds
// nothing, as Core does before its initload thread has loaded mempool.dat; the transactions are still there after.
func TestMempoolLoading(t *testing.T) {
	n := New("d#x", 1)
	txid := n.Pay(n.AddressAt(0), 1000)
	if ok, err := n.MempoolLoaded(bg); !ok || err != nil {
		t.Fatalf("loaded %v %v", ok, err)
	}
	n.Update(func(s *State) { s.MempoolLoading = true })
	if ok, err := n.MempoolLoaded(bg); ok || err != nil {
		t.Fatalf("loading: loaded %v %v", ok, err)
	}
	if ok, err := n.InMempool(bg, txid); ok || err != nil {
		t.Fatalf("loading: in the mempool %v %v", ok, err)
	}
	n.Update(func(s *State) { s.MempoolLoading = false })
	if ok, _ := n.InMempool(bg, txid); !ok {
		t.Fatal("lost after the load")
	}
	n.Break("getmempoolinfo", errors.New("down"))
	if _, err := n.MempoolLoaded(bg); err == nil || n.Calls("getmempoolinfo") != 3 {
		t.Fatalf("Break: %v, %d calls", err, n.Calls("getmempoolinfo"))
	}
}

// entriesOf is the listsinceblock entries of txid, from the genesis.
func entriesOf(t *testing.T, n *Node, txid string) []btcrpc.WalletTx {
	t.Helper()
	sb, err := n.ListSinceBlock(bg, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	var l []btcrpc.WalletTx
	for _, e := range sb.Transactions {
		if e.TxID == txid {
			l = append(l, e)
		}
	}
	return l
}

func sameList(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// walletconflicts lists every other wallet transaction that spends the same input (wallet.cpp GetConflicts), both
// ways and through a chain of replacements; a replacement to a foreign address is not a wallet transaction, so it is
// not listed. A mined replacement conflicts every other transaction of its input, and those drop out of the listing.
func TestWalletConflictsBothWays(t *testing.T) {
	n := New("d#x", 1)
	_, lone := paid(t, n)
	_, a := paid(t, n)
	b := n.Replace(a, true)
	if l := entriesOf(t, n, lone); len(l) != 1 || len(l[0].WalletConflicts) != 0 {
		t.Fatalf("a lone payment: %+v", l)
	}
	if l := entriesOf(t, n, a); len(l) != 1 || !sameList(l[0].WalletConflicts, []string{b}) {
		t.Fatalf("the replaced one: %+v", l)
	}
	if l := entriesOf(t, n, b); len(l) != 1 || !sameList(l[0].WalletConflicts, []string{a}) {
		t.Fatalf("the replacement: %+v", l)
	}
	c := n.Replace(b, true)
	for _, w := range []struct {
		id   string
		want []string
	}{{a, []string{b, c}}, {b, []string{a, c}}, {c, []string{a, b}}} {
		if l := entriesOf(t, n, w.id); len(l) != 1 || !sameList(l[0].WalletConflicts, w.want) {
			t.Fatalf("chain %s: %+v", w.id[:8], l)
		}
	}
	if ta, _ := n.GetTransaction(bg, a); len(ta.MempoolConflicts) != 1 || ta.MempoolConflicts[0] != c {
		t.Fatalf("the first of a chain: mempoolconflicts %+v", ta.MempoolConflicts)
	}
	n.Mine(1)
	for _, id := range []string{a, b} {
		if tx, _ := n.GetTransaction(bg, id); tx.Confirmations >= 0 || len(entriesOf(t, n, id)) != 0 {
			t.Fatalf("%s is not conflicted by the mined replacement: %+v", id[:8], tx)
		}
	}
	if l := entriesOf(t, n, c); len(l) != 1 || l[0].Confirmations != 1 || !sameList(l[0].WalletConflicts, []string{a, b}) {
		t.Fatalf("the mined replacement: %+v", l)
	}
	_, d := paid(t, n)
	n.Replace(d, false)
	if l := entriesOf(t, n, d); len(l) != 1 || len(l[0].WalletConflicts) != 0 {
		t.Fatalf("replaced to a foreign address: %+v", l)
	}
}

// ReplaceWith pays the given outputs from the same input: each wallet output is an entry with the replaced txid in
// walletconflicts, a foreign one is not listed, and the replaced transaction leaves the mempool. Only a mempool
// transaction can be replaced.
func TestReplaceWithTwoOutputs(t *testing.T) {
	n := New("d#x", 1)
	addr, a := paid(t, n)
	other, _ := n.GetNewAddress(bg, "bech32")
	b := n.ReplaceWith(a, []Out{{addr, 6000}, {other, 100}, {"bcrt1qforeign", 50}})
	if b == "" || b == a {
		t.Fatalf("ReplaceWith: %q", b)
	}
	tx, _ := n.GetTransaction(bg, b)
	if len(tx.Outputs) != 3 || tx.Outputs[0].Address != addr || tx.Outputs[0].ValueSat != 6000 ||
		tx.Outputs[1].Address != other || tx.Outputs[1].ValueSat != 100 || tx.Outputs[2].Vout != 2 {
		t.Fatalf("outputs %+v", tx.Outputs)
	}
	l := entriesOf(t, n, b)
	if len(l) != 2 || l[0].Vout != 0 || l[0].AmountSat != 6000 || l[1].Vout != 1 || l[1].Address != other {
		t.Fatalf("entries %+v", l)
	}
	for _, e := range l {
		if !sameList(e.WalletConflicts, []string{a}) {
			t.Fatalf("walletconflicts %+v", e)
		}
	}
	if ok, _ := n.InMempool(bg, a); ok {
		t.Fatal("the replaced one is in the mempool")
	}
	if ta, _ := n.GetTransaction(bg, a); len(ta.MempoolConflicts) != 1 || ta.MempoolConflicts[0] != b {
		t.Fatalf("mempoolconflicts %+v", ta.MempoolConflicts)
	}
	if n.ReplaceWith(a, []Out{{addr, 1}}) != "" || n.ReplaceWith(strings.Repeat("0", 64), []Out{{addr, 1}}) != "" {
		t.Fatal("replaced a transaction that is not in the mempool")
	}
	n.Mine(1)
	if n.ReplaceWith(b, []Out{{addr, 1}}) != "" {
		t.Fatal("replaced a mined transaction")
	}
}

// A transaction may spend several inputs; Spend pays from any of them. Two transactions conflict when they share an
// input: A spending i1 and i2, replaced by B spending i1 and C spending i2, lists both in its walletconflicts, while
// B and C do not conflict and are both mined. A mined input cannot be spent again, and a transaction whose input a
// mempool transaction spends is not rebroadcast.
func TestMultiInputConflicts(t *testing.T) {
	n := New("d#x", 1)
	addr, _ := n.GetNewAddress(bg, "bech32")
	a := n.PayWith(2, []Out{{addr, 100_000}})
	in := n.Inputs(a)
	if len(in) != 2 || in[0] == in[1] || len(n.Inputs(n.Pay(addr, 1))) != 1 {
		t.Fatalf("inputs %v", in)
	}
	b := n.Spend(in[:1], []Out{{addr, 100_000}})
	if ok, _ := n.InMempool(bg, a); ok || b == "" {
		t.Fatal("A is still in the mempool, or B was refused")
	}
	n.Rebroadcast(a)
	if ok, _ := n.InMempool(bg, a); ok {
		t.Fatal("A was rebroadcast while B spends its input")
	}
	c := n.Spend(in[1:], []Out{{addr, 100_000}})
	if c == "" {
		t.Fatal("C was refused")
	}
	for _, w := range []struct {
		id   string
		want []string
	}{{a, []string{b, c}}, {b, []string{a}}, {c, []string{a}}} {
		if l := entriesOf(t, n, w.id); len(l) != 1 || !sameList(l[0].WalletConflicts, w.want) {
			t.Fatalf("%s: %+v", w.id[:8], l)
		}
	}
	if ta, _ := n.GetTransaction(bg, a); !sameList(ta.MempoolConflicts, []string{b, c}) {
		t.Fatalf("A's mempoolconflicts %v", ta.MempoolConflicts)
	}
	n.Mine(1)
	for _, id := range []string{b, c} {
		if tx, _ := n.GetTransaction(bg, id); tx.Confirmations != 1 {
			t.Fatalf("%s not mined: %+v", id[:8], tx)
		}
	}
	if tx, _ := n.GetTransaction(bg, a); tx.Confirmations >= 0 {
		t.Fatalf("A not conflicted: %+v", tx)
	}
	if n.Spend(in[:1], []Out{{addr, 5}}) != "" || n.Spend(nil, []Out{{addr, 5}}) != "" {
		t.Fatal("spent a mined input, or no input")
	}
}

func TestSpentInMempool(t *testing.T) {
	n := New("d#x", 1)
	a := n.AddressAt(0)
	tx := n.Pay(a, 1000)
	ins := n.Inputs(tx)
	if len(ins) != 1 {
		t.Fatal(ins)
	}
	got, err := n.GetTransaction(bg, tx)
	if err != nil || len(got.Inputs) != 1 {
		t.Fatalf("%+v, %v", got, err)
	}
	asked := []btcrpc.Outpoint{got.Inputs[0], {TxID: strings.Repeat("0", 64)}}
	sp, err := n.SpentInMempool(bg, asked)
	if err != nil || len(sp) != 2 || sp[0] != tx || sp[1] != "" {
		t.Fatalf("SpentInMempool = %q, %v", sp, err)
	}
	// replaced: the replacement spends it; evicted: nothing; back: the transaction again
	rep := n.Replace(tx, true)
	if sp, _ = n.SpentInMempool(bg, asked[:1]); sp[0] != rep {
		t.Fatalf("after the replacement: %q, want %s", sp, rep)
	}
	n.Evict(rep)
	if sp, _ = n.SpentInMempool(bg, asked[:1]); sp[0] != "" {
		t.Fatalf("after the eviction: %q", sp)
	}
	n.Rebroadcast(tx)
	if sp, _ = n.SpentInMempool(bg, asked[:1]); sp[0] != tx {
		t.Fatalf("after the rebroadcast: %q", sp)
	}
	n.Mine(1) // mined: not in the mempool any more
	if sp, _ = n.SpentInMempool(bg, asked[:1]); sp[0] != "" {
		t.Fatalf("after the block: %q", sp)
	}
	if _, err := n.SpentInMempool(bg, nil); !errors.Is(err, btcrpc.ErrArgument) {
		t.Fatalf("empty list: %v", err)
	}
	if n.Calls("gettxspendingprevout") != 5 {
		t.Fatalf("%d calls", n.Calls("gettxspendingprevout"))
	}
	n.Fail("gettxspendingprevout", errors.New("boom"))
	if _, err := n.SpentInMempool(bg, asked[:1]); err == nil {
		t.Fatal("Fail ignored")
	}
}

func TestHoldWalletFreezesTheWalletViewsOnly(t *testing.T) {
	n := New("d#x", 1)
	if _, err := n.GetNewAddress(bg, "bech32"); err != nil {
		t.Fatal(err)
	}
	n.Update(func(s *State) { s.NextIndex = 5 })
	a := n.AddressAt(0)
	orig := n.Pay(a, 1000)
	n.HoldWallet()
	rep := n.Replace(orig, true)
	if rep == "" {
		t.Fatal("no replacement")
	}

	// wallet views: as at HoldWallet
	tx, err := n.GetTransaction(bg, orig)
	if err != nil || tx.Confirmations != 0 || len(tx.MempoolConflicts) != 0 || len(tx.Inputs) != 1 {
		t.Fatalf("held gettransaction = %+v, %v", tx, err)
	}
	if _, err := n.GetTransaction(bg, rep); !btcrpcCode(err, -5) {
		t.Fatalf("the wallet knows the replacement: %v", err)
	}
	sb, err := n.ListSinceBlock(bg, "", 1)
	if err != nil || len(sb.Transactions) != 1 || sb.Transactions[0].TxID != orig || len(sb.Transactions[0].MempoolConflicts) != 0 ||
		len(sb.Transactions[0].WalletConflicts) != 0 {
		t.Fatalf("held listsinceblock = %+v, %v", sb, err)
	}
	// node views: live
	if in, _ := n.InMempool(bg, orig); in {
		t.Fatal("the node still has the original")
	}
	if in, _ := n.InMempool(bg, rep); !in {
		t.Fatal("the node lacks the replacement")
	}
	if sp, _ := n.SpentInMempool(bg, tx.Inputs); sp[0] != rep {
		t.Fatalf("spender %q", sp)
	}
	if l, _ := n.MempoolLoaded(bg); !l {
		t.Fatal("loaded")
	}
	if _, err := n.GetBlockchainInfo(bg); err != nil {
		t.Fatal(err)
	}
	// holding again keeps the first freeze
	n.HoldWallet()
	if tx, _ = n.GetTransaction(bg, orig); len(tx.MempoolConflicts) != 0 {
		t.Fatalf("second hold re-froze: %+v", tx)
	}

	// release: live
	n.ReleaseWallet()
	tx, err = n.GetTransaction(bg, orig)
	if err != nil || len(tx.MempoolConflicts) != 1 || tx.MempoolConflicts[0] != rep {
		t.Fatalf("released gettransaction = %+v, %v", tx, err)
	}
	if sb, _ = n.ListSinceBlock(bg, "", 1); len(sb.Transactions) != 2 {
		t.Fatalf("released listsinceblock = %+v", sb)
	}
	// the live state was not changed by the swaps
	if in, _ := n.InMempool(bg, rep); !in {
		t.Fatal("live state lost")
	}
}

func TestMineAndInvalidateReleaseTheWallet(t *testing.T) {
	for name, release := range map[string]func(*Node){"Mine": func(n *Node) { n.Mine(1) }, "Invalidate": func(n *Node) { n.Mine(1); n.HoldWallet(); n.Invalidate(1) }} {
		n := New("d#x", 1)
		n.Update(func(s *State) { s.NextIndex = 5 })
		orig := n.Pay(n.AddressAt(0), 1000)
		n.HoldWallet()
		rep := n.Replace(orig, true)
		release(n)
		tx, err := n.GetTransaction(bg, rep)
		if err != nil {
			t.Fatalf("%s: the wallet still does not know the replacement: %v", name, err)
		}
		_ = tx
	}
}

func btcrpcCode(err error, code int) bool {
	var e *btcrpc.Error
	return errors.As(err, &e) && e.Code == code
}
