package qr

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/qr/coding"
)

// eci writes an ECI header (mode 0111 and an 8-bit assignment number), the way ZXing does for a
// UTF-8 hint. It exists only so the byte-mode vector can be built from the library's pieces.
type eci int

func (e eci) Check() error                            { return nil }
func (e eci) Bits(v coding.Version) int               { return 12 }
func (e eci) Encode(b *coding.Bits, _ coding.Version) { b.Write(7, 4); b.Write(uint(e), 8) }

func readMatrix(t testing.TB, name string) [][]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var m [][]bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row []bool
		for _, f := range strings.Fields(line) {
			switch f {
			case "0":
				row = append(row, false)
			case "1":
				row = append(row, true)
			default:
				t.Fatalf("%s: bad cell %q", name, f)
			}
		}
		m = append(m, row)
	}
	return m
}

// TestVectors compares the copied encoder, module by module, with symbols that another
// implementation produced (ZXing's unit-test goldens; see testdata/SOURCE.txt).
func TestVectors(t *testing.T) {
	cases := []struct {
		file  string
		level coding.Level
		mask  coding.Mask
		enc   []coding.Encoding
	}{
		{"zxing_alpha_ABCDEF_v1_H_mask0.txt", coding.H, 0, []coding.Encoding{coding.Alpha("ABCDEF")}},
		{"zxing_num_0123_v1_M_mask0.txt", coding.M, 0, []coding.Encoding{coding.Num("0123")}},
		{"zxing_byte_hello_eci26_v1_H_mask3.txt", coding.H, 3, []coding.Encoding{eci(26), coding.String("hello")}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			want := readMatrix(t, tc.file)
			p, err := coding.NewPlan(1, tc.level, tc.mask)
			if err != nil {
				t.Fatal(err)
			}
			c, err := p.Encode(tc.enc...)
			if err != nil {
				t.Fatal(err)
			}
			if c.Size != len(want) {
				t.Fatalf("size %d, want %d", c.Size, len(want))
			}
			for y := range want {
				if len(want[y]) != c.Size {
					t.Fatalf("golden row %d has %d cells", y, len(want[y]))
				}
				for x := range want[y] {
					if c.Black(x, y) != want[y][x] {
						t.Fatalf("module (x=%d,y=%d) = %v, want %v", x, y, c.Black(x, y), want[y][x])
					}
				}
			}
		})
	}
}

const alnum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

// genInputs returns n seeded inputs: lengths at and around every version's capacity, random
// lengths up to MaxInput, all three kinds (alphanumeric, bytes that include every value, and a
// lowercase-only invoice-like text), so that every length class and every byte value occurs.
func genInputs(seed int64, n int) []string {
	r := rand.New(rand.NewSource(seed))
	var lengths []int
	for v := coding.Version(1); v <= coding.MaxVersion; v++ {
		cap := v.DataBytes(coding.M)
		for d := -4; d <= 4; d++ {
			if l := cap + d; l >= 1 && l <= MaxInput {
				lengths = append(lengths, l)
			}
		}
	}
	lengths = append(lengths, 1, 2, 3, MaxInput-1, MaxInput)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var l int
		if i < len(lengths) {
			l = lengths[i]
		} else {
			l = 1 + r.Intn(MaxInput)
		}
		b := make([]byte, l)
		switch i % 3 {
		case 0:
			for j := range b {
				b[j] = alnum[r.Intn(len(alnum))]
			}
		case 1:
			for j := range b {
				b[j] = byte(r.Intn(256))
			}
		default:
			for j := range b {
				b[j] = byte(j + i) // sweeps through every byte value
			}
		}
		out = append(out, string(b))
	}
	return out
}

func bch(v, gen uint32, dataBits, total int) uint32 {
	r := v << uint(total-dataBits)
	for i := total - 1; i >= total-dataBits; i-- {
		if r&(1<<uint(i)) != 0 {
			r ^= gen << uint(i-(total-dataBits))
		}
	}
	return r
}

// formatBits reads the two copies of the 15 format bits, placed as in the ZXing goldens.
func formatBits(c *coding.Code) (a, b uint32) {
	n := c.Size
	set := func(dst *uint32, j int, x, y int) {
		if c.Black(x, y) {
			*dst |= 1 << uint(j)
		}
	}
	for j := 0; j <= 14; j++ {
		switch {
		case j <= 5:
			set(&a, j, 8, j)
		case j == 6:
			set(&a, j, 8, 7)
		case j == 7:
			set(&a, j, 8, 8)
		case j == 8:
			set(&a, j, 7, 8)
		default:
			set(&a, j, 14-j, 8)
		}
		if j < 8 {
			set(&b, j, n-1-j, 8)
		} else {
			set(&b, j, 8, n-15+j)
		}
	}
	return a, b
}

