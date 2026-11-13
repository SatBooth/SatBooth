package service

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/satbooth/satbooth/store"
	"github.com/satbooth/satbooth/webhook"
)

// The list validator against a model written from the rules, not from the validator's helpers:
//
//	the list has 1 to 8 entries; a name is 1 to 64 characters of A-Z a-z 0-9 . _ - and no two are equal (exactly, so
//	names that differ in case differ); a URL is http or https with a host, no user info or fragment, no space, at most
//	512 bytes, and no two are the same endpoint (scheme and host without case, a default port the same as none, an empty
//	path the same as "/", path and query exactly); a secret file is not empty and no two are the same string; with an
//	alerts block, its URL is not an endpoint's.
//
// Every value the generator makes carries the marker "zq7" (the empty ones excepted), which no message text contains, so
// "a refusal echoes none of the entry's values" is a search for the marker (and for the whole raw URL, which is longer
// than any text).

const marker = "zq7"

type genURL struct {
	scheme, host, port, path, query string // as written
	bad                             string // "" for a valid URL, else the kind of flaw
	raw                             string
}

// key is the model's identity of a valid URL, the rule stated plainly: two URLs are the same endpoint when their
// scheme, their host, their port and their path and query are the same, where the scheme and the host are compared
// without case, a default port (80 for http, 443 for https) is the same as no port, and an empty path is the same as
// "/". Nothing else is folded: a path or query that differs by a trailing slash ("/p" and "/p/") or by the case of a
// letter ("/p" and "/P", "a=1" and "A=1") is another endpoint.
func (u genURL) key() string {
	scheme, host := strings.ToLower(u.scheme), strings.ToLower(u.host)
	port := u.port
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	path := u.path
	if path == "" {
		path = "/"
	}
	return scheme + "|" + host + "|" + port + "|" + path + "|" + u.query
}

func (u genURL) build() genURL {
	hostport := u.host
	if u.port != "" {
		hostport += ":" + u.port
	}
	rest := u.path
	if u.query != "" {
		rest += "?" + u.query
	}
	switch u.bad {
	case "":
		u.raw = u.scheme + "://" + hostport + rest
	case "userinfo":
		u.raw = u.scheme + "://" + marker + "user@" + hostport + rest
	case "fragment":
		u.raw = u.scheme + "://" + hostport + rest + "#" + marker
	case "scheme":
		u.raw = "ftp://" + hostport + rest
	case "long":
		u.raw = u.scheme + "://" + hostport + "/" + strings.Repeat("p", 520)
	case "space":
		u.raw = u.scheme + "://" + hostport + "/" + marker + " x"
	case "nohost":
		u.raw = u.scheme + ":///" + marker
	case "empty":
		u.raw = ""
	case "port0", "port65536", "port99999", "portempty": // a port outside 1 to 65535, or a colon with no port
		u.raw = u.scheme + "://" + u.host + ":" + map[string]string{"port0": "0", "port65536": "65536", "port99999": "99999", "portempty": ""}[u.bad] + rest
	}
	return u
}

type genEntry struct {
	name string
	url  genURL
	file string
}

