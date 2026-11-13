package monero

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/monero/monerotest"
	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// refID is the specification of the transfer ids, written out again: the first 16 bytes of SHA-256(domain || the 32
// raw txid bytes || index, 4 bytes big-endian || epoch, 4 bytes big-endian).
func refID(domain string, raw []byte, index, epoch uint32) ledger.U128 {
	var n [8]byte
	binary.BigEndian.PutUint32(n[:4], index)
	binary.BigEndian.PutUint32(n[4:], epoch)
	sum := sha256.Sum256(append(append([]byte(domain), raw...), n[:]...))
	id, err := ledger.U128FromBytes(sum[:16])
	if err != nil {
		panic(err)
	}
	return id
}

func TestTransferIDsAreDistinctAndFollowTheSpecification(t *testing.T) {
	txid := strings.Repeat("3c", 32)
	raw, _ := hex.DecodeString(txid)
	ids := map[ledger.U128]string{}
	for _, c := range []struct{ index, epoch uint32 }{{0, 0}, {1, 0}, {0, 1}} {
		p, r := PaidTransferID(txid, c.index, c.epoch), ReversalTransferID(txid, c.index, c.epoch)
		if p != refID("satbooth/monero-paid/v1", raw, c.index, c.epoch) || r != refID("satbooth/monero-reversed/v1", raw, c.index, c.epoch) {
			t.Fatalf("%+v: not the specification", c)
		}
		for _, id := range []ledger.U128{p, r} {
			if id.IsZero() {
				t.Fatal("zero id")
			}
			if prev, dup := ids[id]; dup {
				t.Fatalf("%+v collides with %s", c, prev)
			}
			ids[id] = txid
		}
	}
	// The on-chain domain gives another id for the same bytes.
	if PaidTransferID(txid, 0, 0) == refID("satbooth/onchain-paid/v1", raw, 0, 0) {
		t.Fatal("the Monero id equals the on-chain one")
	}
}

func TestTransferIDBadTxidIsZero(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("a", 66),
		strings.Repeat("g", 64), strings.Repeat("a", 62) + " a", strings.Repeat("a", 62) + "\x00a"} {
		if !PaidTransferID(bad, 1, 0).IsZero() || !ReversalTransferID(bad, 1, 0).IsZero() {
			t.Fatalf("%q gives an id", bad)
		}
	}
}

// The seeded property: over random txids and indexes and epochs at their boundaries, every id is the specification,
// no two inputs share an id, and the paid and reversal ids never meet.
func TestTransferIDsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	edges := []uint32{0, 1, 2, 1<<16 - 1, 1 << 16, 1<<31 - 1, 1 << 31, 1<<32 - 1}
	pick := func() uint32 {
		if rng.Intn(2) == 0 {
			return edges[rng.Intn(len(edges))]
		}
		return rng.Uint32()
	}
	seen := map[ledger.U128]bool{}
	for i := 0; i < 3000; i++ {
		raw := make([]byte, 32)
		rng.Read(raw)
		txid := hex.EncodeToString(raw)
		idx, ep := pick(), pick()
		p, r := PaidTransferID(txid, idx, ep), ReversalTransferID(txid, idx, ep)
		if p != refID(paidDomain, raw, idx, ep) || r != refID(reversedDomain, raw, idx, ep) {
			t.Fatal("not the specification")
		}
		if seen[p] || seen[r] || p == r {
			t.Fatal("collision")
		}
		seen[p], seen[r] = true, true
	}
}

func FuzzTransferIDs(f *testing.F) {
	f.Add(strings.Repeat("ab", 32), uint32(0), uint32(0))
	f.Add(strings.Repeat("AB", 32), uint32(1<<32-1), uint32(1<<31))
	f.Add(strings.Repeat("a", 63), uint32(1), uint32(0))
	f.Add("", uint32(0), uint32(0))
	f.Add(strings.Repeat("\xff", 64), uint32(5), uint32(5))
	f.Fuzz(func(t *testing.T, txid string, index, epoch uint32) {
		p, r := PaidTransferID(txid, index, epoch), ReversalTransferID(txid, index, epoch)
		raw, err := hex.DecodeString(txid)
		if err != nil || len(raw) != 32 {
			if !p.IsZero() || !r.IsZero() {
				t.Fatalf("%q: an id for a bad txid", txid)
			}
			return
		}
		if p != refID(paidDomain, raw, index, epoch) || r != refID(reversedDomain, raw, index, epoch) || p == r {
			t.Fatalf("%q %d %d", txid, index, epoch)
		}
		if p != PaidTransferID(txid, index, epoch) {
			t.Fatal("not deterministic")
		}
	})
}

