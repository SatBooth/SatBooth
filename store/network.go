package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrNetwork is returned by BindNetwork when the store belongs to another network.
var ErrNetwork = errors.New("store: this data directory belongs to another network")

// NetworkError is the error BindNetwork returns for another network: it is ErrNetwork (errors.Is) and carries the
// network the store belongs to (errors.As), so a caller needs no second read.
type NetworkError struct{ Network string }

func (e *NetworkError) Error() string { return ErrNetwork.Error() + ": it belongs to " + e.Network }

func (e *NetworkError) Is(target error) bool { return target == ErrNetwork }

// networks are the Lightning network names the configuration's nwc.network accepts (service.Config,
// approval.ParseNetwork).
var networks = []string{"mainnet", "testnet", "signet", "regtest"}

func validNetwork(n string) bool {
	for _, k := range networks {
		if n == k {
			return true
		}
	}
	return false
}

// networkSchema is migration 10: the table that holds the Lightning network this store was first recorded under. It
// holds at most one row, written once: the immutable guards refuse an update, a delete and any second insert, of the
// same network or another (the replace condition is always true; the key alone would let a different one in), and the
// CHECK allows only the network names. A database migrated from an earlier version has no row until the first start
// that passes its checks under a network (BindNetwork).
func networkSchema() string {
	quoted := make([]string, len(networks))
	for i, n := range networks {
		quoted[i] = "'" + n + "'"
	}
	return fmt.Sprintf(`CREATE TABLE store_network (
	network TEXT NOT NULL PRIMARY KEY CHECK (network IN (%s))
) STRICT, WITHOUT ROWID;
`, strings.Join(quoted, ", ")) + immutable("store_network", "1")
}

// BindNetwork ties the store to a Lightning network. The first call records it; a later call with the same network
// writes nothing; a call with another network returns an error that wraps ErrNetwork and names the stored network (a
// network name is not a secret). A name that is not a network is ErrInvalid and writes nothing.
func (s *Store) BindNetwork(ctx context.Context, network string) error {
	if !validNetwork(network) {
		return fmt.Errorf("%w: unknown network", ErrInvalid)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		have, err := networkOf(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case have == network:
			return nil
		case have != "":
			return &NetworkError{Network: have}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO store_network (network) VALUES (?)`, network)
		return err
	})
}

// Network returns the recorded network, or "" if none is recorded.
func (s *Store) Network(ctx context.Context) (string, error) {
	return networkOf(ctx, s.db)
}

func networkOf(ctx context.Context, q queryer) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT network FROM store_network`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(out) {
	case 0:
		return "", nil
	case 1:
		return out[0], nil
	}
	return "", fmt.Errorf("store: the network table has %d rows", len(out))
}
