package service

import (
	"context"
	"encoding/hex"
	"errors"
	"io"

	"github.com/satbooth/satbooth/ledger"
	"github.com/satbooth/satbooth/store"
)

// The three refusals of the pair check (plan 10). Each is a fixed message: no path, id or generation.
var (
	ErrPairOlderStore  = errors.New("the store is older than the ledger: restore both databases from one backup")
	ErrPairOlderLedger = errors.New("the ledger is older than the store: restore both databases from one backup")
	ErrPairDifferent   = errors.New("the store and the ledger are not a pair: restore both databases from one backup")
)

// pairStore and pairLedger are the parts of *store.Store and *ledger.Ledger the pair check uses.
type pairStore interface {
	Pair(ctx context.Context) (store.PairStamp, error)
	SetPair(ctx context.Context, s store.PairStamp) error
}

type pairLedger interface {
	Pair(ctx context.Context) (ledger.PairStamp, error)
	SetPair(ctx context.Context, s ledger.PairStamp) error
}

// pairState is the state of the two pair stamps (plan 10): one of the five states a start allows, or refused.
type pairState int

const (
	pairRefused          pairState = iota // anything else; classifyPair returns one of the three refusals
	pairFirstStamp                        // S and L empty: no id, generation 0, no nonce, nothing pending
	pairFinishFirstStamp                  // L has the id, S does not; generations 0, no nonces, nothing pending
	pairEqual                             // ids, generations and nonces equal, nothing pending
	pairStoreWritten                      // ids equal, S = L's pending (generation and nonce), pending = L.generation + 1
	pairLedgerMarked                      // ids equal, S = L (generation and nonce), pending = L.generation + 1
)

// nonceFits reports whether a stamp's nonce has the shape its generation needs: "" at generation 0, set above it. An
// empty nonce is never a wildcard.
func nonceFits(gen int64, nonce string) bool { return (gen == 0) == (nonce == "") }

// classifyPair maps the two stamps to their state; exactly the five states of the pair check are accepted, everything
// else is refused. For pairRefused it also returns ErrPairOlderStore or ErrPairOlderLedger when the generations differ,
// and ErrPairDifferent when the ids differ or the generations are equal; for every other state the error is nil. Two
// timelines of one pair (a restore, then starts) reach the same generations with other nonces, so equal ids and
// generations with different nonces are not a pair. checkPair and Backup both use it, so the two read the stamps the
// same way.
func classifyPair(s store.PairStamp, l ledger.PairStamp) (pairState, error) {
	noPending := l.Pending == 0 && l.PendingNonce == ""
	empty := s.Gen == 0 && l.Gen == 0 && s.Nonce == "" && l.Nonce == "" && noPending
	switch {
	case l.ID == "" && s.ID == "" && empty:
		return pairFirstStamp, nil
	case l.ID != "" && s.ID == "" && empty:
		// A pending value cannot arise before the store has the id.
		return pairFinishFirstStamp, nil
	case l.ID == "" || l.ID != s.ID:
		return pairRefused, ErrPairDifferent
	case !nonceFits(s.Gen, s.Nonce) || !nonceFits(l.Gen, l.Nonce):
		// Not an accepted state; refused below by generation.
	case s.Gen == l.Gen && s.Nonce == l.Nonce && noPending:
		return pairEqual, nil
	case l.Pending != l.Gen+1 || l.PendingNonce == "":
		// Not an accepted state; refused below by generation.
	case s.Gen == l.Pending && s.Nonce == l.PendingNonce:
		// Only the nonce the ledger recorded for this advance: a store at that generation from another timeline has
		// another nonce.
		return pairStoreWritten, nil
	case s.Gen == l.Gen && s.Nonce == l.Nonce:
		return pairLedgerMarked, nil
	}
	switch {
	case s.Gen < l.Gen:
		return pairRefused, ErrPairOlderStore
	case s.Gen > l.Gen:
		return pairRefused, ErrPairOlderLedger
	default:
		return pairRefused, ErrPairDifferent
	}
}

