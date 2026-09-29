// Package registry asks image registries what a tag points at, without
// downloading the image.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Largest manifest or image config read; real ones are a few kilobytes.
const maxBlob = 4 << 20

// Digest returns the digest ref's tag points at now. Registries answer it
// with a HEAD request, which Docker Hub does not count as a pull.
func Digest(ctx context.Context, ref string) (digest.Digest, error) {
	name, err := normalize(ref)
	if err != nil {
		return "", err
	}
	_, desc, err := docker.NewResolver(docker.ResolverOptions{}).Resolve(ctx, name)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	return desc.Digest, nil
}

// Version reads the version an image says it is, for this machine's
// platform: the org.opencontainers.image.version label, or a variable
// such as PG_VERSION or NGINX_VERSION. It is empty when the image says
// nothing. ref may be pinned to a digest.
func Version(ctx context.Context, ref string) (string, error) {
	name, err := normalize(ref)
	if err != nil {
		return "", err
	}
	r := docker.NewResolver(docker.ResolverOptions{})
	resolved, desc, err := r.Resolve(ctx, name)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	f, err := r.Fetcher(ctx, resolved)
	if err != nil {
		return "", err
	}
	p := provider{f}
	cdesc, err := images.Config(ctx, p, desc, platforms.Default())
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	b, err := content.ReadBlob(ctx, p, cdesc)
	if err != nil {
		return "", err
	}
	var img ocispec.Image
	if err := json.Unmarshal(b, &img); err != nil {
		return "", fmt.Errorf("%s config: %w", name, err)
	}
	named, _ := reference.ParseNormalizedNamed(name)
	return versionOf(img.Config, path.Base(reference.Path(named))), nil
}

// versionOf finds the version in an image's config. Official images set
// a variable named after the image; postgres calls its own PG_VERSION.
func versionOf(c ocispec.ImageConfig, repo string) string {
	if v := c.Labels["org.opencontainers.image.version"]; v != "" {
		return v
	}
	names := []string{strings.ToUpper(strings.ReplaceAll(repo, "-", "_")) + "_VERSION"}
	if repo == "postgres" {
		names = append(names, "PG_VERSION")
	}
	for _, n := range names {
		for _, e := range c.Env {
			if k, v, ok := strings.Cut(e, "="); ok && k == n && v != "" {
				if n == "PG_VERSION" {
					// 18.6-1.pgdg13+2 is Debian's package version.
					v, _, _ = strings.Cut(v, "-")
				}
				return v
			}
		}
	}
	return ""
}

func normalize(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("image %q: %w", ref, err)
	}
	return reference.TagNameOnly(named).String(), nil
}

// provider reads blobs straight from the registry, into memory.
type provider struct{ f remotes.Fetcher }

func (p provider) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	if desc.Size > maxBlob {
		return nil, fmt.Errorf("%s is %d bytes, more than a manifest or config should be", desc.Digest, desc.Size)
	}
	rc, err := p.f.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxBlob+1))
	if err != nil {
		return nil, err
	}
	if desc.Digest != "" && digest.FromBytes(b) != desc.Digest {
		return nil, fmt.Errorf("%s: content does not match its digest", desc.Digest)
	}
	return readerAt{bytes.NewReader(b)}, nil
}

type readerAt struct{ *bytes.Reader }

func (readerAt) Close() error { return nil }
