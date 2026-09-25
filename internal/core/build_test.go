package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/secret"
)

type fakeBuilder struct{ got build.Request }

func (f *fakeBuilder) Build(_ context.Context, req build.Request, out io.Writer) (build.Result, error) {
	src, _ := io.ReadAll(req.Source)
	f.got = req
	io.WriteString(out, "step one\n")
	if string(src) == "broken" {
		return build.Result{}, errors.New("the build step failed with exit code 1")
	}
	return build.Result{Image: "zelie.local/" + req.App + ":" + req.Version, TestCommand: "npm test"}, nil
}

// buildClient returns a client talking to s over a real connection, so the
// streamed output and the trailers go through net/http as they would over
// the socket.
func buildClient(t *testing.T, s *Server) *Client {
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return peer.WithPeer(ctx, peer.Peer{UID: 0})
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
	}}}
}

func TestBuild(t *testing.T) {
	fb := &fakeBuilder{}
	s := &Server{Engine: &fakeEngine{}, Builder: fb, Log: slog.New(slog.DiscardHandler)}
	c := buildClient(t, s)

	var out bytes.Buffer
	res, err := c.Build(context.Background(), "web", "abc", []string{"A=1"}, nil, strings.NewReader("source"), &out)
	if err != nil || res.Image != "zelie.local/web:abc" || res.TestCommand != "npm test" {
		t.Fatalf("Build = %+v, %v", res, err)
	}
	if out.String() != "step one\n" || fb.got.App != "web" || fb.got.Version != "abc" {
		t.Errorf("output %q, request %+v", out.String(), fb.got)
	}

	out.Reset()
	_, err = c.Build(context.Background(), "web", "abc", nil, nil, strings.NewReader("broken"), &out)
	var ce *Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "exit code 1") || out.String() != "step one\n" {
		t.Fatalf("failed build: %v, output %q", err, out.String())
	}
}

func TestBuildWithoutBuilder(t *testing.T) {
	s := &Server{Engine: &fakeEngine{}, Log: slog.New(slog.DiscardHandler)}
	_, err := buildClient(t, s).Build(context.Background(), "web", "abc", nil, nil, strings.NewReader(""), io.Discard)
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildOpensSealedVariables(t *testing.T) {
	keys, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBuilder{}
	s := &Server{Engine: &fakeEngine{}, Builder: fb, Secrets: keys, Log: slog.New(slog.DiscardHandler)}
	sealed, _ := secret.Seal(keys.Public(), "web", "NPM_TOKEN", "npm_s3cret")
	if _, err := buildClient(t, s).Build(context.Background(), "web", "abc", []string{"NODE_ENV=production"}, []string{sealed}, strings.NewReader("source"), io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []build.Var{{Name: "NODE_ENV", Value: "production"}, {Name: "NPM_TOKEN", Value: "npm_s3cret", Secret: true}}
	if !slices.Equal(fb.got.Vars, want) {
		t.Errorf("vars %+v", fb.got.Vars)
	}
	// Sealed for another app, it does not open.
	if _, err := buildClient(t, s).Build(context.Background(), "api", "abc", nil, []string{sealed}, strings.NewReader("source"), io.Discard); err == nil {
		t.Error("another app's secret was opened")
	}
}
