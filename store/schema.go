package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/satbooth/satbooth/rates"
)

// The column checks, written once so the Go validators and the SQL agree (the fuzz target runs
// both on the same input). Each is a SQL expression over the column %[1]s. Every text check also
// requires length(x) = length(CAST(x AS BLOB)): length() and GLOB stop at a NUL byte, so without it
// "ab\x00!" would pass; this makes the value plain ASCII without NUL.
const (
	asciiOnly = `typeof(%[1]s) = 'text' AND length(%[1]s) = length(CAST(%[1]s AS BLOB))`
	// maxAmount is 2^53, the largest amount that every JSON consumer reads exactly.
	maxAmount = 1 << 53
)

func checkLower(col string, n int) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) = %[2]d AND %[1]s NOT GLOB '*[^0-9a-f]*'`, col, n)
}

func checkName(col string) string { // account ids and merchant references
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) BETWEEN 1 AND 64 AND %[1]s NOT GLOB '*[^A-Za-z0-9._-]*'`, col)
}

func checkBolt11(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) BETWEEN 1 AND 4096 AND %[1]s NOT GLOB '*[^!-~]*'`, col)
}

func checkAmount(col string) string {
	return fmt.Sprintf(`%[1]s BETWEEN 1 AND %[2]d`, col, int64(maxAmount))
}

// checkAddress: the address of an on-chain order, 14 to 90 bytes of [a-z0-9] starting bc1 (mainnet),
// tb1 (testnet, signet) or bcrt1 (regtest). SatBooth never decodes bech32: bitcoind issues and
// checks the addresses; this is only the shape the store will hold.
func checkAddress(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) BETWEEN 14 AND 90`+
		` AND (%[1]s GLOB 'bc1*' OR %[1]s GLOB 'tb1*' OR %[1]s GLOB 'bcrt1*') AND %[1]s NOT GLOB '*[^a-z0-9]*'`, col)
}

// checkMarkShape: a "mined" mark has a height of at least 1 and a 64-hex block hash; "mempool" and
// "dead" marks have height 0 and no hash. kind, height and hash are SQL expressions.
func checkMarkShape(kind, height, hash string) string {
	return fmt.Sprintf(`((%[1]s = 'mined' AND %[2]s >= 1 AND `+checkLower(hash, 64)+`)`+
		` OR (%[1]s IN ('mempool', 'dead') AND %[2]s = 0 AND %[3]s = ''))`, kind, height, hash)
}

func checkTime(col string) string { return col + ` > 0` }

// checkPiconero: 1 to MaxPiconero (2^53 − 1), the SQL mirror of validPiconero.
func checkPiconero(col string) string {
	return fmt.Sprintf(`%[1]s BETWEEN 1 AND %[2]d`, col, int64(MaxPiconero))
}

// checkSubaddress: a Monero subaddress, 95 bytes of the base58 alphabet (no 0, O, I or l) starting 8 (mainnet,
// regtest), B (testnet) or 7 (stagenet): the SQL mirror of ValidSubaddress. SatBooth never decodes it.
func checkSubaddress(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) = 95 AND %[1]s NOT GLOB '*[^1-9A-HJ-NP-Za-km-z]*'`+
		` AND substr(%[1]s, 1, 1) IN ('8', 'B', '7')`, col)
}

// checkDecimal: one optional point, 1 to 20 integer digits, 1 to 18 fraction digits, nothing else: the SQL mirror of
// rates.ParseDecimal's grammar, which the Go side calls (validDecimal).
func checkDecimal(col string) string {
	return fmt.Sprintf(asciiOnly+` AND %[1]s NOT GLOB '*[^0-9.]*' AND (CASE WHEN instr(%[1]s, '.') = 0`+
		` THEN (length(%[1]s) BETWEEN 1 AND 20)`+
		` ELSE (instr(%[1]s, '.') BETWEEN 2 AND 21 AND length(%[1]s) - instr(%[1]s, '.') BETWEEN 1 AND 18`+
		` AND length(%[1]s) - length(replace(%[1]s, '.', '')) = 1) END)`, col)
}

// checkCurrency: 3 bytes of [A-Z], neither BTC nor XMR: the SQL mirror of rates.ValidCurrency, which the Go side calls.
func checkCurrency(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) = 3 AND %[1]s NOT GLOB '*[^A-Z]*' AND %[1]s NOT IN ('BTC', 'XMR')`, col)
}

// immutable returns the three guards every insert-only table gets (store_schema_version and pair, whose one row is
// updated, get none; the delete guard of onchain_settled is narrowed by migration 3, and that of monero_settled by
// migration 5, to deadSettledDelete).
// replaceWhen is the SQL condition (over NEW) that an existing row has the same key: INSERT OR REPLACE would delete
// that row, and DELETE triggers fire for that only when recursive_triggers is on, so the insert is refused outright.
func immutable(table, replaceWhen string) string {
	return fmt.Sprintf(`CREATE TRIGGER %[1]s_no_update BEFORE UPDATE ON %[1]s
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
CREATE TRIGGER %[1]s_no_delete BEFORE DELETE ON %[1]s
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
CREATE TRIGGER %[1]s_no_replace BEFORE INSERT ON %[1]s
WHEN EXISTS (SELECT 1 FROM %[1]s WHERE %[2]s)
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
`, table, replaceWhen)
}

// deadSettledDelete replaces the delete guard of a settled table (onchain_settled, monero_settled): a settled fact may
// be deleted while the current mark of its key is dead, and only then. keyMatch joins the marks table to OLD.
func deadSettledDelete(settled, marks, keyMatch string) string {
	return fmt.Sprintf(`DROP TRIGGER %[1]s_no_delete;
CREATE TRIGGER %[1]s_delete_dead BEFORE DELETE ON %[1]s
WHEN coalesce((SELECT kind FROM %[2]s WHERE %[3]s ORDER BY seq DESC LIMIT 1), '') != 'dead'
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
`, settled, marks, keyMatch)
}

