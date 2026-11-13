package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// invalidEndpoint is the one refusal of a bad endpoint name.
func invalidEndpoint() error {
	return invalid("endpoint name must be 1 to 64 characters of A-Z a-z 0-9 . _ -")
}

// maxEventSeq bounds the sequence number of a repeating event (a reversal count nobody reaches).
const maxEventSeq = 1 << 20

// AddEvent stores a webhook event for an existing order (ErrNotFound otherwise) and returns true,
// or false if the event already exists. An empty ID is derived with EventID; a given ID must equal
// it. CreatedAt is always stamped from the store's clock; a caller-set value is ignored.
//
// Seq 0 means 1. Only order.reverted and order.paid_again take a Seq above 1. An order.reverted's Seq
// must be the number of the order's reverted events plus 1, or an existing one (then false); an
// order.paid_again's Seq must name an existing order.reverted. Otherwise ErrInvalid.
//
// One final event per order, for every rail and lnpay alike: an order.expired or order.underpaid is not added (false,
// nothing written) when the order already has an order.paid, order.expired or order.underpaid, checked in the write
// transaction, so two settles racing on one order never store contradicting final events. The rule is one-directional:
// order.paid after a final event is added (a payment that arrives late still pays the order), and so are order.late,
// order.reverted and order.paid_again.
func (s *Store) AddEvent(ctx context.Context, e Event) (bool, error) {
	switch e.Type {
	case EventPaid, EventExpired, EventUnderpaid, EventLate, EventReverted, EventPaidAgain:
	default:
		return false, invalid("unknown event type")
	}
	if !validLowerHex(e.OrderID, 32) {
		return false, invalid("order id must be 32 lowercase hex characters")
	}
	seq := e.Seq
	if seq == 0 {
		seq = 1
	}
	if seq < 1 || seq > maxEventSeq || (seq != 1 && e.Type != EventReverted && e.Type != EventPaidAgain) {
		return false, invalid("event sequence number is out of range for the type")
	}
	id := EventID(e.Type, e.OrderID, seq)
	if e.ID != "" && e.ID != id {
		return false, invalid("event id is not the derived id")
	}
	var added bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM orders WHERE id = ?`, e.OrderID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, id).Scan(&one)
		if err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		switch e.Type {
		case EventExpired, EventUnderpaid:
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE order_id = ? AND type IN ('order.paid', 'order.expired', 'order.underpaid') LIMIT 1`,
				e.OrderID).Scan(&one)
			if err == nil {
				return nil // the order already has its final event
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		case EventReverted:
			// Reversals are numbered in order: the next one is the count plus 1.
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE order_id = ? AND type = ?`, e.OrderID, EventReverted).Scan(&n); err != nil {
				return err
			}
			if seq != n+1 {
				return invalid("order.reverted sequence number must be the next one")
			}
		case EventPaidAgain:
			// Each payment after a reversal answers one reversal.
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, EventID(EventReverted, e.OrderID, seq)).Scan(&one); errors.Is(err, sql.ErrNoRows) {
				return invalid("order.paid_again needs the order.reverted with the same sequence number")
			} else if err != nil {
				return err
			}
		}
		created := s.now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (id, type, order_id, seq, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, e.Type, e.OrderID, seq, ts(created)); err != nil {
			return err
		}
		added = true
		return nil
	})
	// The transaction has ended and the write lock is released. Whatever the commit's error, a spurious wake-up costs
	// a watcher one pass and a missing one up to its fallback.
	if added {
		s.eventWatch.wake()
	}
	return added, err
}

// EventsForOrder returns the order's events, oldest first (then by type and seq: the store's clock has whole
// seconds). An unknown or malformed order id gives none.
func (s *Store) EventsForOrder(ctx context.Context, orderID string) ([]Event, error) {
	if !validLowerHex(orderID, 32) {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, type, order_id, seq, created_at FROM events WHERE order_id = ?
ORDER BY created_at, type, seq`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var created int64
		if err := rows.Scan(&e.ID, &e.Type, &e.OrderID, &e.Seq, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = fromTS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PendingEvent is an undelivered event with the facts a deliverer needs to apply its own retry
// policy: how many delivery attempts have failed and when the last one was made.
type PendingEvent struct {
	Event
	Failures int       // number of delivery attempts recorded (none succeeded, or the event would not be listed)
	LastAt   time.Time // time of the latest attempt; the zero time if there is none
}

// UndeliveredEvents returns the events created at or after since that have no successful attempt for endpoint, oldest
// first, with that endpoint's failure count and latest attempt. Of those it lists the ones created at or after admit
// and the ones that already have an attempt for endpoint (an event once tried stays listed until it is delivered or
// given up, whatever admit does); an admit at or before since lists everything from since. It states facts only: when
// to retry, and when to give up, are the caller's policy (package webhook). Events older than since are not listed, so
// a caller that logs "gave up" passes a since older than its retry window. The endpoint's name must be valid
// (ErrInvalid); an endpoint with no attempts is listed the events from admit on.
func (s *Store) UndeliveredEvents(ctx context.Context, endpoint string, since, admit time.Time) ([]PendingEvent, error) {
	if !ValidEndpoint(endpoint) {
		return nil, invalidEndpoint()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.type, e.order_id, e.seq, e.created_at,
  (SELECT count(*) FROM webhook_attempts a WHERE a.endpoint = ?1 AND a.event_id = e.id),
  (SELECT coalesce(max(a.at), 0) FROM webhook_attempts a WHERE a.endpoint = ?1 AND a.event_id = e.id)
FROM events e
WHERE e.created_at >= ?2
  AND NOT EXISTS (SELECT 1 FROM webhook_attempts a WHERE a.endpoint = ?1 AND a.event_id = e.id AND a.ok = 1)
  AND (e.created_at >= ?3 OR EXISTS (SELECT 1 FROM webhook_attempts a WHERE a.endpoint = ?1 AND a.event_id = e.id))
ORDER BY e.created_at, e.id`, endpoint, ts(since), ts(admit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEvent
	for rows.Next() {
		var p PendingEvent
		var created, last int64
		if err := rows.Scan(&p.ID, &p.Type, &p.OrderID, &p.Seq, &created, &p.Failures, &last); err != nil {
			return nil, err
		}
		p.CreatedAt = fromTS(created)
		if p.Failures > 0 {
			p.LastAt = fromTS(last)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddAttempt records one delivery attempt of a webhook event for a.Endpoint (not an invoice attempt; see
// AddInvoiceAttempt). The sequence number is per event and endpoint. The endpoint's name must be valid (ErrInvalid),
// the event must exist (ErrNotFound) and Status must be 0 (no response) or an HTTP status up to 999. A zero At is
// stamped from the clock.
func (s *Store) AddAttempt(ctx context.Context, a Attempt) error {
	if !ValidEndpoint(a.Endpoint) {
		return invalidEndpoint()
	}
	if !validLowerHex(a.EventID, 32) {
		return invalid("event id must be 32 lowercase hex characters")
	}
	if a.Status < 0 || a.Status > 999 {
		return invalid("status must be 0 to 999")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, a.EventID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var seq int
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) + 1 FROM webhook_attempts WHERE endpoint = ? AND event_id = ?`,
			a.Endpoint, a.EventID).Scan(&seq); err != nil {
			return err
		}
		at := a.At
		if at.IsZero() {
			at = s.now()
		}
		ok := 0
		if a.OK {
			ok = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_attempts (endpoint, event_id, seq, at, status, ok) VALUES (?, ?, ?, ?, ?, ?)`,
			a.Endpoint, a.EventID, seq, ts(at), a.Status, ok); err != nil {
			return err
		}
		if a.Final && !a.OK {
			_, err := tx.ExecContext(ctx, `INSERT INTO webhook_abandoned (endpoint, event_id, at) VALUES (?, ?, ?)`, a.Endpoint, a.EventID, ts(at))
			return err
		}
		return nil
	})
}

// PendingAbandoned returns the ids of the events abandoned for endpoint (a Final attempt that failed) whose give-up
// line has not been announced (ClearAbandoned), oldest abandonment first. The rows stay until cleared, whatever the
// age of the event and whether the endpoint is still configured.
func (s *Store) PendingAbandoned(ctx context.Context, endpoint string) ([]string, error) {
	if !ValidEndpoint(endpoint) {
		return nil, invalidEndpoint()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id FROM webhook_abandoned WHERE endpoint = ? ORDER BY at, event_id`, endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ClearAbandoned deletes the row PendingAbandoned lists for the event and endpoint, once its line is logged. It is a
// no-op if there is none.
func (s *Store) ClearAbandoned(ctx context.Context, endpoint, eventID string) error {
	if !ValidEndpoint(endpoint) {
		return invalidEndpoint()
	}
	if !validLowerHex(eventID, 32) {
		return invalid("event id must be 32 lowercase hex characters")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM webhook_abandoned WHERE endpoint = ? AND event_id = ?`, endpoint, eventID)
		return err
	})
}

// AttemptsFor returns the delivery attempts of an event for one endpoint, in the order they were recorded.
func (s *Store) AttemptsFor(ctx context.Context, endpoint, eventID string) ([]Attempt, error) {
	if !ValidEndpoint(endpoint) {
		return nil, invalidEndpoint()
	}
	if !validLowerHex(eventID, 32) {
		return nil, invalid("event id must be 32 lowercase hex characters")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT at, status, ok FROM webhook_attempts WHERE endpoint = ? AND event_id = ? ORDER BY seq`,
		endpoint, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a := Attempt{Endpoint: endpoint, EventID: eventID}
		var at int64
		var ok int
		if err := rows.Scan(&at, &a.Status, &ok); err != nil {
			return nil, err
		}
		a.At, a.OK = fromTS(at), ok == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// RegisterEndpoint returns the time of the endpoint's first delivery pass in this store. The first call for a name
// records the store's clock; every later call returns the recorded time and writes nothing. The writes are serialised,
// so concurrent first calls agree.
func (s *Store) RegisterEndpoint(ctx context.Context, endpoint string) (firstPass time.Time, err error) {
	if !ValidEndpoint(endpoint) {
		return time.Time{}, invalidEndpoint()
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var n int64
		err := tx.QueryRowContext(ctx, `SELECT first_pass FROM webhook_endpoints WHERE name = ?`, endpoint).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			n = ts(s.now())
			_, err = tx.ExecContext(ctx, `INSERT INTO webhook_endpoints (name, first_pass) VALUES (?, ?)`, endpoint, n)
		}
		if err != nil {
			return err
		}
		firstPass = fromTS(n)
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return firstPass, nil
}
