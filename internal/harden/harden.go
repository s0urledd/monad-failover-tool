// Package harden narrows how far a secret held by this process can travel:
// no core dump can carry it, the process is not dumpable by anyone but
// root, and (as root) its memory is never paged out to swap.
package harden

import "syscall"

// Apply sets the process limits. Every step is best effort: a failure
// leaves the run no less safe than before, so errors are ignored.
func Apply(euid int) {
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0)
	if euid == 0 {
		// MCL_FUTURE pins every later mapping too. Only as root: an
		// unprivileged process could hit RLIMIT_MEMLOCK on a later
		// allocation and fail there instead of here.
		_ = syscall.Mlockall(syscall.MCL_CURRENT | syscall.MCL_FUTURE)
	}
}

// Dumpable reports the process's dumpable flag, for tests.
func Dumpable() int {
	r, _, _ := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	return int(r)
}
