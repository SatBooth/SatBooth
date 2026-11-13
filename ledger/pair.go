package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// validPairID reports whether id is 32 lowercase hex characters, the shape the pair table's CHECKs pin for the id and
// the nonces.
func validPairID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// PairStamp is the ledger's pair stamp (plan 10): the pair id ("" while none is set), the generation, the nonce
// the advance to that generation drew ("" exactly while the generation is 0), and the generation and nonce of an
// advance in progress (0 and "" when none is).
type PairStamp struct {
	ID           string
	Gen          int64
	Nonce        string
	Pending      int64
	PendingNonce string
}

// Pair returns the ledger's pair stamp. The table must hold exactly one row (migration 4 writes it and the schema
// keeps it); any other count is an error.
func (l *Ledger) Pair(ctx context.Context) (PairStamp, error) {
	var n int
	var pid, nonce, pn sql.NullString
	var g, p sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `SELECT count(*), max(pair_id), max(generation), max(nonce), max(pending), max(pending_nonce) FROM pair`).
		Scan(&n, &pid, &g, &nonce, &p, &pn); err != nil {
		return PairStamp{}, err
	}
	if n != 1 {
		return PairStamp{}, fmt.Errorf("ledger: the pair table has %d rows", n)
	}
	return PairStamp{ID: pid.String, Gen: g.Int64, Nonce: nonce.String, Pending: p.Int64, PendingNonce: pn.String}, nil
}

// SetPair writes the ledger's pair stamp in one statement of its own transaction; a pending value of 0 and an empty
// nonce are stored as NULL. The id is set once (the schema refuses a change) and the generation never goes back. The
// nonce is "" at generation 0 and 32 lowercase hex characters above it; the pending nonce is "" exactly when the
// pending value is 0.
func (l *Ledger) SetPair(ctx context.Context, s PairStamp) error {
	if !validPairID(s.ID) {
		return errors.New("ledger: the pair id must be 32 lowercase hex characters")
	}
	if s.Gen < 0 || s.Pending < 0 {
		return errors.New("ledger: the pair generation or pending value is negative")
	}
	if (s.Gen == 0) != (s.Nonce == "") || (s.Nonce != "" && !validPairID(s.Nonce)) {
		return errors.New("ledger: the pair nonce must be empty at generation 0 and 32 lowercase hex characters above it")
	}
	if (s.Pending == 0) != (s.PendingNonce == "") || (s.PendingNonce != "" && !validPairID(s.PendingNonce)) {
		return errors.New("ledger: the pending nonce must be empty without a pending value and 32 lowercase hex characters with one")
	}
	nonce := sql.NullString{String: s.Nonce, Valid: s.Nonce != ""}
	p := sql.NullInt64{Int64: s.Pending, Valid: s.Pending != 0}
	pn := sql.NullString{String: s.PendingNonce, Valid: s.PendingNonce != ""}
	return l.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE pair SET pair_id = ?, generation = ?, nonce = ?, pending = ?, pending_nonce = ?`,
			s.ID, s.Gen, nonce, p, pn)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return fmt.Errorf("ledger: the pair table has %d rows", n)
		}
		return nil
	})
}
