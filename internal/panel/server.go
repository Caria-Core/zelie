// Package panel is the unprivileged half of Zelie: the web interface, its API
// and user accounts. It never listens on the network. The proxy hands it web
// traffic over a Unix socket, and it asks the core to do anything that needs
// root.
package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
	"github.com/Caria-Core/zelie/internal/version"
	"github.com/Caria-Core/zelie/internal/webui"
)

// setupLinkTTL is how long a setup link stays valid.
const setupLinkTTL = 24 * time.Hour

type Server struct {
	// Shorter waits for tests; zero means the usual ones.
	testStopGrace, testWatchRetry time.Duration
	// testPortFree stands in for trying a port on the machine.
	testPortFree func(port int) bool

	Store  *store.Store
	Sealer *Sealer
	Core   Core
	Proxy  Proxy
	Source Source
	Log    *slog.Logger
	// DataDir holds files that do not belong in the database, such as
	// deployment logs.
	DataDir string
	// SFTPUID is the user the SFTP service runs as; it returns 0 while
	// there is no such user. It is looked up on each request because the
	// service may be set up after the panel starts, by an update.
	SFTPUID func() uint32
	// ProxyUID is the user the proxy runs as. Requests from it are web
	// traffic; requests from root come from the zelie command on the server.
	ProxyUID uint32
	Now      func() time.Time
	// Where GitHub is. Empty means the real one; tests set them.
	GitHubAPI  string
	GitHubWeb  string
	GitHubHTTP *http.Client
	// HealthCheck replaces the HTTP request of the health check in tests.
	HealthCheck func(ctx context.Context, url, host string) (int, error)
	// Registry answers what image tags point at; nil asks the real ones.
	Registry Registry
	// Eggs downloads egg files; nil asks the real sources.
	Eggs EggFetcher
	// PortCheck replaces the connection a database's health check makes.
	PortCheck func(ctx context.Context, ip netip.Addr, port int) bool
	// SteamWebAPI is where Steam's Web API is; empty means the real one.
	SteamWebAPI string
	// PlayerAddr makes the address the panel asks a game server's query
	// and remote console at; nil uses the container's own. Tests set it.
	PlayerAddr func(ip netip.Addr, port int) string
	// PlayerCount replaces the query that counts the players of a game
	// server; nil asks the server itself.
	PlayerCount func(ctx context.Context, steamGame bool, addr netip.AddrPort) (int, error)

	guards  *guards
	loops   loops
	deploys deploys
	gh      ghCache
	crashes crashes
	samples samples
	meter   meter
	powUsed powUsed
	// webhooks limits the GitHub webhook bodies read at once.
	webhooks webhookGate
	// Console tokens already used, and the consoles being watched.
	consoleUsed powUsed
	consoles    consoleHubs
	consoleHist consoleHistory
	sizes       volumeSizes
	// Releases returns the latest release; tests replace GitHub.
	Releases func(ctx context.Context) (Release, error)
	releases releases
	// imageUpdates are newer builds of the tags apps run.
	imageUpdates imageUpdates
	// steam is what Steam last said the newest builds of games are.
	steam steamTracker
	sftp  sftpState

	// steamProfiles looks up players on Steam; banKeepers lift bans that
	// run out, and banLocks keep them from undoing a new ban.
	steamProfiles players.Steam
	steamOnce     sync.Once
	banKeepers    banKeepers
	banLocks      banLocks

	// externalMu keeps two requests from picking the same port, and holds
	// back the job that retries held ports while one changes a database's
	// access. It guards held too.
	externalMu sync.Mutex
	// held are the databases whose outside access port something else holds.
	held externalHeld
	// backupBusy holds the databases being backed up or restored.
	backupBusy keyset
	// uploading holds the ids of backups being sent off-site, and
	// uploadKick wakes the sender when there is a new one.
	uploading  keyset
	uploadKick chan struct{}
	pauses     pauses
	// schedulesBusy holds the schedules whose tasks are running.
	schedulesBusy keyset
	// gameRuns and watchers follow the game servers' containers.
	gameRuns gameRuns
	watchers sync.WaitGroup
	restores restores
	dumps    dumpUploads
	exports  exports
	// jobs are backups and restores running in the background.
	jobs sync.WaitGroup
	ctx  context.Context // lives as long as the server

	// cloneFailures remembers why copies of servers failed, for the page of
	// the server that is no more.
	cloneFailures cloneFailures
}

