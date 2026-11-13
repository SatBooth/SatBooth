//go:build linux

package memlock

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const childEnv = "SATBOOTH_MEMLOCK_CHILD"

// TestProtectChild is the subprocess body: it runs the real Protect and prints what the kernel says.
func TestProtectChild(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		t.Skip("subprocess body of TestProtect")
	}
	st, _ := Protect()
	fmt.Printf("locked=%t\nnotdumpable=%t\ncorezero=%t\n", st.Locked, st.NotDumpable, st.CoreZero)
	dump, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	fmt.Printf("prctl_dumpable=%d err=%v\n", dump, err)
	var rl syscall.Rlimit
	syscall.Getrlimit(syscall.RLIMIT_CORE, &rl)
	fmt.Printf("rlimit_core=%d/%d\n", rl.Cur, rl.Max)
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "VmLck:") {
			fmt.Println(sc.Text())
		}
	}
}

func TestProtect(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProtectChild$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	t.Logf("child output:\n%s", out)
	kv := map[string]string{}
	vmlck := -1
	for _, l := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(l, "VmLck:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				vmlck, _ = strconv.Atoi(f[0])
			}
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = v
		}
	}
	// Dumps are off whatever the memlock limit is.
	if kv["notdumpable"] != "true" || kv["corezero"] != "true" {
		t.Fatalf("status: %v", kv)
	}
	if !strings.HasPrefix(kv["prctl_dumpable"], "0 ") {
		t.Fatalf("PR_GET_DUMPABLE is not 0: %q", kv["prctl_dumpable"])
	}
	if kv["rlimit_core"] != "0/0" {
		t.Fatalf("RLIMIT_CORE is %q", kv["rlimit_core"])
	}
	if kv["locked"] != "true" {
		// The fail-closed path: mlockall refused (RLIMIT_MEMLOCK too low, no CAP_IPC_LOCK).
		if vmlck > 0 {
			t.Fatalf("not locked, yet VmLck is %d kB", vmlck)
		}
		t.Skip("mlockall refused: RLIMIT_MEMLOCK is too low for this environment (run with --ulimit memlock=-1:-1); dumps were verified off")
	}
	if vmlck <= 0 {
		t.Fatalf("locked, but VmLck is %d kB", vmlck)
	}
}
