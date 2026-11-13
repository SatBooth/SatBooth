//go:build linux

package memlock

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// Protect locks all current and future memory (mlockall), marks the process not dumpable and sets the
// core file size limit to 0. It tries all three whatever the others do, and reports which succeeded:
// each field of Status is set only after its step succeeded, so every failure is reflected in Status.
// The error is non-nil if any step failed (it adds nothing that Status does not say); it names no
// value of the process.
func Protect() (Status, error) {
	st := Status{Available: true}
	var errs []error
	// Dumps first: they must be off even if locking fails.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		errs = append(errs, errors.New("memlock: PR_SET_DUMPABLE failed"))
	} else {
		st.NotDumpable = true
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		errs = append(errs, errors.New("memlock: RLIMIT_CORE failed"))
	} else {
		st.CoreZero = true
	}
	if err := unix.Mlockall(unix.MCL_CURRENT | unix.MCL_FUTURE); err != nil {
		errs = append(errs, errors.New("memlock: mlockall failed"))
	} else {
		st.Locked = true
	}
	return st, errors.Join(errs...)
}
