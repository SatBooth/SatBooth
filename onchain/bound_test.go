package onchain

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/satbooth/satbooth/rail"
	"github.com/satbooth/satbooth/store"
)

// boundOut applies an observation only to a mined view at its mark's height and block hash; without one an unrecorded
// mined view counts zero and a recorded one is unbounded; any other view is untouched.
func TestBoundOut(t *testing.T) {
	obs := observed{height: 10, hash: "X", confirmations: 3}
	mined := func(h int64, recorded bool) rail.Out {
		return rail.Out{Marked: true, Kind: store.MarkMined, Height: h, Recorded: recorded}
	}
	for _, c := range []struct {
		name       string
		v          rail.Out
		have       bool
		hash       string
		bounded    bool
		observed   int64
		confirmsAt int64 // Confirmations(100)
	}{
		{"matched height and hash", mined(10, false), true, "X", true, 3, 3},
		{"matched, recorded", mined(10, true), true, "X", true, 3, 3},
		{"same height, other hash, unrecorded", mined(10, false), true, "Y", true, 0, 0},
		{"same height, other hash, recorded", mined(10, true), true, "Y", false, 0, 91},
		{"other height, unrecorded", mined(9, false), true, "X", true, 0, 0},
		{"no observation, unrecorded", mined(10, false), false, "X", true, 0, 0},
		{"no observation, recorded", mined(10, true), false, "X", false, 0, 91},
		{"mempool", rail.Out{Marked: true, Kind: store.MarkMempool}, true, "X", false, 0, 0},
		{"dead", rail.Out{Marked: true, Kind: store.MarkDead}, true, "X", false, 0, 0},
		{"unmarked", rail.Out{}, true, "X", false, 0, 0},
	} {
		v := c.v
		o := obs
		boundOut(&v, o, c.have, c.hash)
		if v.Bounded != c.bounded || v.Observed != c.observed || v.Confirmations(100) != c.confirmsAt {
			t.Errorf("%s: %+v, %d confirmations", c.name, v, v.Confirmations(100))
		}
		if !c.v.Mined() {
			if !reflect.DeepEqual(v, c.v) {
				t.Errorf("%s: changed: %+v", c.name, v)
			}
		}
	}
}

// Seeded property: after boundOut a bounded view never shows more than the observation (never below 0), a mined
// unrecorded view is always bounded, an unmatched one shows 0, and a recorded unmatched one or a view that is not
// mined is left as it was.
func TestBoundOutProperty(t *testing.T) {
	r := rand.New(rand.NewSource(14))
	kinds := []string{store.MarkMempool, store.MarkMined, store.MarkDead, ""}
	hashes := []string{"", "X", "Y"}
	for i := 0; i < 20_000; i++ {
		v := rail.Out{Marked: r.Intn(5) > 0, Kind: kinds[r.Intn(len(kinds))], Height: r.Int63n(50) - 5, Recorded: r.Intn(2) == 0}
		o := observed{height: r.Int63n(50) - 5, hash: hashes[r.Intn(len(hashes))], confirmations: r.Int63n(60) - 10}
		have, hash, tip := r.Intn(4) > 0, hashes[r.Intn(len(hashes))], r.Int63n(80)-5
		before := v
		boundOut(&v, o, have, hash)
		matched := have && o.height == v.Height && o.hash == hash
		switch {
		case !before.Mined():
			if !reflect.DeepEqual(v, before) {
				t.Fatalf("a view that is not mined changed: %+v -> %+v", before, v)
			}
		case matched:
			if !v.Bounded || v.Observed != o.confirmations || v.Confirmations(tip) > max(o.confirmations, 0) {
				t.Fatalf("matched: %+v %+v tip %d", o, v, tip)
			}
		case !before.Recorded:
			if !v.Bounded || v.Confirmations(tip) != 0 {
				t.Fatalf("unmatched, unrecorded: %+v tip %d", v, tip)
			}
		default:
			if !reflect.DeepEqual(v, before) {
				t.Fatalf("unmatched, recorded: changed: %+v -> %+v", before, v)
			}
		}
		if v.Bounded && v.Confirmations(tip) > max(v.Observed, 0) {
			t.Fatalf("bounded view over its observation: %+v tip %d", v, tip)
		}
	}
}