// baseContext is for work that outlives the request that started it.
func (s *Server) baseContext() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// requestHost is the name or address a request was made to, without the
// port. An IPv6 address comes without its brackets, the way the proxy's
// configuration keeps it.
func requestHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		return h
	}
	return strings.Trim(r.Host, "[]")
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	local := http.NewServeMux()
	local.HandleFunc("POST /local/setup-link", s.setupLink)
	local.HandleFunc("POST /local/reset-link", s.resetLink)
	local.HandleFunc("GET /local/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version.Get().Version})
	})

	s.guards = newGuards(s.Store, s.Log)
	web := http.NewServeMux()
	web.HandleFunc("GET /api/setup", s.setupStatus)
	web.HandleFunc("POST /api/setup", s.setup)
	web.HandleFunc("POST /api/login", s.login)
	web.HandleFunc("POST /api/reset", s.resetLogin)
	web.HandleFunc("POST /api/login/totp", s.loginTOTP)
	web.HandleFunc("POST /api/login/recovery", s.loginRecovery)
	web.HandleFunc("POST /api/login/passkey/options", s.loginPasskeyOptions)
	web.HandleFunc("POST /api/login/passkey", s.loginPasskey)
	web.HandleFunc("GET /api/me", s.me)
	web.HandleFunc("POST /api/logout", s.logout)
	web.HandleFunc("POST /api/2fa/totp/new", s.enrolling(s.newTOTP))
	web.HandleFunc("POST /api/2fa/totp", s.enrolling(s.confirmTOTP))
	web.HandleFunc("POST /api/2fa/passkey/options", s.enrolling(s.passkeyOptions))
	web.HandleFunc("POST /api/2fa/passkey", s.enrolling(s.addPasskey))
	web.HandleFunc("DELETE /api/2fa/totp", s.confirmed(s.removeTOTP))
	web.HandleFunc("DELETE /api/2fa/passkey/{id}", s.confirmed(s.removePasskey))
	web.HandleFunc("POST /api/2fa/recovery", s.confirmed(s.newRecoveryCodes))
	web.HandleFunc("GET /api/account", s.signedIn(s.account))
	web.HandleFunc("POST /api/account/password", s.confirmed(s.changePassword))
	web.HandleFunc("POST /api/confirm", s.signedIn(s.confirm))
	web.HandleFunc("POST /api/confirm/passkey/options", s.signedIn(s.confirmPasskeyOptions))
	web.HandleFunc("POST /api/confirm/passkey", s.signedIn(s.confirmPasskey))
	web.HandleFunc("DELETE /api/sessions/{id}", s.signedIn(s.endSession))
	web.HandleFunc("POST /api/sessions/end-others", s.signedIn(s.endOtherSessions))
	web.HandleFunc("GET /api/apps", s.signedIn(s.listApps))
	web.HandleFunc("POST /api/apps", s.signedIn(s.createApp))
	web.HandleFunc("GET /api/apps/{app}", s.signedIn(s.getApp))
	web.HandleFunc("PATCH /api/apps/{app}", s.signedIn(s.updateApp))
	web.HandleFunc("DELETE /api/apps/{app}", s.signedIn(s.deleteApp))
	web.HandleFunc("PUT /api/apps/{app}/env", s.signedIn(s.setEnv))
	web.HandleFunc("POST /api/apps/{app}/deployments", s.signedIn(s.newDeployment))
	web.HandleFunc("POST /api/apps/{app}/restart", s.signedIn(s.restartApp))
	web.HandleFunc("POST /api/apps/{app}/update", s.signedIn(s.updateImage))
	web.HandleFunc("GET /api/apps/{app}/metrics", s.signedIn(s.appMetrics))
	web.HandleFunc("POST /api/apps/{app}/upgrade", s.confirmed(s.upgradeDatabase))
	web.HandleFunc("GET /api/apps/{app}/kept-volumes", s.signedIn(s.listKeptVolumes))
	web.HandleFunc("DELETE /api/apps/{app}/kept-volumes/{id}", s.confirmed(s.deleteKeptVolume))
	web.HandleFunc("POST /api/apps/{app}/stop", s.signedIn(s.stopHandler))
	web.HandleFunc("POST /api/apps/{app}/start", s.signedIn(s.startHandler))
	web.HandleFunc("POST /api/apps/{app}/deployments/{id}/rollback", s.signedIn(s.rollback))
	web.HandleFunc("GET /api/apps/{app}/deployments/{id}/log", s.signedIn(s.deploymentLog))
	web.HandleFunc("GET /api/apps/{app}/logs", s.signedIn(s.appLogs))
	web.HandleFunc("GET /api/apps/{app}/usage", s.signedIn(s.appUsage))
	web.HandleFunc("GET /api/apps/{app}/volumes", s.signedIn(s.listVolumes))
	web.HandleFunc("POST /api/apps/{app}/volumes", s.signedIn(s.addVolume))
	web.HandleFunc("PATCH /api/apps/{app}/volumes/{id}", s.signedIn(s.updateVolume))
	web.HandleFunc("DELETE /api/apps/{app}/volumes/{id}", s.signedIn(s.deleteVolume))
	web.HandleFunc("GET /api/apps/{app}/links", s.signedIn(s.listLinks))
	web.HandleFunc("POST /api/apps/{app}/links", s.signedIn(s.addLink))
	web.HandleFunc("PATCH /api/apps/{app}/links/{db}", s.signedIn(s.updateLink))
	web.HandleFunc("DELETE /api/apps/{app}/links/{db}", s.signedIn(s.deleteLink))
	web.HandleFunc("POST /api/apps/{app}/password", s.confirmed(requireAdmin(s.showPassword)))
	web.HandleFunc("GET /api/databases/engines", s.signedIn(s.listEngines))
	web.HandleFunc("POST /api/databases", s.signedIn(s.createDatabase))
	web.HandleFunc("GET /api/apps/{app}/backups", s.signedIn(s.listBackups))
	web.HandleFunc("POST /api/apps/{app}/backups", s.signedIn(s.backUpNow))
	web.HandleFunc("PUT /api/apps/{app}/backups/plan", s.signedIn(s.setBackupPlan))
	web.HandleFunc("GET /api/apps/{app}/data/tables", s.signedIn(s.dataTables))
	web.HandleFunc("POST /api/apps/{app}/data/rows", s.signedIn(s.dataRows))
	web.HandleFunc("POST /api/apps/{app}/data/export", s.confirmed(s.startExport))
	web.HandleFunc("GET /api/server", s.signedIn(s.serverInfo))
	web.HandleFunc("POST /api/server/check", s.signedIn(s.checkReleaseNow))
	web.HandleFunc("POST /api/server/update", s.confirmed(s.startUpdate))
	web.HandleFunc("GET /api/apps/{app}/external", s.signedIn(s.getExternal))
	web.HandleFunc("POST /api/apps/{app}/external", s.confirmed(s.setExternal))
	web.HandleFunc("PUT /api/apps/{app}/external", s.signedIn(s.moveExternal))
	web.HandleFunc("DELETE /api/apps/{app}/external", s.signedIn(s.removeExternal))
	web.HandleFunc("GET /api/apps/{app}/data/export/{token}", s.signedIn(s.downloadExport))
	web.HandleFunc("POST /api/apps/{app}/data/keys", s.signedIn(s.dataKeys))
	web.HandleFunc("POST /api/apps/{app}/data/key", s.signedIn(s.dataKey))
	web.HandleFunc("POST /api/apps/{app}/uploads", s.confirmed(s.startDumpUpload))
	web.HandleFunc("PUT /api/apps/{app}/uploads/{id}", s.signedIn(s.appendDumpUpload))
	web.HandleFunc("DELETE /api/apps/{app}/uploads/{id}", s.signedIn(s.cancelDumpUpload))
	web.HandleFunc("POST /api/apps/{app}/uploads/{id}/finish", s.signedIn(s.finishDumpUpload))
	web.HandleFunc("GET /api/backups/deleted", s.signedIn(s.listDeletedBackups))
	web.HandleFunc("GET /api/backups/{id}/download", s.signedIn(ownPageOnly(s.downloadBackup)))
	web.HandleFunc("POST /api/backups/{id}/restore", s.confirmed(s.restoreBackup))
	web.HandleFunc("DELETE /api/backups/{id}", s.confirmed(s.deleteBackup))
	web.HandleFunc("GET /api/backups/recovery", s.confirmed(ownPageOnly(s.recoveryFile)))
	web.HandleFunc("POST /api/backups/{id}/offsite", s.signedIn(s.sendNow))
	web.HandleFunc("GET /api/offsite", s.signedIn(s.getOffsite))
	web.HandleFunc("PUT /api/offsite", s.confirmed(s.setOffsite))
	web.HandleFunc("DELETE /api/offsite", s.confirmed(s.removeOffsiteStorage))
	web.HandleFunc("GET /api/offsite/found", s.signedIn(s.findOffsite))
	web.HandleFunc("POST /api/offsite/found", s.signedIn(s.addFound))
	web.HandleFunc("POST /api/backups/keys", s.confirmed(s.addOldKey))
	web.HandleFunc("GET /api/nodes/{node}", s.signedIn(s.getNode))
	web.HandleFunc("PUT /api/nodes/{node}", s.adminOnly(s.setNode))
	web.HandleFunc("GET /api/nodes/{node}/allocations", s.adminOnly(s.listAllocations))
	web.HandleFunc("GET /api/nodes/{node}/allocations/suggest", s.adminOnly(s.suggestAllocations))
	web.HandleFunc("POST /api/nodes/{node}/allocations", s.adminOnly(s.addAllocations))
	web.HandleFunc("DELETE /api/nodes/{node}/allocations/{id}", s.adminOnly(s.deleteAllocation))
	web.HandleFunc("GET /api/eggs/catalog", s.adminOnly(s.eggCatalog))
	web.HandleFunc("POST /api/eggs/preview", s.adminOnly(s.eggPreview))
	web.HandleFunc("POST /api/games", s.adminOnly(s.createGame))
	web.HandleFunc("POST /api/apps/files", s.adminOnly(s.createFilesApp))
	web.HandleFunc("GET /api/games/{app}", s.adminOnly(s.getGame))
	web.HandleFunc("POST /api/games/{app}/eula", s.adminOnly(s.acceptEULA))
	web.HandleFunc("PUT /api/games/{app}/variables", s.managesGame(s.updateGameSettings))
	web.HandleFunc("PUT /api/games/{app}/ports", s.managesGame(s.updateGamePorts))
	web.HandleFunc("PUT /api/games/{app}/resources", s.managesGame(s.updateGameResources))
	web.HandleFunc("GET /api/games/{app}/files/list", s.managesGame(s.listGameFiles))
	web.HandleFunc("GET /api/games/{app}/files/content", s.managesGame(s.readGameFile))
	web.HandleFunc("PUT /api/games/{app}/files/content", s.managesGame(s.writeGameFile))
	web.HandleFunc("POST /api/games/{app}/files/mkdir", s.managesGame(s.makeGameFolder))
	web.HandleFunc("POST /api/games/{app}/files/rename", s.managesGame(s.renameGameFile))
	web.HandleFunc("POST /api/games/{app}/files/delete", s.managesGame(s.deleteGameFiles))
	web.HandleFunc("PUT /api/games/{app}/files/upload", s.managesGame(s.uploadGameFile))
	web.HandleFunc("GET /api/games/{app}/files/download", s.managesGame(s.downloadGameFile))
	web.HandleFunc("POST /api/games/{app}/files/compress", s.managesGame(s.compressGameFiles))
	web.HandleFunc("POST /api/games/{app}/files/extract", s.managesGame(s.extractGameFile))
	web.HandleFunc("GET /api/games/{app}/files/favorites", s.managesGame(s.listFileFavorites))
	web.HandleFunc("PUT /api/games/{app}/files/favorites", s.managesGame(s.addFileFavorite))
	web.HandleFunc("DELETE /api/games/{app}/files/favorites", s.managesGame(s.removeFileFavorite))
	web.HandleFunc("GET /api/games/{app}/backups", s.gameBackups(s.listBackups))
	web.HandleFunc("POST /api/games/{app}/backups", s.gameBackups(s.backUpNow))
	web.HandleFunc("PUT /api/games/{app}/backups/plan", s.gameBackups(s.setBackupPlan))
	web.HandleFunc("GET /api/games/{app}/backups/{id}/download", s.gameBackups(ownPageOnly(s.downloadBackup)))
	web.HandleFunc("POST /api/games/{app}/backups/{id}/restore", s.gameBackupsConfirmed(s.restoreBackup))
	web.HandleFunc("DELETE /api/games/{app}/backups/{id}", s.gameBackupsConfirmed(s.deleteBackup))
	web.HandleFunc("POST /api/games/{app}/backups/{id}/offsite", s.gameBackups(s.sendNow))
	web.HandleFunc("GET /api/games/{app}/schedules", s.managesGame(s.listSchedules))
	web.HandleFunc("POST /api/games/{app}/schedules", s.managesGame(s.createSchedule))
	web.HandleFunc("PUT /api/games/{app}/schedules/{id}", s.managesGame(s.updateSchedule))
	web.HandleFunc("DELETE /api/games/{app}/schedules/{id}", s.managesGame(s.deleteSchedule))
	web.HandleFunc("POST /api/games/{app}/schedules/{id}/run", s.managesGame(s.runScheduleNow))
	web.HandleFunc("GET /api/games/{app}/sftp", s.managesGame(s.getGameSFTP))
	web.HandleFunc("POST /api/games/{app}/sftp/password", s.confirmed(requireAdmin(s.newGameSFTPPassword)))
	web.HandleFunc("DELETE /api/games/{app}/sftp/password", s.confirmed(requireAdmin(s.removeGameSFTPPassword)))
	web.HandleFunc("GET /api/sftp", s.signedIn(s.getSFTP))
	web.HandleFunc("PUT /api/sftp", s.adminOnly(s.setSFTP))
	web.HandleFunc("GET /api/account/ssh-keys", s.signedIn(s.listSSHKeys))
	web.HandleFunc("POST /api/account/ssh-keys", s.confirmed(s.addSSHKey))
	web.HandleFunc("DELETE /api/account/ssh-keys/{id}", s.confirmed(s.deleteSSHKey))
	web.HandleFunc("GET /api/games/{app}/diagnosis", s.managesGame(s.getDiagnosis))
	web.HandleFunc("POST /api/games/{app}/diagnosis/fix", s.managesGame(s.applyDiagnosisFix))
	web.HandleFunc("POST /api/games/{app}/reinstall", s.adminOnly(s.reinstallGame))
	web.HandleFunc("POST /api/games/{app}/clone", s.managesGame(s.cloneGame))
	web.HandleFunc("GET /api/games/{app}/players/online", s.managesGame(s.onlinePlayers))
	web.HandleFunc("GET /api/games/{app}/players", s.managesGame(s.listPlayers))
	web.HandleFunc("GET /api/games/{app}/players/{id}", s.managesGame(s.getPlayer))
	web.HandleFunc("POST /api/games/{app}/players/{id}/kick", s.managesGame(s.kickPlayer))
	web.HandleFunc("POST /api/games/{app}/players/{id}/ban", s.managesGame(s.banPlayer))
	web.HandleFunc("POST /api/games/{app}/players/{id}/notes", s.managesGame(s.addPlayerNote))
	web.HandleFunc("DELETE /api/games/{app}/players/{id}/notes/{note}", s.managesGame(s.deletePlayerNote))
	web.HandleFunc("POST /api/games/{app}/players/{id}/op", s.managesGame(s.opPlayer))
	web.HandleFunc("POST /api/games/{app}/players/{id}/whitelist", s.managesGame(s.whitelistPlayer))
	web.HandleFunc("GET /api/games/{app}/chat", s.managesGame(s.playerChat))
	web.HandleFunc("GET /api/games/{app}/reports", s.managesGame(s.playerReports))
	web.HandleFunc("GET /api/games/{app}/bans", s.managesGame(s.listBans))
	web.HandleFunc("DELETE /api/games/{app}/bans/{ban}", s.managesGame(s.unbanPlayer))
	web.HandleFunc("GET /api/games/{app}/audit", s.managesGame(s.gameAudit))
	web.HandleFunc("PUT /api/server/steam-key", s.confirmed(requireAdmin(s.setSteamKey)))
	web.HandleFunc("DELETE /api/server/steam-key", s.confirmed(requireAdmin(s.removeSteamKey)))
	web.HandleFunc("PUT /api/games/{app}/steam", s.adminOnly(s.setSteam))
	web.HandleFunc("POST /api/games/{app}/steam/update", s.adminOnly(s.updateSteam))
	web.HandleFunc("POST /api/games/{app}/power", s.adminOnly(s.gamePower))
	web.HandleFunc("POST /api/games/{app}/console/token", s.adminOnly(s.consoleToken))
	// Not behind signedIn: a browser's socket carries the token instead,
	// which consoleSocket checks together with the session.
	web.HandleFunc("GET /api/games/{app}/console", s.consoleSocket)
	web.HandleFunc("GET /api/host", s.signedIn(s.hostInfo))
	web.HandleFunc("GET /api/github", s.signedIn(s.githubStatus))
	web.HandleFunc("POST /api/github/manifest", s.confirmed(s.githubManifest))
	web.HandleFunc("POST /api/github/app", s.signedIn(s.githubCreated))
	web.HandleFunc("DELETE /api/github", s.confirmed(s.githubDisconnect))
	web.HandleFunc("GET /api/github/repos", s.signedIn(s.githubRepos))
	web.HandleFunc("POST /api/github/webhook", s.githubWebhook)
	web.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, errNoEndpoint.Err())
	})
	web.Handle("GET /", webui.Handler())
	// Browsers say where a request comes from; anything that changes state
	// must come from the panel's own pages.
	webSafe := secureHeaders(http.NewCrossOriginProtection().Handler(web))

	main := peer.Require(peer.Policy{UIDs: []uint32{s.ProxyUID}}, s.Log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := peer.From(r.Context())
		if p.UID == 0 {
			local.ServeHTTP(w, r)
			return
		}
		webSafe.ServeHTTP(w, r)
	}))
	// The SFTP service gets its own routes and none of the others.
	sftpOnly := s.sftpRoutes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := peer.From(r.Context()); ok && s.SFTPUID != nil {
			if uid := s.SFTPUID(); uid != 0 && p.UID == uid {
				sftpOnly.ServeHTTP(w, r)
				return
			}
		}
		main.ServeHTTP(w, r)
	})
}

