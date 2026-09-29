package engine

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// MaxOpenFiles is the most a container may ask for as its limit on open
// files, and what game servers ask for. containerd's own spec leaves
// containers at 1024, which Wine's file-descriptor based synchronisation
// and big Unity or Unreal servers run out of. Docker's default is this
// number too.
const MaxOpenFiles = 1 << 20

// A process cannot raise its hard limit above the one it inherited, and a
// container's limit is set by runc, which inherits from containerd. So the
// limit a container gets is what was asked for, but no more than the hard
// limit containerd runs with (the unit sets it to MaxOpenFiles, and a
// machine that has not restarted containerd since an update still has the
// old one) and no more than the kernel's fs.nr_open.
func (e *Engine) openFiles(want uint64) uint64 {
	if want == 0 {
		return 0
	}
	ceiling, ok := openFilesCeiling(e.paths.Socket)
	if !ok {
		return 0
	}
	return clampOpenFiles(want, ceiling)
}

// defaultOpenFiles is what containerd gives a container without a say.
const defaultOpenFiles = 1024

// clampOpenFiles is want, held under the ceiling. A result that would not
// be more than the default is 0, which leaves the default in place.
func clampOpenFiles(want, ceiling uint64) uint64 {
	n := min(want, ceiling, MaxOpenFiles)
	if n <= defaultOpenFiles {
		return 0
	}
	return n
}

// hardOpenFiles reads the hard limit on open files from the text of a
// process's /proc/<pid>/limits.
func hardOpenFiles(limits string) (uint64, bool) {
	for line := range strings.SplitSeq(limits, "\n") {
		rest, ok := strings.CutPrefix(line, "Max open files")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return 0, false
		}
		if fields[1] == "unlimited" {
			return math.MaxUint64, true
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		return n, err == nil
	}
	return 0, false
}

// withOpenFiles sets the limit on open files, soft and hard.
func withOpenFiles(n uint64) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		for i := range s.Process.Rlimits {
			if s.Process.Rlimits[i].Type == "RLIMIT_NOFILE" {
				s.Process.Rlimits[i].Soft, s.Process.Rlimits[i].Hard = n, n
				return nil
			}
		}
		s.Process.Rlimits = append(s.Process.Rlimits, specs.POSIXRlimit{Type: "RLIMIT_NOFILE", Soft: n, Hard: n})
		return nil
	}
}
