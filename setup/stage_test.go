package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	mrand "math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/satbooth/satbooth/api"
	"github.com/satbooth/satbooth/btcrpc"
	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/relay"
	"github.com/satbooth/satbooth/service"
	"github.com/satbooth/satbooth/webhook"
)

// detRand is a deterministic source of "random" bytes.
func detRand(seed int64) io.Reader { return mrand.New(mrand.NewSource(seed)) }

// env is one pair of hosts: two base directories, the two hosts' answers, and the files carried between them.
type env struct {
	t              *testing.T
	pub            PublicAnswers
	hub            HubAnswers
	pubDir, hubDir string
	wallets        [2]string // invoice, payout ("" for a Lightning-only pair)
}

func newEnv(t *testing.T, f *fixture) *env {
	t.Helper()
	e := &env{t: t, pub: f.pub, hub: f.hub, pubDir: baseDir(t), hubDir: baseDir(t)}
	e.wallets[0] = curveKeys[2]
	if len(f.pub.ApprovalKeys) > 0 {
		e.wallets[1] = curveKeys[3]
	}
	return e
}

// baseDir is a fresh base directory as the wrapper makes it: mode 0700.
func baseDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// clone is a copy of the pair: the same answers and the same two trees, in fresh directories.
func (e *env) clone() *env {
	c := *e
	c.pubDir, c.hubDir = baseDir(e.t), baseDir(e.t)
	copyTree(e.t, e.pubDir, c.pubDir)
	copyTree(e.t, e.hubDir, c.hubDir)
	return &c
}

