package panel

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Games such as Rust listen on several ports: one for players, one for
// queries, one for RCON and one for a companion app. Eggs give each its
// own variable, ending in _PORT, with a default that is only a suggestion.
// Pterodactyl users set them to extra allocations by hand; Zelie does it
// when it makes the server.

// isPortVariable reports whether the egg means the variable to hold a port
// of the server: its name ends in _PORT and its default is empty or a
// number. SERVER_PORT is the main port, which the runtime sets. A default
// such as "-1" (off) or a word is the egg's own choice and is left alone.
func isPortVariable(v egg.Variable) bool {
	if v.Env == "SERVER_PORT" || !strings.HasSuffix(v.Env, "_PORT") {
		return false
	}
	if v.Default == "" {
		return true
	}
	for _, c := range v.Default {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// portVariables lists the egg's port variables in the order the startup
// command uses them, and after those any it does not mention, in the egg's
// order. That is the order extra ports are handed out in.
func portVariables(e *egg.Egg) []egg.Variable {
	var vars []egg.Variable
	for _, v := range e.Variables {
		if isPortVariable(v) {
			vars = append(vars, v)
		}
	}
	at := func(v egg.Variable) int {
		if i := strings.Index(e.Startup, "{{"+v.Env+"}}"); i >= 0 {
			return i
		}
		return len(e.Startup) + 1
	}
	slices.SortStableFunc(vars, func(a, b egg.Variable) int { return at(a) - at(b) })
	return vars
}

// assignPorts gives a server's extra ports to the egg's port variables, in
// the order of portVariables, and changes vars in place. A variable the
// request set itself keeps its value and does not use up a port, and one
// that has no port left keeps its default. The variables may be locked
// against users: this is how the server's own ports are set, not a user's
// choice. extra are the ports after the first, which players use.
func assignPorts(e *egg.Egg, vars map[string]string, given map[string]string, extra []int) {
	for _, v := range portVariables(e) {
		if len(extra) == 0 {
			return
		}
		if _, set := given[v.Env]; set {
			continue
		}
		value := strconv.Itoa(extra[0])
		if v.Check(value) != nil {
			continue
		}
		vars[v.Env] = value
		extra = extra[1:]
	}
}

// portUseJSON is a variable that holds one of the server's ports.
type portUseJSON struct {
	Env  string `json:"env"`
	Name string `json:"name"`
}

// portUses says which of the egg's port variables hold port, given the
// server's values.
func portUses(e *egg.Egg, values map[string]string, port int) []portUseJSON {
	uses := []portUseJSON{}
	for _, v := range e.Variables {
		if isPortVariable(v) && values[v.Env] == strconv.Itoa(port) {
			name := v.Name
			if name == "" {
				name = v.Env
			}
			uses = append(uses, portUseJSON{Env: v.Env, Name: name})
		}
	}
	return uses
}

// recommendedPorts is how many ports a catalog egg takes when the request
// does not say.
func recommendedPorts(req gameRequest) int {
	if req.Ports != 0 || req.Egg == "" {
		return req.Ports
	}
	if entry, ok := egg.Lookup(req.Egg); ok {
		return entry.Ports
	}
	return 0
}

// recommendedSize fills in the memory and disk of a catalog egg the request
// left out, for games that need far more than the general default. Neither
// goes beyond what the machine has.
func recommendedSize(req gameRequest, h engine.Host) (memoryMB, diskMB int64) {
	memoryMB, diskMB = req.MemoryMB, req.DiskMB
	entry, ok := egg.Lookup(req.Egg)
	if !ok {
		return
	}
	if memoryMB == 0 && entry.MemoryMB > 0 {
		memoryMB = min(entry.MemoryMB, h.MemoryBytes>>20)
	}
	if diskMB == 0 && entry.DiskMB > 0 {
		diskMB = min(entry.DiskMB, h.DiskBytes>>20/2)
	}
	return
}

func portNumbers(list []store.Allocation) []int {
	out := make([]int, len(list))
	for i, a := range list {
		out[i] = a.Port
	}
	return out
}

func portVariableNames(e *egg.Egg) []portUseJSON {
	out := []portUseJSON{}
	for _, v := range portVariables(e) {
		name := v.Name
		if name == "" {
			name = v.Env
		}
		out = append(out, portUseJSON{Env: v.Env, Name: name})
	}
	return out
}

var (
	errStopForPorts = msg.Define(http.StatusConflict, "game.stop_for_ports", "Stop the server to change its ports.")
	errNotForFiles  = msg.Define(http.StatusConflict, "game.not_for_files", "This is for game servers. A files app has its own settings page.")
	errNoPortRole   = msg.Define(http.StatusBadRequest, "game.no_port_role", "{env} is not a port variable of this egg.")
)

// portPick is the pool allocations, by id, a request wants for the roles
// of a new server's ports. Roles it leaves out get the lowest free ports.
type portPick struct {
	Primary   int64            `json:"primary"`
	Variables map[string]int64 `json:"variables"`
}

// portWish is a portPick sorted out for place: the allocations to ask for,
// and which role each is for.
type portWish struct {
	ids     []int64
	primary bool
	envs    []string // the variable of each id after the first, if primary
	vars    int
}

// wishedPorts checks that the variables a pick names hold ports of the egg.
func wishedPorts(e *egg.Egg, pick *portPick) (portWish, *msg.Error) {
	var w portWish
	if pick == nil {
		return w, nil
	}
	if pick.Primary != 0 {
		w.primary = true
		w.ids = append(w.ids, pick.Primary)
	}
	known := map[string]egg.Variable{}
	for _, v := range portVariables(e) {
		known[v.Env] = v
	}
	for _, env := range slices.Sorted(maps.Keys(pick.Variables)) {
		if _, ok := known[env]; !ok {
			return w, errNoPortRole.Err("env", truncate(env, 64))
		}
		w.ids = append(w.ids, pick.Variables[env])
		w.envs = append(w.envs, env)
	}
	w.vars = len(w.envs)
	return w, nil
}

// count is how many ports the server needs at least: its main one, or the
// run of ports a game needs (block), and one for each variable that was
// given a port.
func (w portWish) count(block int) int { return max(block, 1) + w.vars }

// split says which of the placed allocations is the main port, which port
// each chosen variable got, and the ports left for the others. A game with
// a block has its run first, and the ports after the main one are not
// given to variables.
func (w portWish) split(all []store.Allocation, block int) (primary store.Allocation, chosen map[string]int, auto []int) {
	chosen = map[string]int{}
	block = max(block, 1)
	rest := all
	if w.primary || block > 1 {
		primary, rest = all[0], all[block:]
	}
	for _, env := range w.envs {
		chosen[env] = rest[0].Port
		rest = rest[1:]
	}
	if !w.primary && block == 1 {
		primary, rest = rest[0], rest[1:]
	}
	return primary, chosen, portNumbers(rest)
}

type gamePortsRequest struct {
	// Primary is the port players connect to. Zero keeps the current one.
	Primary int64 `json:"primary"`
	// Variables gives a port to a port variable. One that is not listed
	// keeps the port it holds, if it holds one of the server's.
	Variables map[string]int64 `json:"variables"`
	// Extra are the other ports the server keeps, which no variable holds.
	Extra []int64 `json:"extra"`
}

// updateGamePorts changes which pool ports a stopped server holds. The
// forwards follow at the next start.
func (s *Server) updateGamePorts(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	if a.IsFiles() {
		writeError(w, errNotForFiles.Err())
		return
	}
	var req gamePortsRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	if g.InstallState == store.InstallRunning {
		writeError(w, errInstalling.Err())
		return
	}
	// Starts run under the same lock, so none can begin between the check
	// below and the change, with half of the old ports.
	unlock := s.deploys.lock(a.ID)
	defer unlock()
	if s.gameState(ctx, a) != stateStopped {
		writeError(w, errStopForPorts.Err())
		return
	}
	_, e, _, err := s.gameParts(ctx, a.ID)
	if err != nil {
		s.fail(w, "load egg", err)
		return
	}
	pool, err := s.Store.Allocations(ctx, store.ThisNode)
	if err != nil {
		s.fail(w, "list allocations", err)
		return
	}
	held := map[int64]store.Allocation{}
	byPort := map[int]int64{}
	for _, p := range pool {
		if p.AppID == a.ID {
			held[p.ID] = p
			byPort[p.Port] = p.ID
		}
	}

	primary := req.Primary
	if primary == 0 {
		primary = byPort[a.Port]
	}
	ids := []int64{primary}
	// A game that needs a run of ports gets it from the main port; the
	// ports after it are not chosen. A server whose run is already broken
	// keeps its ports as they are until the main port moves to a run that
	// is free.
	block := s.gameBlock(ctx, g)
	following := map[int64]bool{}
	if i := slices.IndexFunc(pool, func(p store.Allocation) bool { return p.ID == primary }); block > 1 && i >= 0 {
		start := pool[i]
		usable := func(x store.Allocation) bool { return x.AppID == "" || x.AppID == a.ID }
		if run, ok := runAt(portIndex(pool), start, block, usable); ok {
			for _, x := range run[1:] {
				ids = append(ids, x.ID)
				following[x.ID] = true
			}
		} else if start.Port != a.Port {
			writeError(w, errBrokenBlock.Err("port", start.Port, "count", block))
			return
		}
	}
	roles := map[string]int64{}
	for _, env := range slices.Sorted(maps.Keys(req.Variables)) {
		if !slices.ContainsFunc(portVariables(e), func(v egg.Variable) bool { return v.Env == env }) {
			writeError(w, errNoPortRole.Err("env", truncate(env, 64)))
			return
		}
		roles[env] = req.Variables[env]
		ids = append(ids, req.Variables[env])
	}
	for _, id := range req.Extra {
		if !following[id] {
			ids = append(ids, id)
		}
	}
	// A variable the request leaves out keeps its port, so long as that
	// port stays with the server.
	for _, v := range portVariables(e) {
		if _, listed := roles[v.Env]; listed {
			continue
		}
		port, err := strconv.Atoi(g.Variables[v.Env])
		if id, ok := byPort[port]; err == nil && ok && following[id] {
			writeError(w, errPortTwice.Err())
			return
		} else if err == nil && ok && !slices.Contains(ids, id) {
			roles[v.Env] = id
			ids = append(ids, id)
		}
	}
	if len(ids) > maxGamePorts {
		writeError(w, errGamePorts.Err("max", maxGamePorts))
		return
	}
	chosen := map[int64]store.Allocation{}
	for _, id := range ids {
		i := slices.IndexFunc(pool, func(p store.Allocation) bool { return p.ID == id })
		switch {
		case i < 0:
			writeError(w, errNoAllocation.Err())
			return
		case pool[i].AppID != "" && pool[i].AppID != a.ID:
			writeError(w, errPortNotFree.Err("port", pool[i].Port))
			return
		}
		if _, twice := chosen[id]; twice {
			writeError(w, errPortTwice.Err())
			return
		}
		chosen[id] = pool[i]
	}

	vars := maps.Clone(g.Variables)
	for env, id := range roles {
		value := strconv.Itoa(chosen[id].Port)
		for _, v := range e.Variables {
			if v.Env == env {
				if err := v.Check(value); err != nil {
					writeError(w, errBadVariable.Err("name", env, "detail", err.Error()))
					return
				}
			}
		}
		vars[env] = value
	}
	err = s.Store.ReplaceGamePorts(ctx, store.ThisNode, a.ID, ids, chosen[primary].Port, vars)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoAllocation.Err())
		return
	case errors.Is(err, store.ErrInUse):
		writeError(w, errPortNotFree.Err("port", 0))
		return
	case err != nil:
		s.fail(w, "change game ports", err)
		return
	}
	a.Port = chosen[primary].Port
	s.Log.Info("game ports changed", "server", a.ID, "user", loginFrom(ctx).account.ID, "count", len(ids))
	out, err := s.gameOut(ctx, a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
