package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/satbooth/satbooth/setup"
)

const (
	xpubV1  = "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8"
	xmrMain = "4kcfbiPsJFptZtA4KDH26YKnA9APQpr28aLUWg4PJ5Q9a7KN35LsHRRQ7MVDS1gXyYNVo8PqLy639jeTZQN6urMNCVRUWMc"
	wallet1 = "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	wallet2 = "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
)

func edKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		seed := make([]byte, 32)
		seed[0], seed[1] = byte(i+1), 0x5a
		out[i] = hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	}
	return out
}

func writeJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func everyRailAnswers() map[string]any {
	return map[string]any{"network": "mainnet", "host_dir": "/srv/satbooth", "hub_private_addr": "192.0.2.20",
		"public_private_addr": "192.0.2.10", "checkout_base_url": "https://pay.example.com",
		"webhook_url": "https://hooks.example.com/satbooth", "alerts_url": "https://monitor.example.com/alert",
		"descriptor": "wpkh(" + xpubV1 + "/0/*)", "monero_address": xmrMain, "monero_network": "mainnet",
		"approval_keys": edKeys(2), "payout_budget_sat": 100000, "payout_max_sat": 10000, "payout_daily_sat": 50000,
		"payout_alert_sat": 5000, "currencies": []string{"USD", "EUR"}}
}

func hubAnswersFor(host string) map[string]any {
	return map[string]any{"network": "mainnet", "hub_private_addr": "192.0.2.20", "public_private_addr": "192.0.2.10",
		"host_dir": host}
}

// tmp is a fresh base directory as the wrapper makes it: mode 0700.
func tmp(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

type result struct {
	code        int
	out, errOut string
}

func do(stdin string, args ...string) result {
	var o, e bytes.Buffer
	c := run(args, strings.NewReader(stdin), &o, &e)
	return result{c, o.String(), e.String()}
}

func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if rel == "." || err != nil {
			return nil
		}
		fi, _ := os.Lstat(p)
		b, _ := os.ReadFile(p)
		m[rel] = fi.Mode().String() + string(b)
		return nil
	})
	return m
}

func sameTree(t *testing.T, what string, a, b map[string]string) {
	t.Helper()
	if names(a) != names(b) {
		t.Fatalf("%s: the names changed", what)
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("%s: %s changed", what, k)
		}
	}
}

func names(m map[string]string) string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return strings.Join(k, "\n")
}

func TestUsage(t *testing.T) {
	d := tmp(t)
	for _, args := range [][]string{
		nil, {"public"}, {"public", "bogus", "-dir", d}, {"bogus", "init", "-dir", d}, {"hub", "finish", "-dir", d},
		{"public", "init"}, {"public", "init", "-dir", d, "extra"}, {"public", "init", "-dir", d, "-nope"},
		{"hub", "init", "-dir", d}, {"public", "finish", "-dir", d}, {"hub", "pair", "-dir", d, "-handoff", "x"},
		{"public", "init", "-dir", d, "-handoff", "x"},
	} {
		if r := do("", args...); r.code != 2 || !strings.Contains(r.errOut, "usage") {
			t.Errorf("%v: %d %q", args, r.code, r.errOut)
		}
	}
	if len(tree(t, d)) != 0 {
		t.Fatal("a usage error wrote something")
	}
}

func secretsIn(t *testing.T, dirs ...string) []string {
	var out []string
	hex64 := regexp.MustCompile(`[0-9a-f]{64}`)
	for _, d := range dirs {
		for _, p := range []string{"etc/*.key", "etc/*.secret", "etc/*.pass", "setup/api-keys/*", "config/hub.env",
			"config/fulcrum.conf", "config/wallet-rpc/*", "setup/carry/*"} {
			ms, _ := filepath.Glob(filepath.Join(d, p))
			for _, m := range ms {
				b, _ := os.ReadFile(m)
				out = append(out, hex64.FindAllString(string(b), -1)...)
			}
		}
	}
	return out
}

