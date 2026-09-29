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
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/Caria-Core/zelie/internal/update"
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
	// Code and Params are set for a message meant for the user.
	Code   string
	Params map[string]any
}

func (e *Error) Error() string { return e.Message }

// Msg is the error as a message for the user: the core's own, when it
// wrote one, or else its text as it is.
func (e *Error) Msg() msg.Msg {
	if e.Code != "" {
		return msg.Msg{Code: e.Code, Params: e.Params, Text: e.Message}
	}
	return msg.Other.With("detail", e.Message)
}

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
	_, err := c.RunApp(ctx, s, nil)
	return err
}

// RunApp runs a container of s.App with secret variables sealed for it,
// and variables made from the secrets of apps it is linked to. It returns
// the image that runs, pinned by digest (see engine.Pin).
func (c *Client) RunApp(ctx context.Context, s engine.Spec, sealedEnv []string, linked ...LinkedVar) (string, error) {
	req := runRequest{
		ID: s.ID, App: s.App, Image: s.Image, Args: s.Args, Env: s.Env, SealedEnv: sealedEnv, Network: s.Network,
		MemoryBytes: s.MemoryBytes, CPUs: s.CPUs, Pids: s.Pids,
		WorkDir: s.WorkDir, Stdin: s.Stdin, OpenFiles: s.OpenFiles,
	}
	if s.User != nil {
		req.User = &userJSON{UID: s.User.UID, GID: s.User.GID}
	}
	for _, v := range linked {
		req.LinkedEnv = append(req.LinkedEnv, linkedVarJSON(v))
	}
	for _, v := range s.Volumes {
		req.Volumes = append(req.Volumes, volumeMountJSON{Name: v.Name, Target: v.Target})
	}
	var res runResponse
	err := c.do(ctx, http.MethodPost, "/v1/containers", req, &res)
	return res.Image, err
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
	return engine.Usage{MemoryBytes: out.MemoryBytes, CPUUsec: out.CPUUsec, RxBytes: out.RxBytes, TxBytes: out.TxBytes}, err
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

// SweepImages tells the core which images are still needed. It deletes
// those that have not been for a week and returns their names.
func (c *Client) SweepImages(ctx context.Context, keep []string) ([]string, error) {
	var res sweepResponse
	err := c.do(ctx, http.MethodPost, "/v1/images/sweep", sweepRequest{Keep: keep}, &res)
	return res.Removed, err
}

// Pin returns image pinned to the digest its tag points at here.
func (c *Client) Pin(ctx context.Context, image string) (string, error) {
	var res runResponse
	err := c.do(ctx, http.MethodPost, "/v1/images/pin", pinRequest{Image: image}, &res)
	return res.Image, err
}

// RemoveBuildCache deletes what builds of an app keep between them.
func (c *Client) RemoveBuildCache(ctx context.Context, app string) error {
	return c.do(ctx, http.MethodDelete, "/v1/builds/"+url.PathEscape(app)+"/cache", nil, nil)
}

// SetExternal opens a database's loopback port. With a sealed password it
// also makes the external user, or gives it the new password.
func (c *Client) SetExternal(ctx context.Context, app, engine, container string, port, target int, sealedPassword string) error {
	return c.do(ctx, http.MethodPut, "/v1/external/"+url.PathEscape(app),
		externalRequest{Engine: engine, Container: container, Port: port, Target: target, Password: sealedPassword}, nil)
}

// RemoveExternal closes a database's loopback port and, if container is
// running, drops the external user.
func (c *Client) RemoveExternal(ctx context.Context, app, engine, container string) error {
	return c.do(ctx, http.MethodDelete, "/v1/external/"+url.PathEscape(app), externalRequest{Engine: engine, Container: container}, nil)
}

// SyncExternal makes the core's loopback ports exactly these.
func (c *Client) SyncExternal(ctx context.Context, list []ExternalListener) error {
	if list == nil {
		list = []ExternalListener{}
	}
	return c.do(ctx, http.MethodPut, "/v1/external", externalSync{Listeners: list}, nil)
}

// Version asks the running core which version it is.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/version", nil, &out)
	return out.Version, err
}

// UpdateStatus is the running version and how the last update went.
type UpdateStatus struct {
	Version string         `json:"version"`
	Last    *update.Result `json:"last,omitempty"`
}

func (c *Client) UpdateStatus(ctx context.Context) (UpdateStatus, error) {
	var out UpdateStatus
	err := c.do(ctx, http.MethodGet, "/v1/update", nil, &out)
	return out, err
}

// Update starts an update to version. It returns once the release is
// downloaded and checked; the services restart after.
func (c *Client) Update(ctx context.Context, version string) error {
	return c.do(ctx, http.MethodPost, "/v1/update", updateRequest{Version: version}, nil)
}

// readError turns an error response into an *Error.
func readError(resp *http.Response) error {
	var e errorJSON
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return &Error{Status: resp.StatusCode, Message: e.Error, Code: e.Code, Params: e.Params}
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