// migrations is the ordered list of schema migrations. A migration is edited only while no release
// exists; after the first release, changes go in a new entry at the end.
var migrations = []string{
	// 1
	fmt.Sprintf(`CREATE TABLE orders (
	id TEXT PRIMARY KEY CHECK (%s),
	token TEXT NOT NULL UNIQUE CHECK (%s),
	account_id TEXT NOT NULL CHECK (%s),
	merchant_ref TEXT NOT NULL UNIQUE CHECK (%s),
	amount_msat INTEGER NOT NULL CHECK (amount_msat = 0 OR %s),
	amount_sat INTEGER NOT NULL CHECK (amount_sat = 0 OR amount_sat BETWEEN 1 AND %d),
	amount_piconero INTEGER NOT NULL CHECK (amount_piconero = 0 OR %s),
	created_at INTEGER NOT NULL CHECK (%s),
	expires_at INTEGER NOT NULL CHECK (%s AND expires_at > created_at),
	CHECK (amount_msat > 0 OR amount_sat > 0 OR amount_piconero > 0)
) STRICT, WITHOUT ROWID;
CREATE INDEX orders_account ON orders (account_id, created_at);
CREATE INDEX orders_expires ON orders (expires_at);

CREATE TABLE invoices (
	payment_hash TEXT PRIMARY KEY CHECK (%s),
	order_id TEXT NOT NULL REFERENCES orders (id) CHECK (%s),
	bolt11 TEXT NOT NULL CHECK (%s),
	amount_msat INTEGER NOT NULL CHECK (%s),
	created_at INTEGER NOT NULL CHECK (%s),
	expires_at INTEGER NOT NULL CHECK (%s AND expires_at > created_at)
) STRICT, WITHOUT ROWID;
CREATE INDEX invoices_order ON invoices (order_id, created_at, payment_hash);
CREATE INDEX invoices_expires ON invoices (expires_at, payment_hash);

CREATE TABLE closed_invoices (
	payment_hash TEXT PRIMARY KEY REFERENCES invoices (payment_hash) CHECK (%s),
	closed_at INTEGER NOT NULL CHECK (%s),
	reason TEXT NOT NULL CHECK (reason IN ('lookup', 'deadline'))
) STRICT, WITHOUT ROWID;

CREATE TABLE invoice_attempts (
	id TEXT PRIMARY KEY CHECK (%s),
	order_id TEXT NOT NULL REFERENCES orders (id) CHECK (%s),
	at INTEGER NOT NULL CHECK (%s),
	amount_msat INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
CREATE INDEX invoice_attempts_at ON invoice_attempts (at);

CREATE TABLE events (
	id TEXT PRIMARY KEY CHECK (%s),
	type TEXT NOT NULL CHECK (type IN ('order.paid', 'order.expired', 'order.underpaid', 'order.late', 'order.reverted', 'order.paid_again')),
	order_id TEXT NOT NULL REFERENCES orders (id) CHECK (%s),
	seq INTEGER NOT NULL CHECK (seq >= 1 AND (seq = 1 OR type IN ('order.reverted', 'order.paid_again'))),
	created_at INTEGER NOT NULL CHECK (%s),
	UNIQUE (order_id, type, seq)
) STRICT, WITHOUT ROWID;
CREATE INDEX events_created ON events (created_at);

CREATE TABLE webhook_attempts (
	event_id TEXT NOT NULL REFERENCES events (id) CHECK (%s),
	seq INTEGER NOT NULL CHECK (seq >= 1),
	at INTEGER NOT NULL CHECK (%s),
	status INTEGER NOT NULL CHECK (status BETWEEN 0 AND 999),
	ok INTEGER NOT NULL CHECK (ok IN (0, 1)),
	PRIMARY KEY (event_id, seq)
) STRICT, WITHOUT ROWID;
CREATE INDEX webhook_attempts_ok ON webhook_attempts (event_id) WHERE ok = 1;
`,
		checkLower("id", 32), checkLower("token", 32), checkName("account_id"), checkName("merchant_ref"),
		checkAmount("amount_msat"), int64(MaxSat), checkPiconero("amount_piconero"), checkTime("created_at"), checkTime("expires_at"),
		checkLower("payment_hash", 64), checkLower("order_id", 32), checkBolt11("bolt11"), checkAmount("amount_msat"),
		checkTime("created_at"), checkTime("expires_at"),
		checkLower("payment_hash", 64), checkTime("closed_at"),
		checkLower("id", 32), checkLower("order_id", 32), checkTime("at"), checkAmount("amount_msat"),
		checkLower("id", 32), checkLower("order_id", 32), checkTime("created_at"),
		checkLower("event_id", 32), checkTime("at"),
	) +
		immutable("orders", "id = NEW.id OR token = NEW.token OR merchant_ref = NEW.merchant_ref") +
		immutable("invoices", "payment_hash = NEW.payment_hash") +
		immutable("closed_invoices", "payment_hash = NEW.payment_hash") +
		immutable("invoice_attempts", "id = NEW.id") +
		immutable("events", "id = NEW.id OR (order_id = NEW.order_id AND type = NEW.type AND seq = NEW.seq)") +
		immutable("webhook_attempts", "event_id = NEW.event_id AND seq = NEW.seq") +
		// Parents must exist on any connection, whatever its foreign_keys setting. A close must not
		// precede the invoice's expiry (a "deadline" close, not before expiry + DeadlineGrace), or the
		// order could expire with a live invoice.
		fmt.Sprintf(`CREATE TRIGGER invoices_known_order BEFORE INSERT ON invoices
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
CREATE TRIGGER invoices_amount_matches_order BEFORE INSERT ON invoices
WHEN NEW.amount_msat != (SELECT amount_msat FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'invoice amount differs from the order'); END;
CREATE TRIGGER invoice_attempts_known_order BEFORE INSERT ON invoice_attempts
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
CREATE TRIGGER events_known_order BEFORE INSERT ON events
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
CREATE TRIGGER webhook_attempts_known_event BEFORE INSERT ON webhook_attempts
WHEN NOT EXISTS (SELECT 1 FROM events WHERE id = NEW.event_id)
BEGIN SELECT RAISE(ABORT, 'unknown event'); END;
CREATE TRIGGER closed_invoices_known_invoice BEFORE INSERT ON closed_invoices
WHEN NOT EXISTS (SELECT 1 FROM invoices WHERE payment_hash = NEW.payment_hash)
BEGIN SELECT RAISE(ABORT, 'unknown invoice'); END;
CREATE TRIGGER closed_invoices_not_early BEFORE INSERT ON closed_invoices
WHEN NEW.closed_at < (SELECT expires_at FROM invoices WHERE payment_hash = NEW.payment_hash)
  OR (NEW.reason = 'deadline' AND NEW.closed_at < (SELECT expires_at FROM invoices WHERE payment_hash = NEW.payment_hash) + %d)
BEGIN SELECT RAISE(ABORT, 'invoice closed before its time'); END;
`, int64(DeadlineGrace/time.Second)) +
		// The fact that an account has a paid order (AddAccountPaid), so that the "known" class is one
		// indexed read. One row per account; the order it names must exist and belong to it.
		fmt.Sprintf(`CREATE TABLE account_paid (
	account_id TEXT PRIMARY KEY CHECK (%s),
	first_order_id TEXT NOT NULL REFERENCES orders (id) CHECK (%s),
	created_at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkName("account_id"), checkLower("first_order_id", 32), checkTime("created_at")) +
		immutable("account_paid", "account_id = NEW.account_id") +
		`CREATE TRIGGER account_paid_known_order BEFORE INSERT ON account_paid
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.first_order_id AND account_id = NEW.account_id)
BEGIN SELECT RAISE(ABORT, 'unknown order for the account'); END;` +
		onchainSchema(),
	// 2: an output the watcher settled for good (recorded SettledDepth deep, by the count of the scan's tip and by
	// the count bitcoind's gettransaction answered with the mark in the same pass, the lower of the two);
	// height is the height of the scan it was settled against. WatchedTXOs leaves it out, in any process. A new entry,
	// so a database made by an earlier build of plan 6 gains the table.
	fmt.Sprintf(`CREATE TABLE onchain_settled (
	txid TEXT NOT NULL CHECK (%s),
	vout INTEGER NOT NULL CHECK (vout BETWEEN 0 AND 4294967295),
	height INTEGER NOT NULL CHECK (height >= 1),
	at INTEGER NOT NULL CHECK (%s),
	PRIMARY KEY (txid, vout)
) STRICT, WITHOUT ROWID;
`, checkLower("txid", 64), checkTime("at")) +
		immutable("onchain_settled", "txid = NEW.txid AND vout = NEW.vout") +
		`CREATE TRIGGER onchain_settled_known_txo BEFORE INSERT ON onchain_settled
WHEN NOT EXISTS (SELECT 1 FROM onchain_txos WHERE txid = NEW.txid AND vout = NEW.vout)
BEGIN SELECT RAISE(ABORT, 'unknown output'); END;
`,
	// 3: a settled fact may be deleted while its output's current mark is dead, and only then: the watcher settles a
	// dead on-time output past its window, and RecordScan reopens it when a scan lists it mined again (the sender
	// rebroadcast it). The marks keep the history (dead, then mined), so nothing is lost with the fact. Updates and
	// replaces stay refused.
	deadSettledDelete("onchain_settled", "onchain_marks", "txid = OLD.txid AND vout = OLD.vout"),
	// 4: the quote of a priced order: the price, currency and margin it was converted at, and the rate of each asset
	// with its sources (order_quote_rates), written in the order's transaction and never changed. An explicit order
	// has no row here.
	fmt.Sprintf(`CREATE TABLE order_quotes (
	order_id TEXT PRIMARY KEY REFERENCES orders (id) CHECK (%s),
	price TEXT NOT NULL CHECK (%s),
	currency TEXT NOT NULL CHECK (%s),
	margin_bps INTEGER NOT NULL CHECK (margin_bps BETWEEN %d AND %d),
	quoted_at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkLower("order_id", 32), checkDecimal("price"), checkCurrency("currency"),
		rates.MinMarginBps, rates.MaxMarginBps, checkTime("quoted_at")) +
		immutable("order_quotes", "order_id = NEW.order_id") +
		`CREATE TRIGGER order_quotes_known_order BEFORE INSERT ON order_quotes
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
` + fmt.Sprintf(`CREATE TABLE order_quote_rates (
	order_id TEXT NOT NULL REFERENCES order_quotes (order_id) CHECK (%s),
	asset TEXT NOT NULL CHECK (asset IN ('BTC', 'XMR')),
	rate TEXT NOT NULL CHECK (%s),
	sources TEXT NOT NULL CHECK (json_valid(sources) AND json_type(sources) = 'array'
	                             AND json_array_length(sources) BETWEEN 1 AND %d AND length(sources) <= 4096),
	PRIMARY KEY (order_id, asset)
) STRICT, WITHOUT ROWID;
`, checkLower("order_id", 32), checkDecimal("rate"), MaxQuoteSources) +
		immutable("order_quote_rates", "order_id = NEW.order_id AND asset = NEW.asset") +
		`CREATE TRIGGER order_quote_rates_known_quote BEFORE INSERT ON order_quote_rates
WHEN NOT EXISTS (SELECT 1 FROM order_quotes WHERE order_id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown quote'); END;
`,
	// 5: the Monero rail, the twin of the on-chain tables (onchainSchema and migrations 2 and 3).
	moneroSchema(),
	// 6: Lightning refunds (plan 9): the refund claimed from a signed approval, the one send marker, and the one
	// result. Insert-only; the triggers are the second layer of the state machine (refundSchema).
	refundSchema(),
	// 7: the pair stamp (plan 10): the one row that ties this store to the ledger it was written with. With
	// store_schema_version, the only table whose row is updated: pairSchema.
	pairSchema(),
	// 8: alerts to the operator (plan 10): the alert, its repeats in its hour, and its delivery attempts.
	// Insert-only: alertSchema.
	alertSchema(),
	// 9: webhook attempts per endpoint (plan 13): the first pass of each endpoint, and the attempts table rebuilt with
	// the endpoint in its key. Attempts recorded before belong to main: webhookEndpointSchema.
	webhookEndpointSchema(),
	// 10: the Lightning network the store was first started under (plan 13): networkSchema.
	networkSchema(),
	// 11: the outbox of abandoned events (plan 14) and the newest-invoice index: abandonedSchema.
	abandonedSchema(),
}

