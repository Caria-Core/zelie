package store

import (
	"context"
	"database/sql"
	"time"
)

// Metric is what an app used in one minute.
type Metric struct {
	AppID        string
	At           time.Time
	MemoryBytes  int64
	CPU          float64 // CPUs, on average
	RxBytes      int64
	TxBytes      int64
	Requests     int64
	ClientErrors int64 // 4xx answers
	ServerErrors int64 // 5xx answers
}

// AddMetrics saves a minute's readings. A second reading for the same app
// and minute replaces the first.
func (s *Store) AddMetrics(ctx context.Context, list []Metric) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, m := range list {
			_, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO metrics (app_id, at, memory_bytes, cpu, rx_bytes, tx_bytes, requests, client_errors, server_errors) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
				m.AppID, m.At.Unix(), m.MemoryBytes, m.CPU, m.RxBytes, m.TxBytes, m.Requests, m.ClientErrors, m.ServerErrors)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Metrics returns an app's readings since a time, oldest first.
func (s *Store) Metrics(ctx context.Context, appID string, since time.Time) ([]Metric, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT at, memory_bytes, cpu, rx_bytes, tx_bytes, requests, client_errors, server_errors FROM metrics WHERE app_id = ? AND at >= ? ORDER BY at", appID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Metric
	for rows.Next() {
		m := Metric{AppID: appID}
		var at int64
		if err := rows.Scan(&at, &m.MemoryBytes, &m.CPU, &m.RxBytes, &m.TxBytes, &m.Requests, &m.ClientErrors, &m.ServerErrors); err != nil {
			return nil, err
		}
		m.At = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneMetrics deletes the readings from before a time.
func (s *Store) PruneMetrics(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM metrics WHERE at < ?", before.Unix())
	return err
}

// Recoveries counts the times Zelie brought an app back up after it
// stopped by itself, since a time.
func (s *Store) Recoveries(ctx context.Context, appID string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM deployments WHERE app_id = ? AND cause = ? AND created_at >= ?", appID, CauseRecover, since.Unix()).Scan(&n)
	return n, err
}
