package panel

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

// The crash doctor. When a game server or files app has crashed, or its
// install failed, the last lines of its output usually say why. Each rule
// below knows one cause by the lines it leaves, and may offer a fix. It runs
// only when someone asks, and reads what the console hub already keeps.

var (
	msgOOM         = msg.Define(0, "diagnosis.oom", "The server ran out of memory. Its limit is {size}.")
	msgOOMKilled   = msg.Define(0, "diagnosis.oom_killed", "The server was killed from outside, which is what happens when it goes over its memory limit of {size}.")
	msgJava        = msg.Define(0, "diagnosis.java_version", "The game needs Java {java} or newer, and this server runs an older one.")
	msgEULA        = msg.Define(0, "diagnosis.eula", "The server stopped because the Minecraft EULA has not been accepted.")
	msgPort        = msg.Define(0, "diagnosis.port_in_use", "The server could not use its port because something else is already using it.")
	msgStartFile   = msg.Define(0, "diagnosis.missing_file", "The server could not find the file it starts from. It may be missing, or the install did not finish.")
	msgModule      = msg.Define(0, "diagnosis.missing_module", "The app needs the package {module}, which is not installed. Run npm install, or add it to package.json.")
	msgDisk        = msg.Define(0, "diagnosis.disk_full", "The server's disk is full. Its limit is {size}.")
	msgWorld       = msg.Define(0, "diagnosis.world_corrupt", "The world could not be read, so its files may be damaged. Restoring a backup usually fixes it.")
	errFixNotFound = msg.Define(http.StatusConflict, "diagnosis.fix_gone", "That fix does not apply to this server any more.")
)

const (
	fixRaiseMemory = "raise_memory"
	fixRaiseDisk   = "raise_disk"
	fixSwitchImage = "switch_image"
	fixAcceptEULA  = "accept_eula"
	fixReinstall   = "reinstall"
	fixOpenNetwork = "open_network"
	fixOpenBackups = "open_backups"
)

// diagnoseLines is how many of the newest output lines are read.
const diagnoseLines = 200

// installTail is how much of an install's log is read.
const installTail = 64 << 10

type diagnosis struct {
	Cause msg.Msg       `json:"cause"`
	Fix   *diagnosisFix `json:"fix,omitempty"`
}

type diagnosisFix struct {
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params,omitempty"`

	// What applying the fix sets, worked out from the server as it is now.
	memoryMB, diskMB int64
	image            string
}

// diagInput is what a rule looks at.
type diagInput struct {
	lines      []string // newest first, without colour codes
	exit       int      // the process's exit code, or -1 when not known
	install    bool     // lines are from the install, not the server
	app        store.App
	game       store.GameServer
	egg        *egg.Egg
	volumeFull bool  // the volume is over its limit
	diskMB     int64 // the volume's limit

	// late is how many of the newest lines the game printed after it said it
	// was ready. Players can be in by then, and a player can make the game
	// print almost anything. The rest were printed by the server alone.
	late int
	// base is the place of lines[0] among all the lines, and hit the place of
	// the newest line the rule being tried matched, or -1. Both count from
	// the newest line.
	base, hit int
}

// newest returns the groups of the first pattern match, looking from the
// newest line back, or nil.
func (in *diagInput) newest(re *regexp.Regexp) []string {
	for i, l := range in.lines {
		if m := re.FindStringSubmatch(l); m != nil {
			if at := in.base + i; in.hit < 0 || at < in.hit {
				in.hit = at
			}
			return m
		}
	}
	return nil
}

type diagRule struct {
	id string
	// start marks a rule for a server that failed to start. It reads only
	// the lines printed before the game said it was ready.
	start bool
	// install marks a rule that also applies to a failed install.
	install bool
	cause   msg.Template
	// match says whether the rule fits, with the values for the cause.
	match func(in *diagInput) (map[string]any, bool)
	// fix is optional and returns nil when there is nothing to offer.
	fix func(ctx context.Context, s *Server, in *diagInput, found map[string]any) *diagnosisFix
}

func lineMatcher(pattern string) func(in *diagInput) (map[string]any, bool) {
	re := sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pattern) })
	return func(in *diagInput) (map[string]any, bool) {
		return nil, in.newest(re()) != nil
	}
}