// waitForCore returns once the core answers, or false when ctx ends first.
func (s *Server) waitForCore(ctx context.Context) bool {
	for i := 0; ; i++ {
		if _, err := s.Core.Host(ctx); err == nil {
			return true
		} else if i == 30 {
			s.Log.Warn("still waiting for the core", "err", err)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// Serve listens on the socket until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.ctx = ctx
	s.uploadKick = make(chan struct{}, 1)
	if err := s.Store.FailUnfinished(ctx, s.now()); err != nil {
		return err
	}
	if err := s.Store.FailUnfinishedBackups(ctx, s.now()); err != nil {
		return err
	}
	if err := s.failInterruptedRestores(ctx); err != nil {
		return err
	}
	if err := s.Store.FailUnfinishedInstalls(ctx); err != nil {
		return err
	}
	if err := s.Store.FailUnfinishedRuns(ctx); err != nil {
		return err
	}
	// Everything that watches containers needs the core, which may still be
	// starting when the panel's service comes up.
	go func() {
		if !s.waitForCore(ctx) {
			return
		}
		s.pinLive(ctx)
		s.removeStaleInstalls(ctx)
		s.resumeGames(ctx)
		// These jobs run only for what exists, so each starts by looking
		// and ends at once when there is nothing; see wake in loops.go.
		s.loops.start(ctx)
		s.wakeSupervise()
		s.wakeVolumes()
		s.wakeBackups()
		s.wakeUploads()
		s.syncAllLinks(ctx)
		s.syncExternal(ctx)
		s.syncSFTPVolumes(ctx)
		s.syncSFTPPort(ctx)
		go s.runReleaseCheck(ctx)
		s.wakeImageCheck()
		s.wakeSteam()
		s.wakeMetrics()
		s.wakeSchedules()
		s.runImageSweep(ctx)
	}()
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// peer.Require decides who gets in; the file mode only lets them knock.
	if err := os.Chmod(socket, 0o666); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ConnContext:       peer.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.Log.Info("panel listening", "socket", socket)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Deployments stop with ctx; wait so none is cut off halfway through
	// writing its state.
	s.deploys.wg.Wait()
	s.waitForJobs()
	return nil
}

// waitForJobs gives the backups and restores that are running time to end.
// A restore goes on after ctx ends, and stopping now would leave its
// database emptied and the way back unused.
func (s *Server) waitForJobs() {
	finished := make(chan struct{})
	go func() {
		s.jobs.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(jobsGrace):
		s.Log.Warn("stopping while backups or restores are still running")
	}
}

// setupLink makes a new one-time setup token. Only root on the server can ask
// for one, and only while the panel has no users.
func (s *Server) setupLink(w http.ResponseWriter, r *http.Request) {
	token := make([]byte, 32)
	rand.Read(token)
	hash := sha256.Sum256(token)
	err := s.Store.SetSetupToken(r.Context(), hash[:], s.now().Add(setupLinkTTL))
	switch {
	case errors.Is(err, store.ErrSetupDone):
		writeError(w, errSetupDone.Err())
		return
	case err != nil:
		s.fail(w, "setup link", err)
		return
	}
	s.Log.Info("setup link created")
	writeJSON(w, http.StatusOK, map[string]string{"token": base64.RawURLEncoding.EncodeToString(token)})
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	open, err := s.Store.SetupOpen(r.Context())
	if err != nil {
		s.fail(w, "setup status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"open": open})
}

// secureHeaders applies to every web response, the API included.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

var errOwnPageOnly = msg.Define(http.StatusForbidden, "server.own_page_only", "This only works from the panel's own page.")

// ownPageOnly is for GET routes that do more than read. The cross-origin
// check leaves GET alone, and the session cookie is SameSite=Lax, so it still
// goes along with a link from another site or an image on a page of another
// app on the same domain. Browsers say where a request comes from, and one
// that does not come from the panel's own page is refused. Without the
// header the request is not from a browser, or from one too old to say.
func ownPageOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			writeError(w, errOwnPageOnly.Err())
			return
		}
		next(w, r)
	}
}

// fail logs an internal error and tells the client only that something went
// wrong, so details of the server never reach the browser.
var (
	errServer     = msg.Define(http.StatusInternalServerError, "server.failed", "Something went wrong on the server.")
	errNoEndpoint = msg.Define(http.StatusNotFound, "server.no_endpoint", "There is no such API endpoint.")
	errSetupDone  = msg.Define(http.StatusConflict, "setup.done", "Setup is already done.")
	errBadBody    = msg.Define(http.StatusBadRequest, "server.bad_request", "The request was not understood.")
)

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	writeError(w, errServer.Err())
}

