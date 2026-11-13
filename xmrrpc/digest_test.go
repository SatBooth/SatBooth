package xmrrpc

import (
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"
)

// authParams is the test's own reader of an Authorization value: "Digest " then key=value pairs separated
// by commas, values bare or quoted with backslash escapes. It also reports which values were quoted.
func authParams(t *testing.T, h string) (map[string]string, map[string]bool) {
	t.Helper()
	vals, quoted, err := parseAuth(h)
	if err != nil {
		t.Fatal(err)
	}
	return vals, quoted
}

// parseAuth is authParams without the test: the fake server's handler goroutine uses it, where
// t.Fatal is not allowed.
func parseAuth(h string) (map[string]string, map[string]bool, error) {
	rest, ok := strings.CutPrefix(h, "Digest ")
	if !ok {
		return nil, nil, fmt.Errorf("authorization %q does not start with Digest", h)
	}
	vals, quoted := map[string]string{}, map[string]bool{}
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ")
		eq := strings.IndexByte(rest, '=')
		if eq < 1 {
			return nil, nil, fmt.Errorf("authorization %q: no key at %q", h, rest)
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' {
					i++
				}
				b.WriteByte(rest[i])
			}
			if i >= len(rest) {
				return nil, nil, fmt.Errorf("authorization %q: unterminated quote", h)
			}
			val, rest, quoted[key] = b.String(), rest[i+1:], true
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			val, rest = rest[:end], rest[end:]
		}
		if _, dup := vals[key]; dup {
			return nil, nil, fmt.Errorf("authorization %q: %s twice", h, key)
		}
		vals[key] = val
		rest = strings.TrimPrefix(strings.TrimLeft(rest, " "), ",")
	}
	return vals, quoted, nil
}