// copyTree copies the contents of src into the existing directory dst, modes kept.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			return os.Symlink(tgt, to)
		case fi.IsDir():
			if err := os.Mkdir(to, 0o700); err != nil {
				return err
			}
			return os.Chmod(to, fi.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, b, 0o600); err != nil {
			return err
		}
		return os.Chmod(to, fi.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func (e *env) dir(n int) string {
	if n == 1 || n == 4 {
		return e.pubDir
	}
	return e.hubDir
}

func (e *env) read(dir, name string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// run runs stage n (1 to 4) with its inputs read from the files the earlier stages wrote.
func (e *env) run(n int, seed int64) (*Report, error) {
	switch n {
	case 1:
		return PublicInit(e.pubDir, &e.pub, "http://127.0.0.1:8080", detRand(seed))
	case 2:
		h := e.read(e.pubDir, "setup/public-to-hub.json")
		return HubInit(e.hubDir, &e.hub, h, Digest(h), detRand(seed))
	case 3:
		return HubPair(e.hubDir, e.wallets[0], e.wallets[1])
	}
	h := e.read(e.hubDir, "setup/hub-to-public.json")
	var pass []byte
	if e.pub.MoneroAddress != "" {
		pass = e.read(e.hubDir, "setup/carry/wallet-rpc.pass")
	}
	return PublicFinish(e.pubDir, h, Digest(h), pass)
}

// upTo runs stages 1 to n with seed 1; it fails the test on an error.
func (e *env) upTo(n int) {
	e.t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := e.run(i, 1); err != nil {
			e.t.Fatalf("stage %d: %v", i, err)
		}
	}
}

// snapshot is every name under dir with its mode and content (a symbolic link with its target).
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			tgt, _ := os.Readlink(p)
			out[rel] = "link->" + tgt
		case fi.IsDir():
			out[rel+"/"] = fmt.Sprintf("dir %o", fi.Mode().Perm())
		default:
			b, _ := os.ReadFile(p)
			out[rel] = fmt.Sprintf("file %o %s", fi.Mode().Perm(), b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, what string, a, b map[string]string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: the tree changed: %v then %v", what, keys(a), keys(b))
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("%s: %s changed", what, k)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// wantTree checks the names under dir, and that each file is 0600 and each directory 0700.
func wantTree(t *testing.T, what string, dir string, want []string) {
	t.Helper()
	snap := snapshot(t, dir)
	for k, v := range snap {
		if strings.HasSuffix(k, "/") && v != "dir 700" || !strings.HasSuffix(k, "/") && !strings.HasPrefix(v, "file 600 ") {
			t.Errorf("%s: %s has the wrong mode: %.12s", what, k, v)
		}
	}
	got := keys(snap)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s: tree\n got  %v\n want %v", what, got, want)
	}
}

var (
	pubStage1 = []string{"etc/", "backups/", "setup/", "setup/api-keys/", "etc/nwc-client.key", "etc/payout.key",
		"etc/webhook.secret", "etc/alerts.secret", "etc/bitcoind.pass", "etc/api-keys.json",
		"setup/api-keys/orders-write.key", "setup/api-keys/orders-read.key", "setup/api-keys/refunds-write.key",
		"setup/api-keys/refunds-read.key", "setup/public-to-hub.json", "setup/state.json"}
	pubStage4 = append(append([]string{}, pubStage1...), "compose.env", "etc/satbooth.json", "etc/wallet-rpc.pass",
		"setup/read-hub-to-public.json")
	hubStage2 = []string{"config/", "config/wallet-rpc/", "wallet/", "setup/", "setup/carry/", "config/hub.env",
		"config/fulcrum.conf", "config/bitcoin.conf", "config/monerod.conf", "config/wallet-rpc/rpc-login",
		"config/wallet-rpc/wallet.pass", "setup/carry/wallet-rpc.pass", "setup/read-public-to-hub.json",
		"setup/state.json", "compose.env"}
	hubStage3 = append(append([]string{}, hubStage2...), "config/relay.json", "setup/hub-to-public.json")
)

func TestFourStages(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	var reports [4]*Report
	for n := 1; n <= 4; n++ {
		r, err := e.run(n, 1)
		if err != nil {
			t.Fatalf("stage %d: %v", n, err)
		}
		reports[n-1] = r
		switch n {
		case 1:
			wantTree(t, "stage 1", e.pubDir, pubStage1)
		case 2:
			wantTree(t, "stage 2", e.hubDir, hubStage2)
		case 3:
			wantTree(t, "stage 3", e.hubDir, hubStage3)
		case 4:
			wantTree(t, "stage 4", e.pubDir, pubStage4)
		}
	}
	if len(reports[0].Links) != 2 || reports[0].Digest == "" || reports[1].Digest != reports[0].Digest ||
		reports[2].Digest == "" || reports[3].Digest != reports[2].Digest {
		t.Fatalf("reports: %+v", reports)
	}
	// The files load with the real loaders.
	b := e.read(e.pubDir, "etc/satbooth.json")
	cfg, err := service.ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg
	etc := filepath.Join(e.pubDir, "etc")
	for _, k := range []string{"nwc-client.key", "payout.key"} {
		if _, err := nostr.LoadSecretKeyFile(filepath.Join(etc, k)); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	if _, err := api.LoadKeys(filepath.Join(etc, "api-keys.json")); err != nil {
		t.Error(err)
	}
	for _, k := range []string{"webhook.secret", "alerts.secret"} {
		if _, err := webhook.LoadSecret(filepath.Join(etc, k)); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"bitcoind.pass", "wallet-rpc.pass"} {
		if _, err := btcrpc.LoadPassword(filepath.Join(etc, k)); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	if _, err := relay.ParseConfig(e.read(e.hubDir, "config/relay.json")); err != nil {
		t.Error(err)
	}
	// The carried password is the one in rpc-login.
	pass := strings.TrimSpace(string(e.read(e.hubDir, "setup/carry/wallet-rpc.pass")))
	if string(e.read(e.hubDir, "config/wallet-rpc/rpc-login")) != "satbooth:"+pass+"\n" ||
		string(e.read(e.pubDir, "etc/wallet-rpc.pass")) != pass+"\n" {
		t.Error("the carried wallet-rpc password differs from the login's")
	}
	// The rpcauth line of the handoff is the one in bitcoin.conf.
	in, _ := ParsePublicToHub(e.read(e.pubDir, "setup/public-to-hub.json"))
	if !strings.Contains(string(e.read(e.hubDir, "config/bitcoin.conf")), in.SatboothRPCAuth+"\n") {
		t.Error("bitcoin.conf lacks SatBooth's rpcauth line")
	}
	// The next steps use the host path of the directory.
	if !strings.Contains(strings.Join(reports[1].Next, "\n"), e.hub.HostDir+"/compose.env up -d --build hub") ||
		!strings.Contains(strings.Join(reports[3].Next, "\n"), e.pub.HostDir+"/compose.env up -d --build") {
		t.Errorf("next steps: %v %v", reports[1].Next, reports[3].Next)
	}
	// The printed monero-wallet-cli command takes the wallet's password from the file wallet-rpc reads (inside the
	// container), so the operator neither types it nor reads it, and the step does not point at the host file.
	next := strings.Join(reports[1].Next, "\n")
	if !strings.Contains(next, "monero-wallet-cli wallet-rpc --generate-from-view-key") {
		t.Fatalf("no monero-wallet-cli command (this fixture has Monero): %v", reports[1].Next)
	}
	if !strings.Contains(next, " --password-file /etc/wallet-rpc/wallet.pass ") || strings.Contains(next, "Its password is in") ||
		strings.Contains(next, e.hub.HostDir+"/config/wallet-rpc/wallet.pass") {
		t.Errorf("the wallet-cli command does not read the password from the file: %v", reports[1].Next)
	}
	// Stage 1 prints the links, stage 2 (on the hub host, whose report carries none) tells the operator to open them:
	// both say which permission to leave ticked.
	for _, i := range []int{0, 1} {
		if !strings.Contains(strings.Join(reports[i].Next, "\n"), TickSentence) {
			t.Errorf("stage %d: the step that opens the links does not say which permission to leave ticked: %v", i+1, reports[i].Next)
		}
	}
}

func TestLightningOnlyStages(t *testing.T) {
	e := newEnv(t, fxLightning(t))
	rep, err := e.run(1, 1)
	if err != nil || len(rep.Links) != 1 || !strings.Contains(rep.Links[0], "pubkey="+clientKey(t, e.pubDir, "nwc-client.key")) {
		t.Fatalf("one link was expected: %+v %v", rep, err)
	}
	r, _ := ParsePublicToHub(e.read(e.pubDir, "setup/public-to-hub.json"))
	if r.SatboothRPCAuth != "" || r.PayoutClient != "" || r.Monero {
		t.Fatalf("%+v", r)
	}
	e.upTo(0)
	for n := 2; n <= 4; n++ {
		if _, err := e.run(n, 1); err != nil {
			t.Fatal(err)
		}
	}
	wantTree(t, "public", e.pubDir, []string{"etc/", "backups/", "setup/", "setup/api-keys/", "etc/nwc-client.key",
		"etc/webhook.secret", "etc/api-keys.json", "setup/api-keys/orders-write.key", "setup/api-keys/orders-read.key",
		"setup/public-to-hub.json", "setup/state.json", "compose.env", "etc/satbooth.json", "setup/read-hub-to-public.json"})
	wantTree(t, "hub", e.hubDir, []string{"config/", "wallet/", "setup/", "config/hub.env", "config/fulcrum.conf",
		"config/bitcoin.conf", "setup/read-public-to-hub.json", "setup/state.json", "compose.env", "config/relay.json",
		"setup/hub-to-public.json"})
	if _, err := service.ParseConfig(e.read(e.pubDir, "etc/satbooth.json")); err != nil {
		t.Fatal(err)
	}
	rc, err := relay.ParseConfig(e.read(e.hubDir, "config/relay.json"))
	if err != nil || len(rc.Pairs) != 1 {
		t.Fatalf("%v %v", rc, err)
	}
	if strings.Contains(string(e.read(e.hubDir, "config/bitcoin.conf")), "satbooth") {
		t.Error("a SatBooth rpcauth user without the on-chain rail")
	}
}

func clientKey(t *testing.T, dir, name string) string {
	t.Helper()
	k, err := nostr.LoadSecretKeyFile(filepath.Join(dir, "etc", name))
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKeyHex()
}

func TestLinksComeFromStageOne(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	r, err := e.run(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Links) != 2 || !strings.Contains(r.Links[0], "pubkey="+clientKey(t, e.pubDir, "nwc-client.key")) ||
		!strings.Contains(r.Links[1], "pubkey="+clientKey(t, e.pubDir, "payout.key")) ||
		!strings.Contains(r.Links[1], "max_amount=") {
		t.Fatalf("%v", r.Links)
	}
	r2, err := e.run(2, 1)
	if err != nil || len(r2.Links) != 0 {
		t.Fatalf("%v %v", r2, err)
	}
}

func TestStage4IgnoresURLsInTheHandoff(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	good := e.read(e.hubDir, "setup/hub-to-public.json")
	bad := bytes.Replace(good, []byte(`"version": 1,`), []byte(`"version": 1,
  "relay_url": "ws://192.0.2.99:7448",`), 1)
	before := snapshot(t, e.pubDir)
	_, err := PublicFinish(e.pubDir, bad, Digest(bad), e.read(e.hubDir, "setup/carry/wallet-rpc.pass"))
	if !errors.Is(err, ErrHandoffFormat) {
		t.Fatalf("%v", err)
	}
	sameTree(t, "refused", before, snapshot(t, e.pubDir))
	if _, err := e.run(4, 1); err != nil {
		t.Fatal(err)
	}
	cfg := string(e.read(e.pubDir, "etc/satbooth.json"))
	want := `"relay_url": "` + RelayURL(e.pub.HubPrivateAddr) + `"`
	if strings.Count(cfg, want) != 2 || strings.Contains(cfg, "192.0.2.99") {
		t.Fatalf("relay URLs in satbooth.json: %s", cfg)
	}
}

func TestDigestIsChecked(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(1)
	h1 := e.read(e.pubDir, "setup/public-to-hub.json")
	flip := []byte(strings.ReplaceAll(Digest(h1), " ", ""))
	flip[0] ^= 1 // 0<->1 or a<->b: still lowercase hex
	other := Digest([]byte("something else\n"))
	for _, d := range []string{string(flip), "", " ", other, Digest(h1) + "0", strings.ToUpper(Digest(h1))} {
		before := snapshot(t, e.hubDir)
		if _, err := HubInit(e.hubDir, &e.hub, h1, d, detRand(1)); !errors.Is(err, ErrDigest) {
			t.Fatalf("digest %q: %v", d, err)
		}
		sameTree(t, "hub init", before, snapshot(t, e.hubDir))
	}
	for i, d := range []string{Digest(h1), strings.ReplaceAll(Digest(h1), " ", "")} {
		e2 := newEnv(t, fxEveryRail(t))
		e2.upTo(1)
		h := e2.read(e2.pubDir, "setup/public-to-hub.json")
		d = map[int]string{0: Digest(h), 1: strings.ReplaceAll(Digest(h), " ", "")}[i]
		if _, err := HubInit(e2.hubDir, &e2.hub, h, d, detRand(1)); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	// Stage 4, with the other handoff's digest.
	e.upTo(0)
	for n := 2; n <= 3; n++ {
		if _, err := e.run(n, 1); err != nil {
			t.Fatal(err)
		}
	}
	h2 := e.read(e.hubDir, "setup/hub-to-public.json")
	pass := e.read(e.hubDir, "setup/carry/wallet-rpc.pass")
	for _, d := range []string{"", Digest(h1), flipFirst(h2)} {
		before := snapshot(t, e.pubDir)
		if _, err := PublicFinish(e.pubDir, h2, d, pass); !errors.Is(err, ErrDigest) {
			t.Fatalf("digest %q: %v", d, err)
		}
		sameTree(t, "public finish", before, snapshot(t, e.pubDir))
	}
	if _, err := PublicFinish(e.pubDir, h2, strings.ReplaceAll(Digest(h2), " ", ""), pass); err != nil {
		t.Fatal(err)
	}
}

func flipFirst(b []byte) string {
	d := []byte(strings.ReplaceAll(Digest(b), " ", ""))
	d[0] ^= 1
	return string(d)
}

func TestStageOrder(t *testing.T) {
	// One full run, with a copy of each host's directory after each of its stages.
	full := newEnv(t, fxEveryRail(t))
	saved := map[string]string{}
	save := func(name, dir string) {
		d := baseDir(t)
		copyTree(t, dir, d)
		saved[name] = d
	}
	saved["pub0"], saved["hub0"] = baseDir(t), baseDir(t)
	for n := 1; n <= 4; n++ {
		if _, err := full.run(n, 1); err != nil {
			t.Fatal(err)
		}
		save(map[int]string{1: "pub1", 2: "hub2", 3: "hub3", 4: "pub4"}[n], full.dir(n))
	}
	h1 := full.read(full.pubDir, "setup/public-to-hub.json")
	h2 := full.read(full.hubDir, "setup/hub-to-public.json")
	pass := full.read(full.hubDir, "setup/carry/wallet-rpc.pass")
	// For each stage, every state of its host except the predecessor.
	for n, names := range map[int][]string{1: {"pub1", "pub4"}, 2: {"hub2", "hub3"}, 3: {"hub0", "hub3"}, 4: {"pub0", "pub4"}} {
		for _, name := range names {
			d := baseDir(t)
			copyTree(t, saved[name], d)
			before := snapshot(t, d)
			var err error
			switch n {
			case 1:
				_, err = PublicInit(d, &full.pub, "http://127.0.0.1:8080", detRand(9))
			case 2:
				_, err = HubInit(d, &full.hub, h1, Digest(h1), detRand(9))
			case 3:
				_, err = HubPair(d, full.wallets[0], full.wallets[1])
			case 4:
				_, err = PublicFinish(d, h2, Digest(h2), pass)
			}
			if !errors.Is(err, ErrWrongStage) {
				t.Fatalf("stage %d at %s: %v", n, name, err)
			}
			sameTree(t, fmt.Sprintf("stage %d at %s", n, name), before, snapshot(t, d))
		}
	}
	// The other host's directory.
	e := newEnv(t, fxEveryRail(t))
	e.upTo(1)
	h1 = e.read(e.pubDir, "setup/public-to-hub.json")
	before := snapshot(t, e.pubDir)
	if _, err := HubInit(e.pubDir, &e.hub, h1, Digest(h1), detRand(1)); !errors.Is(err, ErrNotAStage) {
		t.Fatalf("%v", err)
	}
	if _, err := HubPair(e.pubDir, curveKeys[2], curveKeys[3]); !errors.Is(err, ErrNotAStage) {
		t.Fatalf("%v", err)
	}
	sameTree(t, "hub init on the public host", before, snapshot(t, e.pubDir))
	e.upTo(0)
	if _, err := e.run(2, 1); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, e.hubDir)
	if _, err := PublicInit(e.hubDir, &e.pub, "http://127.0.0.1:8080", detRand(1)); !errors.Is(err, ErrNotAStage) {
		t.Fatalf("%v", err)
	}
	if _, err := PublicFinish(e.hubDir, h1, Digest(h1), nil); !errors.Is(err, ErrNotAStage) {
		t.Fatalf("%v", err)
	}
	sameTree(t, "public init on the hub host", before, snapshot(t, e.hubDir))
}

// stageTargets is every name stage n creates, found by running it.
func stageTargets(t *testing.T, f func(*testing.T) *fixture, n int) []string {
	e := newEnv(t, f(t))
	e.upTo(n - 1)
	before := snapshot(t, e.dir(n))
	if _, err := e.run(n, 1); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range snapshot(t, e.dir(n)) {
		if _, ok := before[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return append(out, "setup/tmp-"+string(map[int]Stage{1: StagePublicInit, 2: StageHubInit, 3: StageHubPair, 4: StagePublicDone}[n]))
}

func TestStageRefusesExistingFile(t *testing.T) {
	for n := 1; n <= 4; n++ {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		for _, target := range append(stageTargets(t, fxEveryRail, n), stateTmp) {
			target = strings.TrimSuffix(target, "/")
			for _, kind := range []string{"file", "symlink"} {
				e := base.clone()
				d := e.dir(n)
				p := filepath.Join(d, target)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "file" {
					err := os.WriteFile(p, []byte("mine"), 0o600)
					if err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(d, "dangling-target"), p); err != nil {
					t.Fatal(err)
				}
				before := snapshot(t, d)
				_, err := e.run(n, 2)
				if !errors.Is(err, ErrExists) {
					t.Fatalf("stage %d, %s %s: %v", n, kind, target, err)
				}
				// A first stage names "setup" itself when something else sits in an empty setup directory.
				if !namesOrParent(err.Error(), target) {
					t.Fatalf("stage %d, %s %s: the error does not name it: %v", n, kind, target, err)
				}
				sameTree(t, fmt.Sprintf("stage %d, %s %s", n, kind, target), before, snapshot(t, d))
			}
		}
	}
}

// crashAt runs stage n of e with a crash at the k-th hook call and returns the error.
func (e *env) crashAt(n, k int, seed int64) error {
	boom := errors.New("crash")
	i := 0
	stepHook = func(int) error {
		if i++; i == k {
			return boom
		}
		return nil
	}
	defer func() { stepHook = nil }()
	_, err := e.run(n, seed)
	if !errors.Is(err, boom) {
		e.t.Fatalf("stage %d, crash %d: %v", n, k, err)
	}
	return err
}

// hookCalls counts the hook calls of a whole run of stage n.
func hookCalls(t *testing.T, base *env, n int) (int, *env) {
	c := base.clone()
	calls := 0
	stepHook = func(int) error { calls++; return nil }
	defer func() { stepHook = nil }()
	if _, err := c.run(n, 1); err != nil {
		t.Fatal(err)
	}
	return calls, c
}

func TestCrashAtEveryStep(t *testing.T) {
	for n := 1; n <= 4; n++ {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		total, dry := hookCalls(t, base, n)
		if total < 6 {
			t.Fatalf("stage %d: only %d hook calls", n, total)
		}
		stage := tmpStage(n)
		for k := 1; k <= total; k++ {
			e := base.clone()
			d := e.dir(n)
			before := snapshot(t, d)
			e.crashAt(n, k, 1)
			crashed := snapshot(t, d)
			if _, err := e.run(n, 2); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("stage %d, crash %d: second run: %v", n, k, err)
			}
			sameTree(t, "second run", crashed, snapshot(t, d))
			// A file planted beside the journaled names survives.
			planted := []string{filepath.Join(d, "planted.txt")}
			switch n {
			case 3:
				planted = append(planted, filepath.Join(d, "config", "planted.txt"))
			case 4:
				planted = append(planted, filepath.Join(d, "etc", "planted.txt"))
			}
			for _, p := range planted {
				if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := Discard(d, stage); err != nil {
				t.Fatalf("stage %d, crash %d: discard: %v", n, k, err)
			}
			after := snapshot(t, d)
			for _, p := range planted {
				rel, _ := filepath.Rel(d, p)
				if _, ok := after[rel]; !ok {
					t.Fatalf("stage %d, crash %d: %s was removed", n, k, rel)
				}
				os.Remove(p)
			}
			sameTree(t, fmt.Sprintf("stage %d, crash %d, after discard", n, k), before, snapshot(t, d))
			if _, _, err := Discard(d, stage); err == nil {
				t.Fatal("a second discard succeeded")
			}
			// The stage now runs to the end with other secrets.
			if _, err := e.run(n, 2); err != nil {
				t.Fatalf("stage %d, crash %d: rerun: %v", n, k, err)
			}
			if exists(filepath.Join(d, journalName)) || exists(filepath.Join(d, journalTmp)) ||
				exists(filepath.Join(d, "setup", "tmp-"+string(stage))) {
				t.Fatal("the journal or the temporary directory is left")
			}
			for _, name := range map[int][]string{1: {"etc/webhook.secret", "setup/api-keys/orders-read.key"}, 2: {"config/hub.env", "config/fulcrum.conf"}}[n] {
				if bytes.Equal(e.read(d, name), dryRead(t, dry.dir(n), name)) {
					t.Fatalf("stage %d, crash %d: %s is the dead run's", n, k, name)
				}
			}
		}
	}
}

// A crash during Discard, after each of its removals: a second Discard finishes, and the tree is the one before the
// stage.
func TestCrashDuringDiscard(t *testing.T) {
	for n := 1; n <= 4; n++ {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		total, _ := hookCalls(t, base, n)
		dead := base.clone()
		dead.crashAt(n, total, 1) // the latest crash: everything is in place but the journal
		before := snapshot(t, base.dir(n))
		count := 0
		discardHook = func() error { count++; return nil }
		if _, _, err := Discard(dead.clone().dir(n), tmpStage(n)); err != nil {
			t.Fatal(err)
		}
		discardHook = nil
		if count < 3 {
			t.Fatalf("stage %d: only %d removals", n, count)
		}
		for k := 1; k <= count; k++ {
			e := dead.clone()
			d := e.dir(n)
			i := 0
			discardHook = func() error {
				if i++; i == k {
					return errors.New("crash")
				}
				return nil
			}
			_, _, err := Discard(d, tmpStage(n))
			discardHook = nil
			if err == nil {
				t.Fatalf("stage %d, discard crash %d: no crash", n, k)
			}
			if _, err := e.run(n, 2); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("stage %d, discard crash %d: %v", n, k, err)
			}
			if _, _, err := Discard(d, tmpStage(n)); err != nil {
				t.Fatalf("stage %d, discard crash %d: second discard: %v", n, k, err)
			}
			sameTree(t, fmt.Sprintf("stage %d, discard crash %d", n, k), before, snapshot(t, d))
		}
	}
}

func tmpStage(n int) Stage {
	return map[int]Stage{1: StagePublicInit, 2: StageHubInit, 3: StageHubPair, 4: StagePublicDone}[n]
}

func dryRead(t *testing.T, dir, name string) []byte {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNothingSecretIsPrinted(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	var text []string
	for n := 1; n <= 4; n++ {
		r, err := e.run(n, 1)
		if err != nil {
			t.Fatal(err)
		}
		text = append(text, r.Digest, strings.Join(r.Links, "\n"), strings.Join(r.Next, "\n"))
	}
	// The refusals too: each error string.
	h1 := e.read(e.pubDir, "setup/public-to-hub.json")
	h2 := e.read(e.hubDir, "setup/hub-to-public.json")
	pass := e.read(e.hubDir, "setup/carry/wallet-rpc.pass")
	var errs []error
	add := func(_ *Report, err error) { errs = append(errs, err) }
	add(PublicInit(e.pubDir, &e.pub, "http://127.0.0.1:8080", detRand(5)))
	add(HubInit(e.hubDir, &e.hub, h1, Digest(h1), detRand(5)))
	add(HubPair(e.hubDir, curveKeys[2], curveKeys[3]))
	add(PublicFinish(e.pubDir, h2, Digest(h2), pass))
	add(PublicFinish(e.pubDir, h2, "0", pass))
	other := newEnv(t, fxEveryRail(t))
	if _, err := other.run(1, 7); err != nil { // other randomness: other keys and another handoff
		t.Fatal(err)
	}
	hh := other.read(other.pubDir, "setup/public-to-hub.json")
	add(HubInit(baseDir(t), &e.hub, hh, Digest(h1), detRand(5)))
	add(PublicInit(e.hubDir, &e.pub, "http://127.0.0.1:8080", detRand(5)))
	_, _, derr := Discard(e.pubDir, StagePublicInit)
	errs = append(errs, derr)
	add(PublicInit(e.pubDir, &e.pub, "ftp://x", detRand(5)))
	for i, err := range errs {
		if err == nil {
			t.Fatalf("an attempt that should be refused was not: %d", i)
		}
		text = append(text, err.Error())
	}
	all := strings.Join(text, "\n")
	secrets := []string{e.pub.Descriptor, e.pub.MoneroAddress, xpubV1, xmrMain}
	hex64 := regexp.MustCompile(`[0-9a-f]{64}`)
	for _, c := range []struct {
		dir string
		pat []string
	}{
		{e.pubDir, []string{"etc/*.key", "etc/*.secret", "etc/*.pass", "setup/api-keys/*"}},
		{e.hubDir, []string{"config/hub.env", "config/fulcrum.conf", "config/wallet-rpc/*", "setup/carry/*"}},
	} {
		for _, p := range c.pat {
			ms, _ := filepath.Glob(filepath.Join(c.dir, p))
			if len(ms) == 0 {
				t.Fatalf("no file for %s", p)
			}
			for _, m := range ms {
				found := hex64.FindAllString(string(dryRead(t, "", m)), -1)
				if len(found) == 0 {
					t.Fatalf("no secret in %s", m)
				}
				for _, f := range found {
					// the whole secret and slices of it
					secrets = append(secrets, f, f[:16], f[24:40], f[48:])
				}
			}
		}
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(all, s) {
			t.Fatalf("a secret reached the report or an error")
		}
	}
}

func TestModesIgnoreUmask(t *testing.T) {
	var trees [2]map[string]string
	for i, um := range []int{0, 0o077} {
		old := syscall.Umask(um)
		e := newEnv(t, fxEveryRail(t))
		e.upTo(4)
		syscall.Umask(old)
		trees[i] = map[string]string{}
		for _, d := range []string{e.pubDir, e.hubDir} {
			for k, v := range snapshot(t, d) {
				trees[i][k] = v[:strings.IndexByte(v+" ", ' ')+4]
			}
		}
		wantTree(t, "pub", e.pubDir, pubStage4)
		wantTree(t, "hub", e.hubDir, hubStage3)
	}
	if len(trees[0]) != len(trees[1]) {
		t.Fatal("different trees")
	}
	for k, v := range trees[0] {
		if trees[1][k] != v {
			t.Fatalf("%s: %s then %s", k, v, trees[1][k])
		}
	}
}

// namesOrParent reports whether msg names target or a directory above it (a first stage names the directory it
// finds in its way, which is a clash too).
func namesOrParent(msg, target string) bool {
	for t := target; t != "." && t != "/"; t = filepath.Dir(t) {
		if strings.Contains(msg, t) {
			return true
		}
	}
	return false
}

// run3 runs stage n with fixed inputs and no clone: for the refusal tests, which compare trees.
func mustRefuse(t *testing.T, what string, e *env, n int, want string) {
	t.Helper()
	d := e.dir(n)
	before := snapshot(t, d)
	_, err := e.run(n, 3)
	if err == nil || want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: %v", what, err)
	}
	sameTree(t, what, before, snapshot(t, d))
}

func TestBaseDirectoryIsChecked(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o777, 0o750, 0o770, 0o701, 0o500} {
		e := newEnv(t, fxEveryRail(t))
		if err := os.Chmod(e.pubDir, mode); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, fmt.Sprintf("mode %o", mode), e, 1, "0700")
		os.Chmod(e.pubDir, 0o700)
	}
	// A symbolic link to a good directory, a file, a missing directory.
	e := newEnv(t, fxEveryRail(t))
	link := filepath.Join(baseDir(t), "link")
	if err := os.Symlink(e.pubDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PublicInit(link, &e.pub, "http://127.0.0.1:8080", detRand(1)); err == nil {
		t.Fatal("a symbolic link as the base directory")
	}
	if len(snapshot(t, e.pubDir)) != 0 {
		t.Fatal("something was written through the link")
	}
	if _, err := PublicInit(filepath.Join(e.pubDir, "missing"), &e.pub, "http://127.0.0.1:8080", detRand(1)); err == nil {
		t.Fatal("a missing directory")
	}
	if err := Preflight(e.pubDir, StagePublicInit); err != nil {
		t.Fatal(err)
	}
	os.Chmod(e.pubDir, 0o755)
	if err := Preflight(e.pubDir, StagePublicInit); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("%v", err)
	}
}

func TestFirstStagesNeverAdoptADirectory(t *testing.T) {
	for n, dirs := range map[int][]string{1: {"setup", "etc", "backups"}, 2: {"setup", "config", "wallet"}} {
		for _, d := range dirs {
			for _, mode := range []os.FileMode{0o700, 0o755, 0o777} {
				e := newEnv(t, fxEveryRail(t))
				if n == 2 {
					e.upTo(1)
				}
				p := filepath.Join(e.dir(n), d)
				if err := os.Mkdir(p, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(p, mode); err != nil {
					t.Fatal(err)
				}
				before := snapshot(t, e.dir(n))
				_, err := e.run(n, 3)
				if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), d) {
					t.Fatalf("stage %d, %s %o: %v", n, d, mode, err)
				}
				sameTree(t, fmt.Sprintf("stage %d, %s %o", n, d, mode), before, snapshot(t, e.dir(n)))
				if fi, _ := os.Lstat(p); fi.Mode().Perm() != mode {
					t.Fatalf("the mode of %s changed", d)
				}
			}
		}
	}
}

func TestLaterStagesCheckTheDirectoriesTheyWriteInto(t *testing.T) {
	for n, dirs := range map[int][]string{3: {"setup", "config"}, 4: {"setup", "etc"}} {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		for _, d := range dirs {
			for _, mode := range []os.FileMode{0o755, 0o777, 0o750, 0o770, 0o500} {
				e := base.clone()
				if err := os.Chmod(filepath.Join(e.dir(n), d), mode); err != nil {
					t.Fatal(err)
				}
				mustRefuse(t, fmt.Sprintf("stage %d, %s %o", n, d, mode), e, n, d)
				os.Chmod(filepath.Join(e.dir(n), d), 0o700)
			}
			// A symbolic link in place of the directory, to a copy inside the base directory.
			e := base.clone()
			p := filepath.Join(e.dir(n), d)
			if err := os.Rename(p, p+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(d+"-real", p); err != nil {
				t.Fatal(err)
			}
			mustRefuse(t, fmt.Sprintf("stage %d, %s as a link", n, d), e, n, d)
		}
	}
	// What the hub bundle's first start changes is not a refusal: stage 3 writes only config/relay.json.
	e := newEnv(t, fxEveryRail(t))
	e.upTo(2)
	for _, p := range []string{"config/wallet-rpc", "config/hub.env", "config/bitcoin.conf"} {
		if err := os.Chmod(filepath.Join(e.hubDir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.run(3, 1); err != nil {
		t.Fatal(err)
	}
}

func TestNothingResolvesOutsideTheDirectory(t *testing.T) {
	// A stage whose etc/ is a link to somewhere else writes nothing there.
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	outside := baseDir(t)
	if err := os.Rename(filepath.Join(e.pubDir, "etc"), filepath.Join(outside, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "etc"), filepath.Join(e.pubDir, "etc")); err != nil {
		t.Fatal(err)
	}
	outBefore := snapshot(t, outside)
	mustRefuse(t, "etc as a link outside", e, 4, "etc")
	sameTree(t, "outside", outBefore, snapshot(t, outside))

	// Discard does not follow a link outside, nor one that stays inside, to remove files.
	for _, inside := range []bool{false, true} {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(3)
		total, _ := hookCalls(t, base, 4)
		e := base.clone()
		e.crashAt(4, total, 1)
		real := filepath.Join(e.pubDir, "etc")
		var target string
		if inside {
			target = filepath.Join(e.pubDir, "etc-copy")
		} else {
			target = filepath.Join(baseDir(t), "etc-copy")
		}
		os.Mkdir(target, 0o700)
		copyTree(t, real, target)
		if err := os.RemoveAll(real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, real); err != nil {
			t.Fatal(err)
		}
		decoys := snapshot(t, target)
		before := snapshot(t, e.pubDir)
		if rm, _, err := Discard(e.pubDir, StagePublicDone); err == nil || len(rm) != 0 { // refused: etc/ is a link
			t.Fatalf("%v %v", rm, err)
		}
		sameTree(t, fmt.Sprintf("tree (inside %v)", inside), before, snapshot(t, e.pubDir))
		sameTree(t, fmt.Sprintf("decoys (inside %v)", inside), decoys, snapshot(t, target))
		if len(decoys) == 0 {
			t.Fatal("no decoys")
		}
	}
}

func TestLinkRaceUndoesTheRun(t *testing.T) {
	for _, c := range []struct {
		n       int
		planted string
	}{{3, "config/relay.json"}, {3, "setup/hub-to-public.json"}, {4, "compose.env"}, {4, "etc/satbooth.json"}} {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(c.n - 1)
		e := base.clone()
		d := e.dir(c.n)
		before := snapshot(t, d)
		planted := filepath.Join(d, c.planted)
		first := true
		stepHook = func(step int) error {
			if step == 5 && first {
				first = false
				return os.WriteFile(planted, []byte("not ours"), 0o600)
			}
			return nil
		}
		_, err := e.run(c.n, 1)
		stepHook = nil
		if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), c.planted) {
			t.Fatalf("%v: %v", c, err)
		}
		if b, _ := os.ReadFile(planted); string(b) != "not ours" {
			t.Fatal("the other program's file was touched")
		}
		after := snapshot(t, d)
		if v, ok := after[c.planted]; !ok || !strings.HasSuffix(v, "not ours") {
			t.Fatal("the planted file is gone")
		}
		delete(after, c.planted)
		sameTree(t, fmt.Sprintf("%v", c), before, after)
		// And the stage can run again once the other file is out of the way.
		os.Remove(planted)
		if _, err := e.run(c.n, 2); err != nil {
			t.Fatalf("%v: rerun: %v", c, err)
		}
	}
}

func TestTornJournal(t *testing.T) {
	// A journal.json.tmp left by a run that died before the rename: the next run removes it and proceeds.
	e := newEnv(t, fxEveryRail(t))
	if err := os.Mkdir(filepath.Join(e.pubDir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.pubDir, journalTmp), []byte(`{"vers`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(e.pubDir, StagePublicInit); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(1, 1); err != nil {
		t.Fatal(err)
	}
	wantTree(t, "stage 1", e.pubDir, pubStage1)
	// The same before a later stage.
	e.upTo(0)
	for n := 2; n <= 3; n++ {
		if _, err := e.run(n, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.pubDir, journalTmp), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(4, 1); err != nil {
		t.Fatal(err)
	}
	wantTree(t, "stage 4", e.pubDir, pubStage4)
	// Discard removes a lone journal.json.tmp (and the empty setup/ a first stage made around it).
	e2 := newEnv(t, fxEveryRail(t))
	os.Mkdir(filepath.Join(e2.pubDir, "setup"), 0o700)
	os.WriteFile(filepath.Join(e2.pubDir, journalTmp), []byte("x"), 0o600)
	if rm, _, err := Discard(e2.pubDir, StagePublicInit); err != nil || len(rm) != 1 {
		t.Fatalf("%v %v", rm, err)
	}
	if len(snapshot(t, e2.pubDir)) != 0 { // the empty setup/ a first stage made goes with it
		t.Fatalf("%v", keys(snapshot(t, e2.pubDir)))
	}

	// A journal.json that is empty or not a journal: every stage says ErrIncomplete and tells the operator what to do;
	// Discard never guesses.
	for _, content := range []string{"", "{", "null", `{"version":1}`, "[]"} {
		e := newEnv(t, fxEveryRail(t))
		e.upTo(1)
		os.WriteFile(filepath.Join(e.pubDir, journalName), []byte(content), 0o600)
		before := snapshot(t, e.pubDir)
		h := e.read(e.pubDir, "setup/public-to-hub.json")
		_, err := HubInit(e.hubDir, &e.hub, h, Digest(h), detRand(1))
		if err != nil {
			t.Fatal(err) // the hub host is a different directory
		}
		_, err = PublicFinish(e.pubDir, h, Digest(h), nil)
		if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "by hand") {
			t.Fatalf("%q: %v", content, err)
		}
		if _, _, err := Discard(e.pubDir, StagePublicDone); err == nil || !strings.Contains(err.Error(), "by hand") {
			t.Fatalf("%q: discard: %v", content, err)
		}
		sameTree(t, fmt.Sprintf("journal %q", content), before, snapshot(t, e.pubDir))
	}
}

func TestDiscardRefusals(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(2)
	d := e.hubDir
	prev := string(e.read(d, "setup/state.json")) // the state at hub-init
	pubState := string(e.read(e.pubDir, "setup/state.json"))
	jr := func(mod func(*journal)) []byte {
		p := prev
		j := journal{Version: 1, Stage: StageHubPair, Dirs: []string{"setup/tmp-hub-pair"},
			Files: []string{stateTmp, "config/relay.json"}, PrevState: &p}
		mod(&j)
		if j.SHA256 == nil {
			for range j.Files {
				j.SHA256 = append(j.SHA256, sumHex([]byte("decoy")))
			}
		}
		b, _ := json.Marshal(j)
		return b
	}
	str := func(s string) *string { return &s }
	cases := map[string][]byte{
		"a hub-init file":            jr(func(j *journal) { j.Files = append(j.Files, "config/hub.env") }),
		"the wallet directory":       jr(func(j *journal) { j.Dirs = append(j.Dirs, "wallet") }),
		"the carried password":       jr(func(j *journal) { j.Files = append(j.Files, "setup/carry/wallet-rpc.pass") }),
		"a dotdot":                   jr(func(j *journal) { j.Files = append(j.Files, "config/../setup/state.json") }),
		"a leading dotdot":           jr(func(j *journal) { j.Files = []string{"../victim"} }),
		"an absolute name":           jr(func(j *journal) { j.Files = []string{"/etc/passwd"} }),
		"the journal itself":         jr(func(j *journal) { j.Files = append(j.Files, journalName) }),
		"state.json in a later one":  jr(func(j *journal) { j.Files = append(j.Files, stateName) }),
		"another stage's temp file":  jr(func(j *journal) { j.Files = append(j.Files, "setup/tmp-hub-init/0") }),
		"a temp file past the bound": jr(func(j *journal) { j.Files = append(j.Files, "setup/tmp-hub-pair/64") }),
		"a leading-zero temp file":   jr(func(j *journal) { j.Files = append(j.Files, "setup/tmp-hub-pair/01") }),
		"a null prev_state":          jr(func(j *journal) { j.PrevState = nil }),
		"the public host's state":    jr(func(j *journal) { j.PrevState = &pubState }),
		"a garbage prev_state":       jr(func(j *journal) { j.PrevState = str("{}") }),
		"a prev_state at hub-pair": jr(func(j *journal) {
			b, _ := os.ReadFile(filepath.Join(d, "setup/state.json"))
			_ = b
			s := strings.Replace(prev, `"stage": "hub-init"`, `"stage": "hub-pair"`, 1)
			j.PrevState = &s
		}),
		"a missing hash list":   []byte(strings.Replace(string(jr(func(*journal) {})), `"sha256"`, `"sha257"`, 1)),
		"a null hash list":      jr(func(j *journal) { j.SHA256 = nil; j.Files = nil; j.Dirs = nil }),
		"too few hashes":        jr(func(j *journal) { j.SHA256 = []string{sumHex(nil)} }),
		"too many hashes":       jr(func(j *journal) { j.SHA256 = []string{sumHex(nil), sumHex(nil), sumHex(nil)} }),
		"the empty file's hash": jr(func(j *journal) { j.SHA256 = []string{sumHex(nil), sumHex([]byte("x"))} }),
		"a short hash":          jr(func(j *journal) { j.SHA256 = []string{sumHex(nil)[:63], sumHex(nil)} }),
		"a long hash":           jr(func(j *journal) { j.SHA256 = []string{sumHex(nil) + "0", sumHex(nil)} }),
		"an uppercase hash":     jr(func(j *journal) { j.SHA256 = []string{strings.ToUpper(sumHex(nil)), sumHex(nil)} }),
		"a non-hex hash":        jr(func(j *journal) { j.SHA256 = []string{strings.Repeat("g", 64), sumHex(nil)} }),
		"a duplicate key":       []byte(strings.Replace(string(jr(func(*journal) {})), `"version":1`, `"version":1,"version":1`, 1)),
		"a case-variant key":    []byte(strings.Replace(string(jr(func(*journal) {})), `"stage"`, `"Stage"`, 1)),
		"a trailing brace":      append(jr(func(*journal) {}), '}'),
		"a version":             jr(func(j *journal) { j.Version = 2 }),
		"an unknown stage":      jr(func(j *journal) { j.Stage = "hub-start" }),
		"extra data":            append(jr(func(*journal) {}), []byte(` {}`)...),
		"an unknown key":        []byte(strings.Replace(string(jr(func(*journal) {})), `"version":1`, `"version":1,"x":1`, 1)),
	}
	decoys := []string{"config/hub.env", "config/relay.json", "wallet/keep", "setup/carry/wallet-rpc.pass"}
	os.WriteFile(filepath.Join(d, "config/relay.json"), []byte("decoy"), 0o600)
	os.WriteFile(filepath.Join(d, "wallet/keep"), []byte("decoy"), 0o600)
	victim := filepath.Join(filepath.Dir(d), "victim")
	os.WriteFile(victim, []byte("x"), 0o600)
	defer os.Remove(victim)
	for name, j := range cases {
		os.WriteFile(filepath.Join(d, journalName), j, 0o600)
		before := snapshot(t, d)
		if rm, _, err := Discard(d, StageHubPair); err == nil || len(rm) != 0 {
			t.Fatalf("%s: accepted (%v, %v)", name, rm, err)
		}
		sameTree(t, name, before, snapshot(t, d))
		for _, c := range decoys {
			if !exists(filepath.Join(d, c)) {
				t.Fatalf("%s: %s was removed", name, c)
			}
		}
		if !exists(victim) {
			t.Fatal("a file outside was removed")
		}
	}
	// The same journal under another stage's name, and a good journal of the stage as the positive control.
	os.WriteFile(filepath.Join(d, journalName), jr(func(*journal) {}), 0o600)
	before := snapshot(t, d)
	for _, st := range []Stage{StageHubInit, StagePublicInit, StagePublicDone} {
		if _, _, err := Discard(d, st); err == nil {
			t.Fatalf("a hub-pair journal was discarded as %s", st)
		}
		sameTree(t, "wrong stage", before, snapshot(t, d))
	}
	if _, _, err := Discard(d, "nonsense"); err == nil {
		t.Fatal("an unknown stage")
	}
	if rm, _, err := Discard(d, StageHubPair); err != nil || len(rm) != 1 || rm[0] != "config/relay.json" {
		t.Fatalf("%v %v", rm, err)
	}
	if exists(filepath.Join(d, journalName)) || !exists(filepath.Join(d, "config/hub.env")) {
		t.Fatal("the control did not behave")
	}
	if _, _, err := Discard(d, StageHubPair); err == nil || !strings.Contains(err.Error(), "nothing to discard") {
		t.Fatalf("%v", err)
	}
}

func TestNextLinesUseHostPaths(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.hub.HubAdminAddr = "10.9.8.7"
	rel := regexp.MustCompile(`(^|[ :=(])(setup|etc|config|backups)/`)
	var all []string
	for n := 1; n <= 4; n++ {
		r, err := e.run(n, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range r.Next {
			if rel.MatchString(l) {
				t.Errorf("stage %d: a relative path: %s", n, l)
			}
			all = append(all, l)
		}
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{e.pub.HostDir + "/setup/public-to-hub.json", e.pub.HostDir + "/setup/api-keys/",
		e.pub.HostDir + "/etc/webhook.secret", "--password-file /etc/wallet-rpc/wallet.pass",
		e.hub.HostDir + "/setup/hub-to-public.json", e.hub.HostDir + "/setup/carry/wallet-rpc.pass",
		"ssh -L 8080:10.9.8.7:8080"} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in the next steps", want)
		}
	}
}

// Every secret, the NWC client keys included, comes from rand, in the format nostr's own writer and loader use.
func TestClientKeysComeFromRand(t *testing.T) {
	a, b, c := newEnv(t, fxEveryRail(t)), newEnv(t, fxEveryRail(t)), newEnv(t, fxEveryRail(t))
	for i, e := range []*env{a, b, c} {
		seed := int64(11)
		if i == 2 {
			seed = 12
		}
		if _, err := e.run(1, seed); err != nil {
			t.Fatal(err)
		}
	}
	rd := detRand(11)
	first, _ := NewSecret(rd)
	second, _ := NewSecret(rd)
	if string(a.read(a.pubDir, "etc/nwc-client.key")) != first+"\n" || string(a.read(a.pubDir, "etc/payout.key")) != second+"\n" {
		t.Fatal("the client keys are not the first two draws of rand")
	}
	for _, name := range []string{"etc/nwc-client.key", "etc/payout.key", "setup/public-to-hub.json"} {
		if !bytes.Equal(a.read(a.pubDir, name), b.read(b.pubDir, name)) {
			t.Fatalf("%s differs between two runs with the same randomness", name)
		}
		if bytes.Equal(a.read(a.pubDir, name), c.read(c.pubDir, name)) {
			t.Fatalf("%s is the same with other randomness", name)
		}
	}
	for _, name := range []string{"nwc-client.key", "payout.key"} {
		fi, err := os.Stat(filepath.Join(a.pubDir, "etc", name))
		if err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 65 {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}
	in, _ := ParsePublicToHub(a.read(a.pubDir, "setup/public-to-hub.json"))
	if clientKey(t, a.pubDir, "nwc-client.key") != in.InvoiceClient || clientKey(t, a.pubDir, "payout.key") != in.PayoutClient {
		t.Fatal("the handoff's client keys are not the files'")
	}
	// A reader that cannot give the bytes is an error, and nothing is written.
	e := newEnv(t, fxEveryRail(t))
	before := snapshot(t, e.pubDir)
	if _, err := PublicInit(e.pubDir, &e.pub, "http://127.0.0.1:8080", strings.NewReader(strings.Repeat("x", 40))); err == nil {
		t.Fatal("a short random source")
	}
	sameTree(t, "short randomness", before, snapshot(t, e.pubDir))
}

func TestDiscardRefusesALinkedSetup(t *testing.T) {
	for n := 1; n <= 4; n++ {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		total, _ := hookCalls(t, base, n)
		e := base.clone()
		e.crashAt(n, total, 1)
		d := e.dir(n)
		if err := os.Rename(filepath.Join(d, "setup"), filepath.Join(d, "setup-real")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("setup-real", filepath.Join(d, "setup")); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, d)
		rm, _, err := Discard(d, tmpStage(n))
		if err == nil || !strings.Contains(err.Error(), "setup") || len(rm) != 0 {
			t.Fatalf("stage %d: %v %v", n, rm, err)
		}
		sameTree(t, fmt.Sprintf("stage %d", n), before, snapshot(t, d))
	}
}

func TestEmptySetupIsRefusedWithAHint(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	if err := os.Mkdir(filepath.Join(e.pubDir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, e.pubDir)
	_, err := e.run(1, 1)
	if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "rmdir") || !strings.Contains(err.Error(), "empty setup/") {
		t.Fatalf("%v", err)
	}
	sameTree(t, "empty setup", before, snapshot(t, e.pubDir))
	if _, _, err := Discard(e.pubDir, StagePublicInit); err == nil || !strings.Contains(err.Error(), "rmdir") {
		t.Fatalf("%v", err)
	}
	// A setup/ with something in it gets no such hint.
	os.WriteFile(filepath.Join(e.pubDir, "setup", "x"), []byte("x"), 0o600)
	if _, err := e.run(1, 1); !errors.Is(err, ErrExists) || strings.Contains(err.Error(), "rmdir") {
		t.Fatalf("%v", err)
	}
}

func TestSetupLinkedOutsideIsNamedSetup(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	outside := baseDir(t)
	if err := os.Symlink(outside, filepath.Join(e.pubDir, "setup")); err != nil {
		t.Fatal(err)
	}
	_, err := e.run(1, 1)
	if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "setup") || strings.Contains(err.Error(), "state.json") {
		t.Fatalf("%v", err)
	}
	if len(snapshot(t, outside)) != 0 {
		t.Fatal("something was written outside")
	}
}

// If the undo of a lost race cannot remove something, the journal stays and the error says what to do.
func TestUndoKeepsTheJournalWhenARemovalFails(t *testing.T) {
	base := newEnv(t, fxEveryRail(t))
	base.upTo(2)
	e := base.clone()
	d := e.dir(3)
	config := filepath.Join(d, "config")
	planted := filepath.Join(d, "setup/hub-to-public.json")
	planted1 := false
	stepHook = func(step int) error {
		switch {
		case step == 5 && !planted1:
			planted1 = true
			return os.WriteFile(planted, []byte("not ours"), 0o600)
		case step == 6: // after config/relay.json is linked: it can no longer be removed
			return os.Chmod(config, 0o500)
		}
		return nil
	}
	_, err := e.run(3, 1)
	stepHook = nil
	os.Chmod(config, 0o700)
	if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "-discard-incomplete") {
		t.Fatalf("%v", err)
	}
	if !exists(filepath.Join(d, journalName)) {
		t.Fatal("the journal was removed although the undo did not finish")
	}
	if b, _ := os.ReadFile(planted); string(b) != "not ours" {
		t.Fatal("the other program's file was touched")
	}
	if _, err := e.run(3, 2); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("%v", err)
	}
	// Discard finishes the job. (It cannot tell the other program's file at a journaled name from ours: this
	// double failure is the one case where that file goes too.)
	if _, _, err := Discard(d, StageHubPair); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(config, "relay.json")) || exists(filepath.Join(d, journalName)) {
		t.Fatal("discard did not finish the job")
	}
}

// A directory that appears between the check and its mkdir is handled like a lost link race.
func TestMkdirRaceUndoesTheRun(t *testing.T) {
	for _, plant := range []string{"etc", "setup/tmp-public-init", "backups"} {
		e := newEnv(t, fxEveryRail(t))
		d := e.pubDir
		first := true
		stepHook = func(step int) error {
			if step == 4 && first {
				first = false
				return os.Mkdir(filepath.Join(d, plant), 0o700)
			}
			return nil
		}
		_, err := e.run(1, 1)
		stepHook = nil
		if !errors.Is(err, ErrExists) || !strings.Contains(err.Error(), plant) {
			t.Fatalf("%s: %v", plant, err)
		}
		after := snapshot(t, d)
		if exists(filepath.Join(d, journalName)) {
			t.Fatal("a journal is left")
		}
		// Only the other program's directory (and what must hold it) is left.
		want := map[string]bool{plant + "/": true}
		for p := filepath.Dir(plant); p != "."; p = filepath.Dir(p) {
			want[p+"/"] = true
		}
		for k := range after {
			if !want[k] {
				t.Fatalf("%s: %s is left", plant, k)
			}
		}
		if len(after) != len(want) {
			t.Fatalf("%s: %v", plant, keys(after))
		}
	}
}

// A second crash during the rerun of Discard.
func TestSecondCrashDuringDiscard(t *testing.T) {
	for n := 1; n <= 4; n++ {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		total, _ := hookCalls(t, base, n)
		dead := base.clone()
		dead.crashAt(n, total, 1)
		before := snapshot(t, base.dir(n))
		count := 0
		discardHook = func() error { count++; return nil }
		if _, _, err := Discard(dead.clone().dir(n), tmpStage(n)); err != nil {
			t.Fatal(err)
		}
		discardHook = nil
		for _, k := range []int{1, count / 2, count - 1} {
			if k < 1 {
				continue
			}
			for j := 1; j <= count-k+1; j++ {
				e := dead.clone()
				d := e.dir(n)
				crash := func(at int) bool {
					i := 0
					discardHook = func() error {
						if i++; i == at {
							return errors.New("crash")
						}
						return nil
					}
					_, _, err := Discard(d, tmpStage(n))
					discardHook = nil
					return err != nil
				}
				if !crash(k) {
					t.Fatalf("stage %d: no crash at %d", n, k)
				}
				if crash(j) { // the rerun crashed too: a third Discard finishes
					if _, _, err := Discard(d, tmpStage(n)); err != nil {
						t.Fatalf("stage %d, crashes %d,%d: %v", n, k, j, err)
					}
				}
				sameTree(t, fmt.Sprintf("stage %d, crashes %d,%d", n, k, j), before, snapshot(t, d))
			}
		}
	}
}

// Discard checks every directory it touches like the stages do: the ones the stage writes into before it reads the
// journal, the ones the journal names after (config/wallet-rpc excepted, but never as a link).
func TestDiscardChecksEveryDirectoryItTouches(t *testing.T) {
	for _, c := range []struct {
		n   int
		dir string
	}{{3, "config"}, {4, "etc"}} {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(c.n - 1)
		total, _ := hookCalls(t, base, c.n)
		for _, how := range []string{"mode", "link"} {
			e := base.clone()
			e.crashAt(c.n, total, 1)
			d := e.dir(c.n)
			p := filepath.Join(d, c.dir)
			if how == "mode" {
				if err := os.Chmod(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(p, p+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(c.dir+"-real", p); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshot(t, d)
			rm, _, err := Discard(d, tmpStage(c.n))
			if err == nil || !strings.Contains(err.Error(), c.dir) || len(rm) != 0 {
				t.Fatalf("stage %d, %s: %v %v", c.n, how, rm, err)
			}
			sameTree(t, fmt.Sprintf("stage %d, %s", c.n, how), before, snapshot(t, d))
		}
	}
	// config/wallet-rpc belongs to another user after the bundle's first start: a loose mode is no refusal, a link is.
	base := newEnv(t, fxEveryRail(t))
	base.upTo(1)
	total, _ := hookCalls(t, base, 2)
	e := base.clone()
	e.crashAt(2, total, 1)
	w := filepath.Join(e.hubDir, "config/wallet-rpc")
	if err := os.Chmod(w, 0o755); err != nil {
		t.Fatal(err)
	}
	e2 := e.clone()
	if _, _, err := Discard(e.hubDir, StageHubInit); err != nil {
		t.Fatal(err)
	}
	w2 := filepath.Join(e2.hubDir, "config/wallet-rpc")
	os.Rename(w2, w2+"-real")
	os.Symlink("wallet-rpc-real", w2)
	before := snapshot(t, e2.hubDir)
	if rm, _, err := Discard(e2.hubDir, StageHubInit); err == nil || len(rm) != 0 {
		t.Fatalf("%v %v", rm, err)
	}
	sameTree(t, "wallet-rpc as a link", before, snapshot(t, e2.hubDir))
}

// A file someone made under a journaled name after the run died is not removed by Discard, and the stage then
// refuses, naming it.
func TestDiscardKeepsAFileTheRunDidNotWrite(t *testing.T) {
	for _, c := range []struct {
		n    int
		name string
	}{{3, "config/relay.json"}, {2, "compose.env"}, {4, "compose.env"}, {1, "etc/webhook.secret"}} {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(c.n - 1)
		e := base.clone()
		e.crashAt(c.n, 1, 1) // dead right after the journal is in place: no file exists yet
		d := e.dir(c.n)
		if c.n == 1 { // the directory of the file is the run's own
			os.Mkdir(filepath.Join(d, "etc"), 0o700)
		}
		mine := filepath.Join(d, c.name)
		if err := os.WriteFile(mine, []byte("made by hand"), 0o600); err != nil {
			t.Fatal(err)
		}
		rm, kept, err := Discard(d, tmpStage(c.n))
		if err != nil || len(kept) != 1 || kept[0] != c.name || slices.Contains(rm, c.name) {
			t.Fatalf("%v: %v %v %v", c, rm, kept, err)
		}
		if b, _ := os.ReadFile(mine); string(b) != "made by hand" {
			t.Fatalf("%v: the file was touched", c)
		}
		if exists(filepath.Join(d, journalName)) {
			t.Fatal("the journal is left")
		}
		// The stage cannot run until the file is gone, and says which it is.
		if _, err := e.run(c.n, 2); !errors.Is(err, ErrExists) || !namesOrParent(err.Error(), c.name) {
			t.Fatalf("%v: %v", c, err)
		}
		os.Remove(mine)
		if c.n == 1 {
			os.Remove(filepath.Dir(mine))
		}
		if _, err := e.run(c.n, 2); err != nil {
			t.Fatalf("%v: %v", c, err)
		}
	}
}

// A file the dead run linked but that was changed afterwards is kept; the run's own others are removed.
func TestDiscardKeepsAChangedFile(t *testing.T) {
	base := newEnv(t, fxEveryRail(t))
	base.upTo(2)
	total, _ := hookCalls(t, base, 3)
	e := base.clone()
	e.crashAt(3, total, 1)
	d := e.dir(3)
	f := filepath.Join(d, "config/relay.json")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append(b, []byte(" ")...), 0o600)
	rm, kept, err := Discard(d, StageHubPair)
	if err != nil || len(kept) != 1 || kept[0] != "config/relay.json" || !slices.Contains(rm, "setup/hub-to-public.json") {
		t.Fatalf("%v %v %v", rm, kept, err)
	}
	if exists(filepath.Join(d, "setup/hub-to-public.json")) || !exists(f) {
		t.Fatal("not exactly the run's own files were removed")
	}
}

// The undo of a lost race keeps a file that was changed after the run linked it.
func TestUndoKeepsAChangedFile(t *testing.T) {
	base := newEnv(t, fxEveryRail(t))
	base.upTo(2)
	e := base.clone()
	d := e.dir(3)
	planted := filepath.Join(d, "setup/hub-to-public.json")
	relay := filepath.Join(d, "config/relay.json")
	first := true
	stepHook = func(step int) error {
		switch {
		case step == 5 && first:
			first = false
			return os.WriteFile(planted, []byte("not ours"), 0o600)
		case step == 6:
			f, err := os.OpenFile(relay, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString("edited")
			return err
		}
		return nil
	}
	_, err := e.run(3, 1)
	stepHook = nil
	if !errors.Is(err, ErrExists) {
		t.Fatalf("%v", err)
	}
	if b, _ := os.ReadFile(relay); !strings.HasSuffix(string(b), "edited") {
		t.Fatal("the edited file was removed by the undo")
	}
	if b, _ := os.ReadFile(planted); string(b) != "not ours" {
		t.Fatal("the other program's file was touched")
	}
}

// The command's own scratch names (state.json.tmp, the files in the temporary directory) are removed whatever
// their content, and never reported as kept: a torn write must not block the stage for ever.
func TestDiscardRemovesTornScratchFiles(t *testing.T) {
	for n := 1; n <= 4; n++ {
		base := newEnv(t, fxEveryRail(t))
		base.upTo(n - 1)
		total, _ := hookCalls(t, base, n)
		before := snapshot(t, base.dir(n))
		stage := tmpStage(n)
		finish := func(what string, e *env) {
			t.Helper()
			d := e.dir(n)
			_, kept, err := Discard(d, stage)
			if err != nil || len(kept) != 0 {
				t.Fatalf("stage %d, %s: kept %v, %v", n, what, kept, err)
			}
			sameTree(t, fmt.Sprintf("stage %d, %s", n, what), before, snapshot(t, d))
			if _, err := e.run(n, 2); err != nil {
				t.Fatalf("stage %d, %s: rerun: %v", n, what, err)
			}
		}
		// A. a torn state.json.tmp after a commit died before its rename (the journal is in place).
		e := base.clone()
		e.crashAt(n, 1, 1)
		if err := os.WriteFile(filepath.Join(e.dir(n), stateTmp), []byte(`{"vers`), 0o600); err != nil {
			t.Fatal(err)
		}
		finish("torn state.json.tmp", e)
		// A'. the same left by Discard's own restore, which died before its rename.
		e = base.clone()
		e.crashAt(n, total, 1)
		i := 0
		discardHook = func() error {
			if i++; i == 1 {
				return errors.New("crash")
			}
			return nil
		}
		if _, _, err := Discard(e.dir(n), stage); err == nil {
			t.Fatal("no crash")
		}
		discardHook = nil
		if err := os.WriteFile(filepath.Join(e.dir(n), stateTmp), []byte(`{"vers`), 0o600); err != nil {
			t.Fatal(err)
		}
		finish("torn restore", e)
		// B. a torn file in the temporary directory after a crash at step 5.
		e = base.clone()
		e.crashAt(n, 2, 1)
		tmpFile := filepath.Join(e.dir(n), tmpDirName(stage), "0")
		if !exists(tmpFile) {
			t.Fatalf("stage %d: no temporary file to tear", n)
		}
		if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		finish("torn temporary file", e)
	}
}
