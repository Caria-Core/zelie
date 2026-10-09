package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// memImages is an image store that keeps its images in memory.
type memImages struct {
	mu   sync.Mutex
	list map[string]images.Image
}

func (m *memImages) Get(_ context.Context, name string) (images.Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	img, ok := m.list[name]
	if !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	return img, nil
}

func (m *memImages) List(context.Context, ...string) ([]images.Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []images.Image
	for _, img := range m.list {
		out = append(out, img)
	}
	return out, nil
}

func (m *memImages) Create(_ context.Context, img images.Image) (images.Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.list[img.Name]; ok {
		return images.Image{}, errdefs.ErrAlreadyExists
	}
	m.list[img.Name] = img
	return img, nil
}

func (m *memImages) Update(_ context.Context, img images.Image, _ ...string) (images.Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.list[img.Name]; !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	m.list[img.Name] = img
	return img, nil
}

func (m *memImages) Delete(context.Context, string, ...images.DeleteOpt) error { return nil }

// memLabels keeps the labels of content in memory.
type memLabels struct {
	mu     sync.Mutex
	labels map[digest.Digest]map[string]string
}

func (m *memLabels) Get(d digest.Digest) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.labels[d]), nil
}

func (m *memLabels) Set(d digest.Digest, l map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.labels[d] = maps.Clone(l)
	return nil
}

func (m *memLabels) Update(d digest.Digest, l map[string]string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.labels[d] == nil {
		m.labels[d] = map[string]string{}
	}
	for k, v := range l {
		if v == "" {
			delete(m.labels[d], k)
		} else {
			m.labels[d][k] = v
		}
	}
	return maps.Clone(m.labels[d]), nil
}

// ociArchive makes the archive BuildKit's OCI exporter writes: a layout
// with one image, whose index entry carries the given annotations.
func ociArchive(t *testing.T, annotations map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, b []byte) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(b)
	}
	blob := func(mediaType string, b []byte) ocispec.Descriptor {
		sum := sha256.Sum256(b)
		add("blobs/sha256/"+hex.EncodeToString(sum[:]), b)
		return ocispec.Descriptor{MediaType: mediaType, Digest: digest.NewDigestFromBytes(digest.SHA256, sum[:]), Size: int64(len(b))}
	}
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	add("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	config := blob(ocispec.MediaTypeImageConfig, []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`))
	layer := blob(ocispec.MediaTypeImageLayer, []byte("layer"))
	manifest := blob(ocispec.MediaTypeImageManifest, marshal(ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest, Config: config, Layers: []ocispec.Descriptor{layer},
	}))
	manifest.Annotations = annotations
	add("index.json", marshal(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{manifest}}))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportNamesOnlyTheImageItWasGiven(t *testing.T) {
	cs, err := local.NewLabeledStore(t.TempDir(), &memLabels{labels: map[digest.Digest]map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	// The builder is pinned by digest and trusted by name. The archive
	// names it, and an app's image, as if it were those.
	builder := "docker.io/moby/buildkit:v0.33.0@sha256:" + strings.Repeat("6", 64)
	other := LocalImages + "other:abc123abc123"
	trusted := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("trusted"), Size: 7}
	store := &memImages{list: map[string]images.Image{
		builder: {Name: builder, Target: trusted},
		other:   {Name: other, Target: trusted},
	}}
	archive := ociArchive(t, map[string]string{
		images.AnnotationImageName: builder,
		ocispec.AnnotationRefName:  other,
	})

	name := LocalImages + "web:abc123abc123-7"
	if err := importArchive(t.Context(), cs, store, platforms.All, bytes.NewReader(archive), name); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{builder, other} {
		if img, _ := store.Get(t.Context(), n); img.Target.Digest != trusted.Digest {
			t.Errorf("%s was moved to %s", n, img.Target.Digest)
		}
	}
	img, err := store.Get(t.Context(), name)
	if err != nil {
		t.Fatalf("the image was not stored: %v", err)
	}
	if list, _ := store.List(t.Context()); len(list) != 3 {
		t.Errorf("%d images, want the 2 that were there and the new one", len(list))
	}

	// The archive did carry the names, so the check above means something.
	raw, err := content.ReadBlob(t.Context(), cs, img.Target)
	if err != nil {
		t.Fatal(err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(raw, &idx); err != nil || len(idx.Manifests) != 1 || idx.Manifests[0].Annotations[images.AnnotationImageName] != builder {
		t.Fatalf("stored index %s, %v", raw, err)
	}

	// The blobs are held by the image, or containerd would collect them.
	info, err := cs.Info(t.Context(), img.Target.Digest)
	if err != nil {
		t.Fatal(err)
	}
	refs := 0
	for k := range info.Labels {
		if strings.HasPrefix(k, "containerd.io/gc.ref.content") {
			refs++
		}
	}
	if refs == 0 {
		t.Errorf("the index has no references to its manifests: %v", info.Labels)
	}

	// The same name again moves it to the new build.
	if err := importArchive(t.Context(), cs, store, platforms.All, bytes.NewReader(ociArchive(t, nil)), name); err != nil {
		t.Fatalf("again: %v", err)
	}
}

func TestPinnedNameMustMatchItsImage(t *testing.T) {
	good := digest.FromString("good")
	name := "docker.io/moby/buildkit:v0.33.0@" + good.String()
	for _, c := range []struct {
		name string
		got  digest.Digest
		want bool
	}{
		{name, good, true},
		{name, digest.FromString("evil"), false},
		{"docker.io/moby/buildkit:v0.33.0", good, false},
		{"nginx@" + good.String(), good, true},
		{"not a name", good, false},
	} {
		if got := namesDigest(c.name, c.got); got != c.want {
			t.Errorf("namesDigest(%q, %s) = %v", c.name, c.got, got)
		}
	}
}
