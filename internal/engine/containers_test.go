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
