package main

import (
	"os"
	"strconv"
	"testing"
)

func TestSystemdListenerWantsASocketForThisProcess(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	pid := os.Getpid()
	for name, m := range map[string]map[string]string{
		"started by hand":       {},
		"a socket for another":  {"LISTEN_PID": strconv.Itoa(pid + 1), "LISTEN_FDS": "1"},
		"no descriptor counted": {"LISTEN_PID": strconv.Itoa(pid)},
	} {
		if l, err := systemdListener(env(m), pid); err == nil {
			l.Close()
			t.Errorf("%s: got a listener", name)
		}
	}
}
