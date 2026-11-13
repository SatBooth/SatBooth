package onchain

import (
	"crypto/sha256"
	"math/rand"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/ledger"
)

func TestTransferIDsAreDistinctAndRefuseBadTxids(t *testing.T) {
	tx := strings.Repeat("ab", 32)
	ids := map[ledger.U128]string{}
	for name, id := range map[string]ledger.U128{
		"paid 0,0": PaidTransferID(tx, 0, 0), "paid 1,0": PaidTransferID(tx, 1, 0), "paid 0,1": PaidTransferID(tx, 0, 1),
		"rev 0,0": ReversalTransferID(tx, 0, 0), "rev 1,0": ReversalTransferID(tx, 1, 0), "rev 0,1": ReversalTransferID(tx, 0, 1),
	} {
		if id.IsZero() {
			t.Fatalf("%s: zero id", name)
		}
		if other, dup := ids[id]; dup {
			t.Fatalf("%s equals %s", name, other)
		}
		ids[id] = name
	}
	for _, bad := range []string{"", "ab", strings.Repeat("ab", 31), strings.Repeat("ab", 33), strings.Repeat("zz", 32), tx + " "} {
		if !PaidTransferID(bad, 0, 0).IsZero() || !ReversalTransferID(bad, 0, 0).IsZero() {
			t.Errorf("%q gave a nonzero id", bad)
		}
	}
}

// A change of the domain strings or the layout would orphan every recorded transfer: known answers for the txid
// bytes 0 to 31, vout 7, epoch 3, written out here and computed independently of the code.
func TestTransferIDKnownAnswers(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	txid := hexOf(raw)
	if txid != "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" {
		t.Fatal("test txid")
	}
	for _, c := range []struct {
		name   string
		got    ledger.U128
		domain string
		hex    string
	}{
		{"paid", PaidTransferID(txid, 7, 3), "satbooth/onchain-paid/v1", "7223f1b6400391f258c02c186682637f"},
		{"reversal", ReversalTransferID(txid, 7, 3), "satbooth/onchain-reversed/v1", "d9ba82863b22d8e1d71481edb11711c0"},
	} {
		in := append([]byte(c.domain), raw...)
		in = append(in, 0, 0, 0, 7, 0, 0, 0, 3) // vout and epoch, 4 bytes big-endian each
		sum := sha256.Sum256(in)
		b := c.got.Bytes()
		if hexOf(b[:]) != hexOf(sum[:16]) {
			t.Errorf("%s: %x, sha256 of the concatenation gives %x", c.name, b, sum[:16])
		}
		if hexOf(b[:]) != c.hex {
			t.Errorf("%s: %x, want literal %s", c.name, b, c.hex)
		}
	}
}

func FuzzTransferIDs(f *testing.F) {
	f.Add(strings.Repeat("ab", 32), uint32(0), uint32(0))
	f.Add("", uint32(1<<32-1), uint32(1<<32-1))
	f.Fuzz(func(t *testing.T, txid string, vout, ep uint32) {
		p, r := PaidTransferID(txid, vout, ep), ReversalTransferID(txid, vout, ep)
		if len(txid) != 64 {
			if !p.IsZero() || !r.IsZero() {
				t.Fatal("nonzero for a bad txid")
			}
			return
		}
		if !p.IsZero() && p == r {
			t.Fatal("paid and reversal ids collide")
		}
	})
}

func TestTransferIDsSeeded(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	seen := map[ledger.U128]bool{}
	for i := 0; i < 2000; i++ {
		var b [32]byte
		r.Read(b[:])
		id := PaidTransferID(hexOf(b[:]), r.Uint32(), r.Uint32())
		if id.IsZero() || seen[id] {
			t.Fatal("zero or duplicate")
		}
		seen[id] = true
	}
}

func hexOf(b []byte) string {
	const h = "0123456789abcdef"
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(h[c>>4])
		sb.WriteByte(h[c&15])
	}
	return sb.String()
}

func TestAccountsAreCreatedAndNewIsIdempotent(t *testing.T) {
	e := newEnv(t)
	for id, want := range map[ledger.U128]struct {
		code  uint16
		flags ledger.AccountFlags
	}{
		OnchainWallet:   {3, ledger.AccountCreditsMustNotExceedDebits},
		MerchantOnchain: {4, ledger.AccountDebitsMustNotExceedCredits},
	} {
		a, ok, err := e.lg.LookupAccount(bg, id)
		if err != nil || !ok || a.Ledger != 2 || a.Code != want.code || a.Flags != want.flags {
			t.Fatalf("%v: %+v %v %v", id, a, ok, err)
		}
	}
	if OnchainWallet != ledger.U128From64(0x4f43_0001) || MerchantOnchain != ledger.U128From64(0x4f43_0002) {
		t.Fatal("account ids")
	}
	e.newService() // a restart creates nothing new and fails nothing
}

func TestPaidOnchainWalksEpochs(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	a, _ := e.svc.Address(bg, o.ID)
	if ok, err := e.svc.paidOnchain(bg, o); err != nil || ok {
		t.Fatalf("unpaid: %v %v", ok, err)
	}
	e.recordPaid(o, a, 999)
	if ok, _ := e.svc.paidOnchain(bg, o); ok {
		t.Fatal("999 of 1000 reads paid")
	}
	// A second output tops it up.
	tx2 := strings.Repeat("ef", 32)
	if _, err := e.st.RecordScan(bg, []storeTXO{{TxID: tx2, Vout: 1, Address: a.Address, AmountSat: 1}}, e.cursor()); err != nil {
		t.Fatal(err)
	}
	e.forward(o, tx2, 1, 0, 1)
	if sum, err := e.svc.confirmedSum(bg, o); err != nil || sum != 1000 {
		t.Fatalf("sum %d %v", sum, err)
	}
	if ok, _ := e.svc.paidOnchain(bg, o); !ok {
		t.Fatal("1000 of 1000 reads unpaid")
	}
	// Reversing epoch 0 of the second output takes it out; a forward at epoch 1 puts it back.
	e.reverse(o, tx2, 1, 0, 1)
	if ok, _ := e.svc.paidOnchain(bg, o); ok {
		t.Fatal("a reversed output counts")
	}
	e.forward(o, tx2, 1, 1, 1)
	if ok, _ := e.svc.paidOnchain(bg, o); !ok {
		t.Fatal("epoch 1 forward not counted")
	}
}
