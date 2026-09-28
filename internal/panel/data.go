package panel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/dataview"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// The data viewer only reads, and the core makes every query. The panel
// finds the database's running container and passes the request on.

var (
	errDataNotDB   = msg.Define(http.StatusConflict, "data.not_database", "Only a database has data to look at.")
	errNoExport    = msg.Define(http.StatusNotFound, "data.export_gone", "This download has expired. Export again.")
	errRedisTables = msg.Define(http.StatusBadRequest, "data.redis_tables", "A Redis database has keys, not tables.")
	errNotRedis    = msg.Define(http.StatusBadRequest, "data.not_redis", "Only a Redis database has keys.")
)

// exportTTL is how long an export confirmed on the page waits for its
// download to start.
const exportTTL = time.Minute

type pendingExport struct {
	app     string
	query   dataview.Query
	account int64
	until   time.Time
}

// exports are downloads confirmed but not started, by one-time token.
type exports struct {
	mu sync.Mutex
	m  map[string]pendingExport
}

func (e *exports) add(p pendingExport, now time.Time) string {
	var b [16]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[string]pendingExport{}
	}
	for t, old := range e.m {
		if now.After(old.until) {
			delete(e.m, t)
		}
	}
	e.m[token] = p
	return token
}

// take hands out an export once.
func (e *exports) take(token string, now time.Time) (pendingExport, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.m[token]
	delete(e.m, token)
	return p, ok && now.Before(p.until)
}

// dataTarget finds the database in the path and its running container.
func (s *Server) dataTarget(w http.ResponseWriter, r *http.Request) (store.App, string, bool) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return a, "", false
	}
	if !a.IsDatabase() {
		writeError(w, errDataNotDB.Err())
		return a, "", false
	}
	container, err := s.liveContainer(r.Context(), a)
	var me *msg.Error
	if errors.As(err, &me) {
		writeError(w, me)
		return a, "", false
	}
	if err != nil {
		s.fail(w, "find database", err)
		return a, "", false
	}
	return a, container, true
}

func (s *Server) dataTables(w http.ResponseWriter, r *http.Request) {
	a, container, ok := s.dataTarget(w, r)
	if !ok {
		return
	}
	if a.Engine == "redis" {
		writeError(w, errRedisTables.Err())
		return
	}
	out, err := s.Core.DataTables(r.Context(), a.ID, container, a.Engine)
	if err != nil {
		s.coreFailed(w, "read tables", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dataRows(w http.ResponseWriter, r *http.Request) {
	a, container, ok := s.dataTarget(w, r)
	if !ok {
		return
	}
	var q dataview.Query
	if !decode(w, r, &q) {
		return
	}
	out, err := s.Core.DataRows(r.Context(), a.ID, container, a.Engine, q)
	if err != nil {
		s.coreFailed(w, "read rows", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// startExport takes the confirmation for a download of a whole table and
// returns a link to it. The download itself is a plain request, so a large
// file goes straight to disk rather than through the page.
func (s *Server) startExport(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.dataTarget(w, r)
	if !ok {
		return
	}
	if a.Engine == "redis" {
		writeError(w, errRedisTables.Err())
		return
	}
	var q dataview.Query
	if !decode(w, r, &q) {
		return
	}
	token := s.exports.add(pendingExport{app: a.ID, query: q, account: loginFrom(r.Context()).account.ID, until: s.now().Add(exportTTL)}, s.now())
	writeJSON(w, http.StatusOK, map[string]string{"url": "/api/apps/" + a.ID + "/data/export/" + token})
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (s *Server) downloadExport(w http.ResponseWriter, r *http.Request) {
	a, container, ok := s.dataTarget(w, r)
	if !ok {
		return
	}
	p, ok := s.exports.take(r.PathValue("token"), s.now())
	if !ok || p.app != a.ID || p.account != loginFrom(r.Context()).account.ID {
		writeError(w, errNoExport.Err())
		return
	}
	name := unsafeName.ReplaceAllString(a.ID+"-"+p.query.Table, "_") + "-" + s.now().Format("2006-01-02") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	s.Log.Info("data exported", "database", a.ID, "schema", p.query.Schema, "table", p.query.Table, "user", p.account)
	if err := s.Core.DataExport(r.Context(), a.ID, container, a.Engine, p.query, w); err != nil {
		// The file has started; cutting the connection keeps a partial
		// one from looking whole.
		s.Log.Error("export", "database", a.ID, "table", p.query.Table, "err", err)
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) dataKeys(w http.ResponseWriter, r *http.Request) {
	a, container, ok := s.dataTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Pattern string `json:"pattern"`
		Cursor  string `json:"cursor"`
	}
	if !decode(w, r, &req) {
		return
	}
	if a.Engine != "redis" {
		writeError(w, errNotRedis.Err())
		return
	}
	out, err := s.Core.DataKeys(r.Context(), a.ID, container, req.Pattern, req.Cursor)
	if err != nil {
		s.coreFailed(w, "read keys", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dataKey(w http.ResponseWriter, r *http.Request) {
	a, container, ok := s.dataTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if a.Engine != "redis" {
		writeError(w, errNotRedis.Err())
		return
	}
	out, err := s.Core.DataKey(r.Context(), a.ID, container, req.ID)
	if err != nil {
		s.coreFailed(w, "read key", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
