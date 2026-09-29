package panel

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Game servers get their ports from a pool of allocations, as in
// Pterodactyl and Pelican: the administrator says which ports of which
// addresses may be used, and each server takes some of them.

const (
	// The Minecraft port, where a suggested range starts.
	suggestFrom = 25565
	// Suggestions stop before the range Linux picks outgoing ports from
	// (32768 and up by default), which a game server would share with the
	// connections the machine makes itself.
	suggestUntil = 32767
	// suggestSize is how many ports a suggested block has.
	suggestSize = 100

	// Ports below this need a privileged process; the core refuses them
	// too.
	lowestAllocation = 1024
	maxAddedAtOnce   = 5000
	highestPort      = 65535
)

var (
	errNoNode          = msg.Define(http.StatusNotFound, "allocation.no_node", "There is no such node.")
	errNoAllocation    = msg.Define(http.StatusNotFound, "allocation.not_found", "There is no such port in the pool.")
	errBadPorts        = msg.Define(http.StatusBadRequest, "allocation.bad_ports", "Give ports from 1024 to 65535, alone or as ranges, such as 25565-25600,27015.")
	errBadAddress      = msg.Define(http.StatusBadRequest, "allocation.bad_ip", "{ip} is not an IPv4 address.")
	errTooManyPorts    = msg.Define(http.StatusBadRequest, "allocation.too_many", "Add at most {max} ports at a time.")
	errPortInPool      = msg.Define(http.StatusConflict, "allocation.taken", "Port {port} is already in the pool.")
	errAllocationInUse = msg.Define(http.StatusConflict, "allocation.in_use", "A server uses this port. Delete the server first.")
	errNoRange         = msg.Define(http.StatusConflict, "allocation.no_range", "No free block of {count} ports was found on this server.")
	errNoFreePort      = msg.Define(http.StatusConflict, "allocation.none_free", "There are no free ports left. Add more to the pool first.")
	errAdminOnly       = msg.Define(http.StatusForbidden, "session.admin_only", "Only an administrator can do this.")
)

// adminOnly is signedIn for administrators.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.signedIn(requireAdmin(next))
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !loginFrom(r.Context()).account.Admin {
			writeError(w, errAdminOnly.Err())
			return
		}
		next(w, r)
	}
}

// nodeFrom is the node in the path.
func (s *Server) nodeFrom(w http.ResponseWriter, r *http.Request) (store.Node, bool) {
	id, err := strconv.ParseInt(r.PathValue("node"), 10, 64)
	if err != nil {
		writeError(w, errNoNode.Err())
		return store.Node{}, false
	}
	n, err := s.Store.Node(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoNode.Err())
		return n, false
	case err != nil:
		s.fail(w, "load node", err)
		return n, false
	}
	return n, true
}

