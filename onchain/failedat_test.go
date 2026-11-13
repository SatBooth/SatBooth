package onchain

import (
	"regexp"
	"testing"
)

// Every scan failed and settle failed record names its call with `at` (one fixed word, nothing from the error or the
// data) beside an optional order. Not reached by a test (the code names them): events (a failed events read in
// leftWithoutFinal's carried orders and in settled), add-settled (the settled write of settleDone).
func TestScanAndSettleFailedNameTheirCall(t *testing.T) {
	for _, c := range []struct {
		msg, at string
		scan    bool   // the fault is made before the pass; otherwise after the track step of a second pass
		fault   string // the SQL that makes the call fail
	}{
		{logScanFailed, "cursor", true, `ALTER TABLE onchain_cursor RENAME TO tbl_x`},
		{logScanFailed, "keep", true, `ALTER TABLE addresses RENAME TO tbl_x`},
		{logScanFailed, "order", true, `ALTER TABLE orders RENAME TO tbl_x`},
		{logScanFailed, "record-scan", true, `CREATE TRIGGER f BEFORE INSERT ON onchain_txos BEGIN SELECT RAISE(ABORT, 'x'); END`},
		{logSettleFailed, "past-expiry", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "paid-again", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "settle", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "left-window", false, `ALTER TABLE orders RENAME TO tbl_x`},
		{logSettleFailed, "unused-run", false, `ALTER TABLE addresses RENAME TO tbl_x`},
		{logSettleFailed, "cursor", false, `ALTER TABLE onchain_cursor RENAME TO tbl_x`},
	} {
		t.Run(c.msg+"/"+c.at, func(t *testing.T) {
			e := newWatchEnv(t)
			o := e.order(10_000)
			e.payOrder(o, 10_000)
			if c.scan {
				e.execDB(c.fault)
			} else {
				e.pass()
				e.node.Mine(1)
				e.onceAt("track:after-mark", func() { e.execDB(c.fault) })
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
