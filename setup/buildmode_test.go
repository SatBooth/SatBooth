package setup

import (
	"strings"
	"testing"
)

// runMode runs stage n like env.run, on a Lock whose SourceBuild is set as given.
func (e *env) runMode(n int, sourceBuild bool) (*Report, error) {
	l, err := LockDir(e.dir(n))
	if err != nil {
		return nil, err
	}
	defer l.Close()
	l.SourceBuild = sourceBuild
	switch n {
	case 1:
		return l.PublicInit(&e.pub, "http://127.0.0.1:8080", detRand(1))
	case 2:
		h := e.read(e.pubDir, "setup/public-to-hub.json")
		return l.HubInit(&e.hub, h, Digest(h), detRand(1))
	case 3:
		return l.HubPair(e.wallets[0], e.wallets[1])
	}
	h := e.read(e.hubDir, "setup/hub-to-public.json")
	var pass []byte
	if e.pub.MoneroAddress != "" {
		pass = e.read(e.hubDir, "setup/carry/wallet-rpc.pass")
	}
	return l.PublicFinish(h, Digest(h), pass)
}

// The start commands name the pinned release images' Compose files alone, and, for a source build, the host's
// compose.build.yml after compose.yml.
func TestStartCommandsFollowTheBuildMode(t *testing.T) {
	for _, sourceBuild := range []bool{false, true} {
		e := newEnv(t, fxEveryRail(t))
		var next [4]string
		for n := 1; n <= 4; n++ {
			r, err := e.runMode(n, sourceBuild)
			if err != nil {
				t.Fatalf("source build %v, stage %d: %v", sourceBuild, n, err)
			}
			next[n-1] = strings.Join(r.Next, "\n")
		}
		hub, pub := "deploy/hub/compose.yml", "deploy/public/compose.yml"
		hubB, pubB := "deploy/hub/compose.yml -f deploy/hub/compose.build.yml", "deploy/public/compose.yml -f deploy/public/compose.build.yml"
		if !sourceBuild {
			hubB, pubB = hub, pub
		}
		for _, c := range []struct {
			stage int
			want  string
		}{
			{2, "docker compose -f " + hubB + " --env-file " + e.hub.HostDir + "/compose.env up -d --build hub"},
			{2, "docker compose -f " + hubB + " --env-file " + e.hub.HostDir + "/compose.env exec bitcoind"},
			{2, "docker compose -f " + hubB + " --env-file " + e.hub.HostDir + "/compose.env --profile monero run"},
			{3, "docker compose -f " + hubB + " --env-file " + e.hub.HostDir + "/compose.env --profile electrum --profile monero up -d --build"},
			{4, "docker compose -f " + pubB + " --env-file " + e.pub.HostDir + "/compose.env up -d --build"},
		} {
			if !strings.Contains(next[c.stage-1], c.want) {
				t.Errorf("source build %v, stage %d: no %q in\n%s", sourceBuild, c.stage, c.want, next[c.stage-1])
			}
		}
		for i, s := range next[1:] {
			if got := strings.Contains(s, "compose.build.yml"); got != sourceBuild {
				t.Errorf("source build %v, stage %d: compose.build.yml named: %v", sourceBuild, i+2, got)
			}
		}
	}
}

// The command that starts the rest of the hub host names the electrum profile exactly when the answers ask for an
// Electrum server, and the monero profile exactly when the handoff has Monero.
func TestStartCommandNamesTheProfilesOfTheAnswers(t *testing.T) {
	for _, c := range []struct {
		name string
		fx   func(*testing.T) *fixture
		want string
	}{
		{"electrum and Monero", fxEveryRail, "compose.env --profile electrum --profile monero up -d --build"},
		{"neither", fxLightningPruned, "compose.env up -d --build"},
		{"electrum only", fxLightning, "compose.env --profile electrum up -d --build"},
		{"Monero only", fxEveryRailPruned, "compose.env --profile monero up -d --build"},
	} {
		e := newEnv(t, c.fx(t))
		var rep *Report
		for n := 1; n <= 3; n++ {
			r, err := e.run(n, 1)
			if err != nil {
				t.Fatalf("%s, stage %d: %v", c.name, n, err)
			}
			rep = r
		}
		next := strings.Join(rep.Next, "\n")
		if !strings.Contains(next, c.want) {
			t.Errorf("%s: no %q in\n%s", c.name, c.want, next)
		}
		if got := strings.Contains(next, "--profile electrum"); got != strings.Contains(c.want, "--profile electrum") {
			t.Errorf("%s: electrum profile named: %v", c.name, got)
		}
	}
}
