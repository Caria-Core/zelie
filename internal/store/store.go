// Package store keeps the panel's state in a single SQLite file.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// Store is the panel's database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and brings its schema up to
// date.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// The panel does little work and one connection means writers never
	// have to wait on each other's locks.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate applies every migration newer than the database's user_version.
func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema %d is newer than this version of Zelie understands", v)
	}
	for i := v; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// nextID hands out an id that is never used twice, even after the row that
// had it is gone. seq and table are fixed names: seq holds the last id
// given, and table the rows that may already have a higher one.
func nextID(ctx context.Context, tx *sql.Tx, seq, table string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "UPDATE "+seq+" SET last = max(last, (SELECT coalesce(max(id), 0) FROM "+table+")) + 1 RETURNING last").Scan(&id)
	return id, err
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
