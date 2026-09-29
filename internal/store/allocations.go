package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ThisNode is the machine Zelie runs on, the only node until there are
// more.
const ThisNode = 1

// AnyAddress is the ip of an allocation that covers every address of the
// machine.
const AnyAddress = "0.0.0.0"

// ErrInUse means an allocation belongs to a server.
var ErrInUse = errors.New("in use")

// TakenError says which port of a new range clashes with an allocation
// already there. It matches ErrExists.
type TakenError struct{ Port int }

func (e *TakenError) Error() string        { return fmt.Sprintf("port %d is already in the pool", e.Port) }
func (e *TakenError) Is(target error) bool { return target == ErrExists }

// Node is a machine that runs servers.
type Node struct {
	ID   int64
	Name string
	// PublicAddress is what the administrator says players connect to. It
	// is empty when they left it to Zelie to find one.
	PublicAddress string
	CreatedAt     time.Time
}

// Node returns the node, or ErrNotFound.
func (s *Store) Node(ctx context.Context, id int64) (Node, error) {
	var n Node
	var created int64
	err := s.db.QueryRowContext(ctx, "SELECT id, name, public_address, created_at FROM nodes WHERE id = ?", id).Scan(&n.ID, &n.Name, &n.PublicAddress, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	n.CreatedAt = time.Unix(created, 0)
	return n, err
}

// SetPublicAddress sets the address players use for a node's servers. An
// empty one goes back to the detected address.
func (s *Store) SetPublicAddress(ctx context.Context, id int64, address string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE nodes SET public_address = ? WHERE id = ?", address, id)
	return oneRow(res, err)
}

// Allocation is a port a node may give to a server. It covers TCP and UDP.
type Allocation struct {
	ID        int64
	NodeID    int64
	IP        string
	Port      int
	AppID     string // empty when no server has it
	CreatedAt time.Time
}

const allocationColumns = "id, node_id, ip, port, app_id, created_at"

func scanAllocation(row scanner) (Allocation, error) {
	var a Allocation
	var app sql.NullString
	var created int64
	err := row.Scan(&a.ID, &a.NodeID, &a.IP, &a.Port, &app, &created)
	a.AppID, a.CreatedAt = app.String, time.Unix(created, 0)
	return a, err
}

func (s *Store) allocations(ctx context.Context, where string, args ...any) ([]Allocation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+allocationColumns+" FROM allocations WHERE "+where+" ORDER BY port, ip", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Allocation
	for rows.Next() {
		a, err := scanAllocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Allocations lists a node's ports by number.
func (s *Store) Allocations(ctx context.Context, nodeID int64) ([]Allocation, error) {
	return s.allocations(ctx, "node_id = ?", nodeID)
}

// AppAllocations lists the ports a server uses.
func (s *Store) AppAllocations(ctx context.Context, appID string) ([]Allocation, error) {
	return s.allocations(ctx, "app_id = ?", appID)
}

// AddAllocations adds ports on one address of a node, all or none. A port
// that is already in the pool is refused with a *TakenError, and so is one
// that overlaps it: every address covers each specific one.
func (s *Store) AddAllocations(ctx context.Context, nodeID int64, ip string, ports []int, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		have := map[int][]string{}
		rows, err := tx.QueryContext(ctx, "SELECT port, ip FROM allocations WHERE node_id = ?", nodeID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var port int
			var other string
			if err := rows.Scan(&port, &other); err != nil {
				rows.Close()
				return err
			}
			have[port] = append(have[port], other)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, port := range ports {
			for _, other := range have[port] {
				if other == ip || other == AnyAddress || ip == AnyAddress {
					return &TakenError{Port: port}
				}
			}
		}
		for _, port := range ports {
			_, err := tx.ExecContext(ctx, "INSERT INTO allocations (node_id, ip, port, created_at) VALUES (?, ?, ?, ?)", nodeID, ip, port, now.Unix())
			if err != nil {
				// The same port twice in one request.
				if strings.Contains(err.Error(), "UNIQUE constraint failed") {
					return &TakenError{Port: port}
				}
				return err
			}
		}
		return nil
	})
}

// DeleteAllocation removes a port from a node's pool. One a server uses
// stays, with ErrInUse.
func (s *Store) DeleteAllocation(ctx context.Context, nodeID, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM allocations WHERE id = ? AND node_id = ? AND app_id IS NULL", id, nodeID)
	if err := oneRow(res, err); !errors.Is(err, ErrNotFound) {
		return err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM allocations WHERE id = ? AND node_id = ?", id, nodeID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return ErrInUse
}

// AssignAllocations gives the allocations to a server, all or none. One
// that another server took meanwhile fails with ErrInUse.
func (s *Store) AssignAllocations(ctx context.Context, appID string, ids []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, "UPDATE allocations SET app_id = ? WHERE id = ? AND app_id IS NULL", appID, id)
			if err := oneRow(res, err); errors.Is(err, ErrNotFound) {
				var n int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM allocations WHERE id = ?", id).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return ErrInUse
				}
				return ErrNotFound
			} else if err != nil {
				return err
			}
		}
		return nil
	})
}

// UnassignAllocations takes every port back from a server. Deleting the
// server does the same.
func (s *Store) UnassignAllocations(ctx context.Context, appID string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE allocations SET app_id = NULL WHERE app_id = ?", appID)
	return err
}