// checkPair reads both pair stamps and allows exactly the five states of classifyPair; it refuses every other state
// with one of the three errors above. Each write is its own committed transaction, and each writes a database's
// generation and nonce in one statement. The random values a start needs (the first stamp's id, an advance's nonce) are
// drawn before the first write, so a failing random source writes nothing. The first stamp (ledger first), the finish
// of an interrupted first stamp and the finish of an interrupted advance are always followed by an advance (after the
// last, two copies of one crashed data directory end on different nonces), so a completed check never leaves a
// generation at 0: a lost store.db or ledger.db comes back as a fresh file (id NULL, generation 0) and is then refused,
// never stamped.
func checkPair(ctx context.Context, st pairStore, lg pairLedger, rand io.Reader) error {
	s, err := st.Pair(ctx)
	if err != nil {
		return err
	}
	l, err := lg.Pair(ctx)
	if err != nil {
		return err
	}
	state, err := classifyPair(s, l)
	switch state {
	case pairFirstStamp:
		// First stamp: one fresh id, ledger first, then the advance.
		id, err := randHex(rand)
		if err != nil {
			return err
		}
		nonce, err := randHex(rand)
		if err != nil {
			return err
		}
		if err := lg.SetPair(ctx, ledger.PairStamp{ID: id}); err != nil {
			return err
		}
		if err := st.SetPair(ctx, store.PairStamp{ID: id}); err != nil {
			return err
		}
		return advance(ctx, st, lg, ledger.PairStamp{ID: id}, nonce)
	case pairFinishFirstStamp:
		// Finish the first stamp, then the advance.
		nonce, err := randHex(rand)
		if err != nil {
			return err
		}
		if err := st.SetPair(ctx, store.PairStamp{ID: l.ID}); err != nil {
			return err
		}
		return advance(ctx, st, lg, l, nonce)
	case pairEqual:
		nonce, err := randHex(rand)
		if err != nil {
			return err
		}
		return advance(ctx, st, lg, l, nonce)
	case pairStoreWritten, pairLedgerMarked:
		// The ledger holds a pending stamp. If the store was not written yet (pairLedgerMarked), write it with the
		// recorded nonce; then commit the ledger, then advance once more with a fresh nonce.
		nonce, err := randHex(rand)
		if err != nil {
			return err
		}
		done := ledger.PairStamp{ID: l.ID, Gen: l.Pending, Nonce: l.PendingNonce}
		if state == pairLedgerMarked {
			if err := st.SetPair(ctx, store.PairStamp{ID: l.ID, Gen: l.Pending, Nonce: l.PendingNonce}); err != nil {
				return err
			}
		}
		if err := lg.SetPair(ctx, done); err != nil {
			return err
		}
		return advance(ctx, st, lg, done, nonce)
	default:
		return err
	}
}

// randHex reads 16 bytes from rand and returns them as 32 lowercase hex characters.
func randHex(rand io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// advance moves both stamps from l's generation g to g+1 with the fresh nonce n: the ledger marks (g+1, n) pending,
// the store takes (g+1, n), the ledger takes (g+1, n) and clears pending.
func advance(ctx context.Context, st pairStore, lg pairLedger, l ledger.PairStamp, n string) error {
	g := l.Gen
	if err := lg.SetPair(ctx, ledger.PairStamp{ID: l.ID, Gen: g, Nonce: l.Nonce, Pending: g + 1, PendingNonce: n}); err != nil {
		return err
	}
	if err := st.SetPair(ctx, store.PairStamp{ID: l.ID, Gen: g + 1, Nonce: n}); err != nil {
		return err
	}
	return lg.SetPair(ctx, ledger.PairStamp{ID: l.ID, Gen: g + 1, Nonce: n})
}