func TestRequiredAtThePiconeroTiers(t *testing.T) {
	tiers := DefaultTiers()
	for _, c := range []struct {
		amount int64
		want   int
	}{{1, 1}, {1_000_000_000_000, 1}, {1_000_000_000_001, 3}, {10_000_000_000_000, 3}, {10_000_000_000_001, 10}, {store.MaxPiconero, 10}} {
		if got := rail.Required(tiers, c.amount); got != c.want {
			t.Fatalf("Required(%d) = %d, want %d", c.amount, got, c.want)
		}
	}
	if rail.SettledDepth(tiers) != 10 {
		t.Fatal("settled depth")
	}
	if err := rail.ValidTiers(tiers, store.MaxPiconero); err != nil {
		t.Fatal(err)
	}
}

func TestLockedBeyondNormal(t *testing.T) {
	for _, c := range []struct {
		u, seen int64
		want    bool
	}{{0, 100, false}, {1, 100, false}, {110, 100, false}, {111, 100, true}, {499_999_999, 100, true},
		{500_000_000, 0, true}, {500_000_000, 3_500_000, true}, {1 << 62, 100, true}} {
		if got := lockedBeyondNormal(c.u, c.seen); got != c.want {
			t.Fatalf("lockedBeyondNormal(%d, %d) = %v", c.u, c.seen, got)
		}
	}
}

func TestCounted(t *testing.T) {
	exp := epoch.Add(time.Hour)
	p := store.Payment{FirstSeen: exp.Add(-time.Second), FirstSeenHeight: 100}
	if !counted(p, exp) {
		t.Fatal("on time, no lock")
	}
	p.UnlockTime = 110
	if !counted(p, exp) {
		t.Fatal("within the normal lock")
	}
	p.UnlockTime = 111
	if counted(p, exp) {
		t.Fatal("locked beyond the normal lock is counted")
	}
	p.UnlockTime, p.FirstSeen = 0, exp
	if counted(p, exp) {
		t.Fatal("first seen at the expiry is counted")
	}
}

func validConfig() Config {
	return Config{Address: monerotest.New(1).Primary(), Network: "mainnet"}
}

func TestConfigDefaultsAndValidate(t *testing.T) {
	c := validConfig().WithDefaults()
	if c.UnusedAlert != 100 || c.PollInterval != 10*time.Second || c.PollEvery != 200*time.Millisecond ||
		c.CallTimeout != 30*time.Second || len(c.Tiers) != 3 || c.Tiers[2] != (rail.Tier{UpTo: 0, Confirmations: 10}) ||
		c.AccountIndex != 0 {
		t.Fatalf("%+v", c)
	}
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	mod := func(f func(*Config)) Config { c := validConfig(); f(&c); return c }
	for _, x := range []struct {
		c    Config
		want string
	}{
		{mod(func(c *Config) { c.Network = "testnet" }), "monero: Address must be the wallet's primary address for Network"},
		{mod(func(c *Config) { c.Network = "ETH" }), "monero: Network must be mainnet, testnet, stagenet or regtest"},
		{mod(func(c *Config) { c.Network = "" }), "monero: Network must be mainnet, testnet, stagenet or regtest"},
		{mod(func(c *Config) { c.Address = monerotest.New(1).SubaddressAt(1) }), "monero: Address must be the wallet's primary address for Network"},
		{mod(func(c *Config) { c.Tiers = []rail.Tier{} }), "monero: Tiers is empty"},
		{mod(func(c *Config) {
			c.Tiers = []rail.Tier{{UpTo: store.MaxPiconero + 1, Confirmations: 1}, {UpTo: 0, Confirmations: 2}}
		}),
			"monero: a tier's UpTo must be 1 to the maximum amount, 0 only for the last"},
		{mod(func(c *Config) { c.UnusedAlert = -1 }), "monero: UnusedAlert must be 1 to 10000"},
		{mod(func(c *Config) { c.UnusedAlert = 10_001 }), "monero: UnusedAlert must be 1 to 10000"},
		{mod(func(c *Config) { c.PollInterval = 2*time.Second - 1 }), "monero: PollInterval must be 2 s to 60 s"},
		{mod(func(c *Config) { c.PollInterval = time.Minute + 1 }), "monero: PollInterval must be 2 s to 60 s"},
		{mod(func(c *Config) { c.PollEvery = 100*time.Microsecond - 1 }), "monero: PollEvery must be 100 µs to 1 s"},
		{mod(func(c *Config) { c.PollEvery = time.Second + 1 }), "monero: PollEvery must be 100 µs to 1 s"},
		{mod(func(c *Config) { c.CallTimeout = time.Second - 1 }), "monero: CallTimeout must be 1 s to 10 min"},
		{mod(func(c *Config) { c.CallTimeout = 10*time.Minute + 1 }), "monero: CallTimeout must be 1 s to 10 min"},
	} {
		if err := x.c.Validate(); err == nil || err.Error() != x.want {
			t.Fatalf("%+v: %v, want %q", x.c, err, x.want)
		}
	}
	for _, ok := range []Config{
		mod(func(c *Config) {
			c.UnusedAlert, c.PollInterval, c.PollEvery, c.CallTimeout = 1, 2*time.Second, 100*time.Microsecond, time.Second
		}),
		mod(func(c *Config) {
			c.UnusedAlert, c.PollInterval, c.PollEvery, c.CallTimeout = 10_000, time.Minute, time.Second, 10*time.Minute
		}),
		mod(func(c *Config) { c.Network = "regtest" }),
		mod(func(c *Config) { c.AccountIndex = 1<<32 - 1 }),
		mod(func(c *Config) { c.Tiers = []rail.Tier{{UpTo: 0, Confirmations: 0}} }),
	} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("%+v: %v", ok, err)
		}
	}
}

