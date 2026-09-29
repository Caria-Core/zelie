//go:build eggcheck

package panel

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
)

// This test downloads every game in the catalog at its pinned commit and
// checks what Zelie would do with it, so a game that cannot start fails here
// and not on a customer's machine. It needs the network, so it only runs
// with the eggcheck tag:
//
//	go test -tags eggcheck -run TestCatalogEggs -v ./internal/panel

// portExplanations say why an entry's Ports is not 1 plus the egg's port
// variables: ports the game takes that the egg has no variable for.
var portExplanations = map[string]string{
	"7-days-to-die":         "the game also uses the three ports after its own (UDP)",
	"valheim":               "Steam queries the port after the game's",
	"palworld":              "RCON is only used from inside the container",
	"ark-survival-ascended": "RCON is only used from inside the container",
	"v-rising":              "RCON is off unless the owner turns it on",
}

// ignoredFeatures are egg features that name something Zelie does without
// or does another way. Any other feature that is not "eula" is reported.
var ignoredFeatures = map[string]string{
	"steam_disk_space": "the volume's limit stands in",
	"java_version":     "the image is chosen when the server is made",
	"pid_limit":        "every server has the same process limit",
	"gsl_token":        "the token is an ordinary variable",
}

// noInstall lists entries whose egg has no install script.
var noInstall = map[string]bool{}

type eggReport struct {
	fails, warns []string
}

func (r *eggReport) fail(format string, a ...any) {
	r.fails = append(r.fails, fmt.Sprintf(format, a...))
}
func (r *eggReport) warn(format string, a ...any) {
	r.warns = append(r.warns, fmt.Sprintf(format, a...))
}

func TestCatalogEggs(t *testing.T) {
	type result struct {
		e   *egg.Egg
		err error
	}
	results := make([]result, len(egg.Catalog))
	var wg sync.WaitGroup
	gate := make(chan struct{}, 6)
	for i, entry := range egg.Catalog {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var err error
			for try := 0; try < 3; try++ {
				var e *egg.Egg
				if e, _, err = egg.Fetch(ctx, nil, entry); err == nil {
					results[i] = result{e: e}
					return
				}
				time.Sleep(time.Duration(try+1) * time.Second)
			}
			results[i] = result{err: err}
		}()
	}
	wg.Wait()

	failed := 0
	out := os.Stdout
	for i, entry := range egg.Catalog {
		var r eggReport
		if results[i].err != nil {
			r.fail("download or parse: %v", results[i].err)
		} else {
			checkEgg(entry, results[i].e, &r)
		}
		status := "OK  "
		switch {
		case len(r.fails) > 0:
			status = "FAIL"
			failed++
			t.Errorf("%s: %s", entry.ID, strings.Join(r.fails, "; "))
		case len(r.warns) > 0:
			status = "WARN"
		}
		fmt.Fprintf(out, "%s %-24s %s\n", status, entry.ID, entry.Path)
		for _, m := range r.fails {
			fmt.Fprintf(out, "       fail: %s\n", m)
		}
		for _, m := range r.warns {
			fmt.Fprintf(out, "       warn: %s\n", m)
		}
	}
	fmt.Fprintf(out, "%d of %d catalog entries checked, %d failed\n", len(egg.Catalog), len(egg.Catalog), failed)
}

var startupVar = regexp.MustCompile(`\{\{\s*([\w.\-]+)\s*\}\}`)