func nameOK(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// differ writes a URL that is another endpoint from u, by the one thing the identity does not fold: a trailing slash
// on a path that has more than the root, or the case of the letters of the path or of the query. It reports false when
// u has nothing to change that way.
func differ(r *rand.Rand, u genURL) (genURL, bool) {
	var options []func(genURL) genURL
	if len(u.path) > 1 {
		options = append(options, func(u genURL) genURL {
			if strings.HasSuffix(u.path, "/") {
				u.path = strings.TrimSuffix(u.path, "/")
			} else {
				u.path += "/"
			}
			return u
		})
		options = append(options, func(u genURL) genURL { u.path = strings.ToUpper(u.path); return u })
	}
	if u.query != "" {
		options = append(options, func(u genURL) genURL { u.query = strings.ToUpper(u.query); return u })
	}
	if len(options) == 0 {
		return u, false
	}
	return options[r.IntN(len(options))](u).build(), true
}

// respell writes the same endpoint another way: the case of the scheme and host, a default port added or dropped, the
// empty path as "/".
func respell(r *rand.Rand, u genURL) genURL {
	if r.IntN(2) == 0 {
		u.scheme = strings.ToUpper(u.scheme)
	}
	if r.IntN(2) == 0 {
		u.host = strings.ToUpper(u.host)
	}
	lower := strings.ToLower(u.scheme)
	switch {
	case u.port == "" && r.IntN(2) == 0:
		u.port = map[string]string{"http": "80", "https": "443"}[lower]
	case (lower == "http" && u.port == "80") || (lower == "https" && u.port == "443"):
		if r.IntN(2) == 0 {
			u.port = ""
		}
	}
	if u.path == "" && r.IntN(2) == 0 {
		u.path = "/"
	} else if u.path == "/" && r.IntN(2) == 0 {
		u.path = ""
	}
	return u.build()
}

func genCase(r *rand.Rand) (entries []genEntry, alert *genURL) {
	n := r.IntN(11)
	if r.IntN(3) > 0 && n == 0 {
		n = 1 + r.IntN(8)
	}
	flaw := []float64{0, 0, 0.04, 0.15}[r.IntN(4)]
	paths := []string{"", "/", "/p", "/p/", "/q"}
	for i := 0; i < n; i++ {
		e := genEntry{
			name: fmt.Sprintf("%s-n%d", marker, i),
			url:  genURL{scheme: []string{"http", "https"}[r.IntN(2)], host: fmt.Sprintf("%shost%d.example.com", marker, i), path: paths[r.IntN(len(paths))]}.build(),
			file: fmt.Sprintf("/%s/secrets/s%d", marker, i),
		}
		if r.IntN(3) == 0 {
			e.url.query = marker + "=1"
			e.url = e.url.build()
		}
		if r.IntN(4) == 0 { // a port that is not the default one: a different endpoint (1 and 65535 are the ends)
			e.url.port = []string{"8443", "8443", "1", "65535"}[r.IntN(4)]
			e.url = e.url.build()
		}
		if r.Float64() < flaw {
			switch r.IntN(10) {
			case 0:
				e.name = ""
			case 1:
				e.name = marker + strings.Repeat("n", 61) // 64 characters: valid
			case 2:
				e.name = marker + strings.Repeat("n", 62) // 65
			case 3:
				e.name = []string{marker + " b", marker + "/b", marker + "é", marker + "\n", marker + ":b"}[r.IntN(5)]
			case 4:
				if i > 0 {
					e.name = entries[r.IntN(i)].name
				}
			case 5: // names that differ only in case: valid and different
				if i > 0 {
					prev := entries[r.IntN(i)].name
					e.name = strings.ToUpper(prev)
				}
			case 6:
				e.file = ""
			case 7:
				if i > 0 {
					e.file = entries[r.IntN(i)].file
				}
			case 8:
				if i > 0 {
					e.url = respell(r, entries[r.IntN(i)].url)
				}
			case 9:
				kinds := []string{"userinfo", "fragment", "scheme", "long", "space", "nohost", "empty", "port0", "port65536", "port99999", "portempty"}
				e.url.bad = kinds[r.IntN(len(kinds))]
				e.url = e.url.build()
			}
		}
		if r.Float64() < flaw/2 && i > 0 { // a near duplicate: the same URL but for the path or the query is another endpoint
			u := entries[r.IntN(i)].url
			if r.IntN(2) == 0 {
				u.path = strings.TrimSuffix(u.path, "/") + "/x"
			} else {
				u.query += "&" + marker + "=2"
			}
			if u.bad == "" {
				e.url = u.build()
			}
		}
		// The same as an earlier URL but for a trailing slash or a letter's case.
		if i > 0 && e.url.bad == "" && r.IntN(4) == 0 {
			if u := entries[r.IntN(i)].url; u.bad == "" {
				if d, ok := differ(r, u); ok {
					e.url = d
				}
			}
		}
		entries = append(entries, e)
	}
	if r.IntN(10) < 4 {
		a := genURL{scheme: "https", host: marker + "alert.example.com", path: "/a"}.build()
		if n > 0 && r.IntN(2) == 0 {
			a = respell(r, entries[r.IntN(n)].url)
			a.bad = "" // an alerts URL is always well formed: the model asks whether it equals an endpoint's
			a = a.build()
			if d, ok := differ(r, a); ok && r.IntN(2) == 0 { // another endpoint, by a trailing slash or a letter's case
				a = d
			}
		}
		alert = &a
	}
	return entries, alert
}

// modelAccepts is the rules above, applied to the generated list.
func modelAccepts(entries []genEntry, alert *genURL) bool {
	if len(entries) < 1 || len(entries) > 8 {
		return false
	}
	names, urls, files := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if !nameOK(e.name) || e.url.bad != "" || e.file == "" {
			return false
		}
		if names[e.name] || urls[e.url.key()] || files[e.file] {
			return false
		}
		names[e.name], urls[e.url.key()], files[e.file] = true, true, true
	}
	if alert != nil && urls[alert.key()] {
		return false
	}
	return true
}