// webhookEndpointSchema is migration 9. webhook_endpoints holds one row per endpoint name: the time of its first
// delivery pass in this store, written once (RegisterEndpoint). The row of main is the oldest event's time, so main
// continues as the single webhook did; none is written when there are no events. The attempts table gains the
// endpoint in its key. It is rebuilt: made under a temporary name, filled with the old rows as main's, the old table
// dropped (with its triggers and index), the new one renamed, and only then its triggers and index created, on the
// final name.
func webhookEndpointSchema() string {
	return fmt.Sprintf(`CREATE TABLE webhook_endpoints (
	name TEXT PRIMARY KEY CHECK (%s),
	first_pass INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
INSERT INTO webhook_endpoints (name, first_pass) SELECT 'main', min(created_at) FROM events HAVING count(*) > 0;
CREATE TABLE webhook_attempts_new (
	endpoint TEXT NOT NULL CHECK (%s),
	event_id TEXT NOT NULL REFERENCES events (id) CHECK (%s),
	seq INTEGER NOT NULL CHECK (seq >= 1),
	at INTEGER NOT NULL CHECK (%s),
	status INTEGER NOT NULL CHECK (status BETWEEN 0 AND 999),
	ok INTEGER NOT NULL CHECK (ok IN (0, 1)),
	PRIMARY KEY (endpoint, event_id, seq)
) STRICT, WITHOUT ROWID;
INSERT INTO webhook_attempts_new (endpoint, event_id, seq, at, status, ok)
	SELECT 'main', event_id, seq, at, status, ok FROM webhook_attempts;
DROP TABLE webhook_attempts;
ALTER TABLE webhook_attempts_new RENAME TO webhook_attempts;
CREATE INDEX webhook_attempts_ok ON webhook_attempts (endpoint, event_id) WHERE ok = 1;
CREATE TRIGGER webhook_attempts_known_event BEFORE INSERT ON webhook_attempts
WHEN NOT EXISTS (SELECT 1 FROM events WHERE id = NEW.event_id)
BEGIN SELECT RAISE(ABORT, 'unknown event'); END;
`, checkName("name"), checkTime("first_pass"), checkName("endpoint"), checkLower("event_id", 32), checkTime("at")) +
		immutable("webhook_endpoints", "name = NEW.name") +
		immutable("webhook_attempts", "endpoint = NEW.endpoint AND event_id = NEW.event_id AND seq = NEW.seq")
}

