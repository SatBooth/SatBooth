package monero

import (
	"regexp"
	"testing"
)

// Every scan failed and settle failed record names its call with `at` (one fixed word, nothing from the error or the
// data) beside an optional order. Not reached by a test (the code names them): reopened and addresses (scan's later
// reads, which the earlier ones fail first), events (settled's read) and add-settled (the settled write of
// settleDone).
func TestScanAndSettleFailedNameTheirCall(t *testing.T) {
	for _, c := range []struct {
		msg, at string
		pre     bool   // the fault is made before the pass; otherwise after the commit of a second pass
		fault   string // the SQL that makes the call fail
	}{
		{logScanFailed, "cursor", true, `ALTER TABLE monero_cursor RENAME TO tbl_x`},
		{logScanFailed, "keep", true, `ALTER TABLE monero_subaddresses RENAME TO tbl_x`},
		{logScanFailed, "order", true, `ALTER TABLE orders RENAME TO tbl_x`},
		{logScanFailed, "record-scan", true, `CREATE TRIGGER f BEFORE INSERT ON monero_payments BEGIN SELECT RAISE(ABORT, 'x'); END`},
		{logSettleFailed, "pending", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "past-expiry", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "paid-again", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "settle", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "left-unobserved", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "unused-run", false, `ALTER TABLE monero_subaddresses RENAME TO tbl_x`},
		{logSettleFailed, "cursor", false, `ALTER TABLE monero_cursor RENAME TO tbl_x`},
		{logSettleFailed, "clear-pending", false, `ALTER TABLE monero_pending_settle RENAME TO tbl_x`},
		{logSettleFailed, "left-window", true, `ALTER TABLE events RENAME TO tbl_x`},
	} {
		t.Run(c.msg+"/"+c.at, func(t *testing.T) {
			e := newWatchEnv(t, nil)
			o := e.order(xmr)
			e.payOrder(o, xmr)
			if c.pre {
				e.execDB(c.fault)
			} else {
				e.pass()
				e.w.Mine(1)
				done := false
				e.svc.hook = func(p string) error {
					if p == "scan:after-record" && !done {
						done = true
						e.execDB(c.fault)
					}
					return nil
				}
			}
			e.pass()
			line := regexp.MustCompile(`(?m)^time=\S+ level=ERROR msg="` + c.msg + `"( order=[0-9a-f]+)? at=` + c.at + `$`)
			if !line.MatchString(e.log.String()) {
				t.Fatalf("no %q line at %s:\n%s", c.msg, c.at, e.log)
			}
			any := regexp.MustCompile(`(?m)^.*msg="` + c.msg + `".*$`)
			word := regexp.MustCompile(`^time=\S+ level=ERROR msg="` + c.msg + `"( order=[0-9a-f]+)? at=[a-z-]+$`)
			for _, l := range any.FindAllString(e.log.String(), -1) {
				if !word.MatchString(l) {
					t.Fatalf("a %q line with more than an order and a word: %s", c.msg, l)
				}
			}
		})
	}
}
