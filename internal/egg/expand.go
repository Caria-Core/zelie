package egg

import (
	"regexp"
	"strconv"
	"strings"
)

// RuntimeVars are the environment variables the runtime has to set for
// every server, whatever the egg declares. Startup commands and install
// scripts use them freely, for example -Xmx{{SERVER_MEMORY}}M.
var RuntimeVars = []string{
	"SERVER_MEMORY", "SERVER_IP", "SERVER_PORT", "TZ", "STARTUP",
	"P_SERVER_LOCATION", "P_SERVER_UUID",
}

var placeholder = regexp.MustCompile(`\{\{\s*([\w.\-]+)\s*\}\}`)

// Expand fills in {{...}} placeholders. Eggs name the same value in
// several ways, from {{VAR}} to the long server.build.env.VAR and the
// server.allocations.* paths newer Pelican eggs use; all are understood.
// A placeholder that cannot be resolved is left as written.
func Expand(s string, vars map[string]string, port int) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		key := placeholder.FindStringSubmatch(m)[1]
		if v, ok := lookup(key, vars, port); ok {
			return v
		}
		return m
	})
}

func lookup(key string, vars map[string]string, port int) (string, bool) {
	switch key {
	case "server.build.default.port", "server.allocations.default.port":
		return portText(port)
	case "server.build.default.ip", "server.allocations.default.ip":
		return "0.0.0.0", true
	case "server.build.memory", "server.build.memory_limit":
		v, ok := vars["SERVER_MEMORY"]
		return v, ok
	}
	name := key
	for _, prefix := range []string{"env.", "server.build.env.", "server.build.environment.", "server.environment."} {
		if rest, ok := strings.CutPrefix(key, prefix); ok {
			name = rest
			break
		}
	}
	if strings.Contains(name, ".") {
		return "", false
	}
	if v, ok := vars[name]; ok {
		return v, true
	}
	switch name {
	case "SERVER_PORT":
		return portText(port)
	case "SERVER_IP":
		return "0.0.0.0", true
	}
	return "", false
}

func portText(port int) (string, bool) {
	if port <= 0 {
		return "", false
	}
	return strconv.Itoa(port), true
}
