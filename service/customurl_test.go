package service

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The token in the tests' custom URL files: it never reaches an error (nor, in run_test.go, a log).
const fileToken = "file-token-for-tests-9f8e7d"

// writeURLFile writes body to dir/name with mode and returns the path.
func writeURLFile(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // past the umask
		t.Fatal(err)
	}
	return p
}

// noLeak fails when an error text holds the token, the custom host or the file's directory.
func noLeak(t *testing.T, what string, err error, dir string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, bad := range []string{fileToken, "example.invalid", dir} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("%s: the error %q holds %q", what, err, bad)
		}
	}
}

func TestLoadCustomURL(t *testing.T) {
	dir := t.TempDir()
	good := "https://rates.example.invalid/v1/rates?token=" + fileToken
	for name, c := range map[string]struct {
		body string
		mode os.FileMode
		want string
	}{
		"a trailing newline":      {good + "\n", 0o600, good},
		"no trailing newline":     {good, 0o600, good},
		"mode 0400":               {good + "\n", 0o400, good},
		"no query":                {"https://rates.example.invalid/v1", 0o600, "https://rates.example.invalid/v1"},
		"512 bytes":               {urlOfLen(512) + "\n", 0o600, urlOfLen(512)},
		"a port and a path query": {"https://rates.example.invalid:8443/a/b?x=1&y=" + fileToken, 0o600, "https://rates.example.invalid:8443/a/b?x=1&y=" + fileToken},
	} {
		p := writeURLFile(t, dir, "ok-"+strings.ReplaceAll(name, " ", "-"), c.body, c.mode)
		got, err := loadCustomURL(p)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}

	refused := map[string]struct {
		body string
		mode os.FileMode
	}{
		"mode 0640":          {good, 0o640},
		"mode 0604":          {good, 0o604},
		"mode 0644":          {good, 0o644},
		"mode 0700":          {good, 0o700},
		"mode 0660":          {good, 0o660},
		"empty":              {"", 0o600},
		"a newline only":     {"\n", 0o600},
		"two trailing lines": {good + "\n\n", 0o600},
		"a CRLF":             {good + "\r\n", 0o600},
		"a leading space":    {" " + good, 0o600},
		"a space inside":     {"https://rates.example.invalid/a b?token=" + fileToken, 0o600},
		"a tab inside":       {"https://rates.example.invalid/a\tb?token=" + fileToken, 0o600},
		"a NUL":              {good + "\x00", 0o600},
		"a non-ASCII byte":   {"https://rates.example.invalid/\xc3\xa9?token=" + fileToken, 0o600},
		"an invalid UTF-8":   {"https://rates.example.invalid/\xff?token=" + fileToken, 0o600},
		"http":               {"http://rates.example.invalid/?token=" + fileToken, 0o600},
		"user info":          {"https://" + fileToken + "@rates.example.invalid/", 0o600},
		"user and password":  {"https://u:" + fileToken + "@rates.example.invalid/", 0o600},
		"a fragment":         {good + "#x", 0o600},
		"an empty fragment":  {good + "#", 0o600},
		"no host":            {"https:///v1?token=" + fileToken, 0o600},
		"relative":           {"/v1?token=" + fileToken, 0o600},
		"opaque":             {"https:rates.example.invalid?token=" + fileToken, 0o600},
		"513 bytes":          {urlOfLen(513), 0o600},
		"two URLs":           {good + "\n" + good + "\n", 0o600},
		"far too long":       {good + strings.Repeat("a", 4096), 0o600},
		"a JSON string":      {`"` + good + `"`, 0o600},
	}
	for name, c := range refused {
		p := writeURLFile(t, dir, "bad-"+strings.ReplaceAll(name, " ", "-"), c.body, c.mode)
		got, err := loadCustomURL(p)
		if err == nil {
			t.Errorf("%s: accepted %q", name, got)
		}
		noLeak(t, name, err, dir)
	}

	// A URL that already names an asset: SatBooth adds asset=<asset> to every request, so the operator's own would make
	// two. Refused with one fixed text that echoes nothing of the URL; a parameter that only looks alike is kept.
	for name, body := range map[string]string{
		"asset first":         "https://rates.example.invalid/v1?asset=BTC&token=" + fileToken,
		"asset last":          "https://rates.example.invalid/v1?token=" + fileToken + "&asset=XMR",
		"asset empty":         "https://rates.example.invalid/v1?token=" + fileToken + "&asset=",
		"asset with no value": "https://rates.example.invalid/v1?asset&token=" + fileToken,
		"asset escaped":       "https://rates.example.invalid/v1?%61sset=BTC&token=" + fileToken,
		"asset twice":         "https://rates.example.invalid/v1?asset=BTC&asset=XMR",
		// A query that does not parse: url.Query skips the bad pair, but a lenient server may still read an asset.
		"a bad escape":          "https://rates.example.invalid/v1?asset=%zz",
		"a cut escape":          "https://rates.example.invalid/v1?asset=BTC%",
		"a semicolon after":     "https://rates.example.invalid/v1?asset=BTC;x=1",
		"a semicolon before":    "https://rates.example.invalid/v1?x=1;asset=BTC",
		"a bad escape no asset": "https://rates.example.invalid/v1?token=%zz",
	} {
		p := writeURLFile(t, dir, "asset-"+strings.ReplaceAll(name, " ", "-"), body+"\n", 0o600)
		got, err := loadCustomURL(p)
		if err == nil || err.Error() != msgCustomURLAsset {
			t.Errorf("%s: %q, %v; want %q", name, got, err, msgCustomURLAsset)
		}
		noLeak(t, name, err, dir)
	}
	for _, body := range []string{
		"https://rates.example.invalid/v1?assets=1&token=" + fileToken,
		"https://rates.example.invalid/v1?token=asset",
		"https://rates.example.invalid/asset?x=1",
		"https://rates.example.invalid/v1?x=asset%3DBTC",
	} {
		p := writeURLFile(t, dir, "like-asset", body, 0o600)
		if got, err := loadCustomURL(p); err != nil || got != body {
			t.Errorf("%q: %q, %v", body, got, err)
		}
	}

	// Not a regular file: a symbolic link to a good file, a directory, a FIFO (opening it must not wait), missing.
	target := writeURLFile(t, dir, "target", good, 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"a symbolic link": link, "a directory": sub, "a FIFO": fifo, "missing": filepath.Join(dir, "nope")} {
		got, err := loadCustomURL(p)
		if err == nil {
			t.Errorf("%s: accepted %q", name, got)
		}
		noLeak(t, name, err, dir)
	}
}