type allocationJSON struct {
	ID   int64  `json:"id"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	App  string `json:"app,omitempty"`
}

func (s *Server) listAllocations(w http.ResponseWriter, r *http.Request) {
	n, ok := s.nodeFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Store.Allocations(r.Context(), n.ID)
	if err != nil {
		s.fail(w, "list allocations", err)
		return
	}
	out := make([]allocationJSON, 0, len(list))
	for _, a := range list {
		out = append(out, allocationJSON{ID: a.ID, IP: a.IP, Port: a.Port, App: a.AppID})
	}
	writeJSON(w, http.StatusOK, out)
}

type rangeJSON struct {
	IP    string `json:"ip"`
	Ports string `json:"ports"` // as the add request takes it
	First int    `json:"first"`
	Last  int    `json:"last"`
}

// suggestAllocations proposes a block of ports for the pool: the
// administrator confirms or changes it. Ports other software on the server
// holds, such as another panel's daemon, are left out.
func (s *Server) suggestAllocations(w http.ResponseWriter, r *http.Request) {
	n, ok := s.nodeFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	used, err := s.Core.UsedPorts(ctx)
	if err != nil {
		s.coreFailed(w, "list used ports", err)
		return
	}
	pool, err := s.Store.Allocations(ctx, n.ID)
	if err != nil {
		s.fail(w, "list allocations", err)
		return
	}
	first, ok := suggestRange(used, pool, suggestSize)
	if !ok {
		writeError(w, errNoRange.Err("count", suggestSize))
		return
	}
	last := first + suggestSize - 1
	writeJSON(w, http.StatusOK, rangeJSON{
		IP: store.AnyAddress, Ports: strconv.Itoa(first) + "-" + strconv.Itoa(last), First: first, Last: last,
	})
}

// suggestRange finds the first block of size ports, starting at the
// Minecraft port, that nothing uses and the pool does not have yet. A block
// that runs into a port in use starts again just after it.
func suggestRange(used []int, pool []store.Allocation, size int) (first int, ok bool) {
	taken := make(map[int]bool, len(used)+len(pool))
	for _, p := range used {
		taken[p] = true
	}
	for _, a := range pool {
		taken[a.Port] = true
	}
	for start := suggestFrom; start+size-1 <= suggestUntil; {
		clash := -1
		for p := start + size - 1; p >= start; p-- {
			if taken[p] {
				clash = p
				break
			}
		}
		if clash < 0 {
			return start, true
		}
		start = clash + 1
	}
	return 0, false
}

type addAllocationsRequest struct {
	IP    string `json:"ip"`
	Ports string `json:"ports"`
}

func (s *Server) addAllocations(w http.ResponseWriter, r *http.Request) {
	n, ok := s.nodeFrom(w, r)
	if !ok {
		return
	}
	var req addAllocationsRequest
	if !decode(w, r, &req) {
		return
	}
	ip := store.AnyAddress
	if req.IP != "" {
		a, err := netip.ParseAddr(req.IP)
		if err != nil || !a.Is4() {
			writeError(w, errBadAddress.Err("ip", truncate(req.IP, 64)))
			return
		}
		ip = a.String()
	}
	ports, bad := parsePorts(req.Ports)
	if bad != nil {
		writeError(w, bad)
		return
	}
	err := s.Store.AddAllocations(r.Context(), n.ID, ip, ports, s.now())
	var taken *store.TakenError
	if errors.As(err, &taken) {
		writeError(w, errPortInPool.Err("port", taken.Port))
		return
	}
	if err != nil {
		s.fail(w, "add allocations", err)
		return
	}
	s.Log.Info("ports added to the pool", "node", n.ID, "ip", ip, "count", len(ports), "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"added": len(ports)})
}

// parsePorts reads "25565-25600,27015" as the ports it names.
func parsePorts(s string) ([]int, *msg.Error) {
	var out []int
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		from, err := strconv.Atoi(strings.TrimSpace(lo))
		to := from
		if err == nil && isRange {
			to, err = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err != nil || from < lowestAllocation || to > highestPort || to < from {
			return nil, errBadPorts.Err()
		}
		if len(out)+to-from+1 > maxAddedAtOnce {
			return nil, errTooManyPorts.Err("max", maxAddedAtOnce)
		}
		for p := from; p <= to; p++ {
			out = append(out, p)
		}
	}
	return out, nil
}

// deleteAllocation takes a port out of the pool. One a server uses stays.
func (s *Server) deleteAllocation(w http.ResponseWriter, r *http.Request) {
	n, ok := s.nodeFrom(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.Store.DeleteAllocation(r.Context(), n.ID, id)
	} else {
		err = store.ErrNotFound
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoAllocation.Err())
	case errors.Is(err, store.ErrInUse):
		writeError(w, errAllocationInUse.Err())
	case err != nil:
		s.fail(w, "delete allocation", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// needs is what a new server needs from a node.
type needs struct {
	Ports int // how many ports it takes; the first is its main one
}

// placement is where a new server goes.
type placement struct {
	Node        int64
	Allocations []store.Allocation
}

// place decides where a new server goes. Every server gets its node, port
// and (later) cores from here and from nowhere else, so that picking
// among several machines is a change to this one function.
func (s *Server) place(ctx context.Context, node int64, w needs) (placement, error) {
	list, err := s.Store.Allocations(ctx, node)
	if err != nil {
		return placement{}, err
	}
	p := placement{Node: node}
	for _, a := range list {
		if a.AppID == "" && len(p.Allocations) < w.Ports {
			p.Allocations = append(p.Allocations, a)
		}
	}
	if len(p.Allocations) < w.Ports {
		return placement{}, errNoFreePort.Err()
	}
	return p, nil
}

// assign gives the placement's ports to the server. A port another server
// took since place ran is an error the caller retries from place.
func (s *Server) assign(ctx context.Context, app string, p placement) error {
	ids := make([]int64, 0, len(p.Allocations))
	for _, a := range p.Allocations {
		ids = append(ids, a.ID)
	}
	return s.Store.AssignAllocations(ctx, app, ids)
}
