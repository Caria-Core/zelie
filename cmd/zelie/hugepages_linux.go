package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// noHugePages keeps the kernel from backing this process with transparent
// huge pages. Where they are enabled for everything, the kernel folds the
// small, sparse Go heap into 2 MB pages: a core with 3 MB of live heap was
// measured holding 18 MB, and giving memory back cannot shrink it.
//
// Pages touched before main runs, such as the binary's data and the heap the
// runtime sets up, are already huge by then. The setting survives exec, so the
// process sets it and starts itself again once, clean.
func noHugePages() {
	if v, err := unix.PrctlRetInt(unix.PR_GET_THP_DISABLE, 0, 0, 0, 0); err != nil || v == 1 {
		return
	}
	if unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0) != nil {
		return
	}
	// By its own path rather than /proc/self/exe, which would rename the
	// process to "exe". If exec fails, carry on as is.
	if exe, err := os.Executable(); err == nil {
		_ = unix.Exec(exe, os.Args, os.Environ())
	}
}
