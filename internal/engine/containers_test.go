package engine

import "testing"

func TestMountPathsMustBeAbsolute(t *testing.T) {
	base := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1}
	for _, m := range []Mount{
		{Source: "relative", Target: "/src"},
		{Source: "/var/lib/x", Target: "src"},
		{Source: "/var/lib/x", Target: "/src/../etc"},
	} {
		s := base
		s.Mounts = []Mount{m}
		if s.Validate() == nil {
			t.Errorf("mount %+v accepted", m)
		}
	}
	base.Mounts = []Mount{{Source: "/var/lib/x", Target: "/src", ReadOnly: true}}
	if err := base.Validate(); err != nil {
		t.Errorf("valid mount rejected: %v", err)
	}
}

func TestOnlyTheBuilderNests(t *testing.T) {
	s := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1, Nesting: true}
	if s.Validate() == nil {
		t.Error("nesting allowed outside the builder")
	}
}

func TestFilesAreChecked(t *testing.T) {
	base := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1}
	for name, s := range map[string]Spec{
		"relative":       {Files: []File{{Target: "install.sh"}}},
		"under /proc":    {Files: []File{{Target: "/proc/x"}}},
		"too large":      {Files: []File{{Target: "/a", Content: make([]byte, MaxFileSize+1)}}},
		"same twice":     {Files: []File{{Target: "/a"}, {Target: "/a"}}},
		"a volume's own": {Volumes: []VolumeMount{{Name: "data", Target: "/a"}}, Files: []File{{Target: "/a"}}},
	} {
		s.ID, s.Image, s.MemoryBytes, s.CPUs, s.Pids = base.ID, base.Image, base.MemoryBytes, base.CPUs, base.Pids
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	base.Files = []File{{Target: "/mnt/install/install.sh", Content: []byte("echo hi\n")}}
	if err := base.Validate(); err != nil {
		t.Errorf("a valid file was rejected: %v", err)
	}
}