// inRange is the specification of the numeric fields after the defaults.
func inRange(c Config) bool {
	c = c.WithDefaults()
	return c.UnusedAlert >= 1 && c.UnusedAlert <= 10_000 && c.PollInterval >= 2*time.Second && c.PollInterval <= time.Minute &&
		c.PollEvery >= 100*time.Microsecond && c.PollEvery <= time.Second && c.CallTimeout >= time.Second && c.CallTimeout <= 10*time.Minute
}

// The seeded property: around every bound of every numeric field, Validate accepts exactly the values in range.
func TestConfigValidateProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	near := func(edges ...int64) int64 {
		e := edges[rng.Intn(len(edges))]
		return e + int64(rng.Intn(5)) - 2
	}
	for i := 0; i < 5000; i++ {
		c := validConfig()
		c.UnusedAlert = near(0, 1, 10_000, -1<<62, 1<<62)
		c.PollInterval = time.Duration(near(0, int64(2*time.Second), int64(time.Minute), -1<<62))
		c.PollEvery = time.Duration(near(0, int64(100*time.Microsecond), int64(time.Second), 1<<62))
		c.CallTimeout = time.Duration(near(0, int64(time.Second), int64(10*time.Minute), -1<<62))
		if err := c.Validate(); (err == nil) != inRange(c) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}

