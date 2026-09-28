package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Caria-Core/zelie/internal/dataview"
)

// dataRequest asks what a database holds. The panel names the database's
// running container and what to read; the queries are made here.
type dataRequest struct {
	Container string         `json:"container"`
	Engine    string         `json:"engine"`
	Op        string         `json:"op"` // tables, rows, export, keys or key
	Query     dataview.Query `json:"query,omitzero"`
	Pattern   string         `json:"pattern,omitempty"`
	Cursor    string         `json:"cursor,omitempty"`
	Key       string         `json:"key,omitempty"`
}

func (s *Server) readData(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req dataRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	if err := s.containerOf(ctx, app, req.Container); err != nil {
		s.backupFailed(w, "read data", app, err)
		return
	}
	exec := dataview.Exec(s.execIn(req.Container))
	redis := req.Engine == "redis"
	if !redis && !dataview.Supports(req.Engine) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no data viewer for %q", req.Engine))
		return
	}
	var out any
	var err error
	switch {
	case req.Op == "tables" && !redis:
		out, err = dataview.Tables(ctx, exec, req.Engine)
	case req.Op == "rows" && !redis:
		out, err = dataview.Rows(ctx, exec, req.Engine, req.Query)
	case req.Op == "export" && !redis:
		s.exportData(w, r, app, exec, req)
		return
	case req.Op == "keys" && redis:
		out, err = dataview.Keys(ctx, exec, req.Pattern, req.Cursor)
	case req.Op == "key" && redis:
		out, err = dataview.KeyValue(ctx, exec, req.Key)
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("no %q for %s", req.Op, req.Engine))
		return
	}
	if err != nil {
		s.backupFailed(w, "read data", app, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// exportData streams the rows as CSV. A failure half way cuts the
// connection, so a partial file never looks complete.
func (s *Server) exportData(w http.ResponseWriter, r *http.Request, app string, exec dataview.Exec, req dataRequest) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	cw := &startedWriter{w: w}
	err := dataview.Export(r.Context(), exec, req.Engine, req.Query, cw)
	if err == nil {
		return
	}
	if !cw.started {
		s.backupFailed(w, "export data", app, err)
		return
	}
	s.Log.Warn("export cut short", "app", app, "table", req.Query.Table, "err", err)
	panic(http.ErrAbortHandler)
}

type startedWriter struct {
	w       io.Writer
	started bool
}

func (s *startedWriter) Write(b []byte) (int, error) {
	s.started = true
	return s.w.Write(b)
}

// DataTables lists a database's tables and views, with their columns.
func (c *Client) DataTables(ctx context.Context, app, container, engine string) ([]dataview.Table, error) {
	var out []dataview.Table
	err := c.do(ctx, http.MethodPost, "/v1/data/"+url.PathEscape(app), dataRequest{Container: container, Engine: engine, Op: "tables"}, &out)
	return out, err
}

// DataRows reads a page of a table.
func (c *Client) DataRows(ctx context.Context, app, container, engine string, q dataview.Query) (dataview.Page, error) {
	var out dataview.Page
	err := c.do(ctx, http.MethodPost, "/v1/data/"+url.PathEscape(app), dataRequest{Container: container, Engine: engine, Op: "rows", Query: q}, &out)
	return out, err
}

// DataExport writes every row the query finds to w as CSV.
func (c *Client) DataExport(ctx context.Context, app, container, engine string, q dataview.Query, w io.Writer) error {
	return c.do(ctx, http.MethodPost, "/v1/data/"+url.PathEscape(app), dataRequest{Container: container, Engine: engine, Op: "export", Query: q}, w)
}

// DataKeys goes one step through a Redis database's keys.
func (c *Client) DataKeys(ctx context.Context, app, container, pattern, cursor string) (dataview.KeyPage, error) {
	var out dataview.KeyPage
	err := c.do(ctx, http.MethodPost, "/v1/data/"+url.PathEscape(app), dataRequest{Container: container, Engine: "redis", Op: "keys", Pattern: pattern, Cursor: cursor}, &out)
	return out, err
}

// DataKey reads what one Redis key holds.
func (c *Client) DataKey(ctx context.Context, app, container, key string) (dataview.Value, error) {
	var out dataview.Value
	err := c.do(ctx, http.MethodPost, "/v1/data/"+url.PathEscape(app), dataRequest{Container: container, Engine: "redis", Op: "key", Key: key}, &out)
	return out, err
}
