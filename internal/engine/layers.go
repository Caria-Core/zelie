package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
)

// A container's own files are what it wrote outside its volumes: the top layer
// of its image, which containerd keeps as a snapshot of its own. They live on
// the host's disk like a volume does, but no limit applies to them unless
// Zelie measures them, so LayerSizes does.

// LayerSize is how much disk the own files of a running container take.
type LayerSize struct {
	Container string
	App       string
	Bytes     int64
	// Unmeasured says the files could not be measured, and Bytes means
	// nothing. A container whose files cannot be measured is not known to be
	// within any limit.
	Unmeasured bool
}

// LayerSizes measures the own files of the running containers that belong to
// an app. Stopped containers are left alone: nothing writes to them, and
// starting the app again makes a new container with empty files. With nothing
// running it returns at once.
func (e *Engine) LayerSizes(ctx context.Context) ([]LayerSize, error) {
	ids := runningContainers(cgroupRoot)
	if len(ids) == 0 {
		return nil, nil
	}
	ctx = e.ctx(ctx)
	find := func(id string) (app, dir string, err error) { return e.layerDir(ctx, id) }
	return measureLayers(ctx, e.logger(), ids, find)
}

// layerDir returns the app of a container and the directory that holds the
// files it wrote.
func (e *Engine) layerDir(ctx context.Context, id string) (app, dir string, err error) {
	c, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return "", "", err
	}
	info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
	if err != nil {
		return "", "", err
	}
	// Nothing limits the files of a container that belongs to no app, such as
	// a build step, so there is no need to find them.
	if info.Labels[labelApp] == "" {
		return "", "", nil
	}
	if info.SnapshotKey == "" {
		return "", "", fmt.Errorf("container %s has no snapshot", id)
	}
	mounts, err := e.client.SnapshotService(info.Snapshotter).Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return "", "", err
	}
	dir, err = upperDir(e.paths.Root, mounts)
	return info.Labels[labelApp], dir, err
}

// upperDir finds the directory with a container's own files in the mounts of
// its snapshot: the upper directory of an overlay, or the directory of a
// snapshot with no layer below it, which containerd mounts as a bind. It has
// to be inside containerd's root, whatever the mounts say.
func upperDir(root string, mounts []mount.Mount) (string, error) {
	if len(mounts) != 1 {
		return "", fmt.Errorf("the snapshot has %d mounts, want 1", len(mounts))
	}
	var dir string
	switch m := mounts[0]; m.Type {
	case "overlay":
		for _, o := range m.Options {
			if d, ok := strings.CutPrefix(o, "upperdir="); ok {
				dir = d
			}
		}
	case "bind":
		dir = m.Source
	default:
		return "", fmt.Errorf("the snapshot is mounted as %q, which is not understood", m.Type)
	}
	if dir == "" {
		return "", errors.New("the snapshot's mount names no directory for the container's files")
	}
	if rel, err := filepath.Rel(root, dir); err != nil || !filepath.IsAbs(dir) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("the container's files are at %s, outside %s", dir, root)
	}
	return dir, nil
}

// measureLayers adds up the files in the directory find names for each
// container. A container removed meanwhile is left out, and so is one that
// belongs to no app, such as a build step: no limit applies to it, so walking
// it would only cost time.
//
// A container whose files fail to be measured is reported as such and does
// not hold up the others, so a container cannot keep itself, or its
// neighbours, from being measured. Not finding the files at all is a fault of
// the container engine, not of the container, and fails the whole call: no
// app should be stopped for it. So does ctx ending, in the middle of a walk
// too.
func measureLayers(ctx context.Context, log *slog.Logger, ids []string, find func(id string) (app, dir string, err error)) ([]LayerSize, error) {
	var out []LayerSize
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		app, dir, err := find(id)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("find the files of container %s: %w", id, err)
		}
		if app == "" {
			continue
		}
		l := LayerSize{Container: id, App: app}
		if l.Bytes, err = diskUsageCtx(ctx, dir); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Warn("the files of a container cannot be measured", "container", id, "err", err)
			l.Bytes, l.Unmeasured = 0, true
		}
		out = append(out, l)
	}
	return out, nil
}
