package engine

import (
	"math"
	"testing"
)

const procLimits = `Limit                     Soft Limit           Hard Limit           Units
Max cpu time              unlimited            unlimited            seconds
Max open files            1024                 524288               files
Max locked memory         8388608              8388608              bytes
`

func TestHardOpenFiles(t *testing.T) {
	if n, ok := hardOpenFiles(procLimits); !ok || n != 524288 {
		t.Errorf("hard limit %d, %v", n, ok)
	}
	unlimited := "Max open files            unlimited            unlimited            files\n"
	if n, ok := hardOpenFiles(unlimited); !ok || n != math.MaxUint64 {
		t.Errorf("unlimited: %d, %v", n, ok)
	}
	for _, bad := range []string{"", "Max cpu time unlimited unlimited seconds\n", "Max open files\n", "Max open files 1 many files\n"} {
		if _, ok := hardOpenFiles(bad); ok {
			t.Errorf("%q was read", bad)
		}
	}
}

func TestClampOpenFiles(t *testing.T) {
	for _, tc := range []struct{ want, ceiling, got uint64 }{
		{MaxOpenFiles, MaxOpenFiles, MaxOpenFiles},
		{MaxOpenFiles, math.MaxUint64, MaxOpenFiles},
		// containerd started with systemd's usual hard limit.
		{MaxOpenFiles, 524288, 524288},
		{65536, MaxOpenFiles, 65536},
		// Nothing to gain over the default.
		{MaxOpenFiles, 1024, 0},
		{MaxOpenFiles, 512, 0},
		{500, MaxOpenFiles, 0},
	} {
		if got := clampOpenFiles(tc.want, tc.ceiling); got != tc.got {
			t.Errorf("want %d under %d: got %d, want %d", tc.want, tc.ceiling, got, tc.got)
		}
	}
}

func TestSpecOpenFiles(t *testing.T) {
	s := Spec{ID: "x", Image: "y", MemoryBytes: 1, CPUs: 1, Pids: 1}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.OpenFiles = MaxOpenFiles
	if err := s.Validate(); err != nil {
		t.Errorf("the most: %v", err)
	}
	s.OpenFiles++
	if err := s.Validate(); err == nil {
		t.Error("more than the most was accepted")
	}
}