// FuzzLoadCustomURL: whatever the file holds, the loader returns either an error that holds no byte of the token, or a
// URL that ParseConfig's rules accept for a custom URL file (https, a host, no user info or fragment) and that is the
// content minus at most one final newline.
func FuzzLoadCustomURL(f *testing.F) {
	for _, s := range []string{"", "\n", "https://rates.example.invalid/?token=" + fileToken + "\n", "https://u@h/", "https://h/#",
		"http://h/", urlOfLen(512), urlOfLen(513), "https://h/\r\n", "https://h/\xff", "https://h/?" + fileToken + "\n\n",
		"https://h/?asset=BTC", "https://h/?x=1&%61sset=XMR\n", "https://h/?assets=1",
		"https://h/?asset=%zz", "https://h/?asset=BTC%", "https://h/?asset=BTC;x=1", "https://h/?x=1;asset=BTC",
		"https://h/?a%2"} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, b []byte) {
		p := filepath.Join(dir, "u")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadCustomURL(p)
		if err != nil {
			if strings.Contains(err.Error(), fileToken) || strings.Contains(err.Error(), dir) {
				t.Fatalf("the error %q leaks", err)
			}
			return
		}
		if got != strings.TrimSuffix(string(b), "\n") {
			t.Fatalf("%q from %q", got, b)
		}
		u, err := plainURL("x", got, false, "https")
		if err != nil {
			t.Fatalf("accepted %q: %v", got, err)
		}
		if !queryNamesNoAsset(u.RawQuery) {
			t.Fatalf("accepted %q, whose query is malformed or names an asset", got)
		}
	})
}

// msgCustomURLAsset is the one text for a custom URL file whose URL's query does not parse or already has an asset
// parameter.
const msgCustomURLAsset = "custom URL file refused: its URL's query must parse (no ';', every '%' followed by two hex " +
	"digits) and must not carry an asset parameter (SatBooth adds asset=<asset> to every request)"

