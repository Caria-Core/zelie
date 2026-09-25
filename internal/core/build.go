package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

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
	trailerKind  = "Zelie-Builder"
	trailerBuild = "Zelie-Build-Command"
	trailerStart = "Zelie-Start-Command"
	trailerError = "Zelie-Error"
)

// buildVars is the first part of a build request: the app's variables,
// secret ones sealed for the core like those of a container.
type buildVars struct {
	Env       []string `json:"env,omitempty"`
	SealedEnv []string `json:"sealed_env,omitempty"`
}

// build takes a multipart body, the variables and then the source archive,
// and streams the build's output back. The archive is read as it arrives.
func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	if s.Builder == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this core cannot build images"))
		return
	}
	q := r.URL.Query()
	req := build.Request{App: q.Get("app"), Version: q.Get("version")}
	r.Body = http.MaxBytesReader(w, r.Body, build.MaxSource+maxBodyBytes*8)
	parts, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("a build is a multipart body"))
		return
	}
	part, err := parts.NextPart()
	if err != nil || part.FormName() != "vars" {
		writeError(w, http.StatusBadRequest, errors.New("the variables must come first"))
		return
	}
	var vars buildVars
	dec := json.NewDecoder(io.LimitReader(part, maxBodyBytes*8))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vars); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("variables: %w", err))
		return
	}
	if req.Vars, err = s.openVars(req.App, vars); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	part, err = parts.NextPart()
	if err != nil || part.FormName() != "source" {
		writeError(w, http.StatusBadRequest, errors.New("the source archive is missing"))
		return
	}
	req.Source = part
	// The output starts before the whole source has been read.
	http.NewResponseController(w).EnableFullDuplex()
	w.Header().Set("Trailer", strings.Join([]string{trailerImage, trailerTest, trailerKind, trailerBuild, trailerStart, trailerError}, ", "))
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
	w.Header().Set(trailerKind, res.Builder)
	w.Header().Set(trailerBuild, res.BuildCommand)
	w.Header().Set(trailerStart, res.StartCommand)
}

// openVars turns the request's variables into the build's, opening the
// sealed ones.
func (s *Server) openVars(app string, vars buildVars) ([]build.Var, error) {
	out := make([]build.Var, 0, len(vars.Env)+len(vars.SealedEnv))
	add := func(kv string, secret bool) error {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			return errors.New("a variable must look like NAME=value")
		}
		out = append(out, build.Var{Name: name, Value: value, Secret: secret})
		return nil
	}
	for _, kv := range vars.Env {
		if err := add(kv, false); err != nil {
			return nil, err
		}
	}
	if len(vars.SealedEnv) > 0 && s.Secrets == nil {
		return nil, errors.New("this core has no secret key")
	}
	for _, sealed := range vars.SealedEnv {
		kv, err := s.Secrets.Open(sealed, app)
		if err != nil {
			return nil, err
		}
		if err := add(kv, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	http.NewResponseController(f.w).Flush()
	return n, err
}

// Build sends source, a gzipped tar with one top-level directory, to be
// built for app, with the app's variables: env as NAME=value, sealedEnv
// sealed for the core. The build's output is written to out as it happens.
func (c *Client) Build(ctx context.Context, app, version string, env, sealedEnv []string, source io.Reader, out io.Writer) (build.Result, error) {
	q := url.Values{"app": {app}, "version": {version}}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(func() error {
			part, err := mw.CreateFormField("vars")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(buildVars{Env: env, SealedEnv: sealedEnv}); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("source", "source.tar.gz")
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, source); err != nil {
				return err
			}
			return mw.Close()
		}())
	}()
	defer pr.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://core/v1/builds?"+q.Encode(), pr)
	if err != nil {
		return build.Result{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
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
	t := resp.Trailer
	res := build.Result{
		Image: t.Get(trailerImage), TestCommand: t.Get(trailerTest),
		Builder: t.Get(trailerKind), BuildCommand: t.Get(trailerBuild), StartCommand: t.Get(trailerStart),
	}
	if res.Image == "" {
		return res, errors.New("the build ended without a result")
	}
	return res, nil
}
