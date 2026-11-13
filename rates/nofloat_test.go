package rates

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The float identifiers: the float and complex types, and the conversions and big.Float entry points that
// produce or read one.
var floatIdents = map[string]bool{
	"float64": true, "float32": true, "complex64": true, "complex128": true,
	"ParseFloat": true, "FormatFloat": true, "AppendFloat": true,
	"Float": true, "NewFloat": true, "SetFloat64": true, "Float32": true, "Float64": true,
}

// floatsIn returns the positions and texts of the float identifiers and the float or imaginary literals in
// one Go source file. go/scanner skips comments, so the prose may say "floating point"; a file that does
// not scan fails the test.
//
// What a token scan cannot see: a math function called with integer literals (math.Pow(10, 8) returns a
// float64 without naming one), and JSON decoded into any or map[string]any (encoding/json makes every
// number a float64 there). Code review keeps those out.
func floatsIn(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile(path, -1, len(src)), src, func(pos token.Position, msg string) {
		t.Errorf("%s: does not scan: %s", pos, msg)
	}, 0)
	var found []string
	for {
		pos, tok, lit := s.Scan()
		switch {
		case tok == token.EOF:
			return found
		case tok == token.IDENT && floatIdents[lit], tok == token.FLOAT, tok == token.IMAG:
			found = append(found, fset.Position(pos).String()+": "+lit)
		}
	}
}

// skippedDirs are the directories the walk leaves out, matched by exact name: the fake rate server
// (rates/ratestest) and the fake wallet-rpc (monero/monerotest), test helpers, and test data. Any other
// directory, whatever its name, is scanned.
var skippedDirs = map[string]bool{"ratestest": true, "monerotest": true, "testdata": true}

// moneyGoFiles returns the non-test Go files under root, recursively, skipping the directories in
// skippedDirs.
func moneyGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := e.Name()
		if e.IsDir() {
			if path != root && skippedDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: %v", root, err)
	}
	return files
}

// TestNoFloatInMoneyCode: money never passes through floating point. The non-test Go files of the
// money packages and their subpackages may not name a float type or conversion, nor hold a float literal.
func TestNoFloatInMoneyCode(t *testing.T) {
	for _, pkg := range []string{".", "../api", "../store", "../checkout", "../rail", "../xmrrpc", "../monero", "../approval", "../lninvoice", "../refund"} {
		files := moneyGoFiles(t, pkg)
		if len(files) == 0 {
			t.Errorf("%s: no Go files scanned", pkg)
		}
		for _, path := range files {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, f := range floatsIn(t, path, src) {
				t.Errorf("%s in money code", f)
			}
		}
	}
}

// TestNoFloatScanCatches: the scan finds each banned identifier, including a selector, and every float
// or imaginary literal, and skips comments and strings.
func TestNoFloatScanCatches(t *testing.T) {
	src := []byte("package p\n// float64 0.5 in a comment\n/* float32 1e3 */\nvar s = \"float64 0.5\"\nvar r = `FormatFloat`\n" +
		"var a float64\nvar b float32\nvar c = strconv.ParseFloat\nvar d = n.Float64\nvar e = strconv.FormatFloat\n" +
		"var m = 0.015\nvar k = 1e8\nvar h = 0x1p-2\nvar i = 2i\nvar f = big.NewFloat\nvar g = x.SetFloat64\n" +
		"var j big.Float\nvar l complex64\nvar o complex128\nvar q = strconv.AppendFloat\nvar u = r.Float32\nvar n = 100_000_000\n")
	var names []string
	for _, f := range floatsIn(t, "p.go", src) {
		names = append(names, f[strings.LastIndex(f, " ")+1:])
	}
	want := "float64,float32,ParseFloat,Float64,FormatFloat,0.015,1e8,0x1p-2,2i,NewFloat,SetFloat64," +
		"Float,complex64,complex128,AppendFloat,Float32"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("found %s\nwant  %s", got, want)
	}
}

// TestMoneyGoFilesWalks: the walk descends into subpackages, skips the directories named ratestest and
// testdata (and nothing else), and leaves out test files.
func TestMoneyGoFilesWalks(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.go", "a_test.go", "sub/b.go", "sub/deep/c.go", "ratestest/d.go", "sub/xtest/e.go",
		"testdata/f.go", "notes.txt", "latest/g.go", "sub/backtest/h.go", "sub/ratestest/i.go", "sub/testdata/j.go",
		"ratestestx/k.go", "test/l.go"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, p := range moneyGoFiles(t, root) {
		rel, _ := filepath.Rel(root, p)
		got = append(got, filepath.ToSlash(rel))
	}
	// Only the directories named exactly ratestest or testdata are skipped, at any depth: a money subpackage
	// whose name merely ends in "test" (latest, backtest) is scanned.
	want := "a.go,latest/g.go,ratestestx/k.go,sub/b.go,sub/backtest/h.go,sub/deep/c.go,sub/xtest/e.go,test/l.go"
	if strings.Join(got, ",") != want {
		t.Errorf("walked %v", got)
	}
}

// TestDecimalNotComparable: == on two Decimals would compare pointers, not values, so it does not compile.
func TestDecimalNotComparable(t *testing.T) {
	if reflect.TypeOf(Decimal{}).Comparable() {
		t.Error("Decimal is comparable: == would compare the pointers, not the values")
	}
}
