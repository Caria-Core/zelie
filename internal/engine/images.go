package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
)

// labelUnused marks an image nothing needs, with the time it was first
// found so, as Unix seconds.
const labelUnused = "zelie.unused-since"

// Image is an image the engine holds.
type Image struct {
	Name string
	// UnusedSince is when the image was first found unneeded; zero while
	// something needs it.
	UnusedSince time.Time
}

// ImageName is the name an image is stored under: "nginx:alpine" means
// docker.io/library/nginx:alpine, as it does everywhere else. Local images
// keep their name.
func ImageName(ref string) (string, error) {
	if strings.HasPrefix(ref, LocalImages) {
		return ref, nil
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("image %q: %w", ref, err)
	}
	return reference.TagNameOnly(named).String(), nil
}

// Pinned reports whether ref names one exact image, by digest, rather than
// whatever its tag points at.
func Pinned(ref string) bool {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}
	_, ok := named.(reference.Canonical)
	return ok
}

// Pin returns ref with the digest of the image it names here, such as
// postgres:18@sha256:…, and keeps the image under that name too. A tag moves
// when a new build is published; the pinned name lets an app be started
// again, or rolled back, on exactly what it ran before. Local images and
// refs that are already pinned come back as they are.
func (e *Engine) Pin(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, LocalImages) || Pinned(ref) {
		return ref, nil
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("image %q: %w", ref, err)
	}
	named = reference.TagNameOnly(named)
	ctx = e.ctx(ctx)
	is := e.client.ImageService()
	img, err := is.Get(ctx, named.String())
	if err != nil {
		return "", fmt.Errorf("image %s: %w", named, err)
	}
	pinned, err := reference.WithDigest(named, img.Target.Digest)
	if err != nil {
		return "", err
	}
	_, err = is.Create(ctx, images.Image{Name: pinned.String(), Target: img.Target})
	if err != nil && !errdefs.IsAlreadyExists(err) {
		return "", err
	}
	return reference.FamiliarString(pinned), nil
}

// Images lists the images the engine holds.
func (e *Engine) Images(ctx context.Context) ([]Image, error) {
	list, err := e.client.ImageService().List(e.ctx(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(list))
	for _, img := range list {
		i := Image{Name: img.Name}
		if v, ok := img.Labels[labelUnused]; ok {
			var sec int64
			if _, err := fmt.Sscan(v, &sec); err == nil {
				i.UnusedSince = time.Unix(sec, 0)
			}
		}
		out = append(out, i)
	}
	return out, nil
}

// SetUnused records since when an image has been unneeded. The zero time
// clears it.
func (e *Engine) SetUnused(ctx context.Context, name string, since time.Time) error {
	ctx = e.ctx(ctx)
	is := e.client.ImageService()
	img, err := is.Get(ctx, name)
	if err != nil {
		return err
	}
	if img.Labels == nil {
		img.Labels = map[string]string{}
	}
	if since.IsZero() {
		delete(img.Labels, labelUnused)
	} else {
		img.Labels[labelUnused] = fmt.Sprint(since.Unix())
	}
	_, err = is.Update(ctx, img, "labels."+labelUnused)
	return err
}

// RemoveImage deletes an image. Containers already made from it keep
// running: they hold their own snapshot. containerd's garbage collector
// frees the layers no other image uses.
func (e *Engine) RemoveImage(ctx context.Context, name string) error {
	err := e.client.ImageService().Delete(e.ctx(ctx), name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}