// CheckFiles loads rates.custom_url_file like every other secret file: a good file passes, a loose or missing one fails
// with the key named and nothing of the content or path.
func TestCheckFilesCustomURLFile(t *testing.T) {
	dir := t.TempDir()
	keyPath, apiPath, whPath := writeFixtures(t, dir)
	u := writeURLFile(t, dir, "rates.url", "https://rates.example.invalid/v1?token="+fileToken+"\n", 0o600)
	m := goodConfig()
	m["nwc"].(map[string]any)["client_key_file"] = keyPath
	m["api_keys_file"] = apiPath
	m["webhooks"].([]any)[0].(map[string]any)["secret_file"] = whPath
	r := ratesBlock()
	delete(r, "custom_url")
	r["custom_url_file"] = u
	m["rates"] = r
	c, err := ParseConfig(enc(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(){
		"mode 0644": func() { os.Chmod(u, 0o644) },
		"mode 0640": func() { os.Chmod(u, 0o640) },
		"empty":     func() { os.WriteFile(u, nil, 0o600); os.Chmod(u, 0o600) },
		"http":      func() { os.WriteFile(u, []byte("http://rates.example.invalid/?token="+fileToken), 0o600) },
		"asset":     func() { os.WriteFile(u, []byte("https://rates.example.invalid/?token="+fileToken+"&asset=BTC"), 0o600) },
		"missing":   func() { os.Remove(u) },
	} {
		mod()
		err := c.CheckFiles()
		if err == nil || !strings.HasPrefix(err.Error(), "config: rates.custom_url_file: ") {
			t.Errorf("%s: %v", name, err)
		}
		noLeak(t, name, err, dir)
		writeURLFile(t, dir, "rates.url", "https://rates.example.invalid/v1?token="+fileToken+"\n", 0o600)
	}
	// Without the file key, CheckFiles opens no custom URL file.
	c.Rates.CustomURLFile, c.Rates.CustomURL = "", "https://rates.example.invalid/v1"
	os.Remove(u)
	if err := c.CheckFiles(); err != nil {
		t.Fatal(err)
	}
}

// queryNamesNoAsset is the fuzz oracle's own reading of a raw query, written without net/url: false when the query has
// a ';', a '%' not followed by two hex digits, or a pair (split on '&') whose key, percent-decoded with '+' as a
// space, is "asset".
func queryNamesNoAsset(raw string) bool {
	if strings.Contains(raw, ";") {
		return false
	}
	hex := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	for _, pair := range strings.Split(raw, "&") {
		key, _, _ := strings.Cut(pair, "=")
		var dec []byte
		for i := 0; i < len(pair); i++ {
			if pair[i] != '%' {
				continue
			}
			if i+2 >= len(pair) {
				return false
			}
			if _, ok := hex(pair[i+1]); !ok {
				return false
			}
			if _, ok := hex(pair[i+2]); !ok {
				return false
			}
		}
		for i := 0; i < len(key); i++ {
			switch c := key[i]; {
			case c == '%':
				hi, _ := hex(key[i+1])
				lo, _ := hex(key[i+2])
				dec = append(dec, hi<<4|lo)
				i += 2
			case c == '+':
				dec = append(dec, ' ')
			default:
				dec = append(dec, c)
			}
		}
		if string(dec) == "asset" {
			return false
		}
	}
	return true
}

// TestQueryNamesNoAsset checks the fuzz oracle itself.
func TestQueryNamesNoAsset(t *testing.T) {
	for raw, want := range map[string]bool{
		"": true, "x=1": true, "token=abc&y=2": true, "assets=1": true, "x=asset": true, "x=%41": true, "a+b=1": true,
		"asset=BTC": false, "x=1&asset": false, "%61sset=1": false, "%61%73set=": false, "asset=%zz": false,
		"x=%zz": false, "x=%4": false, "x=%": false, "x=1;y=2": false, "x=1&asset=BTC;": false,
	} {
		if got := queryNamesNoAsset(raw); got != want {
			t.Errorf("queryNamesNoAsset(%q) = %v, want %v", raw, got, want)
		}
	}
}
