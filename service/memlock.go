package service

import (
	"errors"
	"log/slog"

	"github.com/satbooth/satbooth/memlock"
)

// Fixed lines and errors of the memory protection. They carry no key, path or system error text.
const (
	logMemLocked   = "memory locked; core dumps disabled"
	logNoLockOS    = "memory locking unavailable on this OS; development only"
	logSwapAllowed = "memory not locked because allow_swap is true; keys may reach swap"

	errNoLock = "satbooth: cannot lock memory (mlockall): raise LimitMEMLOCK (systemd LimitMEMLOCK=infinity) or grant CAP_IPC_LOCK, or set allow_swap: true if swap is encrypted or absent"
	errNoDump = "satbooth: cannot disable core dumps (PR_SET_DUMPABLE, RLIMIT_CORE)"
)

// Lock runs protect (memlock.Protect in production) and decides whether the service may start. It
// must run before any secret file is opened and before anything is dialled.
//   - Linux: a failure to disable core dumps is fatal; a failure to lock memory is fatal unless
//     cfg.AllowSwap, which logs one warning instead.
//   - Elsewhere: nothing can be locked; one line is logged and the service continues (development only).
func Lock(cfg *Config, log *slog.Logger, protect func() (memlock.Status, error)) error {
	// The error is neither logged nor needed: memlock.Protect reflects every failure in Status (a
	// field is true only if its step succeeded), and the decision below reads Status alone.
	st, _ := protect()
	fail := func(msg string) error {
		log.Error(logStartFailed, "reason", "memlock")
		return errors.New(msg)
	}
	switch {
	case !st.Available:
		log.Warn(logNoLockOS)
	case !st.NotDumpable || !st.CoreZero:
		return fail(errNoDump)
	case !st.Locked && !cfg.AllowSwap:
		return fail(errNoLock)
	case !st.Locked:
		log.Warn(logSwapAllowed)
	default:
		log.Info(logMemLocked)
	}
	return nil
}
