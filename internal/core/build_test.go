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
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/peer"
)

type fakeBuilder struct{ got build.Request }

func (f *fakeBuilder) Build(_ context.Context, req build.Request, out io.Writer) (string, error) {
	src, _ := io.ReadAll(req.Source)
	f.got = req
	io.WriteString(out, "step one\n")
	if string(src) == "broken" {
		return "", errors.New("the build step failed with exit code 1")
	}
	return "zelie.local/" + req.App + ":" + req.Version, nil
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
	image, err := c.Build(context.Background(), "web", "abc", strings.NewReader("source"), &out)
	if err != nil || image != "zelie.local/web:abc" {
		t.Fatalf("Build = %q, %v", image, err)
	}
	if out.String() != "step one\n" || fb.got.App != "web" || fb.got.Version != "abc" {
		t.Errorf("output %q, request %+v", out.String(), fb.got)
	}

	out.Reset()
	_, err = c.Build(context.Background(), "web", "abc", strings.NewReader("broken"), &out)
	var ce *Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "exit code 1") || out.String() != "step one\n" {
		t.Fatalf("failed build: %v, output %q", err, out.String())
	}
}

func TestBuildWithoutBuilder(t *testing.T) {
	s := &Server{Engine: &fakeEngine{}, Log: slog.New(slog.DiscardHandler)}
	_, err := buildClient(t, s).Build(context.Background(), "web", "abc", strings.NewReader(""), io.Discard)
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
}
