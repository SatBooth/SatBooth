package setup

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runWrapper runs deploy/setup.sh with a docker stub first on the PATH that leaves a marker file when it is called,
// and returns the exit code, the standard error and whether docker was reached. The refusals under test happen before
// the wrapper uses Docker, so no engine is needed.
func runWrapper(t *testing.T, cwd string, args ...string) (code int, stderr string, dockerCalled bool) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "deploy", "setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	stubs := t.TempDir()
	marker := filepath.Join(stubs, "called")
	stub := "#!/bin/sh\ntouch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(filepath.Join(stubs, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	_, statErr := os.Stat(marker)
	return code, errb.String(), statErr == nil
}

// The wrapper refuses a symbolic link as <dir>, in every spelling that would hide it, and a <dir> ending in . or ..,
// before it uses Docker, creates anything, or repeats the path.
func TestSetupWrapperRefusesALinkedDirAndDots(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "rel")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ dir, want string }{
		"link":                    {filepath.Join(root, "link"), "symbolic link"},
		"link with a slash":       {filepath.Join(root, "link") + "/", "symbolic link"},
		"link with slashes":       {filepath.Join(root, "link") + "///", "symbolic link"},
		"link with a trailing /.": {filepath.Join(root, "link") + "/.", "symbolic link"},
		"relative link":           {"rel", "symbolic link"},
		"relative link, slash":    {"rel/", "symbolic link"},
		"dot":                     {".", "may not end in"},
		"dot dot":                 {"..", "may not end in"},
		"dir/..":                  {real + "/..", "may not end in"},
		"dir/../":                 {real + "/../", "may not end in"},
		"dot slash":               {"./", "may not end in"},
	} {
		for _, host := range []string{"public", "hub"} {
			code, stderr, docker := runWrapper(t, root, c.dir, host, "init")
			if code != 2 || !strings.Contains(stderr, c.want) {
				t.Errorf("%s (%s): exit %d, stderr %q, want exit 2 and %q", name, host, code, stderr, c.want)
			}
			if docker {
				t.Errorf("%s (%s): the wrapper reached Docker", name, host)
			}
			if strings.Contains(stderr, root) {
				t.Errorf("%s (%s): the message repeats the path: %q", name, host, stderr)
			}
		}
	}
	// Nothing was created.
	if entries, _ := os.ReadDir(root); len(entries) != 3 {
		t.Errorf("the directory holds %d entries, want 3", len(entries))
	}
}

// Control: a real directory is not refused by those checks; the wrapper then reaches Docker (the stub).
func TestSetupWrapperLetsARealDirThroToDocker(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	code, stderr, docker := runWrapper(t, t.TempDir(), real, "public", "init")
	if !docker {
		t.Fatalf("the wrapper did not reach the docker stub after the refusals (exit %d, stderr %q): the refusal tests could pass for an unrelated early exit", code, stderr)
	}
	if strings.Contains(stderr, "symbolic link") || strings.Contains(stderr, "may not end in") {
		t.Fatalf("a real directory was refused: %q", stderr)
	}
	if code == 0 {
		t.Fatalf("the stub docker cannot succeed, exit %d", code)
	}
}