// diagRules is in order of priority, which settles it when two fit equally
// well. The patterns are compiled the first time anyone asks.
var diagRules = sync.OnceValue(func() []diagRule {
	var (
		classVersion = regexp.MustCompile(`class file version (\d+)`)
		startFile    = regexp.MustCompile(`Unable to access jarfile|Could not find or load main class|Cannot find module '(?:/|\.\.?/)[^']*'`)
		module       = regexp.MustCompile(`Cannot find module '([^'/.][^']*)'`)
		javaTag      = regexp.MustCompile(`(?i)(?:java|jdk|jre|temurin)[\s_:.-]*(\d+)`)
		noSpace      = regexp.MustCompile(`No space left on device|ENOSPC`)
		heap         = regexp.MustCompile(`java\.lang\.OutOfMemoryError|FATAL ERROR: Reached heap limit|JavaScript heap out of memory`)
		killed       = regexp.MustCompile(`(?:^|\s)Killed(?:\s|$)`)
	)
	return []diagRule{
		{
			id: "eula", start: true, cause: msgEULA,
			match: lineMatcher(`(?i)you need to agree to the eula`),
			fix: func(_ context.Context, _ *Server, in *diagInput, _ map[string]any) *diagnosisFix {
				if !in.egg.HasFeature(egg.FeatureEULA) || !in.game.EULAAcceptedAt.IsZero() {
					return nil
				}
				return &diagnosisFix{Kind: fixAcceptEULA}
			},
		},
		{
			id: "java_version", start: true, cause: msgJava,
			match: func(in *diagInput) (map[string]any, bool) {
				m := in.newest(classVersion)
				if m == nil {
					return nil, false
				}
				major, _ := strconv.Atoi(m[1])
				if major < 45 {
					return nil, false
				}
				return map[string]any{"java": major - 44}, true
			},
			fix: func(_ context.Context, _ *Server, in *diagInput, found map[string]any) *diagnosisFix {
				image := imageForJava(in.egg, javaTag, in.game.Image, found["java"].(int))
				if image == "" {
					return nil
				}
				return &diagnosisFix{Kind: fixSwitchImage, Params: map[string]any{"image": image}, image: image}
			},
		},
		{
			id: "port_in_use", start: true, cause: msgPort,
			match: lineMatcher(`Address already in use|FAILED TO BIND TO PORT|EADDRINUSE`),
			fix: func(_ context.Context, _ *Server, in *diagInput, _ map[string]any) *diagnosisFix {
				// A files app has no network page: its port is the app's.
				if in.app.IsFiles() {
					return nil
				}
				return &diagnosisFix{Kind: fixOpenNetwork}
			},
		},
		{
			id: "missing_file", start: true, cause: msgStartFile,
			match: func(in *diagInput) (map[string]any, bool) {
				m := in.newest(startFile)
				// A file inside a package is that package's problem.
				return nil, m != nil && !strings.Contains(m[0], "node_modules")
			},
			fix: func(_ context.Context, _ *Server, in *diagInput, _ map[string]any) *diagnosisFix {
				// What a files app runs is its owner's code, which an install does not bring back.
				if in.app.IsFiles() {
					return nil
				}
				return &diagnosisFix{Kind: fixReinstall}
			},
		},
		{
			id: "missing_module", start: true, cause: msgModule,
			match: func(in *diagInput) (map[string]any, bool) {
				m := in.newest(module)
				if m == nil {
					return nil, false
				}
				return map[string]any{"module": m[1]}, true
			},
		},
		{
			id: "disk_full", install: true, cause: msgDisk,
			match: func(in *diagInput) (map[string]any, bool) {
				found := map[string]any{"size": formatMB(in.diskMB)}
				if in.volumeFull {
					// True now, whatever the log says.
					in.hit = 0
					return found, true
				}
				return found, in.newest(noSpace) != nil
			},
			fix: func(ctx context.Context, s *Server, in *diagInput, _ map[string]any) *diagnosisFix {
				h, err := s.Core.Host(ctx)
				if err != nil {
					return nil
				}
				target := min((in.diskMB*3/2+1023)/1024*1024, h.DiskBytes>>20)
				if target <= in.diskMB {
					return nil
				}
				return &diagnosisFix{Kind: fixRaiseDisk, Params: map[string]any{"disk_mb": target, "size": formatMB(target)}, diskMB: target}
			},
		},
		{
			id: "world_corrupt", cause: msgWorld,
			match: lineMatcher(`Exception reading .*level\.dat|Failed to load level`),
			fix: func(_ context.Context, _ *Server, _ *diagInput, _ map[string]any) *diagnosisFix {
				return &diagnosisFix{Kind: fixOpenBackups}
			},
		},
		{
			id: "oom", cause: msgOOM,
			match: func(in *diagInput) (map[string]any, bool) {
				size := map[string]any{"size": formatMB(in.app.MemoryMB)}
				if in.newest(heap) != nil {
					return size, true
				}
				// The shell says Killed when the kernel ended the process.
				// Without an exit code, the word alone is taken as it is.
				if in.newest(killed) != nil && (in.exit == 137 || in.exit < 0) {
					return size, true
				}
				return nil, false
			},
			fix: raiseMemory,
		},
		{
			// A process that the kernel killed and that said nothing: most
			// often the memory limit.
			id: "oom_killed", cause: msgOOMKilled,
			match: func(in *diagInput) (map[string]any, bool) {
				return map[string]any{"size": formatMB(in.app.MemoryMB)}, in.exit == 137
			},
			fix: raiseMemory,
		},
	}
})