func TestFourStagesThroughTheCommand(t *testing.T) {
	pubDir, hubDir, aDir := tmp(t), tmp(t), tmp(t)
	pubFile := writeJSON(t, aDir, "pub.json", everyRailAnswers())
	hubFile := writeJSON(t, aDir, "hub.json", hubAnswersFor("/srv/satbooth-hub"))
	var outputs []string
	collect := func(r result) { outputs = append(outputs, r.out, r.errOut) }

	r := do("", "public", "init", "-dir", pubDir, "-host-dir", "/srv/satbooth", "-answers", pubFile)
	collect(r)
	if r.code != 0 || !strings.Contains(r.out, "/apps/new?") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
	if !strings.Contains(r.out, setup.Digest(h1)) {
		t.Fatal("stage 1 did not print the digest")
	}
	hf := filepath.Join(aDir, "public-to-hub.json")
	os.WriteFile(hf, h1, 0o600)

	// Wrong digest: exit 1, nothing written, the error says so and carries no value.
	before := tree(t, hubDir)
	r = do("", "hub", "init", "-dir", hubDir, "-handoff", hf, "-answers", hubFile, "-digest", strings.Repeat("0", 64))
	collect(r)
	if r.code != 1 || !strings.Contains(r.errOut, "digest") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	sameTree(t, "wrong digest", before, tree(t, hubDir))
	// An explicitly empty digest is a usage error, not a question.
	r = do("", "hub", "init", "-dir", hubDir, "-handoff", hf, "-answers", hubFile, "-digest", "")
	collect(r)
	if r.code != 2 || !strings.Contains(r.errOut, "digest") || strings.Contains(r.errOut, "printed") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	sameTree(t, "empty digest", before, tree(t, hubDir))
	// The digest by prompt, in groups.
	r = do(setup.Digest(h1)+"\n", "hub", "init", "-dir", hubDir, "-host-dir", "/srv/satbooth-hub", "-handoff", hf, "-answers", hubFile)
	collect(r)
	if r.code != 0 || !strings.Contains(r.out, "bitcoin-cli") || !strings.Contains(r.out, "/srv/satbooth-hub/compose.env") ||
		!strings.Contains(r.out, "monero-wallet-cli") {
		t.Fatalf("%d %s %s", r.code, r.errOut, r.out)
	}
	r = do("", "hub", "pair", "-dir", hubDir, "-invoice-wallet", wallet1, "-payout-wallet", wallet2)
	collect(r)
	if r.code != 0 || !strings.Contains(r.out, "wallet-rpc.pass") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	h2, _ := os.ReadFile(filepath.Join(hubDir, "setup/hub-to-public.json"))
	hf2 := filepath.Join(aDir, "hub-to-public.json")
	os.WriteFile(hf2, h2, 0o600)
	pass := filepath.Join(aDir, "wallet-rpc.pass")
	b, _ := os.ReadFile(filepath.Join(hubDir, "setup/carry/wallet-rpc.pass"))
	os.WriteFile(pass, b, 0o644)
	os.Chmod(pass, 0o644)
	// A password file readable by others is refused.
	r = do("", "public", "finish", "-dir", pubDir, "-handoff", hf2, "-digest", setup.Digest(h2), "-wallet-rpc-pass", pass)
	collect(r)
	if r.code != 1 || !strings.Contains(r.errOut, "0600") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	os.Chmod(pass, 0o600)
	r = do("", "public", "finish", "-dir", pubDir, "-handoff", hf2, "-digest", setup.Digest(h2), "-wallet-rpc-pass", pass)
	collect(r)
	if r.code != 0 || !strings.Contains(r.out, "docker compose -f deploy/public/compose.yml --env-file /srv/satbooth/compose.env up -d --build") {
		t.Fatalf("%d %s %s", r.code, r.errOut, r.out)
	}
	// Run again: refused, nothing changes.
	snap := tree(t, pubDir)
	r = do("", "public", "finish", "-dir", pubDir, "-handoff", hf2, "-digest", setup.Digest(h2), "-wallet-rpc-pass", pass)
	collect(r)
	if r.code != 1 || !strings.Contains(r.errOut, "does not follow") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	sameTree(t, "rerun", snap, tree(t, pubDir))

	// Nothing secret in any output: the whole secret, slices of it, the bare xpub, on every line.
	secrets := append(secretsIn(t, pubDir, hubDir), xpubV1, xmrMain)
	if len(secrets) < 10 {
		t.Fatalf("only %d secrets found", len(secrets))
	}
	for _, o := range outputs {
		for _, line := range strings.Split(o, "\n") {
			for _, sec := range secrets {
				if len(sec) == 64 {
					for _, part := range []string{sec, sec[:16], sec[24:40], sec[48:]} {
						if strings.Contains(line, part) {
							t.Fatalf("a secret reached the output: %q", line)
						}
					}
				} else if strings.Contains(line, sec) {
					t.Fatalf("a private answer reached the output: %q", line)
				}
			}
		}
	}
	// The next steps give host paths.
	rel := regexp.MustCompile(`(^|[ :=(])(setup|etc|config|backups)/`)
	for _, o := range outputs {
		for _, line := range strings.Split(o, "\n") {
			if rel.MatchString(line) && !strings.HasPrefix(line, "satbooth-setup:") {
				t.Fatalf("a relative path in the output: %q", line)
			}
		}
	}
}

