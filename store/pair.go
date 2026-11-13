package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// validPairID reports whether id is 32 lowercase hex characters, the shape the pair table's CHECKs pin for the id and
// the nonce.
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

// PairStamp is the store's pair stamp (plan 10): the pair id ("" while none is set), the generation, and the
// nonce the advance to that generation drew ("" exactly while the generation is 0).
type PairStamp struct {
	ID    string
	Gen   int64
	Nonce string
}

// Pair returns the store's pair stamp. The table must hold exactly one row (migration 7 writes it and the schema
// keeps it); any other count is an error.
func (s *Store) Pair(ctx context.Context) (PairStamp, error) {
	var n int
	var pid, nonce sql.NullString
	var g sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), max(pair_id), max(generation), max(nonce) FROM pair`).Scan(&n, &pid, &g, &nonce); err != nil {
		return PairStamp{}, err
	}
	if n != 1 {
		return PairStamp{}, fmt.Errorf("store: the pair table has %d rows", n)
	}
	return PairStamp{ID: pid.String, Gen: g.Int64, Nonce: nonce.String}, nil
}

// SetPair writes the store's pair stamp, generation and nonce in one statement of its own transaction. The id is set
// once (the schema refuses a change) and the generation never goes back. The nonce is "" at generation 0 and 32
// lowercase hex characters above it.
func (s *Store) SetPair(ctx context.Context, p PairStamp) error {
	if !validPairID(p.ID) {
		return errors.New("store: the pair id must be 32 lowercase hex characters")
	}
	if p.Gen < 0 {
		return errors.New("store: the pair generation is negative")
	}
	if (p.Gen == 0) != (p.Nonce == "") || (p.Nonce != "" && !validPairID(p.Nonce)) {
		return errors.New("store: the pair nonce must be empty at generation 0 and 32 lowercase hex characters above it")
	}
	nonce := sql.NullString{String: p.Nonce, Valid: p.Nonce != ""}
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE pair SET pair_id = ?, generation = ?, nonce = ?`, p.ID, p.Gen, nonce)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return fmt.Errorf("store: the pair table has %d rows", n)
		}
		return nil
	})
}