func checkEgg(entry egg.Entry, e *egg.Egg, r *eggReport) {
	if strings.TrimSpace(e.Name) == "" {
		r.fail("the egg has no name")
	}
	if len(e.Images) == 0 {
		r.fail("no docker image")
	}
	for _, img := range e.Images {
		if bad := checkImageRef(img.Ref); bad != nil {
			r.fail("image %q is not usable", img.Ref)
		}
	}
	switch {
	case e.Install.Script == "" && e.Install.Image == "" && noInstall[entry.ID]:
	case e.Install.Script == "":
		r.fail("no install script")
	case e.Install.Image == "":
		r.fail("no install container")
	default:
		if bad := checkImageRef(e.Install.Image); bad != nil {
			r.fail("install container %q is not usable", e.Install.Image)
		}
	}

	// The values a new server gets: defaults, safe secrets, and ports from
	// the range examples start at.
	ports := max(entry.Ports, 1)
	vars := map[string]string{}
	for _, v := range e.Variables {
		vars[v.Env] = initialValue(v)
	}
	extra := make([]int, 0, ports-1)
	for i := 1; i < ports; i++ {
		extra = append(extra, 27015+i)
	}
	assignPorts(e, vars, nil, extra)
	portVars := portVariables(e)
	_, explained := portExplanations[entry.ID]
	switch want := 1 + len(portVars); {
	case entry.Kind != egg.KindGame:
	case ports != want && !explained:
		r.fail("catalog has %d ports, the egg has %d port variables (want %d)", ports, len(portVars), want)
	case ports == want && explained:
		r.warn("the note on the number of ports is not needed any more")
	}
	seen := map[string]string{"27015": "SERVER_PORT"}
	for _, v := range portVars {
		if other, dup := seen[vars[v.Env]]; dup {
			r.warn("%s shares its port with %s", v.Env, other)
		}
		seen[vars[v.Env]] = v.Env
	}

	var needInput []string
	for _, v := range e.Variables {
		value := vars[v.Env]
		if err := v.Check(value); err != nil {
			if strings.TrimSpace(value) == "" && hasRule(v, "required") {
				needInput = append(needInput, v.Env)
				continue
			}
			r.fail("%s = %q: %v", v.Env, value, err)
		}
	}
	if len(needInput) > 0 {
		r.warn("required with no default, the admin must fill in: %s", strings.Join(needInput, ", "))
	}

	// Everything a startup command or a config value may name.
	known := maps.Clone(vars)
	for _, name := range egg.RuntimeVars {
		known[name] = "x"
	}
	unresolved := func(where, s string) {
		left := startupVar.FindAllString(egg.Expand(s, known, 27015), -1)
		if len(left) > 0 {
			r.fail("%s uses %s, which Zelie does not fill in", where, strings.Join(slices.Compact(slices.Sorted(slices.Values(left))), ", "))
		}
	}
	for _, c := range e.StartupCommands {
		unresolved("startup command "+strconv.Quote(c.Label), c.Line)
	}

	if e.DoneMatcher().Empty() {
		r.warn("no done line, so the server counts as running at once")
	}
	for _, d := range e.Done {
		if expr, ok := strings.CutPrefix(d, "regex:"); ok {
			if _, err := regexp.Compile(expr); err != nil {
				r.fail("done line %q does not compile: %v", d, err)
			}
		}
	}
	switch name, isSignal := e.StopSignal(); {
	case e.Stop == "":
		r.fail("no stop command or signal")
	case isSignal:
		if _, known := engine.ParseSignal(name); !known {
			r.warn("stop signal %q is not one Zelie can send; it uses SIGTERM", e.Stop)
		}
	}

	for _, f := range e.Files {
		if !egg.KnownParser(f.Parser) {
			r.fail("config file %s uses the parser %q", f.Path, f.Parser)
		}
		for _, rep := range f.Find {
			unresolved("config file "+f.Path, rep.Value)
		}
	}

	for _, img := range e.Images {
		if ref := strings.ToLower(img.Ref); strings.Contains(ref, "wine") || strings.Contains(ref, "proton") {
			r.warn("runs a Windows server under Wine or Proton (%s); that only shows on a real run", img.Ref)
			break
		}
	}
	for _, f := range e.Features {
		if f == egg.FeatureEULA {
			continue
		}
		if _, ok := ignoredFeatures[f]; !ok {
			r.warn("feature %q is not known to Zelie and is ignored", f)
		}
	}
}
