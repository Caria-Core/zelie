package store

import (
	"context"
	"testing"
	"time"
)

// A deleted app's deployment ids are not handed out again: logs and other
// files kept under an id must never pass to another app.
func TestDeploymentIDsAreNotReused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	newApp := func(id string) {
		if err := s.CreateApp(ctx, App{ID: id, Source: SourceImage, Image: "busybox:1.37", Port: 80, MemoryMB: 128, CPUs: 1, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	deploy := func(app string) int64 {
		id, err := s.CreateDeployment(ctx, Deployment{AppID: app}, now)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	newApp("mc")
	first, second := deploy("mc"), deploy("mc")
	if second <= first {
		t.Fatalf("ids %d then %d", first, second)
	}
	if err := s.DeleteApp(ctx, "mc"); err != nil {
		t.Fatal(err)
	}
	newApp("rust")
	if id := deploy("rust"); id <= second {
		t.Errorf("after deleting mc, rust got id %d; mc's last was %d", id, second)
	}
}
