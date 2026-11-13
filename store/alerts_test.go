package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testAlertMsg = "ALERT: an order was paid on both rails"

func addAlert(t *testing.T, s *Store, a Alert) bool {
	t.Helper()
	queued, err := s.AddAlert(context.Background(), a)
	if err != nil {
		t.Fatalf("AddAlert(%+v): %v", a, err)
	}
	return queued
}

func pendingAlerts(t *testing.T, s *Store) []PendingAlert {
	t.Helper()
	p, err := s.UndeliveredAlerts(context.Background(), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The id is sha256(message ‖ 0x00 ‖ fields ‖ 0x00 ‖ the hour bucket's start in decimal Unix seconds), cut to 32 hex.
func TestAlertID(t *testing.T) {
	hour := time.Unix(1_799_996_400, 0).UTC()
	sum := sha256.Sum256([]byte(testAlertMsg + "\x00" + `{"order":"ab"}` + "\x00" + "1799996400"))
	if got, want := AlertID(testAlertMsg, `{"order":"ab"}`, hour), hex.EncodeToString(sum[:])[:32]; got != want {
		t.Fatalf("AlertID %s, want %s", got, want)
	}
	// Any instant in the hour gives the bucket's id.
	if AlertID(testAlertMsg, "{}", hour.Add(59*time.Minute+59*time.Second)) != AlertID(testAlertMsg, "{}", hour) {
		t.Fatal("an instant inside the hour changed the id")
	}
	if AlertID(testAlertMsg, "{}", hour.Add(time.Hour)) == AlertID(testAlertMsg, "{}", hour) {
		t.Fatal("the next hour has the same id")
	}
}

// One alert stored, then the same alert in the same hour adds hits; repeats is 1 + hits; the next hour is a new alert.
func TestAddAlertCoalescesByHour(t *testing.T) {
	s, _, clk := freshStore(t)
	a := Alert{Message: testAlertMsg, Fields: `{"order":"` + strings.Repeat("a", 32) + `"}`, Dropped: 2}
	clk.set(time.Unix(1_799_996_400, 0)) // the top of an hour
	if !addAlert(t, s, a) {
		t.Fatal("first alert not queued")
	}
	clk.set(time.Unix(1_799_996_400+3599, 0))
	if !addAlert(t, s, a) || !addAlert(t, s, a) {
		t.Fatal("a repeat not queued")
	}
	p := pendingAlerts(t, s)
	if len(p) != 1 {
		t.Fatalf("%d alerts, want 1", len(p))
	}
	want := PendingAlert{ID: AlertID(a.Message, a.Fields, time.Unix(1_799_996_400, 0)), Alert: a, CreatedAt: time.Unix(1_799_996_400, 0).UTC(), Repeats: 3}
	if p[0] != want {
		t.Fatalf("pending\n got %+v\nwant %+v", p[0], want)
	}
	clk.set(time.Unix(1_799_996_400+3600, 0))
	addAlert(t, s, a)
	if p := pendingAlerts(t, s); len(p) != 2 || p[1].Repeats != 1 || p[1].ID == p[0].ID {
		t.Fatalf("next hour: %+v", p)
	}
}

// repeats stops at 100: no hit row once the alert has 99, and none once it is delivered.
func TestAlertRepeatsCap(t *testing.T) {
	s, _, _ := freshStore(t)
	a := Alert{Message: testAlertMsg, Fields: "{}"}
	for range 150 {
		addAlert(t, s, a)
	}
	var hits int
	if err := s.db.QueryRow(`SELECT count(*) FROM alert_hits`).Scan(&hits); err != nil || hits != MaxAlertRepeats-1 {
		t.Fatalf("hits %d (%v), want %d", hits, err, MaxAlertRepeats-1)
	}
	if p := pendingAlerts(t, s); len(p) != 1 || p[0].Repeats != MaxAlertRepeats {
		t.Fatalf("pending %+v", p)
	}

	b := Alert{Message: testAlertMsg, Fields: `{"count":1}`}
	addAlert(t, s, b)
	id := AlertID(b.Message, b.Fields, t0)
	if err := s.AddAlertAttempt(context.Background(), AlertAttempt{AlertID: id, Status: 204, OK: true}); err != nil {
		t.Fatal(err)
	}
	if !addAlert(t, s, b) {
		t.Fatal("a repeat of a delivered alert must still count as queued (it is coalesced)")
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM alert_hits WHERE alert_id = ?`, id).Scan(&hits); err != nil || hits != 0 {
		t.Fatalf("hits after delivery %d %v", hits, err)
	}
	// The schema refuses it on any connection too.
	if _, err := s.db.Exec(`INSERT INTO alert_hits (alert_id, seq, at) VALUES (?, 1, 5)`, id); err == nil {
		t.Fatal("a hit after delivery was stored")
	}
}

// At most MaxAlertsPerDay alert rows per UTC day, counted from the store: a restart does not reset it, a repeat of a
// stored alert still coalesces, and the next UTC day starts again.
func TestAlertDailyBound(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	clk := &testClock{t: time.Unix(1_800_057_600, 0)} // 00:00 UTC
	s, err := Open(ctx, path, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxAlertsPerDay {
		clk.set(time.Unix(1_800_057_600+int64(i)*60, 0)) // spread over the day, across hour buckets
		if !addAlert(t, s, Alert{Message: testAlertMsg, Fields: fmt.Sprintf(`{"count":%d}`, i)}) {
			t.Fatalf("alert %d refused", i)
		}
	}
	if addAlert(t, s, Alert{Message: testAlertMsg, Fields: `{"count":5000}`}) {
		t.Fatal("alert 1001 queued")
	}
	s.Close()
	if s, err = Open(ctx, path, WithClock(clk.now)); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if addAlert(t, s, Alert{Message: testAlertMsg, Fields: `{"count":5001}`}) {
		t.Fatal("the bound was reset by a restart")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM alerts`).Scan(&n); err != nil || n != MaxAlertsPerDay {
		t.Fatalf("%d rows %v", n, err)
	}
	clk.set(time.Unix(1_800_057_600+86_400, 0))
	if !addAlert(t, s, Alert{Message: testAlertMsg, Fields: `{"count":5002}`}) {
		t.Fatal("the next day is refused")
	}
}

func TestAddAlertRefusesBadInput(t *testing.T) {
	s, _, _ := freshStore(t)
	for name, a := range map[string]Alert{
		"no prefix":         {Message: "alert: x", Fields: "{}"},
		"lowercase prefix":  {Message: "Alert: x", Fields: "{}"},
		"long message":      {Message: "ALERT: " + strings.Repeat("x", maxAlertMessage), Fields: "{}"},
		"control in msg":    {Message: "ALERT: a\nb", Fields: "{}"},
		"non-ascii msg":     {Message: "ALERT: café", Fields: "{}"},
		"fields not json":   {Message: testAlertMsg, Fields: "{"},
		"fields not object": {Message: testAlertMsg, Fields: "[]"},
		"fields empty":      {Message: testAlertMsg, Fields: ""},
		"fields long":       {Message: testAlertMsg, Fields: `{"a":"` + strings.Repeat("b", maxAlertFields) + `"}`},
		"negative dropped":  {Message: testAlertMsg, Fields: "{}", Dropped: -1},
		"huge dropped":      {Message: testAlertMsg, Fields: "{}", Dropped: maxAlertDropped + 1},
		"nested object":     {Message: testAlertMsg, Fields: `{"a":{}}`},
		"nested array":      {Message: testAlertMsg, Fields: `{"a":[1]}`},
	} {
		if _, err := s.AddAlert(context.Background(), a); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The edges pass.
	edge := Alert{Message: "ALERT: " + strings.Repeat("x", maxAlertMessage-7), Fields: "{}", Dropped: maxAlertDropped}
	if !addAlert(t, s, edge) {
		t.Fatal("edge refused")
	}
}

func TestAlertAttempts(t *testing.T) {
	s, _, clk := freshStore(t)
	ctx := context.Background()
	a := Alert{Message: testAlertMsg, Fields: "{}"}
	addAlert(t, s, a)
	id := AlertID(a.Message, a.Fields, t0)
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: strings.Repeat("0", 32), Status: 500}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown alert: %v", err)
	}
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: id, Status: 1000}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("status 1000: %v", err)
	}
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: "x", Status: 500}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	clk.set(t0.Add(10 * time.Second))
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: id, Status: 500}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: id, At: t0.Add(30 * time.Second), Status: 0}); err != nil {
		t.Fatal(err)
	}
	p := pendingAlerts(t, s)
	if len(p) != 1 || p[0].Failures != 2 || !p[0].LastAt.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("pending %+v", p)
	}
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: id, Status: 200, OK: true}); err != nil {
		t.Fatal(err)
	}
	if p := pendingAlerts(t, s); len(p) != 0 {
		t.Fatalf("delivered alert still pending: %+v", p)
	}
	// since: an alert older than since is not listed.
	b := Alert{Message: testAlertMsg, Fields: `{"step":1}`}
	addAlert(t, s, b)
	if p, err := s.UndeliveredAlerts(ctx, clk.now().Add(time.Second)); err != nil || len(p) != 0 {
		t.Fatalf("since: %+v %v", p, err)
	}
}

