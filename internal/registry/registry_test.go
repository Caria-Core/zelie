package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestVersionOf(t *testing.T) {
	for _, c := range []struct {
		repo   string
		config ocispec.ImageConfig
		want   string
	}{
		{"postgres", ocispec.ImageConfig{Env: []string{"PG_MAJOR=18", "PG_VERSION=18.6-1.pgdg13+2"}}, "18.6"},
		{"mariadb", ocispec.ImageConfig{Env: []string{"MARIADB_VERSION=11.8.9"}}, "11.8.9"},
		{"nginx", ocispec.ImageConfig{Env: []string{"PATH=/bin", "NGINX_VERSION=1.27.5"}}, "1.27.5"},
		{"home-assistant", ocispec.ImageConfig{Labels: map[string]string{"org.opencontainers.image.version": "2026.9.4"}, Env: []string{"HOME_ASSISTANT_VERSION=x"}}, "2026.9.4"},
		{"busybox", ocispec.ImageConfig{Env: []string{"PATH=/bin"}}, ""},
	} {
		if got := versionOf(c.config, c.repo); got != c.want {
			t.Errorf("%s: %q, want %q", c.repo, got, c.want)
		}
	}
}

// A registry with one multi-platform image, as Docker Hub serves them.
func TestDigestAndVersion(t *testing.T) {
	blobs := map[digest.Digest][]byte{}
	put := func(v any) ocispec.Descriptor {
		b, _ := json.Marshal(v)
		d := digest.FromBytes(b)
		blobs[d] = b
		return ocispec.Descriptor{Digest: d, Size: int64(len(b))}
	}
	cfg := put(ocispec.Image{Config: ocispec.ImageConfig{Env: []string{"NGINX_VERSION=1.27.5"}}})
	cfg.MediaType = ocispec.MediaTypeImageConfig
	man := put(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: cfg})
	man.MediaType = ocispec.MediaTypeImageManifest
	here := platforms.DefaultSpec()
	man.Platform = &here
	index := put(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{man}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/library/nginx/")
		var d digest.Digest
		var typ string
		switch {
		case p == "manifests/1.27" || p == "manifests/"+index.Digest.String():
			d, typ = index.Digest, ocispec.MediaTypeImageIndex
		case p == "manifests/"+man.Digest.String():
			d, typ = man.Digest, ocispec.MediaTypeImageManifest
		case p == "blobs/"+cfg.Digest.String():
			d, typ = cfg.Digest, "application/octet-stream"
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", typ)
		w.Header().Set("Docker-Content-Digest", d.String())
		w.Write(blobs[d])
	}))
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/library/nginx:1.27"

	d, err := Digest(context.Background(), ref)
	if err != nil || d != index.Digest {
		t.Fatalf("digest %s %v, want %s", d, err, index.Digest)
	}
	if v, err := Version(context.Background(), ref); err != nil || v != "1.27.5" {
		t.Fatalf("version %q %v", v, err)
	}
	if v, err := Version(context.Background(), ref+"@"+index.Digest.String()); err != nil || v != "1.27.5" {
		t.Fatalf("pinned version %q %v", v, err)
	}
}