// abandonedSchema is migration 11. webhook_abandoned is the outbox of the give-up line: AddAttempt writes a row, in
// the transaction of an endpoint's last failed attempt of an event, and the deliverer deletes it once it has logged the
// line (ClearAbandoned), so a row is "abandoned, not yet announced" and the table holds only those. A row is never
// updated or replaced and names a stored event; the delete guard is left off on purpose, as for
// monero_pending_settle. The index serves NewestInvoice, which would otherwise sort every invoice.
func abandonedSchema() string {
	return fmt.Sprintf(`CREATE TABLE webhook_abandoned (
	endpoint TEXT NOT NULL CHECK (%s),
	event_id TEXT NOT NULL REFERENCES events (id) CHECK (%s),
	at INTEGER NOT NULL CHECK (%s),
	PRIMARY KEY (endpoint, event_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX webhook_abandoned_at ON webhook_abandoned (endpoint, at, event_id);
CREATE TRIGGER webhook_abandoned_no_update BEFORE UPDATE ON webhook_abandoned
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
CREATE TRIGGER webhook_abandoned_no_replace BEFORE INSERT ON webhook_abandoned
WHEN EXISTS (SELECT 1 FROM webhook_abandoned WHERE endpoint = NEW.endpoint AND event_id = NEW.event_id)
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
CREATE TRIGGER webhook_abandoned_known_event BEFORE INSERT ON webhook_abandoned
WHEN NOT EXISTS (SELECT 1 FROM events WHERE id = NEW.event_id)
BEGIN SELECT RAISE(ABORT, 'unknown event'); END;
CREATE INDEX invoices_created ON invoices (created_at DESC, payment_hash DESC);
`, checkName("endpoint"), checkLower("event_id", 32), checkTime("at"))
}

// checkAlertMessage: printable ASCII starting "ALERT:", 6 to maxAlertMessage bytes (validAlert's rule).
func checkAlertMessage(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) BETWEEN 6 AND %[2]d AND %[1]s GLOB 'ALERT:*' AND %[1]s NOT GLOB '*[^ -~]*'`,
		col, maxAlertMessage)
}

// checkAlertFields: a flat JSON object of printable ASCII, 2 to maxAlertFields bytes. Flat is checked as no "{" or
// "[" after the first byte, which the allowed values never contain, so Go and SQL need no JSON depth rule.
// json_type is behind a CASE: it raises an error on malformed JSON instead of being false, and AND does not promise to
// stop at json_valid.
func checkAlertFields(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) BETWEEN 2 AND %[2]d AND %[1]s GLOB '{*' AND %[1]s NOT GLOB '*[^ -~]*'`+
		` AND substr(%[1]s, 2) NOT GLOB '*[[{]*' AND CASE WHEN json_valid(%[1]s) THEN json_type(%[1]s) = 'object' ELSE 0 END`, col, maxAlertFields)
}

func checkAlertDropped(col string) string {
	return fmt.Sprintf(`%[1]s BETWEEN 0 AND %[2]d`, col, maxAlertDropped)
}

