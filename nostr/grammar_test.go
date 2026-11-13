package nostr

import (
	"errors"
	"strings"
	"testing"
)

func ev(kind int, content string, tags ...[]string) *Event {
	return &Event{ID: hex64('1'), PubKey: hex64('a'), CreatedAt: 1, Kind: kind, Tags: tags, Content: content, Sig: hex128('c')}
}

func validNWC() map[string]*Event {
	return map[string]*Event{
		"info": ev(KindNWCInfo, "pay_invoice get_balance make_invoice",
			[]string{"encryption", "nip44_v2 nip04"}, []string{"p", hex64('b')},
			[]string{"notifications", "payment_received payment_sent"}, []string{"extensions", "dummy_ext"}),
		"info minimal": ev(KindNWCInfo, "get_info", []string{"encryption", "nip44_v2"}, []string{"p", hex64('b')}),
		"request":      ev(KindNWCRequest, payload(132), []string{"p", hex64('b')}, []string{"encryption", "nip44_v2"}),
		"request exp":  ev(KindNWCRequest, payload(87472), []string{"encryption", "nip44_v2"}, []string{"p", hex64('b')}, []string{"expiration", "1700000000"}),
		"response":     ev(KindNWCResponse, payload(200), []string{"p", hex64('b')}, []string{"e", hex64('d')}),
		"notification": ev(KindNWCNotification, payload(200), []string{"p", hex64('b')}),
		"auth":         ev(KindAuth, "", []string{"relay", "wss://relay.example/path?x=1"}, []string{"challenge", strings.Repeat("a0", 16)}),
		"auth ws":      ev(KindAuth, "", []string{"challenge", strings.Repeat("f", 128)}, []string{"relay", "ws://localhost:7777"}),
	}
}