// failWith sends err to the user: its own message when it has one, what
// the core refused, or else the generic failure, logged.
func (s *Server) failWith(w http.ResponseWriter, what string, err error) {
	var m *msg.Error
	var ce *core.Error
	switch {
	case errors.As(err, &m):
		writeError(w, m)
	case errors.As(err, &ce):
		s.coreFailed(w, what, err)
	default:
		s.fail(w, what, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// errorJSON is how an error goes to the web interface: its message in
// English, and its code and values to show it in the user's language.
type errorJSON struct {
	Error  string         `json:"error"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	// Confirm asks the user to confirm it is them and try again.
	Confirm bool `json:"confirm,omitempty"`
}

// writeError sends a message defined with msg.Define. There is no way to
// send bare text: everything a user reads can be translated.
func writeError(w http.ResponseWriter, e *msg.Error) {
	writeJSON(w, e.Status, errorJSON{Error: e.Text, Code: e.Code, Params: e.Params})
}

// Client talks to the panel's socket from the server itself.
type Client struct{ http *http.Client }

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// Version asks the running panel which version it is.
func (c *Client) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://panel/local/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("panel answered " + resp.Status)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out)
	return out.Version, err
}

// ErrSetupDone means an administrator exists, so there is no setup link.
var ErrSetupDone = errors.New("an administrator exists already")

// SetupLink asks the panel for a new setup token.
func (c *Client) SetupLink(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://panel/local/setup-link", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach the Zelie panel: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("panel answered %s", resp.Status)
	}
	if out.Code == "setup.done" {
		return "", ErrSetupDone
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(out.Error)
	}
	return out.Token, nil
}
