package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Caria-Core/zelie/internal/core"
)

// debugBuild builds a local directory the way a pushed repository is built.
func debugBuild(ctx context.Context, c *core.Client, app, version, dir string, stdout io.Writer) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarGz(dir, pw)) }()
	res, err := c.Build(ctx, app, version, nil, nil, pr, stdout)
	pr.CloseWithError(err)
	if err == nil {
		if res.TestCommand != "" {
			fmt.Fprintf(stdout, "\nTests found: %s\n", res.TestCommand)
		}
		fmt.Fprintf(stdout, "\nRun it with: zelie debug run %s-%s %s\n", app, version, res.Image)
	}
	return err
}

// tarGz writes dir as a gzipped tar with one top-level directory, the shape
// GitHub gives repository archives. Version control data and installed
// dependencies are left out; a pushed repository would not have them.
func tarGz(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(filepath.Join("src", rel))
		if d.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
