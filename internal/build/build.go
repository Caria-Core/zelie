// Package build turns an app's source code into an image. It runs in the
// core, but the source is untrusted, so everything that reads or runs it
// happens inside containers. The core itself only moves files around and
// imports the finished image.
//
// A build has up to three steps, each a fresh container:
//
//  1. unpack: extract the source archive. No network, nothing else mounted.
//  2. plan: if there is no Dockerfile, Railpack works out how to build the
//     app. It downloads tools, so it gets the network, but no build cache.
//  3. build: BuildKit builds the image, each step in a sandbox of its own.
//     Only this container may run nested containers.
//
// Keeping unpack apart matters: an archive can hold symlinks, and a
// container that extracts one must not have anything worth reaching through
// them.
package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

const (
	// Both images are pinned by digest, like the binaries the engine
	// installs, so a moved tag cannot change what runs.
	BuilderImage     = "docker.io/moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3"
	RailpackFrontend = "ghcr.io/railwayapp/railpack-frontend:v0.40.0@sha256:fc6d5fa434c9310500dc18bebb0a4eb4854fee8546a6d7a090e7a36e39d9d153"

	// Network is where build steps that download things run. It is a
	// network of its own, so builds cannot reach running apps.
	Network = "zelie-build"

	// MaxSource is the largest source archive accepted.
	MaxSource = 512 << 20

	// A build that runs longer than this is stopped.
	timeout = 30 * time.Minute
)

// Engine is what a build needs from the container engine.
type Engine interface {
	Run(ctx context.Context, s engine.Spec) error
	Wait(ctx context.Context, id string) (uint32, error)
	Remove(ctx context.Context, id string) error
	ImportImage(ctx context.Context, r io.Reader, name string) error
}

// Builder runs builds one at a time. The cache is shared between apps, and
// with one build at a time a build never has to wait on another's locks.
type Builder struct {
	Engine Engine
	Paths  engine.Paths
	// Dir holds the build cache and the files of the build in progress.
	Dir string

	MemoryBytes int64
	CPUs        float64

	slot  chan struct{}
	chown func(name string, uid, gid int) error // tests, which are not root, replace it
}

func New(e Engine, paths engine.Paths, dir string) *Builder {
	return &Builder{
		Engine: e, Paths: paths, Dir: dir,
		MemoryBytes: 2 << 30, CPUs: 2,
		slot:  make(chan struct{}, 1),
		chown: os.Chown,
	}
}

// Request is one build.
type Request struct {
	App     string // the app's ID, which names the image
	Version string // names this build of it, such as a commit hash
	// Source is a gzipped tar with the code in a single top-level
	// directory, the way GitHub hands out repository archives.
	Source io.Reader
}

var validVersion = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// ErrSourceTooLarge is returned for a source archive over MaxSource.
var ErrSourceTooLarge = fmt.Errorf("the source is larger than %d MB", MaxSource>>20)

