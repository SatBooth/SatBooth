//go:build !linux

package memlock

import (
	"errors"
	"syscall"
)

// Protect locks nothing here (development only): Status.Available is false. It still sets the core
// file size limit to 0 where the OS has one; CoreZero is set only if that succeeded, so its failure
// is reflected in Status too.
func Protect() (Status, error) {
	st := Status{}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		return st, errors.New("memlock: RLIMIT_CORE failed")
	}
	st.CoreZero = true
	return st, nil
}
