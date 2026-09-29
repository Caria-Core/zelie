package panel

import (
	"slices"
	"strconv"
	"strings"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
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
