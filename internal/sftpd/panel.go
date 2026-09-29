package sftpd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"golang.org/x/crypto/ssh"
)

// DefaultPanelSocket is where the panel listens.
const DefaultPanelSocket = "/run/zelie-panel/panel.sock"

// ErrDenied is what a login that is not allowed comes back with. It says
// nothing about why, so it cannot be used to find out who has an account.
var ErrDenied = errors.New("login refused")

// Grant is what the panel lets a login use: one server's files.
type Grant struct {
	Server string `json:"server"`
	Volume string `json:"volume"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	// Account is who logged in, for the log. It is 0 for a password, which
	// belongs to the server and not to a person.
	Account int64 `json:"account,omitempty"`
}

// Panel is what the SFTP service asks of the panel. The panel decides who
// may log in; this process only holds the connection.
type Panel interface {
	// Password checks a server's SFTP password.
	Password(ctx context.Context, server, password, ip string) (Grant, error)
	// Key checks that the key belongs to an account that may use the
	// server. The caller has not seen the client sign anything yet.
	Key(ctx context.Context, server string, key ssh.PublicKey, ip string) (Grant, error)
	// Room is how many more bytes the server's disk limit leaves, or nil
	// when that is not known.
	Room(ctx context.Context, server string) (*int64, error)
}

// The requests and answers of the panel's SFTP routes, which only this
// process may call.
type (
	AuthRequest struct {
		Server   string `json:"server"`
		Password string `json:"password,omitempty"`
		Key      []byte `json:"key,omitempty"` // SSH wire format
		IP       string `json:"ip"`
	}
	RoomRequest struct {
		Server string `json:"server"`
	}
	RoomResponse struct {
		Room *int64 `json:"room,omitempty"`
	}
)

// PanelClient is Panel over the panel's Unix socket.
type PanelClient struct{ http *http.Client }

func NewPanelClient(socket string) *PanelClient {
	return &PanelClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *PanelClient) post(ctx context.Context, path string, in, out any) (int, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://panel"+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reach the Zelie panel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// login sends an AuthRequest. The panel answers 403 for every refusal.
func (c *PanelClient) login(ctx context.Context, req AuthRequest) (Grant, error) {
	var g Grant
	status, err := c.post(ctx, "/local/sftp/auth", req, &g)
	switch {
	case err != nil:
		return g, err
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return g, ErrDenied
	case status != http.StatusOK:
		return g, fmt.Errorf("the panel answered %d", status)
	}
	return g, nil
}

func (c *PanelClient) Password(ctx context.Context, server, password, ip string) (Grant, error) {
	return c.login(ctx, AuthRequest{Server: server, Password: password, IP: ip})
}

func (c *PanelClient) Key(ctx context.Context, server string, key ssh.PublicKey, ip string) (Grant, error) {
	return c.login(ctx, AuthRequest{Server: server, Key: key.Marshal(), IP: ip})
}

func (c *PanelClient) Room(ctx context.Context, server string) (*int64, error) {
	var out RoomResponse
	status, err := c.post(ctx, "/local/sftp/room", RoomRequest{Server: server}, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the panel answered %d", status)
	}
	return out.Room, nil
}
