package main

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzParseHub: parseHub never panics; an accepted value is an http or https URL with a host, without
// query, fragment, whitespace, control characters or a trailing "/"; a rejection is the one fixed message.
func FuzzParseHub(f *testing.F) {
	seeds := []string{
		"http://127.0.0.1:8080", "https://hub.example/", "https://hub.example/sub/", "https://hub.example/sub",
		"", "hub.example", "ftp://hub.example", "javascript:alert(1)", "http://", "https:///x",
		"https://hub.example?x=1", "https://hub.example/?", "https://hub.example/#f", "https://hub.example#",
		"https://hub.example//", "https://u:p@hub.example", "https://hub.example/a b", "http://hub.example\n",
		"http://h:1/", "https://h/../x", "http://[::1]:8080", "HTTP://h", "http://h/%2F",
		"http://h\x00", "http://h\x1f/", "http://h\x7f", "http://h\r\n/", "http://h\t",
		"http://" + strings.Repeat("a", 70*1024),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := parseHub(s)
		if err != nil {
			if got != "" {
				t.Fatalf("error with non-empty result %q", got)
			}
			if err.Error() != "-hub must be an http or https URL with a host and no query or fragment" {
				t.Fatalf("unexpected error text %q", err.Error())
			}
			return
		}
		u, perr := url.Parse(got)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			t.Fatalf("accepted %q -> %q: bad URL (%v)", s, got, perr)
		}
		if strings.ContainsAny(got, "?#") || strings.HasSuffix(got, "/") {
			t.Fatalf("accepted %q -> %q: query, fragment or trailing slash", s, got)
		}
		for _, r := range got {
			if r <= ' ' || r == 0x7f {
				t.Fatalf("accepted %q -> %q: whitespace or control character", s, got)
			}
		}
	})
}