func lines(l ...string) string { return strings.Join(l, "\n") + "\n" }

func stateAnswers(t *testing.T, dir, key string) string {
	b, err := os.ReadFile(filepath.Join(dir, "setup/state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	out, _ := json.Marshal(m[key])
	return string(out)
}

func lightAnswers() map[string]any {
	return map[string]any{"network": "regtest", "host_dir": "/srv/satbooth", "hub_private_addr": "192.0.2.20",
		"public_private_addr": "192.0.2.10", "public_addr": "127.0.0.1", "public_port": 8080,
		"checkout_base_url": "https://pay.example.com", "webhook_url": "https://hooks.example.com/satbooth",
		"alerts_url": "", "descriptor": "", "monero_address": "", "approval_keys": []string{}, "currencies": []string{}}
}

func TestPromptedAnswers(t *testing.T) {
	a, p := tmp(t), tmp(t)
	f := writeJSON(t, tmp(t), "a.json", lightAnswers())
	if r := do("", "public", "init", "-dir", a, "-answers", f); r.code != 0 {
		t.Fatal(r.errOut)
	}
	script := lines(
		"bitcoin", "/srv/satbooth", "192.0.2.20", "192.0.2.20", // the network is refused: its group is asked again
		"regtest", "192.0.2.20", "192.0.2.20", // the same address twice is refused: the group again
		"regtest", "192.0.2.20", "192.0.2.10",
		"", "", // the defaults
		"https://pay.example.com", "https://hooks.example.com/satbooth",
		"", "", "", "", "", // alerts, descriptor, Monero, approval keys, currencies
	)
	r := do(script, "public", "init", "-dir", p)
	if r.code != 0 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if n := strings.Count(r.errOut, "Refused:"); n != 2 {
		t.Fatalf("%d refusals: %s", n, r.errOut)
	}
	if strings.Contains(r.errOut, "bitcoin") {
		t.Fatal("the prompt repeated what was typed")
	}
	// Only the first question of an optional group is asked when it is empty.
	if strings.Contains(r.errOut, "Monero network") || strings.Contains(r.errOut, "budget") {
		t.Fatal("a question of a skipped group was asked")
	}
	if stateAnswers(t, a, "public_answers") != stateAnswers(t, p, "public_answers") {
		t.Fatal("the prompted answers differ from the file's")
	}
	if names(tree(t, a)) != names(tree(t, p)) {
		t.Fatal("different trees")
	}

	// The input ends early.
	e := tmp(t)
	if r := do("regtest\n/srv/satbooth\n", "public", "init", "-dir", e); r.code != 1 || !strings.Contains(r.errOut, "input ended") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if len(tree(t, e)) != 0 {
		t.Fatal("an early end of input wrote something")
	}
}

// Values that depend on each other are asked again together: a wrong Monero network, and a daily cap typed too low.
func TestDependentAnswersAreAskedAgainTogether(t *testing.T) {
	rf := tmp(t)
	keys := strings.Join(edKeys(2), ", ")
	script := lines("mainnet", "/srv/satbooth", "192.0.2.20", "192.0.2.10", "", "", "https://pay.example.com",
		"https://hooks.example.com/satbooth", "", "wpkh("+xpubV1+"/0/*)",
		xmrMain, "stagenet", // the bundle runs Monero on mainnet only: both are asked again
		xmrMain, "mainnet",
		keys, "100000", "10000", "5000", "5000", // the daily cap is below the per-refund cap: the four caps again
		keys, "100000", "10000", "50000", "5000",
		"USD,EUR")
	r := do(script, "public", "init", "-dir", rf)
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 2 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	st := stateAnswers(t, rf, "public_answers")
	for _, want := range []string{`"monero_network":"mainnet"`, `"payout_daily_sat":50000`, `"payout_max_sat":10000`, `"payout_budget_sat":100000`} {
		if !strings.Contains(st, want) {
			t.Fatalf("%s not in the answers", want)
		}
	}
	for _, sec := range []string{xpubV1, xmrMain} {
		if strings.Contains(r.out+r.errOut, sec) {
			t.Fatal("an answer was printed")
		}
	}
	// A hub's Docker networks: a hub address outside its subnet sends the whole group back.
	pubDir, hubDir, aDir := tmp(t), tmp(t), tmp(t)
	if r := do("", "public", "init", "-dir", pubDir, "-answers", writeJSON(t, aDir, "p.json", everyRailAnswers())); r.code != 0 {
		t.Fatal(r.errOut)
	}
	h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
	hf := filepath.Join(aDir, "h")
	os.WriteFile(hf, h1, 0o600)
	script = lines("mainnet", "192.0.2.20", "192.0.2.10", "/srv/satbooth-hub",
		"", "", "172.30.99.10", "", // the hub's address is outside the default subnet
		"", "172.30.78.0/25", "172.30.78.10", "172.30.78.128/25",
		setup.Digest(h1))
	r = do(script, "hub", "init", "-dir", hubDir, "-handoff", hf)
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 1 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
}

func TestPromptedHubAndWallets(t *testing.T) {
	pubDir, hubDir := tmp(t), tmp(t)
	aDir := tmp(t)
	if r := do("", "public", "init", "-dir", pubDir, "-answers", writeJSON(t, aDir, "p.json", everyRailAnswers())); r.code != 0 {
		t.Fatal(r.errOut)
	}
	hf := filepath.Join(aDir, "h")
	h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
	os.WriteFile(hf, h1, 0o600)
	script := lines("mainnet", "192.0.2.20", "192.0.2.10", "/srv/satbooth-hub", "", "", "", "",
		"not a digest", setup.Digest(h1))
	r := do(script, "hub", "init", "-dir", hubDir, "-handoff", hf)
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 1 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	r = do("zz\n"+wallet1+"\n"+wallet1[:10]+"\n"+wallet2+"\n", "hub", "pair", "-dir", hubDir)
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 2 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
}

// -host-dir: the answer to host_dir (not asked), or what the answers file's host_dir must equal.
func TestHostDirFlag(t *testing.T) {
	// With prompts: not asked, and it is the answer.
	d := tmp(t)
	script := lines("regtest", "192.0.2.20", "192.0.2.10", "", "", "https://pay.example.com",
		"https://hooks.example.com/satbooth", "", "", "", "", "")
	r := do(script, "public", "init", "-dir", d, "-host-dir", "/srv/from-flag")
	if r.code != 0 || strings.Contains(r.errOut, "base directory") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if !strings.Contains(stateAnswers(t, d, "public_answers"), `"host_dir":"/srv/from-flag"`) {
		t.Fatal("the flag is not the answer")
	}
	// With an answers file: equal is accepted, unequal is refused.
	f := writeJSON(t, tmp(t), "a.json", lightAnswers())
	d = tmp(t)
	if r := do("", "public", "init", "-dir", d, "-host-dir", "/srv/satbooth", "-answers", f); r.code != 0 {
		t.Fatal(r.errOut)
	}
	d = tmp(t)
	r = do("", "public", "init", "-dir", d, "-host-dir", "/srv/other", "-answers", f)
	if r.code != 1 || !strings.Contains(r.errOut, "the answers' host_dir is not the directory this command was given") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if len(tree(t, d)) != 0 {
		t.Fatal("a refused stage wrote something")
	}
	// A bad flag is refused before any question, by the rule of host_dir.
	for _, bad := range []string{"/srv/a b", "srv", "/srv/$HOME", "/", ""} {
		d = tmp(t)
		r = do("regtest\n", "public", "init", "-dir", d, "-host-dir", bad)
		if bad == "" { // an empty value is a missing one: the question is asked
			continue
		}
		if r.code != 1 || !strings.Contains(r.errOut, "host_dir") || strings.Contains(r.errOut, "Network") || len(tree(t, d)) != 0 {
			t.Fatalf("%q: %d %s", bad, r.code, r.errOut)
		}
	}
	// The hub's stage: the same three cases.
	pubDir, aDir := tmp(t), tmp(t)
	do("", "public", "init", "-dir", pubDir, "-answers", writeJSON(t, aDir, "p.json", everyRailAnswers()))
	h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
	hf := filepath.Join(aDir, "h")
	os.WriteFile(hf, h1, 0o600)
	hubF := writeJSON(t, aDir, "hub.json", hubAnswersFor("/srv/satbooth-hub"))
	hd := tmp(t)
	r = do("", "hub", "init", "-dir", hd, "-host-dir", "/srv/elsewhere", "-handoff", hf, "-digest", setup.Digest(h1), "-answers", hubF)
	if r.code != 1 || !strings.Contains(r.errOut, "host_dir is not the directory") || len(tree(t, hd)) != 0 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	script = lines("mainnet", "192.0.2.20", "192.0.2.10", "", "", "", "")
	r = do(script, "hub", "init", "-dir", hd, "-host-dir", "/srv/satbooth-hub", "-handoff", hf, "-digest", setup.Digest(h1))
	if r.code != 0 || strings.Contains(r.errOut, "base directory") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if !strings.Contains(stateAnswers(t, hd, "hub_answers"), `"host_dir":"/srv/satbooth-hub"`) {
		t.Fatal("the flag is not the answer")
	}
}

func TestHostDirRefused(t *testing.T) {
	for _, hd := range []string{"/srv/a b", "/srv/a\"b", "/srv/$HOME", "/srv/a\nb", "/srv/a`b`", "srv", "/srv/a'b", "/srv/a;b", ""} {
		for _, which := range []string{"public", "hub"} {
			d, a := tmp(t), tmp(t)
			var f string
			if which == "public" {
				m := everyRailAnswers()
				m["host_dir"] = hd
				f = writeJSON(t, a, "x.json", m)
				if r := do("", "public", "init", "-dir", d, "-answers", f); r.code != 1 || !strings.Contains(r.errOut, "host_dir") {
					t.Fatalf("%q: %d %s", hd, r.code, r.errOut)
				}
			} else {
				pubDir := tmp(t)
				if r := do("", "public", "init", "-dir", pubDir, "-answers", writeJSON(t, a, "p.json", everyRailAnswers())); r.code != 0 {
					t.Fatal(r.errOut)
				}
				h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
				hf := filepath.Join(a, "h")
				os.WriteFile(hf, h1, 0o600)
				f = writeJSON(t, a, "x.json", hubAnswersFor(hd))
				if r := do("", "hub", "init", "-dir", d, "-handoff", hf, "-answers", f, "-digest", setup.Digest(h1)); r.code != 1 || !strings.Contains(r.errOut, "host_dir") {
					t.Fatalf("%q: %d %s", hd, r.code, r.errOut)
				}
			}
			if len(tree(t, d)) != 0 {
				t.Fatalf("%q: something was written", hd)
			}
		}
	}
	// By prompt, the bad directory is asked again at once.
	d := tmp(t)
	script := lines("regtest", "/srv/a b", "/srv/ok", "192.0.2.20", "192.0.2.10", "", "", "https://pay.example.com",
		"https://hooks.example.com/x", "", "", "", "", "")
	r := do(script, "public", "init", "-dir", d)
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 1 || strings.Contains(r.errOut, "a b") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	if !strings.Contains(stateAnswers(t, d, "public_answers"), `"host_dir":"/srv/ok"`) {
		t.Fatal("the second directory was not used")
	}
}

// Everything that can be refused without an answer is refused before the first question.
func TestRefusedBeforeTheFirstQuestion(t *testing.T) {
	d := tmp(t)
	f := writeJSON(t, tmp(t), "a.json", lightAnswers())
	if r := do("", "public", "init", "-dir", d, "-answers", f); r.code != 0 {
		t.Fatal(r.errOut)
	}
	before := tree(t, d)
	inTheWay := tmp(t)
	os.Mkdir(filepath.Join(inTheWay, "etc"), 0o700)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"public", "init", "-dir", d}, "this stage does not follow"},
		{[]string{"hub", "init", "-dir", d, "-handoff", f}, "other host"},
		{[]string{"public", "init", "-dir", tmp(t), "-hub-url", "ftp://x"}, "hub URL"},
		{[]string{"public", "init", "-dir", inTheWay}, "already exists"},
	} {
		r := do("regtest\n", c.args...)
		if r.code != 1 || !strings.Contains(r.errOut, c.want) || strings.Contains(r.errOut, "Network (") {
			t.Fatalf("%v: %d %s", c.args, r.code, r.errOut)
		}
	}
	sameTree(t, "refusals", before, tree(t, d))
	// A base directory that is not 0700 is refused first, with the way to fix it.
	loose := t.TempDir()
	os.Chmod(loose, 0o755)
	r := do("regtest\n", "public", "init", "-dir", loose)
	if r.code != 1 || !strings.Contains(r.errOut, "chmod 700") || strings.Contains(r.errOut, "Network (") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
}