func checkStructure(t *testing.T, text string) {
	t.Helper()
	c, v, err := encode(text)
	if err != nil {
		t.Fatalf("len %d: %v", len(text), err)
	}
	n := c.Size
	if n != 17+4*int(v) {
		t.Fatalf("len %d: size %d for version %d", len(text), n, v)
	}
	dark := func(x, y int) bool { return c.Black(x, y) }
	// Finder patterns with their separators.
	for _, o := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				x, y := o[0]+dx, o[1]+dy
				if x < 0 || y < 0 || x >= n || y >= n {
					continue
				}
				want := false
				if dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6 {
					edge := dx == 0 || dx == 6 || dy == 0 || dy == 6
					core := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
					want = edge || core
				}
				if dark(x, y) != want {
					t.Fatalf("len %d: finder module (%d,%d) = %v", len(text), x, y, !want)
				}
			}
		}
	}
	// Timing patterns and the fixed dark module.
	for i := 8; i < n-8; i++ {
		if dark(i, 6) != (i%2 == 0) || dark(6, i) != (i%2 == 0) {
			t.Fatalf("len %d: timing pattern broken at %d", len(text), i)
		}
	}
	if !dark(8, n-8) {
		t.Fatalf("len %d: dark module missing", len(text))
	}
	// Format bits: both copies equal, a valid BCH(15,5) code word, level M (bits 00).
	a, b := formatBits(c)
	if a != b {
		t.Fatalf("len %d: format copies differ: %015b %015b", len(text), a, b)
	}
	f := a ^ 0x5412
	data := f >> 10
	if f != data<<10|bch(data, 0x537, 5, 15) {
		t.Fatalf("len %d: format BCH invalid: %015b", len(text), a)
	}
	if data>>3 != 0 {
		t.Fatalf("len %d: format level bits %02b, want 00 (M)", len(text), data>>3)
	}
	if data&7 != 0 {
		t.Fatalf("len %d: mask %03b, want 000", len(text), data&7)
	}
	// Version bits (version 7 and up): BCH(18,6) and equal to the version, in both blocks.
	if v >= 7 {
		var bl, tr uint32
		for i := 0; i < 18; i++ {
			if dark(i/3, n-11+i%3) {
				bl |= 1 << uint(i)
			}
			if dark(n-11+i%3, i/3) {
				tr |= 1 << uint(i)
			}
		}
		if bl != tr {
			t.Fatalf("len %d: version copies differ", len(text))
		}
		if bl != uint32(v)<<12|bch(uint32(v), 0x1F25, 6, 18) {
			t.Fatalf("len %d: version bits %018b invalid for version %d", len(text), bl, v)
		}
	}
}

func TestStructure(t *testing.T) {
	inputs := genInputs(1, 500)
	seen := map[coding.Version]bool{}
	for _, s := range inputs {
		checkStructure(t, s)
		_, v, _ := encode(s)
		seen[v] = true
	}
	for _, v := range []coding.Version{1, 6, 7, 10, 26, 27} {
		if !seen[v] {
			t.Errorf("no generated input used version %d", v)
		}
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct versions covered", len(seen))
	}
}

func TestModeChoice(t *testing.T) {
	// The same 20 characters: alphanumeric fits version 1 (20 chars, M), a lowercase letter forces byte mode.
	_, v1, err := encode(strings.Repeat("A", 20))
	if err != nil || v1 != 1 {
		t.Fatalf("uppercase text: version %d, err %v", v1, err)
	}
	_, v2, err := encode(strings.Repeat("a", 20))
	if err != nil || v2 != 2 {
		t.Fatalf("lowercase text: version %d, err %v", v2, err)
	}
}

// ---- SVG ----

var cmdRE = regexp.MustCompile(`([MhvZz])(-?\d+)?(?: (-?\d+))?`)

// parsePath replays an M/h/v path (no other command is accepted) into a module grid.
func parsePath(t testing.TB, d string, n int) [][]bool {
	t.Helper()
	g := make([][]bool, n)
	for i := range g {
		g[i] = make([]bool, n)
	}
	rest := d
	var x, y int
	for rest != "" {
		m := cmdRE.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			t.Fatalf("path: cannot parse %q", rest[:min(len(rest), 20)])
		}
		cmd := rest[m[2]:m[3]]
		arg := func(i int) int {
			if m[2*i] < 0 {
				t.Fatalf("path: %s misses argument %d", cmd, i)
			}
			v, err := strconv.Atoi(rest[m[2*i]:m[2*i+1]])
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		switch cmd {
		case "M":
			x, y = arg(2), arg(3)
		case "h":
			w := arg(2)
			if w > 0 { // a filled run, the next command closes it
				if y < 0 || y >= n || x < 0 || x+w > n {
					t.Fatalf("path: run out of bounds")
				}
				for i := 0; i < w; i++ {
					g[y][x+i] = true
				}
			}
			x += w
		case "v":
			y += arg(2)
		default:
			t.Fatalf("path: unexpected command %q", cmd)
		}
		rest = rest[m[1]:]
	}
	return g
}