// imageForJava picks the egg's image that runs Java version java: the one
// that names it, or else the closest newer one. It returns "" when there is
// none, or the server has it already.
func imageForJava(e *egg.Egg, tag *regexp.Regexp, current string, java int) string {
	best, bestJava := "", 0
	for _, img := range e.Images {
		m := tag.FindStringSubmatch(img.Label + " " + img.Ref)
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v < java || v > 200 || img.Ref == current {
			continue
		}
		if best == "" || v < bestJava {
			best, bestJava = img.Ref, v
		}
	}
	return best
}

// raiseMemory offers half as much again, in steps of 256 MB, as far as the
// machine has memory.
func raiseMemory(ctx context.Context, s *Server, in *diagInput, _ map[string]any) *diagnosisFix {
	h, err := s.Core.Host(ctx)
	if err != nil {
		return nil
	}
	target := min((in.app.MemoryMB*3/2+255)/256*256, h.MemoryBytes>>20)
	if target <= in.app.MemoryMB {
		return nil
	}
	return &diagnosisFix{Kind: fixRaiseMemory, Params: map[string]any{"memory_mb": target, "size": formatMB(target)}, memoryMB: target}
}

// diagnose looks for the cause of a crash or a failed install. It returns
// nil when the server is not in that state or no rule fits.
func (s *Server) diagnose(ctx context.Context, a store.App) (*diagnosis, error) {
	g, stored, e, err := s.gameEgg(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	in := &diagInput{exit: -1, app: a, game: g, egg: e}
	// Zelie giving up follows a crash, so a crashed server covers both.
	switch {
	case g.InstallState == store.InstallFailed:
		in.install = true
		in.lines = s.installLines(g.InstallID)
	case g.InstallState == store.InstallDone && s.gameState(ctx, a) == stateCrashed:
		h := s.consoles.hub(a.ID)
		s.loadStoppedConsole(ctx, h)
		h.mu.Lock()
		printed := slices.Clone(lastLines(h.lines, diagnoseLines))
		early := h.beforeReady(len(printed))
		if h.exited {
			in.exit = h.exit
		}
		h.mu.Unlock()
		before := withoutPlayerLines(stored.Source, printed[:early])
		after := withoutPlayerLines(stored.Source, printed[early:])
		slices.Reverse(before)
		slices.Reverse(after)
		in.lines, in.late = slices.Concat(after, before), len(after)
	default:
		return nil, nil
	}
	for i, l := range in.lines {
		in.lines[i] = ansi().ReplaceAllString(l, "")
	}
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	in.volumeFull = s.overLimit(vols) != nil
	for _, v := range vols {
		if v.Path == gameVolumePath {
			in.diskMB = v.LimitMB
		}
	}
	r, found, ok := firstRule(in)
	if !ok {
		return nil, nil
	}
	var pairs []any
	for _, k := range slices.Sorted(maps.Keys(found)) {
		pairs = append(pairs, k, found[k])
	}
	d := &diagnosis{Cause: r.cause.With(pairs...)}
	if r.fix != nil {
		d.Fix = r.fix(ctx, s, in, found)
	}
	return d, nil
}

// firstRule is the rule that fits best: the one whose line is the newest, as
// the last thing the server said is the likeliest reason it stopped. A rule
// that fits with no line to show ranks behind the ones that have one, and of
// two that tie the one higher in diagRules wins.
func firstRule(in *diagInput) (diagRule, map[string]any, bool) {
	var best diagRule
	var bestFound map[string]any
	bestAt, found := 0, false
	for _, r := range diagRules() {
		if in.install && !r.install {
			continue
		}
		view := *in
		view.hit = -1
		if r.start {
			view.lines, view.base = in.lines[in.late:], in.base+in.late
		}
		values, ok := r.match(&view)
		if !ok {
			continue
		}
		at := view.hit
		if at < 0 {
			at = math.MaxInt
		}
		if !found || at < bestAt {
			best, bestFound, bestAt, found = r, values, at, true
		}
	}
	return best, bestFound, found
}

// withoutPlayerLines drops the lines that carry something a player made. The
// rules look for words anywhere in a line, and a player can make the game
// print any.
func withoutPlayerLines(source string, lines []string) []string {
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansi().ReplaceAllString(l, "")
	}
	mine := players.PlayerLines(source, plain)
	kept := make([]string, 0, len(lines))
	for i, l := range lines {
		if !mine[i] {
			kept = append(kept, l)
		}
	}
	return kept
}

