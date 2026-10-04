package main

import "golang.org/x/sys/unix"

// noHugePages keeps the kernel from backing this process with transparent
// huge pages. Where they are enabled for everything, the kernel folds the
// small, sparse Go heap into 2 MB pages: a core with 3 MB of live heap was
// measured holding 18 MB, and giving memory back cannot shrink it.
func noHugePages() {
	_ = unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0)
}
