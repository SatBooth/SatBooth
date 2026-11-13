package onchain

import (
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestParseHDKeyPath(t *testing.T) {
	for _, c := range []struct {
		in  string
		idx int64
		ok  bool
	}{
		{"m/0/7", 7, true},
		{"m/84h/0h/0h/0/12", 12, true},
		{"m/84'/0'/0'/0/12", 12, true},
		{"m/84h/1h/0h/0/0", 0, true},
		{"m/0/2147483647", 2147483647, true},
		{"m/0/0", 0, true},
		{"m/1/5", 0, false},
		{"m/0/7h", 0, false},
		{"m/0/7'", 0, false},
		{"m/0/07", 0, false},
		{"m/0/2147483648", 0, false},
		{"m", 0, false},
		{"m/0", 0, false},
		{"m/0h/7", 0, false},
		{"", 0, false},
		{"m/0/", 0, false},
		{"m//0/7", 0, false},
		{"m/0/ 7", 0, false},
		{"m/0/-1", 0, false},
		{"m/0/+1", 0, false},
		{"M/0/7", 0, false},
		{"m/0/7/", 0, false},
		{"m/0/7x", 0, false},
		{"m/84h/0h/0h/0/12 ", 0, false},
		{"m/01/0/7", 0, false},
		{"m/2147483648h/0/7", 0, false},
	} {
		idx, ok := parseHDKeyPath(c.in)
		if ok != c.ok || (ok && idx != c.idx) {
			t.Errorf("parseHDKeyPath(%q) = %d, %v; want %d, %v", c.in, idx, ok, c.idx, c.ok)
		}
	}
	// 11 steps are accepted, 12 and 13 are not.
	steps := func(n int) string { return "m" + strings.Repeat("/5h", n-2) + "/0/9" }
	for n, want := range map[int]bool{2: true, 10: true, 11: true, 12: false, 13: false} {
		if _, ok := parseHDKeyPath(steps(n)); ok != want {
			t.Errorf("%d steps: ok = %v, want %v", n, ok, want)
		}
	}
	// A byte 0x20 inside a number or at the ends.
	for _, s := range []string{"m/0/1" + string(rune(0x20)) + "2", string(rune(0x20)) + "m/0/7", "m/0" + string(rune(0x20)) + "/7"} {
		if _, ok := parseHDKeyPath(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
}

var stepRE = regexp.MustCompile(`^(0|[1-9][0-9]*)[h']?$`)

// refHD is the reference predicate: written from the rule, not from the code.
func refHD(s string) (int64, bool) {
	parts := strings.Split(s, "/")
	if parts[0] != "m" || len(parts) < 3 || len(parts) > 12 {
		return 0, false
	}
	steps := parts[1:]
	for i, p := range steps {
		if !stepRE.MatchString(p) {
			return 0, false
		}
		num := strings.TrimRight(p, "h'")
		if n, err := strconv.ParseInt(num, 10, 64); err != nil || n > 2147483647 {
			return 0, false
		}
		if i == len(steps)-1 && num != p {
			return 0, false
		}
		if i == len(steps)-2 && p != "0" {
			return 0, false
		}
	}
	n, _ := strconv.ParseInt(steps[len(steps)-1], 10, 64)
	return n, true
}

func genHD(r *rand.Rand) string {
	nums := []string{"0", "1", "7", "84", "2147483646", "2147483647", "2147483648", "4294967296", "01", "00", "", "-1", "9999999999999999999", "5 ", "x"}
	marks := []string{"", "", "", "h", "'", "H", "hh"}
	n := 1 + r.Intn(13)
	shaped := r.Intn(2) == 0 // half the paths follow the valid shape, with a single corruption now and then
	var b strings.Builder
	b.WriteString("m")
	if r.Intn(40) == 0 {
		b.Reset()
		b.WriteString([]string{"", "M", "n", "/"}[r.Intn(4)])
	}
	for i := 0; i < n; i++ {
		b.WriteString("/")
		num := nums[r.Intn(len(nums))]
		mark := marks[r.Intn(len(marks))]
		// bias toward valid shapes so both outcomes are common
		if shaped && r.Intn(25) != 0 {
			switch {
			case i == n-2:
				num, mark = "0", ""
			case i == n-1:
				num, mark = strconv.Itoa(r.Intn(100000)), ""
			default:
				num, mark = strconv.Itoa(r.Intn(100)), marks[r.Intn(3)+0]
			}
		} else if !shaped && r.Intn(3) != 0 {
			num = strconv.Itoa(r.Intn(100))
		}
		b.WriteString(num + mark)
	}
	return b.String()
}

func TestParseHDKeyPathProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20261103))
	okN := 0
	for i := 0; i < 5000; i++ {
		s := genHD(r)
		wi, wok := refHD(s)
		gi, gok := parseHDKeyPath(s)
		if wok != gok || (wok && wi != gi) {
			t.Fatalf("%q: got %d, %v; reference %d, %v", s, gi, gok, wi, wok)
		}
		if gok {
			okN++
		}
	}
	if okN < 500 || okN > 4500 {
		t.Fatalf("the generator is lopsided: %d of 5000 accepted", okN)
	}
}

func FuzzHDKeyPath(f *testing.F) {
	for _, s := range []string{"m/0/7", "m/84h/0h/0h/0/12", "m/84'/0'/0'/0/12", "m", "m/0", "m/0/07", "m/0/2147483648", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		idx, ok := parseHDKeyPath(s)
		if !ok {
			return
		}
		if idx < 0 || idx > 2147483647 {
			t.Fatalf("index %d out of bounds", idx)
		}
		if got, ok2 := parseHDKeyPath(fmt.Sprintf("m/84h/1h/0h/0/%d", idx)); !ok2 || got != idx {
			t.Fatalf("%q: re-parse gave %d, %v", s, got, ok2)
		}
		if got, ok2 := parseHDKeyPath(fmt.Sprintf("m/0/%d", idx)); !ok2 || got != idx {
			t.Fatalf("%q: re-parse (no origin) gave %d, %v", s, got, ok2)
		}
		if wi, wok := refHD(s); !wok || wi != idx {
			t.Fatalf("%q: reference disagrees: %d, %v", s, wi, wok)
		}
	})
}
