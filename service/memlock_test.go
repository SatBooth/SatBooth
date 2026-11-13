package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/memlock"
)

type logLine struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

func lines(t *testing.T, b *bytes.Buffer) []logLine {
	t.Helper()
	var out []logLine
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if l == "" {
			continue
		}
		var m logLine
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func TestLock(t *testing.T) {
	linuxOK := memlock.Status{Available: true, Locked: true, NotDumpable: true, CoreZero: true}
	cases := []struct {
		name      string
		st        memlock.Status
		perr      error
		allowSwap bool
		wantErr   string
		wantLevel string
		wantMsg   string
		reason    string
	}{
		{"success", linuxOK, nil, false, "", "INFO", "memory locked; core dumps disabled", ""},
		{"success with allow_swap", linuxOK, nil, true, "", "INFO", "memory locked; core dumps disabled", ""},
		{"mlock fails", memlock.Status{Available: true, NotDumpable: true, CoreZero: true}, errors.New("x"), false,
			"satbooth: cannot lock memory (mlockall): raise LimitMEMLOCK (systemd LimitMEMLOCK=infinity) or grant CAP_IPC_LOCK, or set allow_swap: true if swap is encrypted or absent",
			"ERROR", "startup failed", "memlock"},
		{"mlock fails, allow_swap", memlock.Status{Available: true, NotDumpable: true, CoreZero: true}, errors.New("x"), true,
			"", "WARN", "memory not locked because allow_swap is true; keys may reach swap", ""},
		{"dumpable fails", memlock.Status{Available: true, Locked: true, CoreZero: true}, errors.New("x"), false,
			"satbooth: cannot disable core dumps (PR_SET_DUMPABLE, RLIMIT_CORE)", "ERROR", "startup failed", "memlock"},
		{"dumpable fails, allow_swap does not help", memlock.Status{Available: true, Locked: true, CoreZero: true}, errors.New("x"), true,
			"satbooth: cannot disable core dumps (PR_SET_DUMPABLE, RLIMIT_CORE)", "ERROR", "startup failed", "memlock"},
		{"rlimit core fails", memlock.Status{Available: true, Locked: true, NotDumpable: true}, errors.New("x"), true,
			"satbooth: cannot disable core dumps (PR_SET_DUMPABLE, RLIMIT_CORE)", "ERROR", "startup failed", "memlock"},
		{"non-Linux", memlock.Status{CoreZero: true}, nil, false, "", "WARN", "memory locking unavailable on this OS; development only", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			err := Lock(&Config{AllowSwap: c.allowSwap}, log, func() (memlock.Status, error) { return c.st, c.perr })
			if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || err.Error() != c.wantErr) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			ls := lines(t, &buf)
			if len(ls) != 1 || ls[0].Level != c.wantLevel || ls[0].Msg != c.wantMsg || ls[0].Reason != c.reason {
				t.Fatalf("log = %+v, want one %s %q reason %q", ls, c.wantLevel, c.wantMsg, c.reason)
			}
			if strings.Contains(buf.String(), `"x"`) {
				t.Fatal("the underlying error text reached the log")
			}
		})
	}
}

func TestAllowSwapConfig(t *testing.T) {
	c, err := ParseConfig(enc(t, goodConfig()))
	if err != nil || c.AllowSwap {
		t.Fatalf("default: %v %v", c, err)
	}
	m := goodConfig()
	m["allow_swap"] = true
	if c, err = ParseConfig(enc(t, m)); err != nil || !c.AllowSwap {
		t.Fatalf("true: %v %v", c, err)
	}
	for _, bad := range []string{`"allow_swap": "yes"`, `"allow_swap": 1`, `"allow_swap": null, "allow_swap": true`, `"Allow_Swap": true`, `"allow_swap": true, "allow_swap": false`} {
		b := enc(t, goodConfig())
		b = append(b[:len(b)-1], []byte(","+bad+"}")...)
		if _, err := ParseConfig(b); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