// Build builds the source into an image and returns its name. The output of
// every step is written to out as it happens.
func (b *Builder) Build(ctx context.Context, req Request, out io.Writer) (string, error) {
	if !engine.ValidID(req.App) {
		return "", fmt.Errorf("invalid app id %q", req.App)
	}
	if !validVersion.MatchString(req.Version) {
		return "", fmt.Errorf("invalid version %q", req.Version)
	}

	select {
	case b.slot <- struct{}{}:
	default:
		fmt.Fprintln(out, "Waiting for another build to finish…")
		select {
		case b.slot <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	defer func() { <-b.slot }()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	job, err := b.prepare(req)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(job.root)

	if err := b.step(ctx, out, "unpack", engine.Spec{
		Args: []string{"tar", "-xzof", "/in/source.tar.gz", "--strip-components=1", "-C", "/src"},
		Mounts: []engine.Mount{
			{Source: job.dir("in"), Target: "/in", ReadOnly: true},
			{Source: job.dir("src"), Target: "/src"},
		},
	}); err != nil {
		return "", err
	}

	buildArgs := []string{"buildctl-daemonless.sh", "build", "--progress", "plain",
		"--local", "context=/src", "--output", "type=oci,dest=/out/image.tar"}
	mounts := []engine.Mount{
		{Source: job.dir("src"), Target: "/src", ReadOnly: true},
		{Source: b.cacheDir("buildkit"), Target: "/cache"},
		{Source: job.dir("out"), Target: "/out"},
	}
	// Lstat, not Stat: a Dockerfile that is a symlink could point anywhere
	// on the host, and the core runs as root.
	if st, err := os.Lstat(filepath.Join(job.dir("src"), "Dockerfile")); err == nil && st.Mode().IsRegular() {
		fmt.Fprintln(out, "Building with the Dockerfile.")
		buildArgs = append(buildArgs, "--frontend", "dockerfile.v0", "--local", "dockerfile=/src")
	} else {
		fmt.Fprintln(out, "No Dockerfile; Railpack works out how to build the app.")
		if err := b.step(ctx, out, "plan", engine.Spec{
			Args:    []string{"railpack", "prepare", "/src", "--plan-out", "/plan/railpack-plan.json"},
			Network: Network,
			Mounts: []engine.Mount{
				{Source: job.dir("src"), Target: "/src", ReadOnly: true},
				{Source: job.dir("plan"), Target: "/plan"},
				{Source: b.Paths.Railpack(), Target: "/usr/local/bin/railpack", ReadOnly: true},
				// Railpack keeps the tools it downloads here. Each app
				// has its own, so one app cannot plant a tool another
				// app's plan will run.
				{Source: b.appCache(req.App), Target: "/tmp/railpack"},
			},
		}); err != nil {
			return "", err
		}
		buildArgs = append(buildArgs, "--frontend", "gateway.v0",
			"--opt", "source="+RailpackFrontend,
			// Package manager caches are separate per app for the same
			// reason.
			"--opt", "build-arg:cache-key="+req.App,
			"--local", "dockerfile=/plan")
		mounts = append(mounts, engine.Mount{Source: job.dir("plan"), Target: "/plan", ReadOnly: true})
	}

	if err := b.step(ctx, out, "build", engine.Spec{
		Args:    buildArgs,
		Env:     []string{"BUILDKITD_FLAGS=--root /cache --oci-worker-net=host"},
		Network: Network,
		Mounts:  mounts,
		Nesting: true,
	}); err != nil {
		return "", err
	}

	f, err := os.Open(filepath.Join(job.dir("out"), "image.tar"))
	if err != nil {
		return "", fmt.Errorf("the build left no image: %w", err)
	}
	defer f.Close()
	image := engine.LocalImages + req.App + ":" + req.Version
	if err := b.Engine.ImportImage(ctx, f, image); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "Built %s.\n", image)
	return image, nil
}

type job struct{ root string }

func (j job) dir(name string) string { return filepath.Join(j.root, name) }

// prepare lays out the directories of a build and saves the source archive.
func (b *Builder) prepare(req Request) (job, error) {
	j := job{root: filepath.Join(b.Dir, "jobs", req.App)}
	for _, d := range []string{b.cacheDir("buildkit"), b.appCache(req.App)} {
		if err := b.builderDir(d); err != nil {
			return j, err
		}
	}
	// Left over from a build that was interrupted.
	if err := os.RemoveAll(j.root); err != nil {
		return j, err
	}
	for _, d := range []string{"in", "src", "plan", "out"} {
		if err := b.builderDir(j.dir(d)); err != nil {
			return j, err
		}
	}
	f, err := os.OpenFile(filepath.Join(j.dir("in"), "source.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return j, err
	}
	n, err := io.Copy(f, io.LimitReader(req.Source, MaxSource+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > MaxSource {
		err = ErrSourceTooLarge
	}
	if err != nil {
		os.RemoveAll(j.root)
		return j, err
	}
	return j, nil
}

func (b *Builder) cacheDir(name string) string {
	return filepath.Join(b.Dir, "cache", name)
}

// appCache is where Railpack keeps the tools it downloads for one app.
func (b *Builder) appCache(app string) string {
	return b.cacheDir(filepath.Join("apps", app, "railpack"))
}

// builderDir creates a directory that root in the build containers owns.
func (b *Builder) builderDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return b.chown(dir, engine.BuilderHostID, engine.BuilderHostID)
}

// step runs one build step to completion, copying its output to out.
func (b *Builder) step(ctx context.Context, out io.Writer, name string, s engine.Spec) error {
	s.ID = "zelie-build-" + name
	s.Image = BuilderImage
	s.Builder = true
	s.MemoryBytes, s.CPUs, s.Pids = b.MemoryBytes, b.CPUs, 4096

	// A step left behind by a crash would hold the builder's ID block.
	b.Engine.Remove(ctx, s.ID)
	if err := b.Engine.Run(ctx, s); err != nil {
		return fmt.Errorf("start %s step: %w", name, err)
	}
	defer b.Engine.Remove(context.WithoutCancel(ctx), s.ID)

	done := make(chan struct{})
	copied := make(chan error, 1)
	go func() { copied <- copyLog(engine.LogPathFor(b.Paths, s.ID), out, done) }()
	code, err := b.Engine.Wait(ctx, s.ID)
	close(done)
	if cerr := <-copied; err == nil {
		err = cerr
	}
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("the %s step took too long or was cancelled", name)
	case err != nil:
		return fmt.Errorf("%s step: %w", name, err)
	case code != 0:
		return &StepError{Step: name, Code: code}
	}
	return nil
}

// StepError is a build step that exited with an error. Its output, already
// written to the build log, says why.
type StepError struct {
	Step string
	Code uint32
}

func (e *StepError) Error() string {
	return fmt.Sprintf("the %s step failed with exit code %d", e.Step, e.Code)
}

// copyLog follows a step's log file until done is closed, then copies what
// is left.
func copyLog(path string, out io.Writer, done <-chan struct{}) error {
	var f *os.File
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		finished := false
		select {
		case <-done:
			finished = true
		case <-t.C:
		}
		if f == nil {
			var err error
			f, err = os.Open(path)
			if errors.Is(err, os.ErrNotExist) && !finished {
				continue
			}
			if err != nil {
				return err
			}
			defer f.Close()
		}
		if _, err := io.Copy(out, f); err != nil {
			return err
		}
		if finished {
			return nil
		}
	}
}
