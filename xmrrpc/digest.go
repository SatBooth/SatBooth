package xmrrpc

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"strings"
)

// HTTP Digest (RFC 7616, RFC 2617) with MD5 and qop=auth: what epee's http_server_auth offers and checks
// (monero tag v0.18.5.4, contrib/epee/src/http_auth.cpp: add_challenge writes one challenge with
// algorithm=MD5 and one with MD5-sess, both qop="auth", realm "monero-rpc"; auth_message::verify). The lines
// wallet-rpc v0.18.5.4 really sends are recorded in testdata/challenge.txt.

// maxChallenge bounds one WWW-Authenticate value.
const maxChallenge = 4 << 10

// challenge is what the client keeps of a usable WWW-Authenticate value. Algorithm is "" (absent) or
// "MD5" in any case, as sent; QopAuth is always true in a challenge parseChallenge accepts.
type challenge struct {
	Realm, Nonce, Opaque, Algorithm string
	QopAuth, Stale                  bool
}

func isTchar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func isWS(c byte) bool { return c == ' ' || c == '\t' }

func skipWS(s string, i int) int {
	for i < len(s) && isWS(s[i]) {
		i++
	}
	return i
}

// asciiFold reports whether a equals b (b lowercase ASCII) ignoring ASCII case only.
func asciiFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != b[i] {
			return false
		}
	}
	return true
}

// paramValue reads a token or a quoted string at s[i:]. A quoted string holds qdtext (HTAB, SP, and any byte
// from 0x21 to 0xff but '"', '\' and DEL: RFC 7230 §3.2.6 with obs-text) and quoted pairs ('\' then HTAB or a
// byte from 0x20 to 0xff but DEL); its value is unescaped. It returns the value, whether it was quoted, and
// the index after it.
func paramValue(s string, i int) (val string, quoted bool, next int, ok bool) {
	if i < len(s) && s[i] == '"' {
		var b strings.Builder
		for j := i + 1; j < len(s); j++ {
			c := s[j]
			switch {
			case c == '"':
				return b.String(), true, j + 1, true
			case c == '\\':
				j++
				if j == len(s) || !(s[j] == '\t' || s[j] >= 0x20 && s[j] != 0x7f) {
					return "", false, 0, false
				}
				b.WriteByte(s[j])
			case c == '\t' || c >= 0x20 && c != 0x7f:
				b.WriteByte(c)
			default:
				return "", false, 0, false
			}
		}
		return "", false, 0, false // no closing quote
	}
	j := i
	for j < len(s) && isTchar(s[j]) {
		j++
	}
	if j == i {
		return "", false, 0, false
	}
	return s[i:j], false, j, true
}

// parseChallenge reads one WWW-Authenticate value: the scheme "Digest" (any case), then comma-separated
// key=value pairs (RFC 7235 auth-params, keys in any case), values quoted or bare, at most 4 KiB. It gives
// false for: another scheme, a syntax error, a key given twice, realm or nonce missing, empty or bare (they
// are quoted strings, RFC 7616 §3.3), a bare opaque, an algorithm other than MD5 (any case; MD5-sess
// included), or a qop list without the token auth. Unknown keys are ignored; stale is true for "true" in
// any case.
func parseChallenge(h string) (challenge, bool) {
	if len(h) > maxChallenge {
		return challenge{}, false
	}
	i := skipWS(h, 0)
	if len(h)-i < len("digest") || !asciiFold(h[i:i+len("digest")], "digest") {
		return challenge{}, false
	}
	i += len("digest")
	if j := skipWS(h, i); j == i {
		return challenge{}, false
	} else {
		i = j
	}
	type param struct {
		val    string
		quoted bool
	}
	params := map[string]param{}
	for {
		k := i
		for i < len(h) && isTchar(h[i]) {
			i++
		}
		if i == k {
			return challenge{}, false
		}
		key := strings.ToLower(h[k:i]) // tchar is ASCII
		i = skipWS(h, i)
		if i == len(h) || h[i] != '=' {
			return challenge{}, false
		}
		val, quoted, next, ok := paramValue(h, skipWS(h, i+1))
		if !ok {
			return challenge{}, false
		}
		if _, dup := params[key]; dup {
			return challenge{}, false
		}
		params[key] = param{val, quoted}
		i = skipWS(h, next)
		if i == len(h) {
			break
		}
		if h[i] != ',' {
			return challenge{}, false
		}
		i = skipWS(h, i+1)
	}
	realm, nonce := params["realm"], params["nonce"]
	if !realm.quoted || realm.val == "" || !nonce.quoted || nonce.val == "" {
		return challenge{}, false
	}
	ch := challenge{Realm: realm.val, Nonce: nonce.val}
	if o, ok := params["opaque"]; ok {
		if !o.quoted {
			return challenge{}, false
		}
		ch.Opaque = o.val
	}
	if a, ok := params["algorithm"]; ok {
		if !asciiFold(a.val, "md5") {
			return challenge{}, false
		}
		ch.Algorithm = a.val
	}
	for _, q := range strings.FieldsFunc(params["qop"].val, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if asciiFold(q, "auth") {
			ch.QopAuth = true
		}
	}
	if !ch.QopAuth {
		return challenge{}, false
	}
	ch.Stale = asciiFold(params["stale"].val, "true")
	return ch, true
}

// pickChallenge returns the first usable value among a 401's WWW-Authenticate values (wallet-rpc sends an
// MD5 one and an MD5-sess one; the second is refused).
func pickChallenge(values []string) (challenge, bool) {
	for _, v := range values {
		if ch, ok := parseChallenge(v); ok {
			return ch, true
		}
	}
	return challenge{}, false
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// authorize builds the Authorization value for one request: HA1 = MD5(user:realm:password), HA2 =
// MD5(method:uri), response = MD5(HA1:nonce:nc:cnonce:auth:HA2) (RFC 7616 §3.4.1), nc as 8 lowercase hex
// digits. The fields have the forms epee's parser needs (auth_message::parse: response, uri, username, nonce,
// realm, cnonce and opaque quoted; nc, algorithm and qop bare; no key outside its table). opaque is echoed
// when the challenge had a non-empty one, as epee's own client does. The password goes into the hash only.
func authorize(ch challenge, user string, password []byte, method, uri string, nc uint32, cnonce string) string {
	h := md5.New()
	h.Write([]byte(user + ":" + ch.Realm + ":"))
	h.Write(password)
	var ha1 [md5.Size * 2]byte
	hex.Encode(ha1[:], h.Sum(nil))
	ha2 := md5.Sum([]byte(method + ":" + uri))
	ncs := strconv.FormatUint(uint64(nc), 16)
	ncs = strings.Repeat("0", 8-len(ncs)) + ncs
	h.Reset()
	h.Write(ha1[:])
	h.Write([]byte(":" + ch.Nonce + ":" + ncs + ":" + cnonce + ":auth:" + hex.EncodeToString(ha2[:])))
	response := hex.EncodeToString(h.Sum(nil))
	clear(ha1[:])

	var b strings.Builder
	b.WriteString("Digest username=" + quote(user))
	b.WriteString(", realm=" + quote(ch.Realm))
	b.WriteString(", nonce=" + quote(ch.Nonce))
	b.WriteString(", uri=" + quote(uri))
	b.WriteString(", algorithm=MD5")
	b.WriteString(", response=" + quote(response))
	b.WriteString(", qop=auth")
	b.WriteString(", nc=" + ncs)
	b.WriteString(", cnonce=" + quote(cnonce))
	if ch.Opaque != "" {
		b.WriteString(", opaque=" + quote(ch.Opaque))
	}
	return b.String()
}