func FuzzConfigValidate(f *testing.F) {
	w := monerotest.New(3)
	f.Add(w.Primary(), "mainnet", int64(0), int64(0), int64(0), int64(0))
	f.Add(w.SubaddressAt(1), "regtest", int64(1), int64(2e9), int64(1e5), int64(1e9))
	f.Add("9"+w.Primary()[1:], "testnet", int64(10_001), int64(6e10+1), int64(1e9+1), int64(6e11+1))
	f.Add("5"+w.Primary()[1:], "stagenet", int64(-1), int64(-1), int64(-1), int64(-1))
	f.Fuzz(func(t *testing.T, addr, network string, unused, interval, every, timeout int64) {
		c := Config{Address: addr, Network: network, UnusedAlert: unused, PollInterval: time.Duration(interval),
			PollEvery: time.Duration(every), CallTimeout: time.Duration(timeout)}
		err := c.Validate()
		okAddr := network != "" && (network == "mainnet" || network == "testnet" || network == "stagenet" || network == "regtest") &&
			len(addr) == 95 && strings.Trim(addr[1:], base58Alphabet) == "" && strings.ContainsRune(primaryFirst[network], rune(addr[0]))
		if (err == nil) != (okAddr && inRange(c)) {
			t.Fatalf("%+v: %v", c, err)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "monero: ") {
			t.Fatalf("text %q", err)
		}
	})
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var primaryFirst = map[string]string{"mainnet": "4", "regtest": "4", "testnet": "9A", "stagenet": "5"}

func TestNewCreatesTheLedgerAccounts(t *testing.T) {
	e := newEnv(t)
	for _, id := range []ledger.U128{MoneroWallet, MerchantMonero} {
		a, ok, err := e.lg.LookupAccount(bg, id)
		if err != nil || !ok || a.Ledger != 3 {
			t.Fatalf("%+v %v %v", a, ok, err)
		}
		switch id {
		case MoneroWallet:
			if a.Code != 5 || a.Flags != ledger.AccountCreditsMustNotExceedDebits {
				t.Fatalf("wallet %+v", a)
			}
		case MerchantMonero:
			if a.Code != 6 || a.Flags != ledger.AccountDebitsMustNotExceedCredits {
				t.Fatalf("merchant %+v", a)
			}
		}
	}
	if MoneroWallet != ledger.U128From64(0x584d_0001) || MerchantMonero != ledger.U128From64(0x584d_0002) {
		t.Fatal("account ids")
	}
	e.svc = e.newService() // a second New finds the accounts
	if _, err := New(validConfig(), nil, e.lg, e.w, e.ln, e.clk.Now, nil); err == nil {
		t.Fatal("a nil store was accepted")
	}
	if _, err := New(Config{Network: "mainnet"}, e.st, e.lg, e.w, e.ln, e.clk.Now, nil); err == nil {
		t.Fatal("a bad config was accepted")
	}
}

// The confirmed sum walks each payment's epochs as the on-chain rail does, with the Monero ids, ledger and accounts.
func TestConfirmedSum(t *testing.T) {
	e := newEnv(t)
	e.ready()
	o := e.order(1000)
	a, err := e.svc.Address(bg, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	sum := func() int64 {
		t.Helper()
		s, err := e.svc.confirmedSum(bg, o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tx1, tx2 := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	e.payment(tx1, a.Address, 600, 0)
	e.payment(tx2, a.Address, 400, 0)
	if sum() != 0 {
		t.Fatal("no transfer, a sum")
	}
	idx := uint32(a.Index)
	e.forward(o, tx1, idx, 0, 600, codePaid)
	if sum() != 600 {
		t.Fatal(sum())
	}
	if paid, _ := e.svc.paidMonero(bg, o); paid {
		t.Fatal("600 of 1000 is paid")
	}
	e.forward(o, tx2, idx, 0, 400, codePaid)
	if paid, err := e.svc.paidMonero(bg, o); !paid || err != nil || sum() != 1000 {
		t.Fatal("1000 of 1000 is not paid", err)
	}
	e.reverse(o, tx2, idx, 0, 400)
	if sum() != 600 {
		t.Fatal("a reversed forward counts")
	}
	e.forward(o, tx2, idx, 1, 400, codePaid) // counted again in epoch 1
	if sum() != 1000 {
		t.Fatal("epoch 1 does not count")
	}
	// A forward with another amount is foreign: it counts for nothing.
	o2 := e.order(1000)
	a2, _ := e.svc.Address(bg, o2.ID)
	tx3 := strings.Repeat("c3", 32)
	e.payment(tx3, a2.Address, 1000, 0)
	e.forward(o2, tx3, uint32(a2.Index), 0, 999, codePaid)
	if s, _ := e.svc.confirmedSum(bg, o2); s != 0 {
		t.Fatal("a foreign forward counts")
	}
	// A reversal with another amount makes the payment foreign, whatever later epochs say.
	o4 := e.order(1000)
	a4, _ := e.svc.Address(bg, o4.ID)
	tx5 := strings.Repeat("e5", 32)
	e.payment(tx5, a4.Address, 1000, 0)
	e.forward(o4, tx5, uint32(a4.Index), 0, 1000, codePaid)
	e.transfer(ReversalTransferID(tx5, uint32(a4.Index), 0), MerchantMonero, MoneroWallet, o4, 999, codeReversed)
	e.forward(o4, tx5, uint32(a4.Index), 1, 1000, codePaid)
	if s, _ := e.svc.confirmedSum(bg, o4); s != 0 {
		t.Fatal("a payment with a foreign reversal counts")
	}
	// A late payment (locked beyond the normal lock) recorded with the late code never counts.
	o3 := e.order(1000)
	a3, _ := e.svc.Address(bg, o3.ID)
	tx4 := strings.Repeat("d4", 32)
	e.payment(tx4, a3.Address, 1000, 1_000)
	e.forward(o3, tx4, uint32(a3.Index), 0, 1000, codeLate)
	if s, _ := e.svc.confirmedSum(bg, o3); s != 0 {
		t.Fatal("a late forward counts")
	}
	// An order without a subaddress or a Monero amount sums to 0.
	if s, err := e.svc.confirmedSum(bg, e.order(5)); s != 0 || err != nil {
		t.Fatal(s, err)
	}
}