// checkAuthShape asserts the forms epee's parser needs (contrib/epee/src/http_auth.cpp, auth_message::parse
// and init_client_value): response, uri, username, nonce, realm, cnonce and opaque quoted; nc exactly 8 hex
// digits, bare; algorithm and qop bare; and no key epee's field table lacks (an unknown key fails its parse).
func checkAuthShape(t *testing.T, vals map[string]string, quoted map[string]bool) {
	t.Helper()
	known := map[string]bool{"username": true, "realm": true, "nonce": true, "uri": true, "algorithm": true,
		"response": true, "qop": true, "nc": true, "cnonce": true, "opaque": true}
	for k := range vals {
		if !known[k] {
			t.Errorf("authorization has key %q, which epee refuses", k)
		}
	}
	for _, k := range []string{"username", "realm", "nonce", "uri", "response", "cnonce"} {
		if !quoted[k] {
			t.Errorf("%s is not quoted", k)
		}
	}
	if _, ok := vals["opaque"]; ok && !quoted["opaque"] {
		t.Error("opaque is not quoted")
	}
	for _, k := range []string{"nc", "algorithm", "qop"} {
		if quoted[k] {
			t.Errorf("%s is quoted; epee wants it bare", k)
		}
	}
	if vals["algorithm"] != "MD5" || vals["qop"] != "auth" {
		t.Errorf("algorithm %q qop %q", vals["algorithm"], vals["qop"])
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(vals["nc"]) {
		t.Errorf("nc %q is not 8 lowercase hex", vals["nc"])
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(vals["response"]) {
		t.Errorf("response %q is not 32 lowercase hex", vals["response"])
	}
}

func TestDigestVector(t *testing.T) {
	cases := []struct {
		name                     string
		ch                       challenge
		user, pass, uri, cnonce  string
		wantResponse, wantOpaque string
	}{
		// RFC 2617 §3.5 (method GET, nc 00000001).
		{"RFC 2617 3.5", challenge{Realm: "testrealm@host.com", Nonce: "dcd98b7102dd2f0e8b11d0f600bfb0c093",
			Opaque: "5ccc069c403ebaf9f0171e9517f40e41", QopAuth: true},
			"Mufasa", "Circle Of Life", "/dir/index.html", "0a4f113b",
			"6629fae49393a05397450978507c4ef1", "5ccc069c403ebaf9f0171e9517f40e41"},
		// RFC 7616 §3.9.1, the MD5 Authorization (copied from the RFC's text; the password there is
		// "Circle of Life", lowercase "of").
		{"RFC 7616 3.9.1 MD5", challenge{Realm: "http-auth@example.org", Nonce: "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v",
			Opaque: "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS", Algorithm: "MD5", QopAuth: true},
			"Mufasa", "Circle of Life", "/dir/index.html", "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ",
			"8ca523f5e9506fed4657c9700eebdbec", "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := authorize(tc.ch, tc.user, []byte(tc.pass), "GET", tc.uri, 1, tc.cnonce)
			vals, quoted := authParams(t, h)
			checkAuthShape(t, vals, quoted)
			want := map[string]string{"username": tc.user, "realm": tc.ch.Realm, "nonce": tc.ch.Nonce, "uri": tc.uri,
				"nc": "00000001", "cnonce": tc.cnonce, "response": tc.wantResponse, "opaque": tc.wantOpaque}
			for k, v := range want {
				if vals[k] != v {
					t.Errorf("%s = %q, want %q", k, vals[k], v)
				}
			}
			if strings.Contains(h, tc.pass) {
				t.Error("the Authorization value holds the password")
			}
		})
	}
	// The RFC 7616 challenges: the SHA-256 one is refused, the MD5 one (qop "auth, auth-int") is accepted.
	sha := `Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=SHA-256, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`
	md5 := strings.Replace(sha, "SHA-256", "MD5", 1)
	if _, ok := parseChallenge(sha); ok {
		t.Error("the SHA-256 challenge was accepted")
	}
	ch, ok := parseChallenge(md5)
	if !ok || ch.Realm != "http-auth@example.org" || ch.Nonce != "7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v" ||
		ch.Opaque != "FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS" || !ch.QopAuth || ch.Stale {
		t.Errorf("MD5 challenge: %+v %v", ch, ok)
	}
	if got, ok := pickChallenge([]string{sha, md5}); !ok || got != ch {
		t.Errorf("pickChallenge took %+v %v, want the MD5 one", got, ok)
	}
}

// TestAuthorizeEscapes: a quote or a backslash in a server value is escaped in the header, and the
// response is computed over the unescaped value; no opaque key is sent when the challenge had none.
func TestAuthorizeEscapes(t *testing.T) {
	ch := challenge{Realm: `r"e\alm`, Nonce: `n"o\nce`, QopAuth: true}
	h := authorize(ch, "u", []byte("p"), "POST", "/json_rpc", 0x1a, "0123456789abcdef")
	vals, quoted := authParams(t, h)
	checkAuthShape(t, vals, quoted)
	if vals["realm"] != ch.Realm || vals["nonce"] != ch.Nonce || vals["nc"] != "0000001a" {
		t.Errorf("realm %q nonce %q nc %q", vals["realm"], vals["nonce"], vals["nc"])
	}
	if _, ok := vals["opaque"]; ok {
		t.Error("opaque sent without one in the challenge")
	}
	if want := refResponse("u", ch.Realm, "p", "POST", "/json_rpc", ch.Nonce, "0000001a", "0123456789abcdef"); vals["response"] != want {
		t.Errorf("response %s, want %s", vals["response"], want)
	}
}

func TestDigestAgainstRecordedChallenge(t *testing.T) {
	lines := recordedChallenges(t)
	if len(lines) != 2 {
		t.Fatalf("recorded %d WWW-authenticate lines, want 2", len(lines))
	}
	ch, ok := parseChallenge(lines[0])
	if !ok || ch.Realm != "monero-rpc" || ch.Nonce != recordedNonce || ch.Algorithm != "MD5" || !ch.QopAuth || ch.Stale || ch.Opaque != "" {
		t.Errorf("MD5 line: %+v %v", ch, ok)
	}
	if !strings.Contains(lines[1], "algorithm=MD5-sess") {
		t.Fatalf("second line %q is not the MD5-sess one", lines[1])
	}
	if _, ok := parseChallenge(lines[1]); ok {
		t.Error("the MD5-sess line was accepted")
	}
	for _, order := range [][]string{lines, {lines[1], lines[0]}} {
		if got, ok := pickChallenge(order); !ok || got != ch {
			t.Errorf("pickChallenge(%q) = %+v %v", order, got, ok)
		}
	}
	if _, ok := pickChallenge([]string{lines[1]}); ok {
		t.Error("pickChallenge took an MD5-sess-only answer")
	}
	if _, ok := pickChallenge(nil); ok {
		t.Error("pickChallenge took nothing")
	}
}

// recordedNonce is the nonce in testdata/challenge.txt, a 401 answer recorded from a regtest monero-wallet-rpc.
const recordedNonce = "P6gXuAKqtriF9PYr2t4wCw=="

// recordedChallenges returns the WWW-authenticate values of testdata/challenge.txt, in order.
func recordedChallenges(t testing.TB) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/challenge.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok && strings.EqualFold(k, "WWW-Authenticate") {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func TestDigestChallengeRefusals(t *testing.T) {
	const base = `realm="monero-rpc",nonce="abc=="`
	refused := map[string]string{
		"no qop":         `Digest ` + base,
		"qop auth-int":   `Digest qop="auth-int",` + base,
		"qop empty":      `Digest qop="",` + base,
		"qop authx":      `Digest qop="authx",` + base,
		"SHA-256":        `Digest qop="auth",algorithm=SHA-256,` + base,
		"MD5-sess":       `Digest qop="auth",algorithm=MD5-sess,` + base,
		"empty algo":     `Digest qop="auth",algorithm="",` + base,
		"no nonce":       `Digest qop="auth",realm="monero-rpc"`,
		"empty nonce":    `Digest qop="auth",realm="monero-rpc",nonce=""`,
		"no realm":       `Digest qop="auth",nonce="abc=="`,
		"empty realm":    `Digest qop="auth",realm="",nonce="abc=="`,
		"bare nonce":     `Digest qop="auth",realm="monero-rpc",nonce=abc`,
		"bare realm":     `Digest qop="auth",realm=monero-rpc,nonce="abc=="`,
		"bare opaque":    `Digest qop="auth",opaque=x,` + base,
		"duplicate":      `Digest qop="auth",` + base + `,nonce="def"`,
		"duplicate case": `Digest qop="auth",` + base + `,Realm="x"`,
		"duplicate qop":  `Digest qop="auth",qop="auth",` + base,
		"5 KiB":          `Digest qop="auth",` + base + `,x="` + strings.Repeat("a", 5<<10) + `"`,
		"Basic":          `Basic realm="x"`,
		"no scheme":      `qop="auth",` + base,
		"scheme glued":   `Digestqop="auth",` + base,
		"empty":          ``,
		"scheme only":    `Digest`,
		"trailing comma": `Digest qop="auth",` + base + `,`,
		"double comma":   `Digest qop="auth",,` + base,
		"no value":       `Digest qop="auth",x=,` + base,
		"no key":         `Digest qop="auth",="x",` + base,
		"open quote":     `Digest qop="auth",` + base + `,x="abc`,
		"NUL in value":   "Digest qop=\"auth\"," + base + ",x=\"a\x00b\"",
		"CR in value":    "Digest qop=\"auth\"," + base + ",x=\"a\rb\"",
		"LF":             "Digest qop=\"auth\",\n" + base,
		"bare quote":     `Digest qop="auth",` + base + `,x=a"b`,
		"two challenges": `Digest qop="auth",` + base + `, Basic realm="x"`,
		"quote escape":   `Digest qop="auth",` + base + `,x="a\"`,
	}
	for name, h := range refused {
		if ch, ok := parseChallenge(h); ok {
			t.Errorf("%s: accepted %q as %+v", name, h, ch)
		}
	}
	type want struct {
		realm, nonce, opaque, algo string
		stale                      bool
	}
	accepted := map[string]struct {
		h string
		w want
	}{
		"recorded epee shape":  {`Digest qop="auth",algorithm=MD5,realm="monero-rpc",nonce="abc==",stale=false`, want{"monero-rpc", "abc==", "", "MD5", false}},
		"bare qop and algo":    {`Digest qop=auth,algorithm=MD5,` + base, want{"monero-rpc", "abc==", "", "MD5", false}},
		"quoted algo, stale":   {`Digest qop="auth",algorithm="MD5",` + base + `,stale="true"`, want{"monero-rpc", "abc==", "", "MD5", true}},
		"no algorithm":         {`Digest qop="auth",` + base, want{"monero-rpc", "abc==", "", "", false}},
		"stale=true":           {`Digest qop="auth",` + base + `,stale=true`, want{"monero-rpc", "abc==", "", "", true}},
		"stale=TRUE":           {`Digest qop="auth",` + base + `,stale=TRUE`, want{"monero-rpc", "abc==", "", "", true}},
		"stale other":          {`Digest qop="auth",` + base + `,stale=maybe`, want{"monero-rpc", "abc==", "", "", false}},
		"mixed case md5":       {`Digest qop="auth",algorithm=md5,` + base, want{"monero-rpc", "abc==", "", "md5", false}},
		"unknown keys":         {`Digest qop="auth",domain="/x /y",charset=UTF-8,userhash=false,` + base, want{"monero-rpc", "abc==", "", "", false}},
		"qop list":             {`Digest qop="auth-int, auth",` + base, want{"monero-rpc", "abc==", "", "", false}},
		"qop AUTH":             {`Digest qop="AUTH",` + base, want{"monero-rpc", "abc==", "", "", false}},
		"opaque":               {`Digest qop="auth",opaque="xyz",` + base, want{"monero-rpc", "abc==", "xyz", "", false}},
		"spaces":               {"  digest \t qop = \"auth\" ,  realm=\"monero-rpc\" ,\tnonce=\"abc==\"  ", want{"monero-rpc", "abc==", "", "", false}},
		"DIGEST, keys in caps": {`DIGEST QOP="auth",REALM="monero-rpc",NONCE="abc=="`, want{"monero-rpc", "abc==", "", "", false}},
		"escapes":              {`Digest qop="auth",realm="a\"b\\c",nonce="\n"`, want{`a"b\c`, "n", "", "", false}},
		"non-ASCII realm":      {"Digest qop=\"auth\",realm=\"caf\xc3\xa9\",nonce=\"abc==\"", want{"caf\xc3\xa9", "abc==", "", "", false}},
		"exactly 4 KiB":        {pad4K(`Digest qop="auth",` + base + `,x="`), want{"monero-rpc", "abc==", "", "", false}},
	}
	for name, c := range accepted {
		ch, ok := parseChallenge(c.h)
		got := want{ch.Realm, ch.Nonce, ch.Opaque, ch.Algorithm, ch.Stale}
		if !ok || !ch.QopAuth || got != c.w {
			t.Errorf("%s: %q gave %+v %v, want %+v", name, c.h, ch, ok, c.w)
		}
	}
	if h := pad4K(`Digest qop="auth",`+base+`,x="`) + " "; len(h) != 4097 {
		t.Fatal("pad4K")
	} else if _, ok := parseChallenge(h); ok {
		t.Error("4 KiB + 1 accepted")
	}
}

// pad4K closes prefix's open quoted value so that the whole header is exactly 4096 bytes.
func pad4K(prefix string) string {
	return prefix + strings.Repeat("a", 4096-len(prefix)-1) + `"`
}

// --- the reference predicate and the generator for the property test ---

const tchar = "!#$%&'*+.^_`|~0-9A-Za-z-"

var (
	// One auth-param: a token key, "=", and a token or a quoted string (qdtext: anything but a control
	// byte other than HTAB, DEL, '"' and '\'; a quoted pair: '\' then anything but a control byte other
	// than HTAB, or DEL). A negated class also matches the U+FFFD that a stray byte above 0x7f decodes to,
	// so every byte above 0x7f is allowed, as RFC 7230's obs-text.
	refParam  = `([` + tchar + `]+)[ \t]*=[ \t]*([` + tchar + `]+|"(?:[^\x00-\x08\x0a-\x1f\x7f"\\]|\\[^\x00-\x08\x0a-\x1f\x7f])*")`
	refWhole  = regexp.MustCompile(`^[ \t]*[Dd][Ii][Gg][Ee][Ss][Tt][ \t]+` + refParam + `(?:[ \t]*,[ \t]*` + refParam + `)*[ \t]*$`)
	refOne    = regexp.MustCompile(refParam)
	refScheme = regexp.MustCompile(`^[ \t]*[Dd][Ii][Gg][Ee][Ss][Tt][ \t]+`)
)

// refParse is the reference predicate: the grammar above, then the semantic rules of parseChallenge.
func refParse(h string) (challenge, bool) {
	if len(h) > 4096 || !refWhole.MatchString(h) {
		return challenge{}, false
	}
	body := h[refScheme.FindStringIndex(h)[1]:]
	vals, quoted := map[string]string{}, map[string]bool{}
	for _, m := range refOne.FindAllStringSubmatch(body, -1) {
		k := strings.ToLower(m[1])
		if _, dup := vals[k]; dup {
			return challenge{}, false
		}
		v := m[2]
		if strings.HasPrefix(v, `"`) {
			v = regexp.MustCompile(`\\(.)`).ReplaceAllString(v[1:len(v)-1], "$1")
			quoted[k] = true
		}
		vals[k] = v
	}
	var ch challenge
	for _, k := range []string{"realm", "nonce"} {
		if !quoted[k] || vals[k] == "" {
			return challenge{}, false
		}
	}
	if _, ok := vals["opaque"]; ok && !quoted["opaque"] {
		return challenge{}, false
	}
	ch.Realm, ch.Nonce, ch.Opaque = vals["realm"], vals["nonce"], vals["opaque"]
	if a, ok := vals["algorithm"]; ok {
		if !strings.EqualFold(a, "MD5") {
			return challenge{}, false
		}
		ch.Algorithm = a
	}
	for _, q := range strings.FieldsFunc(vals["qop"], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if strings.EqualFold(q, "auth") {
			ch.QopAuth = true
		}
	}
	if !ch.QopAuth {
		return challenge{}, false
	}
	ch.Stale = strings.EqualFold(vals["stale"], "true")
	return ch, true
}

// format writes ch as a challenge parseChallenge accepts.
func format(ch challenge) string {
	q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	s := `Digest qop="auth",realm=` + q(ch.Realm) + `,nonce=` + q(ch.Nonce)
	if ch.Algorithm != "" {
		s += ",algorithm=" + ch.Algorithm
	}
	if ch.Opaque != "" {
		s += ",opaque=" + q(ch.Opaque)
	}
	if ch.Stale {
		s += ",stale=true"
	}
	return s
}

// genChallenge builds a challenge-like header over the key set, quoting, order and byte classes,
// valid about half of the time.
func genChallenge(r *rand.Rand) string {
	pick := func(l ...string) string { return l[r.IntN(len(l))] }
	// rare returns good, or one of the faults one time in 40
	rare := func(good string, faults ...string) string {
		if r.IntN(40) == 0 {
			return pick(faults...)
		}
		return good
	}
	value := func() string {
		classes := []string{"a", "Z", "0", "=", "/", "+", "-", " ", "\t", ",", "\\", "\"", "\xc3\xa9", "\xc3", "\xff", "(", ";", "{"}
		controls := []string{"\x00", "\x7f", "\r", "\n", "\x01"}
		var b strings.Builder
		n := r.IntN(6)
		if r.IntN(8) > 0 {
			n = 1 + r.IntN(12)
		}
		for range n {
			switch {
			case r.IntN(10) > 0:
				b.WriteString(pick("a", "b", "Q", "7", "=", "/", "+"))
			case r.IntN(15) > 0:
				b.WriteString(pick(classes...))
			default:
				b.WriteString(pick(controls...))
			}
		}
		return b.String()
	}
	quote := func(v string) string {
		switch r.IntN(40) {
		case 0:
			return v // bare, maybe invalid
		case 1:
			return `"` + v + `"` // raw, maybe invalid
		default:
			return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
		}
	}
	type kv struct{ k, v string }
	var params []kv
	if r.IntN(20) > 0 {
		params = append(params, kv{pick("realm", "Realm", "REALM"), quote(pick("monero-rpc", value()))})
	}
	if r.IntN(20) > 0 {
		params = append(params, kv{pick("nonce", "Nonce"), quote(pick("P6gXuAKqtriF9PYr2t4wCw==", value()))})
	}
	if r.IntN(15) > 0 {
		q := pick("auth", "auth", "auth", "auth-int", "auth,auth-int", "auth-int, auth", "AUTH", "", value())
		if r.IntN(3) == 0 && !strings.ContainsAny(q, ", \"") {
			params = append(params, kv{"qop", q})
		} else {
			params = append(params, kv{"qop", quote(q)})
		}
	}
	if r.IntN(2) == 0 {
		params = append(params, kv{"algorithm", pick("MD5", "MD5", "md5", "Md5", "MD5-sess", "SHA-256", `"MD5"`, quote(value()))})
	}
	if r.IntN(3) == 0 {
		params = append(params, kv{"opaque", quote(value())})
	}
	if r.IntN(2) == 0 {
		params = append(params, kv{"stale", pick("true", "false", "TRUE", `"true"`, quote(value()))})
	}
	for range r.IntN(3) {
		params = append(params, kv{rare(pick("domain", "charset", "userhash", "x-odd", "a!b"), "k y", "", "k\x00"), quote(value())})
	}
	if r.IntN(15) == 0 && len(params) > 0 {
		params = append(params, params[r.IntN(len(params))]) // a duplicate
	}
	r.Shuffle(len(params), func(i, j int) { params[i], params[j] = params[j], params[i] })
	ws := func() string { return pick("", "", "", " ", "  ", "\t") }
	var b strings.Builder
	b.WriteString(ws() + pick("Digest", "digest", "DIGEST") + pick(" ", "  ", "\t"))
	if r.IntN(30) == 0 {
		b.Reset()
		b.WriteString(pick("Basic ", "Digestx ", "", "Digest", "Dig est "))
	}
	for i, p := range params {
		if i > 0 {
			b.WriteString(ws() + rare(",", ",,", ";", "") + ws())
		}
		b.WriteString(p.k + ws() + "=" + ws() + p.v)
	}
	b.WriteString(ws())
	if r.IntN(30) == 0 {
		b.WriteString(`,x="` + strings.Repeat("p", 4000+r.IntN(200)) + `"`)
	}
	s := b.String()
	if r.IntN(40) == 0 && len(s) > 0 {
		s = s[:r.IntN(len(s))]
	}
	return s
}

func TestDigestProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 4))
	var ok int
	for i := range 10000 {
		h := genChallenge(r)
		got, gotOK := parseChallenge(h)
		want, wantOK := refParse(h)
		if gotOK != wantOK || got != want {
			t.Fatalf("case %d %q: parse %+v %v, reference %+v %v", i, h, got, gotOK, want, wantOK)
		}
		if gotOK {
			ok++
			back, okBack := parseChallenge(format(got))
			if !okBack || back != got {
				t.Fatalf("case %d: format %q reparsed as %+v %v, want %+v", i, format(got), back, okBack, got)
			}
		}
	}
	t.Logf("%d of 10000 accepted", ok)
	if ok < 1000 || ok > 9000 {
		t.Fatalf("%d of 10000 accepted: the generator does not cross the boundary", ok)
	}
}