// alertSchema is migration 8. An alert's message is printable ASCII starting "ALERT:"; its fields are the sorted-key
// JSON object the id was derived from. A hit is a repeat of the alert in its hour bucket: at most MaxAlertRepeats-1
// of them, and none once the alert is delivered, so the table is bounded. alert_attempts is webhook_attempts without
// the endpoint.
func alertSchema() string {
	return fmt.Sprintf(`CREATE TABLE alerts (
	id TEXT PRIMARY KEY CHECK (%s),
	message TEXT NOT NULL CHECK (%s),
	fields TEXT NOT NULL CHECK (%s),
	dropped INTEGER NOT NULL CHECK (%s),
	created_at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
CREATE INDEX alerts_created ON alerts (created_at);

CREATE TABLE alert_hits (
	alert_id TEXT NOT NULL REFERENCES alerts (id) CHECK (%s),
	seq INTEGER NOT NULL CHECK (seq BETWEEN 1 AND %d),
	at INTEGER NOT NULL CHECK (%s),
	PRIMARY KEY (alert_id, seq)
) STRICT, WITHOUT ROWID;

CREATE TABLE alert_attempts (
	alert_id TEXT NOT NULL REFERENCES alerts (id) CHECK (%s),
	seq INTEGER NOT NULL CHECK (seq >= 1),
	at INTEGER NOT NULL CHECK (%s),
	status INTEGER NOT NULL CHECK (status BETWEEN 0 AND 999),
	ok INTEGER NOT NULL CHECK (ok IN (0, 1)),
	PRIMARY KEY (alert_id, seq)
) STRICT, WITHOUT ROWID;
CREATE INDEX alert_attempts_ok ON alert_attempts (alert_id) WHERE ok = 1;
`,
		checkLower("id", 32), checkAlertMessage("message"), checkAlertFields("fields"), checkAlertDropped("dropped"), checkTime("created_at"),
		checkLower("alert_id", 32), MaxAlertRepeats-1, checkTime("at"),
		checkLower("alert_id", 32), checkTime("at"),
	) +
		immutable("alerts", "id = NEW.id") +
		immutable("alert_hits", "alert_id = NEW.alert_id AND seq = NEW.seq") +
		immutable("alert_attempts", "alert_id = NEW.alert_id AND seq = NEW.seq") +
		`CREATE TRIGGER alert_hits_known_alert BEFORE INSERT ON alert_hits
WHEN NOT EXISTS (SELECT 1 FROM alerts WHERE id = NEW.alert_id)
BEGIN SELECT RAISE(ABORT, 'unknown alert'); END;
CREATE TRIGGER alert_hits_not_delivered BEFORE INSERT ON alert_hits
WHEN EXISTS (SELECT 1 FROM alert_attempts WHERE alert_id = NEW.alert_id AND ok = 1)
BEGIN SELECT RAISE(ABORT, 'the alert is delivered'); END;
CREATE TRIGGER alert_attempts_known_alert BEFORE INSERT ON alert_attempts
WHEN NOT EXISTS (SELECT 1 FROM alerts WHERE id = NEW.alert_id)
BEGIN SELECT RAISE(ABORT, 'unknown alert'); END;
`
}

// pairSchema is the store's pair table. It holds exactly one row: the key CHECK allows no second row, and the
// triggers refuse a delete and any insert once the row exists (which also refuses REPLACE and an upsert). An update
// may set the id once and never change it afterwards, and never lowers the generation. nonce is the random value the
// advance to this generation drew (the ledger holds the same one): NULL exactly while the generation is 0. A
// pre-existing database gets the row with no id, generation 0 and no nonce; the first start stamps it
// (service.checkPair).
func pairSchema() string {
	return fmt.Sprintf(`CREATE TABLE pair (
	one INTEGER PRIMARY KEY CHECK (one = 1),
	pair_id TEXT CHECK (pair_id IS NULL OR (%s)),
	generation INTEGER NOT NULL CHECK (generation >= 0),
	nonce TEXT CHECK (nonce IS NULL OR (%s)),
	CHECK ((generation = 0) = (nonce IS NULL))
) STRICT, WITHOUT ROWID;
INSERT INTO pair (one, pair_id, generation, nonce) VALUES (1, NULL, 0, NULL);
CREATE TRIGGER pair_no_delete BEFORE DELETE ON pair
BEGIN SELECT RAISE(ABORT, 'the pair row is never deleted'); END;
CREATE TRIGGER pair_no_insert BEFORE INSERT ON pair
WHEN EXISTS (SELECT 1 FROM pair)
BEGIN SELECT RAISE(ABORT, 'the pair table has one row'); END;
CREATE TRIGGER pair_update_rules BEFORE UPDATE ON pair
WHEN NEW.one IS NOT OLD.one OR (OLD.pair_id IS NOT NULL AND NEW.pair_id IS NOT OLD.pair_id) OR NEW.generation < OLD.generation
BEGIN SELECT RAISE(ABORT, 'the pair id is set once and the generation never goes back'); END;
`, checkLower("pair_id", 32), checkLower("nonce", 32))
}

// refundApprovalHex is the length of a refund approval in lowercase hex: 148 bytes.
const refundApprovalHex = 296

// refundInvoiceMax is the longest claimant invoice the store holds, in characters.
const refundInvoiceMax = 2048

// checkRefundInvoice: the SQL side of the claimant-invoice rule: 1 to 2048 characters of [0-9a-z] starting "ln". The
// hrp, network and checksum rules stay in Go (lninvoice).
func checkRefundInvoice(col string) string {
	return fmt.Sprintf(asciiOnly+` AND length(%[1]s) BETWEEN 1 AND %[2]d AND %[1]s NOT GLOB '*[^0-9a-z]*' AND substr(%[1]s, 1, 2) = 'ln'`,
		col, refundInvoiceMax)
}

// checkRefundAmount: 1 to 2^53 msat and a whole number of sats.
func checkRefundAmount(col string) string {
	return fmt.Sprintf(`%s AND %s %% 1000 = 0`, checkAmount(col), col)
}

// checkRefundPair: a settled result has the reason settled or ledger_paid; a failed one has one of the failure reasons.
func checkRefundPair(outcome, reason string) string {
	return fmt.Sprintf(`((%[1]s = 'settled' AND %[2]s IN ('settled', 'ledger_paid')) OR (%[1]s = 'failed' AND %[2]s IN (%[3]s)))`,
		outcome, reason, refundReasons)
}

// refundDomainHex is the hex of the ASCII domain "satbooth/refund-approval/v1" (27 bytes, 54 characters), the start of
// every refund approval.
const refundDomainHex = "736174626f6f74682f726566756e642d617070726f76616c2f7631"

// checkRefundTies: every signed field that the row repeats equals the approval's bytes. Offsets are 1-based hex
// positions (byte offset × 2 + 1): the domain at byte 0 (54 characters), the network at byte 27 (0 to 3), key id at
// byte 28, refund id at byte 36, order id at byte 52, amount_msat at byte 68 and expires_at at byte 76 (each 16
// characters, big-endian). printf('%016x', n) is the same 16 characters for a positive n.
const checkRefundTies = `id = substr(approval, 73, 32) AND order_id = substr(approval, 105, 32) AND key_id = substr(approval, 57, 16)` +
	` AND printf('%016x', amount_msat) = substr(approval, 137, 16) AND printf('%016x', expires_at) = substr(approval, 153, 16)` +
	` AND substr(approval, 1, 54) = '` + refundDomainHex + `' AND substr(approval, 55, 2) IN ('00', '01', '02', '03')`