func TestValidateNWCTable(t *testing.T) {
	for name, e := range validNWC() {
		if err := ValidateNWC(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	legacy := ev(KindNWCNotificationNIP04, "abc?iv=def", []string{"p", hex64('b')})
	if err := ValidateNWC(legacy); err != ErrLegacyNIP04 {
		t.Errorf("23196: got %v", err)
	}
	if err := ValidateNWC(nil); err == nil {
		t.Error("nil accepted")
	}
}

func TestValidateNWCRejects(t *testing.T) {
	p := []string{"p", hex64('b')}
	enc := []string{"encryption", "nip44_v2"}
	info := func(content string, tags ...[]string) *Event { return ev(KindNWCInfo, content, tags...) }
	req := func(content string, tags ...[]string) *Event { return ev(KindNWCRequest, content, tags...) }
	cases := map[string]*Event{
		"unknown kind":            ev(1, "hi", p),
		"kind 0":                  ev(0, "", p),
		"unknown tag":             ev(KindNWCNotification, payload(200), p, []string{"x", "y"}),
		"unknown tag on req":      req(payload(200), p, enc, []string{"t", "x"}),
		"duplicate p":             ev(KindNWCNotification, payload(200), p, p),
		"duplicate encryption":    req(payload(200), p, enc, enc),
		"tag 1 element":           ev(KindNWCNotification, payload(200), p, []string{"x"}),
		"tag 3 elements":          ev(KindNWCNotification, payload(200), []string{"p", hex64('b'), "extra"}),
		"tag 0 elements":          ev(KindNWCNotification, payload(200), p, []string{}),
		"nil tag":                 ev(KindNWCNotification, payload(200), p, nil),
		"missing p":               ev(KindNWCNotification, payload(200)),
		"p not hex":               ev(KindNWCNotification, payload(200), []string{"p", strings.Repeat("g", 64)}),
		"p uppercase":             ev(KindNWCNotification, payload(200), []string{"p", strings.Repeat("B", 64)}),
		"p short":                 ev(KindNWCNotification, payload(200), []string{"p", hex64('b')[:63]}),
		"response missing e":      ev(KindNWCResponse, payload(200), p),
		"response e bad":          ev(KindNWCResponse, payload(200), p, []string{"e", "xyz"}),
		"response dup e":          ev(KindNWCResponse, payload(200), p, []string{"e", hex64('d')}, []string{"e", hex64('d')}),
		"request nip04":           req(payload(200), p, []string{"encryption", "nip04"}),
		"request no encryption":   req(payload(200), p),
		"request enc suffix":      req(payload(200), p, []string{"encryption", "nip44_v2 nip04"}),
		"exp 0":                   req(payload(200), p, enc, []string{"expiration", "0"}),
		"exp 01":                  req(payload(200), p, enc, []string{"expiration", "01"}),
		"exp -1":                  req(payload(200), p, enc, []string{"expiration", "-1"}),
		"exp 19 digits":           req(payload(200), p, enc, []string{"expiration", strings.Repeat("1", 19)}),
		"exp 20 digits":           req(payload(200), p, enc, []string{"expiration", strings.Repeat("1", 20)}),
		"exp empty":               req(payload(200), p, enc, []string{"expiration", ""}),
		"exp dup":                 req(payload(200), p, enc, []string{"expiration", "5"}, []string{"expiration", "6"}),
		"exp on response":         ev(KindNWCResponse, payload(200), p, []string{"e", hex64('d')}, []string{"expiration", "5"}),
		"b64 len 131":             req(payload(132)[:131], p, enc),
		"b64 len 128":             req(payload(128), p, enc),
		"b64 len 87473":           req(payload(87472)+"A", p, enc),
		"b64 len 87476":           req(payload(87476), p, enc),
		"b64 not mult 4":          req(payload(132)+"AA", p, enc),
		"b64 url safe -":          req("-"+payload(132)[1:], p, enc),
		"b64 url safe _":          req("_"+payload(132)[1:], p, enc),
		"b64 pad in middle":       req("AA=A"+payload(132)[4:], p, enc),
		"b64 pad three":           req(strings.Repeat("A", 129)+"===", p, enc),
		"b64 empty":               req("", p, enc),
		"b64 space":               req(" "+payload(132)[1:], p, enc),
		"info double space":       info("get_info  pay_invoice", enc, p),
		"info trailing space":     info("get_info ", enc, p),
		"info leading space":      info(" get_info", enc, p),
		"info uppercase":          info("Get_info", enc, p),
		"info empty":              info("", enc, p),
		"info too long":           info(strings.Repeat("a", 4097), enc, p),
		"info digit":              info("get_info2", enc, p),
		"info dash":               info("get-info", enc, p),
		"info missing encryption": info("get_info", p),
		"info missing p":          info("get_info", enc),
		"info names dbl space":    info("get_info", []string{"encryption", "a  b"}, p),
		"info names empty":        info("get_info", []string{"encryption", ""}, p),
		"info names upper":        info("get_info", []string{"encryption", "NIP44"}, p),
		"info names too long":     info("get_info", []string{"encryption", strings.Repeat("a", 1025)}, p),
		"info notifications bad":  info("get_info", enc, p, []string{"notifications", "a b "}),
		"info extensions bad":     info("get_info", enc, p, []string{"extensions", "A"}),
		"info dup notifications":  info("get_info", enc, p, []string{"notifications", "a"}, []string{"notifications", "b"}),
		"auth content":            ev(KindAuth, "x", []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth no relay":           ev(KindAuth, "", []string{"challenge", strings.Repeat("a", 32)}),
		"auth no challenge":       ev(KindAuth, "", []string{"relay", "wss://r"}),
		"auth http":               ev(KindAuth, "", []string{"relay", "https://r"}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth bare scheme":        ev(KindAuth, "", []string{"relay", "wss://"}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth relay space":        ev(KindAuth, "", []string{"relay", "wss://a b"}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth relay quote":        ev(KindAuth, "", []string{"relay", "wss://a\"b"}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth relay backslash":    ev(KindAuth, "", []string{"relay", `wss://a\b`}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth relay non-ascii":    ev(KindAuth, "", []string{"relay", "wss://é"}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth relay too long":     ev(KindAuth, "", []string{"relay", "wss://" + strings.Repeat("a", 507)}, []string{"challenge", strings.Repeat("a", 32)}),
		"auth chal short":         ev(KindAuth, "", []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("a", 31)}),
		"auth chal long":          ev(KindAuth, "", []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("a", 129)}),
		"auth chal upper":         ev(KindAuth, "", []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("A", 32)}),
		"auth dup relay":          ev(KindAuth, "", []string{"relay", "wss://r"}, []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("a", 32)}),
		"legacy 3-element tag":    ev(KindNWCNotificationNIP04, "x", []string{"p", hex64('b'), "z"}),
		"legacy tag 1 elem":       ev(KindNWCNotificationNIP04, "x", []string{"p"}),
		"legacy non-printable":    ev(KindNWCNotificationNIP04, "x\x01", p),
		"content non-printable":   ev(KindNWCNotification, "é"+payload(200), p),
	}
	for name, e := range cases {
		err := ValidateNWC(e)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if errors.Is(err, ErrLegacyNIP04) {
			t.Errorf("%s: returned ErrLegacyNIP04", name)
		}
	}
	// boundary: exactly 512-byte relay and 4096-byte info content pass
	ok := ev(KindAuth, "", []string{"relay", "wss://" + strings.Repeat("a", 506)}, []string{"challenge", strings.Repeat("a", 32)})
	if err := ValidateNWC(ok); err != nil {
		t.Errorf("512-byte relay: %v", err)
	}
	words := strings.Repeat("a", 4096)
	if err := ValidateNWC(info(words, enc, p)); err != nil {
		t.Errorf("4096-byte info: %v", err)
	}
	if err := ValidateNWC(req(payload(132), p, enc, []string{"expiration", strings.Repeat("9", 18)})); err != nil {
		t.Errorf("18-digit expiration: %v", err)
	}
}

