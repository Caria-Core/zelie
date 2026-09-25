package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Caria-Core/zelie/internal/build"
)

// Builder is what the core needs to build images.
type Builder interface {
	Build(ctx context.Context, req build.Request, out io.Writer) (build.Result, error)
}

// A build answers with its output as it happens. The result only exists at
// the end, so it travels in trailers.
const (
	trailerImage = "Zelie-Image"
	trailerTest  = "Zelie-Test-Command"
	trailerError = "Zelie-Error"
)

// build takes the source archive as the request body and streams the
// build's output back.
func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	if s.Builder == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this core cannot build images"))
		return
	}
	q := r.URL.Query()
	req := build.Request{
		App: q.Get("app"), Version: q.Get("version"),
		Source: http.MaxBytesReader(w, r.Body, build.MaxSource+1),
	}
	// The output starts before the whole source has been read.
	http.NewResponseController(w).EnableFullDuplex()
	w.Header().Set("Trailer", trailerImage+", "+trailerTest+", "+trailerError)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	res, err := s.Builder.Build(r.Context(), req, flushWriter{w})
	if err != nil {
		s.Log.Warn("build failed", "app", req.App, "version", req.Version, "err", err)
		w.Header().Set(trailerError, err.Error())
		return
	}
	s.Log.Info("image built", "app", req.App, "image", res.Image)
	w.Header().Set(trailerImage, res.Image)
	w.Header().Set(trailerTest, res.TestCommand)
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	http.NewResponseController(f.w).Flush()
	return n, err
}

// Build sends source, a gzipped tar with one top-level directory, to be
// built for app. The build's output is written to out as it happens.
func (c *Client) Build(ctx context.Context, app, version string, source io.Reader, out io.Writer) (build.Result, error) {
	q := url.Values{"app": {app}, "version": {version}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://core/v1/builds?"+q.Encode(), source)
	if err != nil {
		return build.Result{}, err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return build.Result{}, fmt.Errorf("reach the Zelie core: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return build.Result{}, readError(resp)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return build.Result{}, err
	}
	if msg := resp.Trailer.Get(trailerError); msg != "" {
		return build.Result{}, &Error{Status: http.StatusUnprocessableEntity, Message: msg}
	}
	res := build.Result{Image: resp.Trailer.Get(trailerImage), TestCommand: resp.Trailer.Get(trailerTest)}
	if res.Image == "" {
		return res, errors.New("the build ended without a result")
	}
	return res, nil
}