func TestLightningOnlyThroughTheCommand(t *testing.T) {
	pubDir, hubDir, aDir := tmp(t), tmp(t), tmp(t)
	r := do("", "public", "init", "-dir", pubDir, "-answers", writeJSON(t, aDir, "p.json", lightAnswers()))
	if r.code != 0 || strings.Count(r.out, "/apps/new?") != 1 {
		t.Fatalf("%d %s %s", r.code, r.errOut, r.out)
	}
	h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
	hf := filepath.Join(aDir, "h")
	os.WriteFile(hf, h1, 0o600)
	hubF := writeJSON(t, aDir, "hub.json", map[string]any{"network": "regtest", "hub_private_addr": "192.0.2.20",
		"public_private_addr": "192.0.2.10", "host_dir": "/srv/satbooth-hub"})
	if r := do("", "hub", "init", "-dir", hubDir, "-handoff", hf, "-digest", setup.Digest(h1), "-answers", hubF); r.code != 0 {
		t.Fatal(r.errOut)
	}
	// Without the flag the question is asked, and empty standard input is an error that writes nothing.
	before := tree(t, hubDir)
	r = do("", "hub", "pair", "-dir", hubDir, "-invoice-wallet", wallet1)
	if r.code != 1 || !strings.Contains(r.errOut, "input ended") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	sameTree(t, "unanswered question", before, tree(t, hubDir))
	// An explicit empty invoice wallet is refused with its own message, not asked and not a handoff error.
	r = do("", "hub", "pair", "-dir", hubDir, "-invoice-wallet", "", "-payout-wallet", "")
	if r.code != 1 || !strings.Contains(r.errOut, "invoice connection's Wallet Public Key is required") || strings.Contains(r.errOut, "handoff") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	sameTree(t, "empty invoice wallet", before, tree(t, hubDir))
	// An explicit empty payout wallet means "none": no question, so empty standard input is enough.
	r = do("", "hub", "pair", "-dir", hubDir, "-invoice-wallet", wallet1, "-payout-wallet", "")
	if r.code != 0 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// A file whose content is not what the unfinished run wrote is kept, and the command says so.
func TestDiscardKeepsFilesItDidNotWrite(t *testing.T) {
	d := tmp(t)
	os.MkdirAll(filepath.Join(d, "setup"), 0o700)
	os.WriteFile(filepath.Join(d, "compose.env"), []byte("written by hand"), 0o600)
	os.WriteFile(filepath.Join(d, "setup/journal.json"),
		[]byte(`{"version":1,"stage":"hub-init","dirs":[],"files":["setup/state.json.tmp","compose.env"],"sha256":["`+sha("t")+`","`+sha("x")+`"],"prev_state":null}`), 0o600)
	r := do("", "hub", "init", "-dir", d, "-discard-incomplete")
	if r.code != 0 || !strings.Contains(r.out, "kept compose.env") || strings.Contains(r.out, "removed compose.env") ||
		!strings.Contains(r.out, "not written by the unfinished run") {
		t.Fatalf("%d %s %s", r.code, r.out, r.errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "compose.env")); string(b) != "written by hand" {
		t.Fatal("the file was touched")
	}
}

func TestDiscardFlag(t *testing.T) {
	d := tmp(t)
	if r := do("", "public", "init", "-dir", d, "-discard-incomplete"); r.code != 1 || !strings.Contains(r.errOut, "nothing to discard") {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	os.MkdirAll(filepath.Join(d, "setup"), 0o700)
	os.WriteFile(filepath.Join(d, "etc-file"), []byte("mine"), 0o600)
	os.WriteFile(filepath.Join(d, "compose.env"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(d, "setup/journal.json"),
		[]byte(`{"version":1,"stage":"hub-init","dirs":[],"files":["setup/state.json.tmp","compose.env"],"sha256":["`+sha("t")+`","`+sha("x")+`"],"prev_state":null}`), 0o600)
	// Another stage's discard refuses this journal.
	os.Mkdir(filepath.Join(d, "config"), 0o700)
	os.Mkdir(filepath.Join(d, "etc"), 0o700)
	before := tree(t, d)
	for _, args := range [][]string{{"public", "init"}, {"hub", "pair"}, {"public", "finish"}} {
		r := do("", append(args, "-dir", d, "-discard-incomplete")...)
		if r.code != 1 || !strings.Contains(r.errOut, "stage hub-init") {
			t.Fatalf("%v: %d %s", args, r.code, r.errOut)
		}
		sameTree(t, "wrong stage's discard", before, tree(t, d))
	}
	r := do("", "hub", "init", "-dir", d, "-discard-incomplete")
	if r.code != 0 || !strings.Contains(r.out, "removed compose.env") {
		t.Fatalf("%d %s %s", r.code, r.out, r.errOut)
	}
	if _, err := os.Stat(filepath.Join(d, "etc-file")); err != nil {
		t.Fatal("a file that was not journaled was removed")
	}
	if _, err := os.Stat(filepath.Join(d, "compose.env")); err == nil {
		t.Fatal("the journaled file is still there")
	}
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// A mistake whose cause is in another group or another field: the second refusal of a field asks every question
// again, nothing typed before is lost, and what was typed before is never shown.
func TestEveryQuestionIsAskedAgainWhenTheCauseIsElsewhere(t *testing.T) {
	desc := "wpkh(" + xpubV1 + "/0/*)"
	head := []string{"regtest", "/srv/satbooth", "192.0.2.20", "192.0.2.10", "", "", "https://pay.example.com",
		"https://hooks.example.com/satbooth", ""}
	// 1. The network was typed wrong; the mainnet descriptor is refused twice; the third pass corrects the network.
	d := tmp(t)
	// refused, asked again, refused: everything
	script := lines(cat(head, []string{desc, desc, "mainnet"}, repeat("", 12))...)
	r := do(script, "public", "init", "-dir", d)
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 2 || strings.Count(r.errOut, "Asking every question again") != 1 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
	st := stateAnswers(t, d, "public_answers")
	for _, want := range []string{`"network":"mainnet"`, `"host_dir":"/srv/satbooth"`, `"hub_private_addr":"192.0.2.20"`,
		`"public_private_addr":"192.0.2.10"`, `"public_port":8080`, `"webhook_url":"https://hooks.example.com/satbooth"`, "xpub661My"} {
		if !strings.Contains(st, want) {
			t.Fatalf("%s is not kept: %s", want, st)
		}
	}
	if strings.Contains(r.out+r.errOut, xpubV1) || strings.Contains(r.out+r.errOut, "xpub661My") {
		t.Fatal("a private value typed earlier was shown")
	}
	if strings.Count(r.errOut, "[keep]") != 10 {
		t.Fatalf("%d [keep] prompts", strings.Count(r.errOut, "[keep]"))
	}

	// 1b. The same, clearing the optional descriptor with "-" instead.
	d = tmp(t)
	script = lines(cat(head, []string{desc, desc}, repeat("", 9), []string{"-"}, repeat("", 3))...)
	r = do(script, "public", "init", "-dir", d)
	if r.code != 0 || !strings.Contains(stateAnswers(t, d, "public_answers"), `"descriptor":""`) ||
		!strings.Contains(stateAnswers(t, d, "public_answers"), `"network":"regtest"`) {
		t.Fatalf("%d %s", r.code, r.errOut)
	}

	// 2. The alerts URL equals the webhook URL; the webhook URL is the wrong one.
	d = tmp(t)
	same := "https://hooks.example.com/satbooth"
	script = lines(cat(head[:8], []string{same, same}, repeat("", 7), []string{"https://hooks.example.com/other"}, repeat("", 5))...)
	r = do(script, "public", "init", "-dir", d)
	st = stateAnswers(t, d, "public_answers")
	if r.code != 0 || !strings.Contains(st, `"webhook_url":"https://hooks.example.com/other"`) ||
		!strings.Contains(st, `"alerts_url":"`+same+`"`) || !strings.Contains(st, `"checkout_base_url":"https://pay.example.com"`) {
		t.Fatalf("%d %s %s", r.code, r.errOut, st)
	}

	// 3. The hub: its private address, which matches the public host's, is inside the Docker network typed after it.
	pubDir, hubDir, aDir := tmp(t), tmp(t), tmp(t)
	m := everyRailAnswers()
	m["hub_private_addr"], m["public_private_addr"] = "10.1.0.20", "10.2.0.10"
	if r := do("", "public", "init", "-dir", pubDir, "-answers", writeJSON(t, aDir, "p.json", m)); r.code != 0 {
		t.Fatal(r.errOut)
	}
	h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
	hf := filepath.Join(aDir, "h")
	os.WriteFile(hf, h1, 0o600)
	script = lines("mainnet", "10.1.0.20", "10.2.0.10", "/srv/satbooth-hub",
		"", "10.1.0.0/24", "10.1.0.10", "10.3.0.0/24", // refused: the group again
		"mainnet", "10.1.0.20", "10.2.0.10", // refused again: everything
		"", "", "", "", "", "10.9.0.0/24", "10.9.0.10", "")
	r = do(script, "hub", "init", "-dir", hubDir, "-handoff", hf, "-digest", setup.Digest(h1))
	st = stateAnswers(t, hubDir, "hub_answers")
	if r.code != 0 || strings.Count(r.errOut, "Refused:") != 2 || !strings.Contains(st, `"hub_private_addr":"10.1.0.20"`) ||
		!strings.Contains(st, `"hub_subnet":"10.9.0.0/24"`) || !strings.Contains(st, `"hub_ip":"10.9.0.10"`) ||
		!strings.Contains(st, `"electrum_subnet":"10.3.0.0/24"`) {
		t.Fatalf("%d %s %s", r.code, r.errOut, st)
	}

	// The input may end in the middle of the second pass: exit 1, nothing written.
	d = tmp(t)
	r = do(lines(cat(head, []string{desc, desc, "mainnet"})...), "public", "init", "-dir", d)
	if r.code != 1 || len(tree(t, d)) != 0 {
		t.Fatalf("%d %s", r.code, r.errOut)
	}
}

func TestEmptyFlagValuesAreUsageErrors(t *testing.T) {
	f := writeJSON(t, tmp(t), "a.json", lightAnswers())
	for _, args := range [][]string{
		{"public", "init", "-host-dir", "", "-answers", f},
		{"public", "init", "-hub-url", "", "-answers", f},
		{"public", "init", "-answers", ""},
		{"hub", "init", "-host-dir", "", "-handoff", f},
		{"public", "finish", "-handoff", f, "-wallet-rpc-pass", ""},
		{"public", "finish", "-handoff", f, "-digest", ""},
	} {
		d := tmp(t)
		r := do("regtest\n", append(args, "-dir", d)...)
		if r.code != 2 || !strings.Contains(r.errOut, "empty value") || len(tree(t, d)) != 0 {
			t.Fatalf("%v: %d %s", args, r.code, r.errOut)
		}
	}
}

// With -source-build the printed start commands add the host's compose.build.yml; without it they do not.
func TestSourceBuildFlagNamesTheBuildFile(t *testing.T) {
	for _, flagGiven := range []bool{false, true} {
		pubDir, hubDir, aDir := tmp(t), tmp(t), tmp(t)
		pubFile := writeJSON(t, aDir, "pub.json", everyRailAnswers())
		hubFile := writeJSON(t, aDir, "hub.json", hubAnswersFor("/srv/satbooth-hub"))
		extra := []string{}
		if flagGiven {
			extra = []string{"-source-build"}
		}
		r := do("", append([]string{"public", "init", "-dir", pubDir, "-host-dir", "/srv/satbooth", "-answers", pubFile}, extra...)...)
		if r.code != 0 {
			t.Fatalf("flag %v: %d %s", flagGiven, r.code, r.errOut)
		}
		h1, _ := os.ReadFile(filepath.Join(pubDir, "setup/public-to-hub.json"))
		hf := filepath.Join(aDir, "public-to-hub.json")
		os.WriteFile(hf, h1, 0o600)
		r = do("", append([]string{"hub", "init", "-dir", hubDir, "-host-dir", "/srv/satbooth-hub", "-handoff", hf,
			"-answers", hubFile, "-digest", setup.Digest(h1)}, extra...)...)
		wantHub := "docker compose -f deploy/hub/compose.yml --env-file /srv/satbooth-hub/compose.env up -d --build hub"
		if flagGiven {
			wantHub = "docker compose -f deploy/hub/compose.yml -f deploy/hub/compose.build.yml --env-file /srv/satbooth-hub/compose.env up -d --build hub"
		}
		if r.code != 0 || !strings.Contains(r.out, wantHub) || strings.Contains(r.out, "compose.build.yml") != flagGiven {
			t.Fatalf("flag %v: %d %s\n%s", flagGiven, r.code, r.errOut, r.out)
		}
	}
}
