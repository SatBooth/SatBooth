package ledger

import "testing"

func TestTempStoreMemory(t *testing.T) {
	l, _ := freshLedger(t)
	var got int
	if err := l.db.QueryRow(`PRAGMA temp_store`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("PRAGMA temp_store = %d, want 2 (memory)", got)
	}
}
