// Package memlock keeps the process's memory out of swap and out of core dumps. SatBooth holds the
// NWC client key, the webhook secret and the API key hashes in memory, and Go copies key material
// onto goroutine stacks that move, so locking a single buffer is not enough: Protect locks the whole
// process, and must run before any secret is loaded.
package memlock

// Status says which protections are in force. On an OS without them the fields are false.
//
// Invariant of Protect, on every OS: every step that fails leaves its field false, and a field is
// true only if its step succeeded. So Status alone says what failed, and a caller may decide on it
// without reading Protect's error (service.Lock does).
type Status struct {
	// Available is true on Linux, the only OS with all three protections. Elsewhere Protect locks
	// nothing (development only).
	Available bool
	// Locked: mlockall(MCL_CURRENT|MCL_FUTURE) succeeded, so no page of the process reaches swap.
	Locked bool
	// NotDumpable: PR_SET_DUMPABLE is 0, so the kernel writes no core dump and ptrace is refused.
	NotDumpable bool
	// CoreZero: RLIMIT_CORE is 0 (soft and hard).
	CoreZero bool
}
