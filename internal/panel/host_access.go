package panel

import (
	"context"
	"net/http"
	"slices"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Host access lets an app connect to port 3306 of the server itself, where a
// MariaDB installed outside Zelie listens. It is off for every app until an
// administrator turns it on, and only that one port opens.

const (
	auditHostAccessOn  = "host_access_on"
	auditHostAccessOff = "host_access_off"
)

var errHostAccessKind = msg.Define(http.StatusConflict, "hostaccess.not_supported", "Host access is only for web apps and game servers.")

type hostAccessJSON struct {
	On   bool   `json:"on"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

func hostAccessOut(on bool) hostAccessJSON {
	return hostAccessJSON{On: on, Name: engine.HostName, Port: engine.HostPort}
}

// hostAccessApp is the app in the path, if it can have host access.
func (s *Server) hostAccessApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return a, false
	}
	if a.IsDatabase() || a.IsFiles() {
		writeError(w, errHostAccessKind.Err())
		return a, false
	}
	return a, true
}

func (s *Server) getHostAccess(w http.ResponseWriter, r *http.Request) {
	a, ok := s.hostAccessApp(w, r)
	if !ok {
		return
	}
	on, err := s.Store.HostAccess(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "read host access", err)
		return
	}
	writeJSON(w, http.StatusOK, hostAccessOut(on))
}

// setHostAccess turns host access on or off. The store keeps the answer, and
// the core the firewall rules; when the core cannot be told, the store goes
// back to what it was.
func (s *Server) setHostAccess(w http.ResponseWriter, r *http.Request) {
	a, ok := s.hostAccessApp(w, r)
	if !ok {
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.hostAccessMu.Lock()
	defer s.hostAccessMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	was, err := s.Store.HostAccess(ctx, a.ID)
	if err != nil {
		s.fail(w, "read host access", err)
		return
	}
	if err := s.Store.SetHostAccess(ctx, a.ID, req.On); err != nil {
		s.fail(w, "save host access", err)
		return
	}
	if err := s.Core.SetHostAccess(ctx, a.ID, req.On); err != nil {
		// The core can have saved the change before it failed to apply it.
		if err := s.Store.SetHostAccess(ctx, a.ID, was); err != nil {
			s.Log.Error("take back host access", "app", a.ID, "err", err)
		}
		if err := s.Core.SetHostAccess(ctx, a.ID, was); err != nil {
			s.Log.Warn("tell the core host access again", "app", a.ID, "err", err)
		}
		s.coreFailed(w, "set host access", err)
		return
	}
	if req.On != was {
		action := auditHostAccessOff
		if req.On {
			action = auditHostAccessOn
		}
		s.audit(ctx, a.ID, action, "")
	}
	writeJSON(w, http.StatusOK, hostAccessOut(req.On))
}

// syncHostAccess tells the core every app's host access when the panel
// starts, in case the two went apart, such as after restoring the panel's
// database. An app the panel has none for is told so: that closes what an
// older database had open.
func (s *Server) syncHostAccess(ctx context.Context) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.Log.Error("host access: list apps", "err", err)
		return
	}
	on, err := s.Store.HostAccesses(ctx)
	if err != nil {
		s.Log.Error("host access: list", "err", err)
		return
	}
	for _, a := range apps {
		if a.IsDatabase() || a.IsFiles() {
			continue
		}
		if err := s.Core.SetHostAccess(ctx, a.ID, slices.Contains(on, a.ID)); err != nil {
			s.Log.Error("host access: sync", "app", a.ID, "err", err)
		}
	}
}
