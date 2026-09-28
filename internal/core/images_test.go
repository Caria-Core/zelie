package core

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
)

func TestSweepImages(t *testing.T) {
	s, f := newServer()
	panel := &peer.Peer{UID: 999}
	long := time.Now().Add(-8 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	builder, _ := engine.ImageName(build.BuilderImage)
	f.images = []engine.Image{
		{Name: "docker.io/library/postgres:18", UnusedSince: long}, // the panel needs it again
		{Name: "docker.io/library/nginx:alpine"},                   // newly unneeded
		{Name: "docker.io/library/redis:7", UnusedSince: recent},   // not long enough
		{Name: "docker.io/library/mariadb:10.11", UnusedSince: long},
		{Name: "zelie.local/gone:abc", UnusedSince: long},
		{Name: "docker.io/library/busybox:latest", UnusedSince: long}, // a stopped container's
		{Name: builder, UnusedSince: long},
	}
	f.containers = []engine.Status{{ID: "old", Image: "docker.io/library/busybox:latest", State: "stopped"}}

	rec := request(t, s, panel, "POST", "/v1/images/sweep", `{"keep":["postgres:18"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sweep: %d %s", rec.Code, rec.Body)
	}
	var res sweepResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	slices.Sort(res.Removed)
	if !slices.Equal(res.Removed, []string{"docker.io/library/mariadb:10.11", "zelie.local/gone:abc"}) || res.Unused != 2 {
		t.Errorf("result %+v", res)
	}
	left := map[string]time.Time{}
	for _, img := range f.images {
		left[img.Name] = img.UnusedSince
	}
	if !left["docker.io/library/postgres:18"].IsZero() {
		t.Error("an image needed again is still marked unused")
	}
	if left["docker.io/library/nginx:alpine"].IsZero() {
		t.Error("a newly unneeded image is not marked")
	}
	if _, ok := left["docker.io/library/busybox:latest"]; !ok {
		t.Error("a stopped container's image was removed")
	}
	if _, ok := left[builder]; !ok {
		t.Error("the builder's image was removed")
	}

	if rec := request(t, s, panel, "POST", "/v1/images/sweep", `{"keep":["Not A Name"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", rec.Code)
	}
}
