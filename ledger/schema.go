package ledger

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations is the ordered list of schema migrations. A released migration is never edited:
// changes go in a new entry at the end.
var migrations = []string{
	// 1
	`CREATE TABLE accounts (
	id BLOB PRIMARY KEY CHECK (length(id) = 16),
	user_data_128 BLOB NOT NULL CHECK (length(user_data_128) = 16),
	user_data_64 BLOB NOT NULL CHECK (length(user_data_64) = 8),
	user_data_32 INTEGER NOT NULL CHECK (user_data_32 BETWEEN 0 AND 4294967295),
	ledger INTEGER NOT NULL CHECK (ledger BETWEEN 1 AND 4294967295),
	code INTEGER NOT NULL CHECK (code BETWEEN 1 AND 65535),
	flags INTEGER NOT NULL CHECK (flags BETWEEN 0 AND 65535),
	timestamp INTEGER NOT NULL UNIQUE CHECK (timestamp > 0)
) STRICT, WITHOUT ROWID;

CREATE TABLE transfers (
	id BLOB PRIMARY KEY CHECK (length(id) = 16),
	debit_account_id BLOB NOT NULL REFERENCES accounts (id),
	credit_account_id BLOB NOT NULL REFERENCES accounts (id),
	amount BLOB NOT NULL CHECK (length(amount) = 16),
	pending_id BLOB NOT NULL CHECK (length(pending_id) = 16),
	user_data_128 BLOB NOT NULL CHECK (length(user_data_128) = 16),
	user_data_64 BLOB NOT NULL CHECK (length(user_data_64) = 8),
	user_data_32 INTEGER NOT NULL CHECK (user_data_32 BETWEEN 0 AND 4294967295),
	timeout INTEGER NOT NULL CHECK (timeout BETWEEN 0 AND 4294967295),
	ledger INTEGER NOT NULL CHECK (ledger BETWEEN 1 AND 4294967295),
	code INTEGER NOT NULL CHECK (code BETWEEN 1 AND 65535),
	flags INTEGER NOT NULL CHECK (flags BETWEEN 0 AND 65535),
	timestamp INTEGER NOT NULL UNIQUE CHECK (timestamp > 0)
) STRICT, WITHOUT ROWID;

CREATE TABLE failed_transfers (
	id BLOB PRIMARY KEY CHECK (length(id) = 16),
	result TEXT NOT NULL
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX transfers_one_resolution ON transfers (pending_id) WHERE (flags & 12) != 0;
CREATE INDEX transfers_debit_account ON transfers (debit_account_id);
CREATE INDEX transfers_credit_account ON transfers (credit_account_id);

CREATE TRIGGER accounts_no_update BEFORE UPDATE ON accounts
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER accounts_no_delete BEFORE DELETE ON accounts
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER transfers_no_update BEFORE UPDATE ON transfers
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER transfers_no_delete BEFORE DELETE ON transfers
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER failed_transfers_no_update BEFORE UPDATE ON failed_transfers
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER failed_transfers_no_delete BEFORE DELETE ON failed_transfers
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;`,
	// 2: guards that hold on any connection, whatever its pragmas. INSERT OR REPLACE deletes the
	// row it conflicts with, and DELETE triggers fire for that only when recursive_triggers is on, so
	// an insert whose id (or timestamp) is taken is refused here. Likewise a second post/void for
	// one pending transfer, and a transfer naming an account that does not exist.
	`CREATE TRIGGER accounts_no_replace BEFORE INSERT ON accounts
WHEN EXISTS (SELECT 1 FROM accounts WHERE id = NEW.id OR timestamp = NEW.timestamp)
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER failed_transfers_no_replace BEFORE INSERT ON failed_transfers
WHEN EXISTS (SELECT 1 FROM failed_transfers WHERE id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER transfers_no_replace BEFORE INSERT ON transfers
WHEN EXISTS (SELECT 1 FROM transfers WHERE id = NEW.id OR timestamp = NEW.timestamp)
  OR ((NEW.flags & 12) != 0 AND EXISTS (SELECT 1 FROM transfers WHERE pending_id = NEW.pending_id AND (flags & 12) != 0))
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER transfers_known_accounts BEFORE INSERT ON transfers
WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE id = NEW.debit_account_id)
  OR NOT EXISTS (SELECT 1 FROM accounts WHERE id = NEW.credit_account_id)
BEGIN SELECT RAISE(ABORT, 'unknown account'); END;`,
	// 3: the record of an expiry the ledger has decided: TigerBeetle commits an expired status, and without a committed
	// record a backward clock step would undo an observed expiry. Once written, a pending
	// transfer is expired whatever the clock says, and it can no longer be posted or voided.
	`CREATE TABLE expired_transfers (
	pending_id BLOB PRIMARY KEY CHECK (length(pending_id) = 16) REFERENCES transfers (id),
	timestamp INTEGER NOT NULL UNIQUE CHECK (timestamp > 0)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER expired_transfers_no_update BEFORE UPDATE ON expired_transfers
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER expired_transfers_no_delete BEFORE DELETE ON expired_transfers
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER expired_transfers_no_replace BEFORE INSERT ON expired_transfers
WHEN EXISTS (SELECT 1 FROM expired_transfers WHERE pending_id = NEW.pending_id OR timestamp = NEW.timestamp)
BEGIN SELECT RAISE(ABORT, 'ledger rows are immutable'); END;
CREATE TRIGGER expired_transfers_known_pending BEFORE INSERT ON expired_transfers
WHEN NOT EXISTS (SELECT 1 FROM transfers WHERE id = NEW.pending_id AND (flags & 2) != 0 AND timeout != 0)
  OR EXISTS (SELECT 1 FROM transfers WHERE pending_id = NEW.pending_id AND (flags & 12) != 0)
BEGIN SELECT RAISE(ABORT, 'not an unresolved pending transfer with a timeout'); END;
CREATE TRIGGER transfers_not_after_expiry BEFORE INSERT ON transfers
WHEN (NEW.flags & 12) != 0 AND EXISTS (SELECT 1 FROM expired_transfers WHERE pending_id = NEW.pending_id)
BEGIN SELECT RAISE(ABORT, 'pending transfer expired'); END;`,
	// 4: the pair stamp (plan 10): the one row that ties this ledger to the store it was written with. With
	// schema_version, the only table whose row is updated. It holds exactly one row: the key CHECK allows no second
	// row, and the triggers refuse a delete and any insert once the row exists (which also refuses REPLACE and an
	// upsert). An update may set the id once and never change it afterwards, and never lowers the generation. nonce is
	// the random value the advance to this generation drew (the store holds the same one): NULL exactly while the
	// generation is 0. pending and pending_nonce are the generation an advance in progress is moving to and the nonce
	// it drew: both NULL, or both set. A pre-existing database gets the row with no id, generation 0, no nonce and no
	// pending value; the first start stamps it (service.checkPair).
	`CREATE TABLE pair (
	one INTEGER PRIMARY KEY CHECK (one = 1),
	pair_id TEXT CHECK (pair_id IS NULL OR (typeof(pair_id) = 'text' AND length(pair_id) = 32 AND pair_id NOT GLOB '*[^0-9a-f]*')),
	generation INTEGER NOT NULL CHECK (generation >= 0),
	nonce TEXT CHECK (nonce IS NULL OR (typeof(nonce) = 'text' AND length(nonce) = 32 AND nonce NOT GLOB '*[^0-9a-f]*')),
	pending INTEGER CHECK (pending IS NULL OR pending >= 1),
	pending_nonce TEXT CHECK (pending_nonce IS NULL OR (typeof(pending_nonce) = 'text' AND length(pending_nonce) = 32 AND pending_nonce NOT GLOB '*[^0-9a-f]*')),
	CHECK ((generation = 0) = (nonce IS NULL)),
	CHECK ((pending IS NULL) = (pending_nonce IS NULL))
) STRICT, WITHOUT ROWID;
INSERT INTO pair (one, pair_id, generation, nonce, pending, pending_nonce) VALUES (1, NULL, 0, NULL, NULL, NULL);
CREATE TRIGGER pair_no_delete BEFORE DELETE ON pair
BEGIN SELECT RAISE(ABORT, 'the pair row is never deleted'); END;
CREATE TRIGGER pair_no_insert BEFORE INSERT ON pair
WHEN EXISTS (SELECT 1 FROM pair)
BEGIN SELECT RAISE(ABORT, 'the pair table has one row'); END;
CREATE TRIGGER pair_update_rules BEFORE UPDATE ON pair
WHEN NEW.one IS NOT OLD.one OR (OLD.pair_id IS NOT NULL AND NEW.pair_id IS NOT OLD.pair_id) OR NEW.generation < OLD.generation
BEGIN SELECT RAISE(ABORT, 'the pair id is set once and the generation never goes back'); END;`,
}

// migrate applies the migrations the database has not seen, in one transaction.
func (l *Ledger) migrate(ctx context.Context) error {
	return l.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL) STRICT`); err != nil {
			return err
		}
		var have, rows int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(max(version), 0) FROM schema_version`).Scan(&rows, &have); err != nil {
			return err
		}
		if rows > 1 {
			return fmt.Errorf("ledger: schema_version has %d rows", rows)
		}
		if have > len(migrations) {
			return fmt.Errorf("ledger: database schema version %d is newer than this build (%d)", have, len(migrations))
		}
		if have == len(migrations) {
			return nil
		}
		for i := have; i < len(migrations); i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("ledger: migration %d: %w", i+1, err)
			}
		}
		if rows == 0 {
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, len(migrations))
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations))
		return err
	})
}