type svgDoc struct {
	root     xml.StartElement
	paths    []string
	elements []string
}

var allowedElements = map[string]bool{"svg": true, "rect": true, "path": true}

// parseSVG checks the document is well formed and only uses the allowed elements and attributes.
func parseSVG(b []byte) (*svgDoc, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	doc := &svgDoc{}
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF && depth == 0 && doc.root.Name.Local == "svg" {
				return doc, nil
			}
			return nil, err
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if !allowedElements[e.Name.Local] {
				return nil, fmt.Errorf("element %q", e.Name.Local)
			}
			for _, a := range e.Attr {
				name := strings.ToLower(a.Name.Local)
				if strings.HasPrefix(name, "on") || strings.Contains(name, "href") || a.Name.Space != "" && a.Name.Space != "xmlns" && a.Name.Local != "xmlns" {
					return nil, fmt.Errorf("attribute %q", a.Name.Local)
				}
				if strings.Contains(strings.ToLower(a.Value), "script") || strings.Contains(a.Value, "://") && name != "xmlns" {
					return nil, fmt.Errorf("attribute value %q", a.Value)
				}
			}
			if depth == 0 {
				doc.root = e
			}
			doc.elements = append(doc.elements, e.Name.Local)
			if e.Name.Local == "path" {
				for _, a := range e.Attr {
					if a.Name.Local == "d" {
						doc.paths = append(doc.paths, a.Value)
					}
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if len(bytes.TrimSpace(e)) != 0 {
				return nil, fmt.Errorf("text content %q", string(e))
			}
		case xml.ProcInst, xml.Directive, xml.Comment:
			return nil, fmt.Errorf("unexpected token %T", tok)
		}
	}
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func TestSVG(t *testing.T) {
	for _, text := range []string{
		"HELLO WORLD",
		"LNBC10U1P3EXAMPLEINVOICE",
		"lnbc10u1p3exampleinvoice",
		strings.Repeat("A", MaxInput),
		strings.Repeat("\x00\xff", MaxInput/2),
		"a",
	} {
		out, err := SVG(text)
		if err != nil {
			t.Fatalf("len %d: %v", len(text), err)
		}
		// Measured worst case: 71,923 bytes for 1,200 mixed bytes; the ceiling leaves about 40 % margin.
		if len(out) > 100_000 {
			t.Fatalf("len %d: output is %d bytes", len(text), len(out))
		}
		doc, err := parseSVG(out)
		if err != nil {
			t.Fatalf("len %d: %v\n%s", len(text), err, out)
		}
		c, v, _ := encode(text)
		n := 17 + 4*int(v) + 8
		if len(doc.paths) != 1 {
			t.Fatalf("%d paths, want 1", len(doc.paths))
		}
		if got := attr(doc.root, "viewBox"); got != fmt.Sprintf("0 0 %d %d", n, n) {
			t.Fatalf("viewBox %q for %d modules", got, n)
		}
		if attr(doc.root, "width") != strconv.Itoa(n*8) || attr(doc.root, "height") != strconv.Itoa(n*8) {
			t.Fatalf("width/height %q/%q", attr(doc.root, "width"), attr(doc.root, "height"))
		}
		if attr(doc.root, "shape-rendering") != "crispEdges" {
			t.Fatal("missing shape-rendering=crispEdges")
		}
		if attr(doc.root, "xmlns") != "http://www.w3.org/2000/svg" {
			t.Fatal("missing xmlns")
		}
		for _, bad := range []string{"<script", "href", "<text", "<image", "<style", "<!"} {
			if strings.Contains(strings.ToLower(string(out)), bad) {
				t.Fatalf("output contains %q", bad)
			}
		}
		// The path, replayed, is the module grid with a 4-module quiet zone around it.
		g := parsePath(t, doc.paths[0], n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				want := c.Black(x-4, y-4)
				if g[y][x] != want {
					t.Fatalf("len %d: module (%d,%d) = %v, want %v", len(text), x, y, g[y][x], want)
				}
			}
		}
	}
}

func TestSVGDeterministic(t *testing.T) {
	a, _ := SVG("HELLO WORLD")
	b, _ := SVG("HELLO WORLD")
	if !bytes.Equal(a, b) {
		t.Fatal("output differs between calls")
	}
}

func TestSVGRefusals(t *testing.T) {
	if _, err := SVG(strings.Repeat("A", MaxInput+1)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("1201 bytes: %v", err)
	}
	if _, err := SVG(strings.Repeat("\xff", 100000)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("100000 bytes: %v", err)
	}
	if _, err := SVG(""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty: %v", err)
	}
	for _, e := range []error{ErrTooLong, ErrEmpty} {
		if strings.Contains(e.Error(), "%") {
			t.Fatalf("error text %q is not fixed", e)
		}
	}
}
