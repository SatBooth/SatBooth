package xmrrpc

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// formatPassword is the password the format tests give New; leakForms gives every form fmt can print its
// bytes in.
const formatPassword = "Fmt-Leak-Probe-7f3a"

func leakForms() []string {
	b := []byte(formatPassword)
	forms := []string{formatPassword, base64.StdEncoding.EncodeToString(b), fmt.Sprintf("%q", formatPassword)}
	for _, v := range []string{"%d", "%o", "%O", "%b", "%x", "%X", "% x", "%U", "%c", "%q", "%v"} {
		forms = append(forms, strings.Trim(fmt.Sprintf(v, b), "[]"))
	}
	// the first three bytes are enough to show a list of them
	for _, v := range []string{"%d", "%o", "%b", "%U"} {
		forms = append(forms, strings.Trim(fmt.Sprintf(v, b[:3]), "[]"))
	}
	return forms
}

func formatConfig() Config { return Config{URL: "http://127.0.0.1:1", User: "u"} }

func formatClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(formatConfig(), []byte(formatPassword))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var formatVerbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "% x", "%#x", "%p", "%d", "%o", "%O", "%b",
	"%c", "%U", "%e", "%f", "%g", "%t", "%T", "%z", "%!", "%10.3v", "%-+#v", "%08d", "%+q", "%#q", "%v%v"}

func checkFormats(t *testing.T, values map[string]any, verbs []string) {
	t.Helper()
	forms := leakForms()
	for name, x := range values {
		for _, v := range verbs {
			out := fmt.Sprintf(v, x)
			for _, f := range forms {
				if f != "" && strings.Contains(out, f) {
					t.Errorf("%s of %s holds the password as %q: %.160q", v, name, f, out)
					break
				}
			}
		}
	}
}

// clientValue returns a copy of a struct holding a Client by value, as an interface (built through reflect,
// because copying a Client in code is what go vet's copylocks refuses): fmt prints such a value with no
// method of the Client.
func clientValue(c *Client) any {
	v := reflect.New(reflect.TypeFor[Client]())
	v.Elem().Set(reflect.ValueOf(c).Elem())
	return v.Elem().Interface()
}

// TestClientEveryVerb: a Client holding a known password, formatted with every verb,
// flag and width, as a pointer, as a value, and inside exported and unexported struct fields, slices, arrays,
// maps and interfaces, prints the password's bytes in no form: the password sits behind a pointer, which fmt
// prints as an address wherever it does not call a method.
func TestClientEveryVerb(t *testing.T) {
	c := formatClient(t)
	checkFormats(t, map[string]any{
		"pointer": c, "value": clientValue(c), "struct": struct{ C *Client }{c}, "unexported": struct{ c *Client }{c},
		"nested unexported": struct{ s struct{ c *Client } }{struct{ c *Client }{c}},
		"slice":             []*Client{c}, "array": [1]*Client{c}, "map": map[string]*Client{"k": c}, "interface": []any{c},
		"map of unexported": map[string]struct{ c *Client }{"k": {c}},
	}, formatVerbs)
}

// TestConfigEveryVerb: Config holds no password (New takes it beside the Config), so no rendering of one can.
func TestConfigEveryVerb(t *testing.T) {
	cfg := formatConfig()
	_ = formatClient(t)
	checkFormats(t, map[string]any{
		"value": cfg, "pointer": &cfg, "struct": struct{ C Config }{cfg}, "unexported": struct{ c Config }{cfg},
		"slice": []Config{cfg}, "array": [1]Config{cfg}, "map": map[string]Config{"k": cfg}, "interface": []any{cfg},
	}, formatVerbs)
}

// TestConfigFormatGaps: the two forms fmt prints with no method at all, %p of a non-pointer and a value in
// an unexported struct field, reach neither a password in a Config (there is none) nor the
// Client's (behind a pointer).
func TestConfigFormatGaps(t *testing.T) {
	cfg, c := formatConfig(), formatClient(t)
	checkFormats(t, map[string]any{
		"config": cfg, "config in a struct": struct{ C Config }{cfg}, "client value": clientValue(c),
		"client in a struct": struct{ C *Client }{c},
	}, []string{"%p"})
	checkFormats(t, map[string]any{
		"unexported config": struct{ c Config }{cfg}, "unexported client": struct{ c *Client }{c},
		"unexported client value": struct{ c any }{clientValue(c)},
	}, []string{"%v", "%+v", "%#v", "%d", "%x", "%s"})
}
