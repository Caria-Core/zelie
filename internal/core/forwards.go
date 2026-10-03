package core

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
)

var errPortForwarded = msg.Define(http.StatusConflict, "forward.port_taken", "Port {port} is already open to another server.")

type forwardJSON struct {
	IP     string `json:"ip,omitempty"`
	Port   uint16 `json:"port"`
	Proto  string `json:"proto"`
	Target uint16 `json:"target"`
}

// forwardsRequest is what an app's player ports should be.
type forwardsRequest struct {
	Forwards []forwardJSON `json:"forwards"`
}

// setForwards replaces the host ports open to an app's container. They
// apply as soon as the app has a running container, and again after every
// restart of it.
func (s *Server) setForwards(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req forwardsRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	forwards := make([]engine.Forward, 0, len(req.Forwards))
	for _, f := range req.Forwards {
		var ip netip.Addr
		if f.IP != "" {
			var err error
			if ip, err = netip.ParseAddr(f.IP); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		forwards = append(forwards, engine.Forward{IP: ip, Port: f.Port, Proto: f.Proto, Target: f.Target})
	}
	s.applyForwards(w, r, app, forwards)
}

// clearForwards closes every port open to an app.
func (s *Server) clearForwards(w http.ResponseWriter, r *http.Request) {
	s.applyForwards(w, r, r.PathValue("app"), nil)
}

func (s *Server) applyForwards(w http.ResponseWriter, r *http.Request, app string, forwards []engine.Forward) {
	if err := engine.CheckForwards(app, forwards); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	unlock, ok := s.guardForwards(w, forwards)
	if !ok {
		return
	}
	defer unlock()
	// Once started it runs to the end: half of it would leave the
	// firewall out of step with what was saved.
	err := s.Engine.SetForwards(context.WithoutCancel(r.Context()), app, forwards)
	var taken *engine.PortForwardedError
	if errors.As(err, &taken) {
		writeError(w, http.StatusConflict, errPortForwarded.Err("port", taken.Port))
		return
	}
	if err != nil {
		s.fail(w, "set forwards", app, err)
		return
	}
	s.Log.Info("forwards set", "app", app, "forwards", len(forwards))
	w.WriteHeader(http.StatusNoContent)
}

// SetForwards replaces the host ports open to an app's container: host
// port, protocol and the container's port. Traffic from outside the server
// goes straight to the container, not through the web proxy.
func (c *Client) SetForwards(ctx context.Context, app string, forwards []engine.Forward) error {
	body := forwardsRequest{Forwards: make([]forwardJSON, 0, len(forwards))}
	for _, f := range forwards {
		j := forwardJSON{Port: f.Port, Proto: f.Proto, Target: f.Target}
		if f.IP.IsValid() {
			j.IP = f.IP.String()
		}
		body.Forwards = append(body.Forwards, j)
	}
	return c.do(ctx, http.MethodPut, "/v1/forwards/"+url.PathEscape(app), body, nil)
}

// ClearForwards closes every port open to an app.
func (c *Client) ClearForwards(ctx context.Context, app string) error {
	return c.do(ctx, http.MethodDelete, "/v1/forwards/"+url.PathEscape(app), nil, nil)
}