// refusalText is every text the list's checks may give.
var refusalText = regexp.MustCompile(`^config: (` +
	`webhooks must list 1 to 8 endpoints` +
	`|webhooks\[\d+\]\.name must be 1 to 64 characters of A-Z a-z 0-9 \. _ -` +
	`|webhooks\[\d+\]\.url must be a http or https URL with a host, without user info or fragment, spaces or control characters, at most 512 bytes \(a query is allowed\)` +
	`|webhooks\[\d+\]\.secret_file is missing` +
	`|webhooks\[\d+\]\.(name|url|secret_file) repeats webhooks\[\d+\]\.(name|url|secret_file)` +
	`|alerts\.url must differ from webhooks\[\d+\]\.url)$`)

func TestWebhooksListAgainstModel(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const seed = 20261111
	var accepted, refused int
	reasons := map[string]int{}
	for i := 0; i < 4000; i++ {
		r := rand.New(rand.NewPCG(seed, uint64(i)))
		entries, alert := genCase(r)
		list := make([]any, 0, len(entries))
		for _, e := range entries {
			list = append(list, map[string]any{"name": e.name, "url": e.url.raw, "secret_file": e.file})
		}
		m := goodConfig()
		m["webhooks"] = list
		if alert != nil {
			m["alerts"] = map[string]any{"url": alert.raw, "secret_file": "/" + marker + "/alerts.secret"}
		}
		c, err := ParseConfig(enc(t, m))
		want := modelAccepts(entries, alert)
		if (err == nil) != want {
			t.Fatalf("seed %d case %d: the model says accepted=%v, ParseConfig says %v\n%s", seed, i, want, err, enc(t, m))
		}
		if err != nil {
			refused++
			msg := err.Error()
			if !refusalText.MatchString(msg) {
				t.Fatalf("seed %d case %d: a message outside the documented texts: %q", seed, i, msg)
			}
			if strings.Contains(strings.ToLower(msg), marker) {
				t.Fatalf("seed %d case %d: the message echoes a value: %q", seed, i, msg)
			}
			for _, e := range entries {
				for _, v := range []string{e.url.raw, e.file} {
					if len(v) > 8 && strings.Contains(msg, v) {
						t.Fatalf("seed %d case %d: the message echoes %q", seed, i, v)
					}
				}
			}
			family := "webhooks"
			if strings.HasPrefix(msg, "config: alerts") {
				family = "alerts"
			}
			reasons[family]++
			continue
		}
		accepted++
		// An accepted list is one the deliverers accept: the start does not fail after the store is opened.
		eps := make([]webhook.Endpoint, len(c.Webhooks))
		for j, w := range c.Webhooks {
			eps[j] = webhook.Endpoint{Name: w.Name, URL: w.URL, Secret: make([]byte, 32)}
		}
		if _, err := webhook.NewEventDeliverers(eps, st, time.Now, nil, nil); err != nil {
			t.Fatalf("seed %d case %d: accepted by the configuration, refused by the deliverers: %v", seed, i, err)
		}
	}
	// The generator reaches both outcomes, and every family of refusal.
	if accepted < 200 || refused < 1000 {
		t.Fatalf("the generator is lopsided: %d accepted, %d refused", accepted, refused)
	}
	for _, k := range []string{"webhooks", "alerts"} {
		if reasons[k] == 0 {
			t.Errorf("no refusal of the family %q (%v)", k, reasons)
		}
	}
}
