package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/update"
)

const releaseCheckEvery = 24 * time.Hour

var errNoRelease = msg.Define(http.StatusNotFound, "update.none", "There is no newer version to update to.")

// Release is a published version of Zelie.
type Release struct {
	Version   string    `json:"version"`
	Notes     string    `json:"notes"`
	Published time.Time `json:"published"`
	URL       string    `json:"url"`
}

// releases remembers the newest release seen.
type releases struct {
	mu      sync.Mutex
	latest  *Release
	checked time.Time
	err     string
}

// GitHubRelease reads the latest release from GitHub.
func GitHubRelease(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+update.Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r struct {
		Tag       string    `json:"tag_name"`
		Body      string    `json:"body"`
		Published time.Time `json:"published_at"`
		URL       string    `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Release{}, err
	}
	if !update.Valid(r.Tag) {
		return Release{}, fmt.Errorf("the latest release is named %q", r.Tag)
	}
	return Release{Version: r.Tag, Notes: r.Body, Published: r.Published, URL: r.URL}, nil
}

func (s *Server) latestRelease(ctx context.Context) (Release, error) {
	if s.Releases != nil {
		return s.Releases(ctx)
	}
	return GitHubRelease(ctx)
}

// checkRelease asks for the newest release and remembers it.
func (s *Server) checkRelease(ctx context.Context) {
	r, err := s.latestRelease(ctx)
	s.releases.mu.Lock()
	defer s.releases.mu.Unlock()
	s.releases.checked = s.now()
	if err != nil {
		s.releases.err = err.Error()
		s.Log.Warn("release check failed", "err", err)
		return
	}
	s.releases.latest, s.releases.err = &r, ""
}

func (s *Server) runReleaseCheck(ctx context.Context) {
	t := time.NewTicker(releaseCheckEvery)
	defer t.Stop()
	for {
		s.checkRelease(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type serverJSON struct {
	Version string `json:"version"`
	// Available is a newer release, if there is one.
	Available *Release       `json:"available,omitempty"`
	CheckedAt time.Time      `json:"checked_at,omitzero"`
	CheckErr  string         `json:"check_error,omitempty"`
	Last      *update.Result `json:"last_update,omitempty"`
	Host      *hostJSON      `json:"host,omitempty"`
	Access    string         `json:"access,omitempty"` // how the panel is reached: acme, self-signed or tunnel
	Address   string         `json:"address,omitempty"`
}

type hostJSON struct {
	CPUs          int   `json:"cpus"`
	MemoryBytes   int64 `json:"memory_bytes"`
	DiskBytes     int64 `json:"disk_bytes"`
	DiskFreeBytes int64 `json:"disk_free_bytes"`
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.Core.UpdateStatus(ctx)
	if err != nil {
		s.coreFailed(w, "update status", err)
		return
	}
	out := serverJSON{Version: st.Version, Last: st.Last}
	s.releases.mu.Lock()
	if l := s.releases.latest; l != nil && update.Newer(l.Version, st.Version) {
		rel := *l
		out.Available = &rel
	}
	out.CheckedAt, out.CheckErr = s.releases.checked, s.releases.err
	s.releases.mu.Unlock()
	if h, err := s.Core.Host(ctx); err == nil {
		out.Host = &hostJSON{h.CPUs, h.MemoryBytes, h.DiskBytes, h.DiskFreeBytes}
	}
	if s.Proxy != nil {
		if cfg, err := s.Proxy.Config(ctx); err == nil {
			out.Access, out.Address = cfg.TLS, cfg.Panel
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) checkReleaseNow(w http.ResponseWriter, r *http.Request) {
	s.checkRelease(r.Context())
	s.serverInfo(w, r)
}

// startUpdate asks the core to update to the newest release. The panel
// restarts during the update; the page asks again until it answers.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.Core.UpdateStatus(ctx)
	if err != nil {
		s.coreFailed(w, "update status", err)
		return
	}
	s.releases.mu.Lock()
	l := s.releases.latest
	s.releases.mu.Unlock()
	if l == nil || !update.Newer(l.Version, st.Version) {
		writeError(w, errNoRelease.Err())
		return
	}
	if err := s.Core.Update(ctx, l.Version); err != nil {
		// The core's own messages, such as a download that failed its
		// signature check, are for the user to read.
		var ce *core.Error
		if errors.As(err, &ce) && ce.Code != "" {
			writeError(w, &msg.Error{Status: ce.Status, Msg: ce.Msg()})
			return
		}
		s.coreFailed(w, "start update", err)
		return
	}
	s.Log.Info("update started", "from", st.Version, "to", l.Version, "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusAccepted)
}