// The three tables are insert-only on any connection.
func TestAlertTablesImmutable(t *testing.T) {
	s, path, _ := freshStore(t)
	a := Alert{Message: testAlertMsg, Fields: "{}"}
	addAlert(t, s, a)
	addAlert(t, s, a)
	id := AlertID(a.Message, a.Fields, t0)
	if err := s.AddAlertAttempt(context.Background(), AlertAttempt{AlertID: id, Status: 500}); err != nil {
		t.Fatal(err)
	}
	db, err := openRawForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`UPDATE alerts SET dropped = 1`, `DELETE FROM alerts`, `INSERT OR REPLACE INTO alerts SELECT * FROM alerts`,
		`UPDATE alert_hits SET at = 6`, `DELETE FROM alert_hits`, `INSERT OR REPLACE INTO alert_hits SELECT * FROM alert_hits`,
		`UPDATE alert_attempts SET ok = 1`, `DELETE FROM alert_attempts`, `INSERT OR REPLACE INTO alert_attempts SELECT * FROM alert_attempts`,
	} {
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), "store rows are immutable") {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO alert_hits (alert_id, seq, at) VALUES ('` + strings.Repeat("0", 32) + `', 1, 5)`,
		`INSERT INTO alert_attempts (alert_id, seq, at, status, ok) VALUES ('` + strings.Repeat("0", 32) + `', 1, 5, 200, 1)`,
		`INSERT INTO alert_hits (alert_id, seq, at) VALUES ('` + id + `', ` + strconv.Itoa(MaxAlertRepeats) + `, 5)`,
		`INSERT INTO alerts (id, message, fields, dropped, created_at) VALUES ('` + strings.Repeat("1", 32) + `', 'ALERT: x', '[]', 0, 5)`,
		`INSERT INTO alerts (id, message, fields, dropped, created_at) VALUES ('` + strings.Repeat("1", 32) + `', 'nope', '{}', 0, 5)`,
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

// A repeat that records nothing (capped, or the alert delivered) takes no write lock: AddAlert returns while another
// write holds the store's mutex.
func TestAlertCappedRepeatTakesNoWriteLock(t *testing.T) {
	s, _, _ := freshStore(t)
	ctx := context.Background()
	capped := Alert{Message: testAlertMsg, Fields: "{}"}
	for range MaxAlertRepeats {
		addAlert(t, s, capped)
	}
	delivered := Alert{Message: testAlertMsg, Fields: `{"count":1}`}
	addAlert(t, s, delivered)
	if err := s.AddAlertAttempt(ctx, AlertAttempt{AlertID: AlertID(delivered.Message, delivered.Fields, t0), Status: 204, OK: true}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range []Alert{capped, delivered} {
		done := make(chan error, 1)
		go func() {
			queued, err := s.AddAlert(ctx, a)
			if err == nil && !queued {
				err = errors.New("not queued")
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("a repeat of %s waited for the write lock", a.Fields)
		}
	}
}
