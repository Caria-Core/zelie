package memtrim

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// unmapCode drops this process's page table entries for its own executable.
// Start-up runs through most of the binary, and the kernel maps sixteen
// pages around every fault, so an idle process ends up holding the whole
// file in its resident set. The pages stay in the page cache; what the
// process still uses is mapped in again the next time it runs, and all
// three Zelie processes share the same file, so each one only pays for what
// it runs.
func unmapCode() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Only read-only file mappings: text and read-only data. Writable
		// ones hold state that would be lost. Zelie is a static executable
		// that is not position independent, so nothing in these was
		// relocated in memory.
		if len(fields) < 6 || fields[5] != exe || fields[1][1] == 'w' {
			continue
		}
		var start, end uintptr
		if n, _ := fmt.Sscanf(fields[0], "%x-%x", &start, &end); n != 2 || end <= start {
			continue
		}
		syscall.Syscall(syscall.SYS_MADVISE, start, end-start, 4 /* MADV_DONTNEED */)
	}
}