// refundReasons are the reasons of a failed refund.
const refundReasons = `'expired_unsent', 'key_revoked', 'invoice_known', 'not_sent', 'failed', 'voided', 'recovered', 'cancelled'`

// refundSchema is migration 6: refunds, refund_sends and refund_results, with the index the invoice-known check uses.
func refundSchema() string {
	return fmt.Sprintf(`CREATE TABLE refunds (
	id TEXT PRIMARY KEY CHECK (%s),
	order_id TEXT NOT NULL REFERENCES orders (id) CHECK (%s),
	invoice TEXT NOT NULL UNIQUE CHECK (%s),
	amount_msat INTEGER NOT NULL CHECK (%s),
	key_id TEXT NOT NULL CHECK (%s),
	approval TEXT NOT NULL CHECK (%s),
	claimed_at INTEGER NOT NULL CHECK (%s),
	expires_at INTEGER NOT NULL CHECK (%s AND expires_at > claimed_at),
	CHECK (%s)
) STRICT, WITHOUT ROWID;
CREATE INDEX refunds_order ON refunds (order_id);
CREATE INDEX refunds_claimed ON refunds (claimed_at);
`, checkLower("id", 32), checkLower("order_id", 32), checkRefundInvoice("invoice"), checkRefundAmount("amount_msat"),
		checkLower("key_id", 16), checkLower("approval", refundApprovalHex), checkTime("claimed_at"), checkTime("expires_at"),
		checkRefundTies) +
		immutable("refunds", "id = NEW.id OR invoice = NEW.invoice") +
		`CREATE TRIGGER refunds_known_order BEFORE INSERT ON refunds
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
` + fmt.Sprintf(`CREATE TABLE refund_sends (
	refund_id TEXT PRIMARY KEY REFERENCES refunds (id) CHECK (%s),
	sent_at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkLower("refund_id", 32), checkTime("sent_at")) +
		immutable("refund_sends", "refund_id = NEW.refund_id") +
		`CREATE TRIGGER refund_sends_known_refund BEFORE INSERT ON refund_sends
WHEN NOT EXISTS (SELECT 1 FROM refunds WHERE id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'unknown refund'); END;
CREATE TRIGGER refund_sends_not_resolved BEFORE INSERT ON refund_sends
WHEN EXISTS (SELECT 1 FROM refund_results WHERE refund_id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'refund already resolved'); END;
CREATE TRIGGER refund_sends_in_time BEFORE INSERT ON refund_sends
WHEN NEW.sent_at < (SELECT claimed_at FROM refunds WHERE id = NEW.refund_id)
  OR NEW.sent_at >= (SELECT expires_at FROM refunds WHERE id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'send outside the approval window'); END;
` + fmt.Sprintf(`CREATE TABLE refund_results (
	refund_id TEXT PRIMARY KEY REFERENCES refunds (id) CHECK (%s),
	outcome TEXT NOT NULL CHECK (outcome IN ('settled', 'failed')),
	reason TEXT NOT NULL,
	resolved_at INTEGER NOT NULL CHECK (%s),
	CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkLower("refund_id", 32), checkTime("resolved_at"), checkRefundPair("outcome", "reason")) +
		immutable("refund_results", "refund_id = NEW.refund_id") +
		`CREATE TRIGGER refund_results_known_refund BEFORE INSERT ON refund_results
WHEN NOT EXISTS (SELECT 1 FROM refunds WHERE id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'unknown refund'); END;
CREATE TRIGGER refund_results_needs_send BEFORE INSERT ON refund_results
WHEN NEW.reason IN ('settled', 'not_sent', 'failed', 'voided')
  AND NOT EXISTS (SELECT 1 FROM refund_sends WHERE refund_id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'result needs a send'); END;
CREATE TRIGGER refund_results_needs_no_send BEFORE INSERT ON refund_results
WHEN NEW.reason IN ('expired_unsent', 'key_revoked', 'invoice_known', 'cancelled', 'ledger_paid')
  AND EXISTS (SELECT 1 FROM refund_sends WHERE refund_id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'result needs no send'); END;
CREATE TRIGGER refund_results_after_claim BEFORE INSERT ON refund_results
WHEN NEW.resolved_at < (SELECT claimed_at FROM refunds WHERE id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'result before the claim'); END;
CREATE TRIGGER refund_results_after_send BEFORE INSERT ON refund_results
WHEN NEW.resolved_at < (SELECT sent_at FROM refund_sends WHERE refund_id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'result before the send'); END;
` + fmt.Sprintf(`CREATE TRIGGER refund_results_void_waits BEFORE INSERT ON refund_results
WHEN NEW.reason = 'voided'
  AND NEW.resolved_at < (SELECT sent_at FROM refund_sends WHERE refund_id = NEW.refund_id) + %d
BEGIN SELECT RAISE(ABORT, 'void before its wait'); END;
`, int64(RefundVoidWait/time.Second)) + `CREATE TRIGGER refund_results_expiry_passed BEFORE INSERT ON refund_results
WHEN NEW.reason = 'expired_unsent'
  AND NEW.resolved_at < (SELECT expires_at FROM refunds WHERE id = NEW.refund_id)
BEGIN SELECT RAISE(ABORT, 'expired_unsent before the approval expired'); END;
CREATE INDEX invoices_bolt11_lower ON invoices (lower(bolt11));
`
}

// onchainSchema is the on-chain part of migration 1: the address of each order, the outputs paid to
// those addresses, their marks (mempool, mined, dead), and the cursor of the chain scan.
func onchainSchema() string {
	return fmt.Sprintf(`
CREATE TABLE addresses (
	address TEXT PRIMARY KEY CHECK (%s),
	order_id TEXT NOT NULL UNIQUE REFERENCES orders (id) CHECK (%s),
	idx INTEGER NOT NULL UNIQUE CHECK (idx BETWEEN 0 AND %d),
	created_at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkAddress("address"), checkLower("order_id", 32), int64(MaxAddressIdx), checkTime("created_at")) +
		immutable("addresses", "address = NEW.address OR order_id = NEW.order_id OR idx = NEW.idx") +
		`CREATE TRIGGER addresses_known_order BEFORE INSERT ON addresses
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
CREATE TRIGGER addresses_index_increases BEFORE INSERT ON addresses
WHEN NEW.idx <= coalesce((SELECT max(idx) FROM addresses), -1)
BEGIN SELECT RAISE(ABORT, 'address index must exceed every stored index'); END;
CREATE TRIGGER addresses_order_onchain BEFORE INSERT ON addresses
WHEN (SELECT amount_sat FROM orders WHERE id = NEW.order_id) = 0
BEGIN SELECT RAISE(ABORT, 'the order has no on-chain amount'); END;
` + fmt.Sprintf(`
CREATE TABLE onchain_txos (
	txid TEXT NOT NULL CHECK (%s),
	vout INTEGER NOT NULL CHECK (vout BETWEEN 0 AND 4294967295),
	address TEXT NOT NULL REFERENCES addresses (address) CHECK (%s),
	amount_sat INTEGER NOT NULL CHECK (amount_sat BETWEEN 1 AND %d),
	first_seen INTEGER NOT NULL CHECK (%s),
	PRIMARY KEY (txid, vout)
) STRICT, WITHOUT ROWID;
CREATE INDEX onchain_txos_address ON onchain_txos (address);
`, checkLower("txid", 64), checkAddress("address"), int64(MaxSat), checkTime("first_seen")) +
		immutable("onchain_txos", "txid = NEW.txid AND vout = NEW.vout") +
		`CREATE TRIGGER onchain_txos_known_address BEFORE INSERT ON onchain_txos
WHEN NOT EXISTS (SELECT 1 FROM addresses WHERE address = NEW.address)
BEGIN SELECT RAISE(ABORT, 'unknown address'); END;
` + fmt.Sprintf(`
CREATE TABLE onchain_marks (
	txid TEXT NOT NULL CHECK (%s),
	vout INTEGER NOT NULL CHECK (vout BETWEEN 0 AND 4294967295),
	seq INTEGER NOT NULL CHECK (seq >= 1),
	kind TEXT NOT NULL CHECK (kind IN ('mempool', 'mined', 'dead')),
	height INTEGER NOT NULL,
	block_hash TEXT NOT NULL,
	at INTEGER NOT NULL CHECK (%s),
	PRIMARY KEY (txid, vout, seq),
	CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkLower("txid", 64), checkTime("at"), checkMarkShape("kind", "height", "block_hash")) +
		immutable("onchain_marks", "txid = NEW.txid AND vout = NEW.vout AND seq = NEW.seq") +
		`CREATE TRIGGER onchain_marks_known_txo BEFORE INSERT ON onchain_marks
WHEN NOT EXISTS (SELECT 1 FROM onchain_txos WHERE txid = NEW.txid AND vout = NEW.vout)
BEGIN SELECT RAISE(ABORT, 'unknown output'); END;
` + fmt.Sprintf(`
CREATE TABLE onchain_cursor (
	seq INTEGER PRIMARY KEY CHECK (seq >= 1),
	block_hash TEXT NOT NULL CHECK (%s),
	height INTEGER NOT NULL CHECK (height >= 0),
	at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkLower("block_hash", 64), checkTime("at")) +
		immutable("onchain_cursor", "seq = NEW.seq")
}

// moneroSchema is migration 5: the subaddress of each Monero order, the payments to those subaddresses, their marks
// (mempool, mined, dead; no block hash; a mined one with wallet-rpc's confirmations, ruling 8-A6), the cursor of the wallet scan, and the settled facts, whose delete guard is
// the dead-mark rule of migration 3; and the orders pending settle (ruling S4), the store's one work queue: a row is
// added by the pass's commit and deleted once its order has settled, so this table alone has no delete guard (its
// rows are never updated or replaced).
func moneroSchema() string {
	return fmt.Sprintf(`CREATE TABLE monero_subaddresses (
	address TEXT PRIMARY KEY CHECK (%s),
	order_id TEXT NOT NULL UNIQUE REFERENCES orders (id) CHECK (%s),
	idx INTEGER NOT NULL UNIQUE CHECK (idx BETWEEN 1 AND %d),
	created_at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkSubaddress("address"), checkLower("order_id", 32), int64(MaxSubaddressIdx), checkTime("created_at")) +
		immutable("monero_subaddresses", "address = NEW.address OR order_id = NEW.order_id OR idx = NEW.idx") +
		`CREATE TRIGGER monero_subaddresses_known_order BEFORE INSERT ON monero_subaddresses
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
CREATE TRIGGER monero_subaddresses_index_increases BEFORE INSERT ON monero_subaddresses
WHEN NEW.idx <= coalesce((SELECT max(idx) FROM monero_subaddresses), 0)
BEGIN SELECT RAISE(ABORT, 'subaddress index must exceed every stored index'); END;
CREATE TRIGGER monero_subaddresses_order_monero BEFORE INSERT ON monero_subaddresses
WHEN coalesce((SELECT amount_piconero FROM orders WHERE id = NEW.order_id), 0) <= 0
BEGIN SELECT RAISE(ABORT, 'the order has no Monero amount'); END;
` + fmt.Sprintf(`
CREATE TABLE monero_payments (
	txid TEXT NOT NULL CHECK (%s),
	address TEXT NOT NULL REFERENCES monero_subaddresses (address) CHECK (%s),
	amount_piconero INTEGER NOT NULL CHECK (%s),
	unlock_time INTEGER NOT NULL CHECK (unlock_time >= 0),
	first_seen INTEGER NOT NULL CHECK (%s),
	first_seen_height INTEGER NOT NULL CHECK (first_seen_height >= 0),
	PRIMARY KEY (txid, address)
) STRICT, WITHOUT ROWID;
CREATE INDEX monero_payments_address ON monero_payments (address);
`, checkLower("txid", 64), checkSubaddress("address"), checkPiconero("amount_piconero"), checkTime("first_seen")) +
		immutable("monero_payments", "txid = NEW.txid AND address = NEW.address") +
		`CREATE TRIGGER monero_payments_known_subaddress BEFORE INSERT ON monero_payments
WHEN NOT EXISTS (SELECT 1 FROM monero_subaddresses WHERE address = NEW.address)
BEGIN SELECT RAISE(ABORT, 'unknown subaddress'); END;
` + fmt.Sprintf(`
CREATE TABLE monero_marks (
	txid TEXT NOT NULL CHECK (%s),
	address TEXT NOT NULL CHECK (%s),
	seq INTEGER NOT NULL CHECK (seq >= 1),
	kind TEXT NOT NULL CHECK (kind IN ('mempool', 'mined', 'dead')),
	height INTEGER NOT NULL,
	at INTEGER NOT NULL CHECK (%s),
	confirmations INTEGER NOT NULL,
	PRIMARY KEY (txid, address, seq),
	CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkLower("txid", 64), checkSubaddress("address"), checkTime("at"), checkMoneroMarkShape("kind", "height", "confirmations")) +
		immutable("monero_marks", "txid = NEW.txid AND address = NEW.address AND seq = NEW.seq") +
		`CREATE TRIGGER monero_marks_known_payment BEFORE INSERT ON monero_marks
WHEN NOT EXISTS (SELECT 1 FROM monero_payments WHERE txid = NEW.txid AND address = NEW.address)
BEGIN SELECT RAISE(ABORT, 'unknown payment'); END;
` + fmt.Sprintf(`
CREATE TABLE monero_cursor (
	seq INTEGER PRIMARY KEY CHECK (seq >= 1),
	height INTEGER NOT NULL CHECK (height >= 0),
	wallet_height INTEGER NOT NULL CHECK (wallet_height BETWEEN 0 AND height),
	at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
`, checkTime("at")) +
		immutable("monero_cursor", "seq = NEW.seq") + fmt.Sprintf(`
CREATE TABLE monero_settled (
	txid TEXT NOT NULL CHECK (%s),
	address TEXT NOT NULL CHECK (%s),
	height INTEGER NOT NULL CHECK (height >= 1),
	at INTEGER NOT NULL CHECK (%s),
	PRIMARY KEY (txid, address)
) STRICT, WITHOUT ROWID;
`, checkLower("txid", 64), checkSubaddress("address"), checkTime("at")) +
		immutable("monero_settled", "txid = NEW.txid AND address = NEW.address") +
		`CREATE TRIGGER monero_settled_known_payment BEFORE INSERT ON monero_settled
WHEN NOT EXISTS (SELECT 1 FROM monero_payments WHERE txid = NEW.txid AND address = NEW.address)
BEGIN SELECT RAISE(ABORT, 'unknown payment'); END;
` + deadSettledDelete("monero_settled", "monero_marks", "txid = OLD.txid AND address = OLD.address") + fmt.Sprintf(`
CREATE TABLE monero_pending_settle (
	order_id TEXT PRIMARY KEY REFERENCES orders (id) CHECK (%s),
	at INTEGER NOT NULL CHECK (%s)
) STRICT, WITHOUT ROWID;
CREATE TRIGGER monero_pending_settle_no_update BEFORE UPDATE ON monero_pending_settle
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
CREATE TRIGGER monero_pending_settle_no_replace BEFORE INSERT ON monero_pending_settle
WHEN EXISTS (SELECT 1 FROM monero_pending_settle WHERE order_id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'store rows are immutable'); END;
CREATE TRIGGER monero_pending_settle_known_order BEFORE INSERT ON monero_pending_settle
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN SELECT RAISE(ABORT, 'unknown order'); END;
`, checkLower("order_id", 32), checkTime("at"))
}

// checkMoneroMarkShape: a "mined" mark has a height of at least 1 and 0 to MaxMoneroConfirmations confirmations;
// "mempool" and "dead" marks have height 0 and no confirmation (ruling 8-A6). kind, height and confirmations are SQL
// expressions.
func checkMoneroMarkShape(kind, height, confirmations string) string {
	return fmt.Sprintf(`((%[1]s = 'mined' AND %[2]s >= 1 AND %[3]s BETWEEN 0 AND %[4]d) OR (%[1]s IN ('mempool', 'dead') AND %[2]s = 0 AND %[3]s = 0))`,
		kind, height, confirmations, int64(MaxMoneroConfirmations))
}

// migrate applies the migrations the database has not seen, in one transaction.
func (s *Store) migrate(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS store_schema_version (version INTEGER NOT NULL) STRICT`); err != nil {
			return err
		}
		var have, rows int
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(max(version), 0) FROM store_schema_version`).Scan(&rows, &have); err != nil {
			return err
		}
		if rows > 1 {
			return fmt.Errorf("store: store_schema_version has %d rows", rows)
		}
		if have > len(migrations) {
			return fmt.Errorf("store: database schema version %d is newer than this build (%d)", have, len(migrations))
		}
		if rows == 1 {
			// A database from before plan 8 has a version row but an orders table without amount_piconero (and,
			// before plan 6, without amount_sat). Migrations 1 and 4 were edited in place (no release existed), so
			// refuse it.
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('orders') WHERE name = 'amount_piconero'`).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrOldSchema
			}
			// A version-5 database from an early plan 8 build has a monero_cursor without wallet_height (ruling R5),
			// monero_marks without confirmations (ruling 8-A6), or no monero_pending_settle (ruling S4; a missing table
			// has no columns): migration 5 was edited in place for each, so refuse it the same way rather than fail at
			// the first cursor, mark or pass.
			if have >= 5 {
				for _, c := range []struct{ table, column string }{{"monero_cursor", "wallet_height"}, {"monero_marks", "confirmations"},
					{"monero_pending_settle", "order_id"}} {
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
						return err
					}
					if n == 0 {
						return ErrOldMoneroSchema
					}
				}
			}
			// A database whose refund_results was made before plan 11 does not know the reason cancelled: migration 6
			// was edited in place for it (no release existed), so refuse it the same way.
			if have >= 6 {
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'refund_results' AND instr(sql, '''cancelled''') > 0`).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					return ErrOldRefundSchema
				}
			}
		}
		if have == len(migrations) {
			return nil
		}
		for i := have; i < len(migrations); i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("store: migration %d: %w", i+1, err)
			}
		}
		if rows == 0 {
			_, err := tx.ExecContext(ctx, `INSERT INTO store_schema_version (version) VALUES (?)`, len(migrations))
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE store_schema_version SET version = ?`, len(migrations))
		return err
	})
}
