// Package docs holds one test: every relative Markdown link of the documentation points to a file that exists, and
// every anchor in such a link matches a heading of that file.
package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// linkRe matches an inline Markdown link [text](target). The target ends at the first space or closing parenthesis, so
// a title after the target is ignored.
var linkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)[^)]*\)`)

// headingRe matches an ATX heading and captures its text.
var headingRe = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)

// slug turns a heading into its GitHub anchor: lowercase, spaces to hyphens, and every character other than a letter, a
// digit, a hyphen or an underscore dropped.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z', r > 127 && isLetter(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isLetter(r rune) bool { return strings.ToLower(string(r)) != strings.ToUpper(string(r)) }

// fence reports whether a line opens or closes a fenced code block, and with which fence character.
func fence(line string) (byte, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, false
	}
	if strings.HasPrefix(t, "```") {
		return '`', true
	}
	if strings.HasPrefix(t, "~~~") {
		return '~', true
	}
	return 0, false
}

// anchors returns the anchors of the headings of a Markdown file. A repeated heading gets -1, -2, ... in order.
func anchors(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	seen := map[string]int{}
	var open byte
	for _, line := range strings.Split(string(b), "\n") {
		if c, ok := fence(line); ok {
			switch open {
			case 0:
				open = c
			case c:
				open = 0
			}
			continue
		}
		if open != 0 {
			continue
		}
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s := slug(m[1])
		if n := seen[s]; n > 0 {
			out[s+"-"+strconv.Itoa(n)] = true
		} else {
			out[s] = true
		}
		seen[s]++
	}
	return out
}

// problem is one broken link: the file and line it is on, the link as written and what is wrong with it.
type problem struct {
	file string
	line int
	link string
	why  string
}

// check returns the broken relative links of one Markdown file. root is the repository root and file is relative to it.
func check(t *testing.T, root, file string) []problem {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		t.Fatal(err)
	}
	var out []problem
	var open byte
	cache := map[string]map[string]bool{}
	for i, line := range strings.Split(string(b), "\n") {
		if c, ok := fence(line); ok {
			switch open {
			case 0:
				open = c
			case c:
				open = 0
			}
			continue
		}
		if open != 0 {
			continue
		}
		for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(target, "#")
			dest := file
			if path != "" {
				dest = filepath.ToSlash(filepath.Join(filepath.Dir(file), path))
			}
			info, err := os.Stat(filepath.Join(root, dest))
			if err != nil {
				out = append(out, problem{file, i + 1, target, "no such file " + dest})
				continue
			}
			if anchor == "" {
				continue
			}
			if info.IsDir() || !strings.HasSuffix(dest, ".md") {
				out = append(out, problem{file, i + 1, target, "an anchor needs a Markdown file, not " + dest})
				continue
			}
			a, ok := cache[dest]
			if !ok {
				a = anchors(t, filepath.Join(root, dest))
				cache[dest] = a
			}
			if !a[anchor] {
				out = append(out, problem{file, i + 1, target, "no heading of " + dest + " has the anchor #" + anchor})
			}
		}
	}
	return out
}

// files lists the Markdown files that are checked: the front documents and every Markdown file of docs/.
func files(t *testing.T, root string) []string {
	t.Helper()
	list := []string{"README.md", "ACKNOWLEDGEMENTS.md", "CLAUDE.md", "SECURITY.md", "deploy/README.md", "qualify/REPORT.md"}
	m, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, filepath.ToSlash(r))
	}
	return list
}

// TestDocLinks: every relative link of the documentation resolves.
func TestDocLinks(t *testing.T) {
	for _, f := range files(t, "..") {
		for _, p := range check(t, "..", f) {
			t.Errorf("%s:%d: link (%s): %s", p.file, p.line, p.link, p.why)
		}
	}
}

// TestDocLinksCheckerBites: the checker itself reports a missing file and a missing anchor, and accepts a good link,
// a repeated heading's numbered anchor and a link inside a code block that points nowhere.
func TestDocLinksCheckerBites(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.md", "# Title\n\n## Same\n\n## Same\n\n## A `code` heading, with (marks)!\n")
	write("a.md", "[ok](b.md) [ok](b.md#title) [ok](b.md#same) [ok](b.md#same-1) [ok](#local)\n"+
		"[ok](b.md#a-code-heading-with-marks) [ext](https://example.com/x.md#nope)\n"+
		"[gone](c.md)\n"+
		"[bad](b.md#nowhere)\n"+
		"[bad](b.md#same-2)\n"+
		"```\n[in code](nowhere.md)\n```\n"+
		"# Local\n")
	got := check(t, dir, "a.md")
	want := map[int]string{3: "c.md", 4: "b.md#nowhere", 5: "b.md#same-2"}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		if want[p.line] != p.link {
			t.Errorf("unexpected problem %+v", p)
		}
	}
}