// installLines reads the end of an install's log, newest line first.
func (s *Server) installLines(id int64) []string {
	if id == 0 {
		return nil
	}
	f, err := os.Open(s.deployLogPath(id))
	if err != nil {
		return nil
	}
	defer f.Close()
	var skip bool
	if st, err := f.Stat(); err == nil && st.Size() > installTail {
		if _, err := f.Seek(-installTail, io.SeekEnd); err != nil {
			return nil
		}
		skip = true
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if skip && len(lines) > 0 {
		// The read began in the middle of a line.
		lines = lines[1:]
	}
	lines = slices.Clone(lastLines(lines, diagnoseLines))
	slices.Reverse(lines)
	return lines
}

// getDiagnosis answers with the likely cause of the server's crash, or an
// empty object when it has not crashed or nothing fits.
func (s *Server) getDiagnosis(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	d, err := s.diagnose(r.Context(), a)
	if err != nil {
		s.fail(w, "diagnose game server", err)
		return
	}
	if d == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type diagnosisFixRequest struct {
	Kind string `json:"kind"`
	// Params is what the diagnosis showed. It is accepted so the request
	// can echo the diagnosis, and never read.
	Params map[string]any `json:"params"`
}

// applyDiagnosisFix does the fix the diagnosis offers right now, when it is
// the kind asked for. The request says nothing about what to set: that is
// worked out again here, so a stale or made-up request changes nothing.
func (s *Server) applyDiagnosisFix(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	var req diagnosisFixRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	d, err := s.diagnose(ctx, a)
	if err != nil {
		s.fail(w, "diagnose game server", err)
		return
	}
	if d == nil || d.Fix == nil || d.Fix.Kind != req.Kind {
		writeError(w, errFixNotFound.Err())
		return
	}
	if err := s.applyFix(ctx, a, g, d.Fix); err != nil {
		s.failWith(w, "apply fix", err)
		return
	}
	s.Log.Info("game diagnosis fix applied", "server", a.ID, "user", loginFrom(ctx).account.ID, "fix", d.Fix.Kind)
	a, err = s.Store.App(ctx, a.ID)
	if err != nil {
		s.fail(w, "load game server", err)
		return
	}
	out, err := s.gameOut(ctx, a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// applyFix runs the fix through the code the settings pages use. The
// server is not started: whoever asked does that when ready.
func (s *Server) applyFix(ctx context.Context, a store.App, g store.GameServer, fix *diagnosisFix) error {
	switch fix.Kind {
	case fixRaiseMemory:
		_, err := s.setGameResources(ctx, a, gameResourcesRequest{MemoryMB: fix.memoryMB})
		return err
	case fixRaiseDisk:
		_, err := s.setGameResources(ctx, a, gameResourcesRequest{DiskMB: fix.diskMB})
		return err
	case fixSwitchImage:
		if err := s.Store.SetGameSettings(ctx, a.ID, fix.image, g.Startup, g.Variables); err != nil {
			return fmt.Errorf("save game settings: %w", err)
		}
		// As in the settings page: the app's image is pinned to a build once
		// it has run, and a new choice starts over from the tag.
		if err := s.Store.SetImage(ctx, a.ID, fix.image); err != nil {
			return fmt.Errorf("save game image: %w", err)
		}
		return nil
	case fixAcceptEULA:
		if err := s.Store.AcceptEULA(ctx, a.ID, s.now()); err != nil {
			return fmt.Errorf("accept eula: %w", err)
		}
		return nil
	case fixReinstall:
		_, err := s.reinstall(ctx, a, g)
		return err
	}
	// The others are links to other pages, which change nothing here.
	return errFixNotFound.Err()
}
