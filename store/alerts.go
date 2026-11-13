package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxAlertsPerDay bounds the alert rows of one UTC day (plan 10). A repeat of a stored alert is not a row.
	MaxAlertsPerDay = 1000
	// MaxAlertRepeats is where an alert's repeats stop: the alert and at most MaxAlertRepeats-1 hits.
	MaxAlertRepeats = 100

	maxAlertMessage = 1024
	maxAlertFields  = 4096
	maxAlertDropped = 1 << 20
)

// Alert is an operator alert as the alert queue stores it: the log line's message, its allowed fields as a JSON
// object with sorted keys (the bytes the id is derived from and the payload carries), and the number of attributes
// that were dropped.
type Alert struct {
	Message string
	Fields  string
	Dropped int
}

// PendingAlert is an undelivered alert with the facts a deliverer needs: when it was stored, its repeats (1 + its
// hits), and its failed attempts.
type PendingAlert struct {
	ID string
	Alert
	CreatedAt time.Time
	Repeats   int
	Failures  int       // number of delivery attempts recorded (none succeeded, or the alert would not be listed)
	LastAt    time.Time // time of the latest attempt; the zero time if there is none
}

// AlertAttempt is one delivery attempt of an alert, as Attempt is of an event.
type AlertAttempt struct {
	AlertID string
	At      time.Time
	Status  int
	OK      bool
}

// AlertID returns hex(SHA-256(message || 0x00 || fields || 0x00 || hour))[:32], where hour is the decimal Unix time of
// the start of the UTC hour that at falls in. The same alert in the same hour has the same id; the next hour a new one.
func AlertID(message, fields string, at time.Time) string {
	sum := sha256.Sum256([]byte(message + "\x00" + fields + "\x00" + strconv.FormatInt(hourStart(at), 10)))
	return hex.EncodeToString(sum[:])[:32]
}

func hourStart(t time.Time) int64 { u := t.Unix(); return u - u%3600 }
func dayStart(t time.Time) int64  { u := t.Unix(); return u - u%86400 }

// printableASCII reports whether s is made of bytes from space to tilde only.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

func validAlert(a Alert) error {
	if !strings.HasPrefix(a.Message, "ALERT:") || len(a.Message) > maxAlertMessage || !printableASCII(a.Message) {
		return invalid("alert message must be printable ASCII starting ALERT:, at most %d bytes", maxAlertMessage)
	}
	if len(a.Fields) < 2 || len(a.Fields) > maxAlertFields || a.Fields[0] != '{' || strings.ContainsAny(a.Fields[1:], "{[") ||
		!printableASCII(a.Fields) || !json.Valid([]byte(a.Fields)) {
		return invalid("alert fields must be a flat JSON object of printable ASCII, at most %d bytes", maxAlertFields)
	}
	if a.Dropped < 0 || a.Dropped > maxAlertDropped {
		return invalid("alert dropped count is out of range")
	}
	return nil
}

// AddAlert stores an alert, stamped with the store's clock, and reports whether it is queued.
//
// An alert whose id (AlertID) is already stored is a repeat: it records a hit, unless the alert is delivered or has
// MaxAlertRepeats-1 hits already (then nothing is written), and is queued in either case. A new alert is stored unless
// the UTC day already has MaxAlertsPerDay alert rows: then nothing is written and queued is false. The count is read
// from the store in the write transaction, so a restart does not reset it.
func (s *Store) AddAlert(ctx context.Context, a Alert) (queued bool, err error) {
	if err := validAlert(a); err != nil {
		return false, err
	}
	// A repeat that would record nothing (capped, or delivered) is answered from a read, without the write lock that
	// payment writes share. The check inside the write transaction stays the authoritative one.
	var settled int
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE id = ?1) AND (
  (SELECT count(*) FROM alert_hits WHERE alert_id = ?1) >= ?2 OR EXISTS (SELECT 1 FROM alert_attempts WHERE alert_id = ?1 AND ok = 1))`,
		AlertID(a.Message, a.Fields, s.now()), MaxAlertRepeats-1).Scan(&settled)
	if err != nil {
		return false, err
	}
	if settled == 1 {
		return true, nil
	}
	var inserted bool
	err = s.write(ctx, func(tx *sql.Tx) error {
		now := s.now()
		id := AlertID(a.Message, a.Fields, now)
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM alerts WHERE id = ?`, id).Scan(&one)
		if err == nil {
			queued = true
			var hits, delivered int
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM alert_hits WHERE alert_id = ?1),
  EXISTS (SELECT 1 FROM alert_attempts WHERE alert_id = ?1 AND ok = 1)`, id).Scan(&hits, &delivered); err != nil {
				return err
			}
			if delivered == 1 || hits >= MaxAlertRepeats-1 {
				return nil
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO alert_hits (alert_id, seq, at) VALUES (?, ?, ?)`, id, hits+1, ts(now))
			return err
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var today int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM alerts WHERE created_at >= ?`, dayStart(now)).Scan(&today); err != nil {
			return err
		}
		if today >= MaxAlertsPerDay {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO alerts (id, message, fields, dropped, created_at) VALUES (?, ?, ?, ?, ?)`,
			id, a.Message, a.Fields, a.Dropped, ts(now)); err != nil {
			return err
		}
		queued = true
		inserted = true
		return nil
	})
	// After the transaction and the write lock, whatever the error, as in AddEvent; a repeat raises none.
	if inserted {
		s.alertWatch.wake()
	}
	if err != nil {
		return false, err
	}
	return queued, nil
}

// UndeliveredAlerts returns the alerts created at or after since that have no successful delivery attempt, oldest
// first, as UndeliveredEvents does for events: the retry policy is the caller's.
func (s *Store) UndeliveredAlerts(ctx context.Context, since time.Time) ([]PendingAlert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.message, a.fields, a.dropped, a.created_at,
  1 + (SELECT count(*) FROM alert_hits h WHERE h.alert_id = a.id),
  (SELECT count(*) FROM alert_attempts t WHERE t.alert_id = a.id),
  (SELECT coalesce(max(t.at), 0) FROM alert_attempts t WHERE t.alert_id = a.id)
FROM alerts a
WHERE a.created_at >= ?
  AND NOT EXISTS (SELECT 1 FROM alert_attempts t WHERE t.alert_id = a.id AND t.ok = 1)
ORDER BY a.created_at, a.id`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAlert
	for rows.Next() {
		var p PendingAlert
		var created, last int64
		if err := rows.Scan(&p.ID, &p.Message, &p.Fields, &p.Dropped, &created, &p.Repeats, &p.Failures, &last); err != nil {
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

// AddAlertAttempt records one delivery attempt of an alert. The alert must exist (ErrNotFound) and Status must be 0
// (no response) or an HTTP status up to 999. A zero At is stamped from the clock.
func (s *Store) AddAlertAttempt(ctx context.Context, a AlertAttempt) error {
	if !validLowerHex(a.AlertID, 32) {
		return invalid("alert id must be 32 lowercase hex characters")
	}
	if a.Status < 0 || a.Status > 999 {
		return invalid("status must be 0 to 999")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM alerts WHERE id = ?`, a.AlertID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var seq int
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) + 1 FROM alert_attempts WHERE alert_id = ?`, a.AlertID).Scan(&seq); err != nil {
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
		_, err := tx.ExecContext(ctx, `INSERT INTO alert_attempts (alert_id, seq, at, status, ok) VALUES (?, ?, ?, ?, ?)`,
			a.AlertID, seq, ts(at), a.Status, ok)
		return err
	})
}