func TestValidateNWCLegacyTolerance(t *testing.T) {
	for name, e := range map[string]*Event{
		"extra tag":    ev(KindNWCNotificationNIP04, "abc?iv=def", []string{"p", hex64('b')}, []string{"zzz", "1"}),
		"no tags":      ev(KindNWCNotificationNIP04, "abc"),
		"odd p value":  ev(KindNWCNotificationNIP04, "abc", []string{"p", "not hex"}),
		"dup tags":     ev(KindNWCNotificationNIP04, "", []string{"p", "a"}, []string{"p", "b"}),
		"huge content": ev(KindNWCNotificationNIP04, strings.Repeat("x", 100000)),
	} {
		if err := ValidateNWC(e); err != ErrLegacyNIP04 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateNWCNamesTagAllowsDigits(t *testing.T) {
	e := ev(KindNWCInfo, "get_info", []string{"encryption", "nip44_v2 nip04"}, []string{"p", hex64('b')},
		[]string{"notifications", "a1 b2"}, []string{"extensions", "x9"})
	if err := ValidateNWC(e); err != nil {
		t.Fatal(err)
	}
}

// reasons: the rule that fired is the one the case is about.
func TestValidateNWCReasons(t *testing.T) {
	p := []string{"p", hex64('b')}
	enc := []string{"encryption", "nip44_v2"}
	chal := []string{"challenge", strings.Repeat("a", 32)}
	cases := []struct {
		name string
		e    *Event
		want string
	}{
		{"payload 128", ev(KindNWCRequest, payload(128), p, enc), "payload length"},
		{"payload 87476", ev(KindNWCResponse, payload(87476), p, []string{"e", hex64('d')}), "payload length"},
		{"payload 131", ev(KindNWCNotification, payload(132)[:131], p), "payload length"},
		{"payload 87473", ev(KindNWCRequest, payload(87472)+"A", p, enc), "payload length"},
		{"payload alphabet", ev(KindNWCRequest, "-"+payload(132)[1:], p, enc), "payload alphabet"},
		{"payload padding", ev(KindNWCRequest, "A=AA"+payload(132)[4:], p, enc), "payload alphabet"},
		{"info 4097", ev(KindNWCInfo, strings.Repeat("a", 4097), enc, p), "info content length"},
		{"info double space", ev(KindNWCInfo, "a  b", enc, p), "info content alphabet"},
		{"info digit", ev(KindNWCInfo, "a1", enc, p), "info content alphabet"},
		{"names 1025", ev(KindNWCInfo, "a", []string{"encryption", strings.Repeat("a", 1025)}, p), "names too long"},
		{"names alphabet", ev(KindNWCInfo, "a", []string{"encryption", "A"}, p), "names alphabet"},
		{"relay 513", ev(KindAuth, "", []string{"relay", "wss://" + strings.Repeat("a", 507)}, chal), "bad relay URL"},
		{"challenge 31", ev(KindAuth, "", []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("a", 31)}), "bad challenge"},
		{"challenge 129", ev(KindAuth, "", []string{"relay", "wss://r"}, []string{"challenge", strings.Repeat("a", 129)}), "bad challenge"},
		{"unknown kind", ev(7, "", p), "outside the grammar"},
		{"unknown tag", ev(KindNWCNotification, payload(132), p, []string{"x", "y"}), "unknown tag"},
		{"duplicate", ev(KindNWCNotification, payload(132), p, p), "duplicate tag"},
		{"tag shape", ev(KindNWCNotification, payload(132), []string{"p"}), "exactly two"},
		{"missing", ev(KindNWCNotification, payload(132)), "missing"},
		{"nip04 encryption", ev(KindNWCRequest, payload(132), p, []string{"encryption", "nip04"}), "not nip44_v2"},
		{"expiration", ev(KindNWCRequest, payload(132), p, enc, []string{"expiration", "0"}), "bad expiration"},
	}
	for _, c := range cases {
		err := ValidateNWC(c.e)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want reason %q", c.name, err, c.want)
		}
	}
}

func TestMissingTagReportedDeterministically(t *testing.T) {
	e := ev(KindNWCResponse, payload(200)) // both "e" and "p" missing
	first := ValidateNWC(e)
	if first == nil || !strings.Contains(first.Error(), `"e"`) {
		t.Fatalf("got %v, want the first missing tag in name order (e)", first)
	}
	for i := 0; i < 50; i++ {
		if err := ValidateNWC(e); err == nil || err.Error() != first.Error() {
			t.Fatalf("run %d: %v differs from %v", i, err, first)
		}
	}
}
