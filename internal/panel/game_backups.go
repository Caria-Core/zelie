package panel

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/store"
)

// saveFlushWait is how long a backup waits for a Minecraft server to say it
// has written its world. Tests shorten it.
var saveFlushWait = 30 * time.Second

// isMinecraft tells a Minecraft server from other games by its egg: the ones
// that ask for the EULA, or that start a .jar file.
func isMinecraft(e *egg.Egg, g store.GameServer) bool {
	return e.HasFeature(egg.FeatureEULA) || strings.Contains(g.Startup, ".jar")
}

// quiesceGame makes a running Minecraft server's files consistent for a
// copy: it stops writing the world and flushes what it holds. The function
// it returns lets it write again, and is safe to call whatever happened. Any
// other server, or one that is not running, is left alone.
func (s *Server) quiesceGame(ctx context.Context, a store.App) (resume func()) {
	resume = func() {}
	if !a.IsGame() || s.gameState(ctx, a) != stateRunning {
		return
	}
	g, e, _, err := s.gameParts(ctx, a.ID)
	if err != nil || !isMinecraft(e, g) {
		return
	}
	container, ok := s.runningGameContainer(ctx, a.ID)
	if !ok {
		return
	}
	send := func(ctx context.Context, cmd string) bool {
		if err := s.Core.WriteStdin(ctx, container, []byte(cmd+"\n")); err != nil {
			s.Log.Warn("backup: console command", "server", a.ID, "command", cmd, "err", err)
			return false
		}
		return true
	}
	lines, stop := s.consoles.hub(a.ID).tap()
	defer stop()
	if !send(ctx, "save-off") {
		return
	}
	resume = func() {
		send(context.WithoutCancel(ctx), "save-on")
	}
	if !send(ctx, "save-all flush") {
		return
	}
	timeout := time.NewTimer(saveFlushWait)
	defer timeout.Stop()
	for {
		select {
		case line := <-lines:
			if strings.Contains(strings.ToLower(ansi.ReplaceAllString(line, "")), "saved the") {
				return
			}
		case <-timeout.C:
			s.Log.Warn("backup: the server did not say it saved", "server", a.ID)
			return
		case <-ctx.Done():
			return
		}
	}
}

// gameBackups is for the backup routes of a game server: the same handlers
// as an app's, for a server that exists.
func (s *Server) gameBackups(next http.HandlerFunc) http.HandlerFunc {
	return s.managesGame(s.onGame(next))
}

// gameBackupsConfirmed is gameBackups for what needs a recent second step.
func (s *Server) gameBackupsConfirmed(next http.HandlerFunc) http.HandlerFunc {
	return s.managesGameConfirmed(s.onGame(next))
}

// onGame answers for the server in the path, and for a backup in the path
// only if it is that server's.
func (s *Server) onGame(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, _, ok := s.gameFrom(w, r)
		if !ok {
			return
		}
		if r.PathValue("id") != "" {
			b, ok := s.backupFrom(w, r)
			if !ok {
				return
			}
			if b.AppID != a.ID {
				writeError(w, errNoBackup.Err())
				return
			}
		}
		next(w, r)
	}
}
