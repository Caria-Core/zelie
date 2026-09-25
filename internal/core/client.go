package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/secret"
)

// DefaultSocket is where the core listens.
const DefaultSocket = "/run/zelie/core.sock"

// Client talks to the core over its socket. The panel and the CLI use it.
type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// Error is a failure reported by the core.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	// The host part is ignored; the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://core"+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the Zelie core: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err := io.Copy(w, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Run(ctx context.Context, s engine.Spec) error {
	return c.RunApp(ctx, s, nil)
}

// RunApp runs a container of s.App with secret variables sealed for it.
func (c *Client) RunApp(ctx context.Context, s engine.Spec, sealedEnv []string) error {
	req := runRequest{
		ID: s.ID, App: s.App, Image: s.Image, Args: s.Args, Env: s.Env, SealedEnv: sealedEnv, Network: s.Network,
		MemoryBytes: s.MemoryBytes, CPUs: s.CPUs, Pids: s.Pids,
	}
	for _, v := range s.Volumes {
		req.Volumes = append(req.Volumes, volumeMountJSON{Name: v.Name, Target: v.Target})
	}
	return c.do(ctx, http.MethodPost, "/v1/containers", req, nil)
}

// SecretKey returns the key to seal secret variables with.
func (c *Client) SecretKey(ctx context.Context) (secret.PublicKey, error) {
	var out struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/secrets/key", nil, &out); err != nil {
		return secret.PublicKey{}, err
	}
	return secret.ParsePublicKey(out.Key)
}

func (c *Client) Stop(ctx context.Context, id string, graceSeconds int) error {
	return c.do(ctx, http.MethodPost, "/v1/containers/"+url.PathEscape(id)+"/stop", stopRequest{GraceSeconds: graceSeconds}, nil)
}

func (c *Client) Remove(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/containers/"+url.PathEscape(id), nil, nil)
}

// Wait waits until the container's process has exited and returns its exit
// code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var out struct {
		ExitCode int `json:"exit_code"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/containers/"+url.PathEscape(id)+"/wait", nil, &out)
	return out.ExitCode, err
}

// Usage reports what a running container uses.
func (c *Client) Usage(ctx context.Context, id string) (engine.Usage, error) {
	var out usageJSON
	err := c.do(ctx, http.MethodGet, "/v1/containers/"+url.PathEscape(id)+"/usage", nil, &out)
	return engine.Usage{MemoryBytes: out.MemoryBytes, CPUUsec: out.CPUUsec}, err
}

// Host describes the server.
func (c *Client) Host(ctx context.Context) (engine.Host, error) {
	var out hostJSON
	err := c.do(ctx, http.MethodGet, "/v1/host", nil, &out)
	return engine.Host{CPUs: out.CPUs, MemoryBytes: out.MemoryBytes, DiskBytes: out.DiskBytes, DiskFreeBytes: out.DiskFreeBytes}, err
}

// RemoveImage deletes an image Zelie built.
func (c *Client) RemoveImage(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/images?"+url.Values{"name": {name}}.Encode(), nil, nil)
}

// readError turns an error response into an *Error.
func readError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return &Error{Status: resp.StatusCode, Message: e.Error}
}

func (c *Client) List(ctx context.Context) ([]engine.Status, error) {
	var list []containerJSON
	if err := c.do(ctx, http.MethodGet, "/v1/containers", nil, &list); err != nil {
		return nil, err
	}
	out := make([]engine.Status, 0, len(list))
	for _, c := range list {
		ip, _ := netip.ParseAddr(c.IP)
		out = append(out, engine.Status{ID: c.ID, App: c.App, Image: c.Image, State: c.State, Pid: c.Pid, Userns: c.Userns, Network: c.Network, IP: ip})
	}
	return out, nil
}

// Logs writes a container's output to w. With tail above zero it starts at
// the first whole line within that many bytes of the end. With follow set it
// keeps streaming new output until ctx is cancelled.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error {
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	}
	if tail > 0 {
		q.Set("tail", strconv.FormatInt(tail, 10))
	}
	path := "/v1/containers/" + url.PathEscape(id) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, w)
}
