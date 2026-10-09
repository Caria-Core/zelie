package panel

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

// sealSteamKey binds a sealed Steam key to its field.
const sealSteamKey = "steam-web-key"

// banCheckEvery is how often a server with timed bans looks for ones that
// have run out. Tests shorten it.
var banCheckEvery = time.Minute

var (
	errSteamKeyShape       = msg.Define(http.StatusBadRequest, "players.steam_key_shape", "A Steam Web API key has 32 letters and digits.")
	errSteamKeyRejected    = msg.Define(http.StatusBadRequest, "players.steam_key_rejected", "Steam did not accept that key.")
	errSteamKeyUnreachable = msg.Define(http.StatusBadGateway, "players.steam_unreachable", "Steam could not be reached to check the key. Try again in a moment.")
)

// banKeepers has one entry for each running server that has timed bans.
type banKeepers struct {
	mu sync.Mutex
	m  map[string]*banKeeper
}

type banKeeper struct {
	container string
	cancel    context.CancelFunc
}

// banLocks holds a server's bans and pardons to one at a time, from the
// check that decides on a command to the record of it. Without it a pardon
// for a ban that ran out could land after a new ban of the same player.
type banLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock waits for the server's turn and returns the function that ends it.
func (b *banLocks) lock(app string) (unlock func()) {
	b.mu.Lock()
	if b.m == nil {
		b.m = map[string]*sync.Mutex{}
	}
	l := b.m[app]
	if l == nil {
		l = new(sync.Mutex)
		b.m[app] = l
	}
	b.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// gameReady is called once when a server's game says it is ready. Bans that
// ran out while it was off are lifted now, since the game can be told.
func (s *Server) gameReady(app, container string) {
	s.watchers.Add(1)
	go func() {
		defer s.watchers.Done()
		s.keepBans(app, container)
	}()
}

// keepBans lifts bans of a running server as they run out, until none are
// left or the container is gone. Nothing runs for a server without timed
// bans. It returns at once when the container is being watched already.
func (s *Server) keepBans(app, container string) {
	base := s.baseContext()
	if has, err := s.Store.HasTimedBans(base, app); err != nil || !has {
		return
	}
	ctx, cancel := context.WithCancel(base)
	k := &banKeeper{container: container, cancel: cancel}
	s.banKeepers.mu.Lock()
	old := s.banKeepers.m[app]
	if old != nil && old.container == container {
		s.banKeepers.mu.Unlock()
		cancel()
		return
	}
	if old != nil {
		old.cancel()
	}
	if s.banKeepers.m == nil {
		s.banKeepers.m = map[string]*banKeeper{}
	}
	s.banKeepers.m[app] = k
	s.banKeepers.mu.Unlock()
	defer func() {
		cancel()
		s.banKeepers.mu.Lock()
		if s.banKeepers.m[app] == k {
			delete(s.banKeepers.m, app)
		}
		s.banKeepers.mu.Unlock()
	}()

	for {
		s.liftExpiredBans(ctx, app)
		if has, err := s.Store.HasTimedBans(ctx, app); err == nil && !has {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(banCheckEvery):
		}
		c, ok, err := s.findGameContainer(ctx, app)
		if err != nil {
			// The core may be restarting; the server runs on.
			s.Log.Info("players: could not look for the server", "server", app, "err", err)
			continue
		}
		if !ok || c != container {
			return
		}
	}
}

// liftExpiredBans tells the game to forget the bans that ran out and marks
// them lifted. One the game could not be told about stays, to be tried at
// the next check.
func (s *Server) liftExpiredBans(ctx context.Context, app string) {
	now := s.now()
	expired, err := s.Store.ExpiredPlayerBans(ctx, app, now)
	if err != nil || len(expired) == 0 {
		if err != nil {
			s.Log.Error("expired bans", "server", app, "err", err)
		}
		return
	}
	pg, err := s.loadPlayerGame(ctx, app)
	if err != nil {
		s.Log.Error("expired bans", "server", app, "err", err)
		return
	}
	container, ip, running := s.runningAddr(ctx, app)
	if !running {
		return
	}
	for _, b := range expired {
		if !s.liftExpiredBan(ctx, pg, container, ip, b, now) {
			return
		}
	}
}

// liftExpiredBan tells the game to forget one ban that ran out, unless the
// player is banned again, and marks it lifted. It returns false when the game
// could not be told or the database failed, which ends the round.
func (s *Server) liftExpiredBan(ctx context.Context, pg playerGame, container string, ip netip.Addr, b store.PlayerBan, now time.Time) bool {
	app := pg.app.ID
	defer s.banLocks.lock(app)()
	// A player banned again since this one ran out is banned in the game by
	// the new ban: pardoning them would undo it. Minecraft bans and pardons
	// by name, so a ban kept under another id of the same name counts.
	name := ""
	if pg.family == players.GameMinecraft {
		name = b.Name
	}
	held, err := s.Store.BanInForce(ctx, app, b.PlayerID, name, now)
	if err != nil {
		s.Log.Error("expired bans", "server", app, "err", err)
		return false
	}
	if !held {
		if pg.family == players.GameMinecraft && !minecraftName.MatchString(b.Name) {
			return true
		}
		if err := s.sendPlayerCommands(ctx, pg, container, ip, unbanCommands(pg.family, b.PlayerID, b.Name)...); err != nil {
			s.Log.Info("players: could not lift an expired ban", "server", app, "err", err)
			return false
		}
	}
	if lifted, err := s.Store.LiftPlayerBan(ctx, app, b.ID, 0, now); err != nil {
		s.Log.Error("lift ban", "server", app, "err", err)
	} else if lifted {
		if err := s.Store.AddAudit(ctx, now, 0, app, auditBanExpired, b.PlayerID, b.Name); err != nil {
			s.Log.Error("audit", "err", err)
		}
	}
	return true
}

// profiles is the Steam lookup, set up on first use.
func (s *Server) profiles() *players.Steam {
	s.steamOnce.Do(func() {
		s.steamProfiles.BaseURL, s.steamProfiles.Now = s.SteamWebAPI, s.now
	})
	return &s.steamProfiles
}

// steamKey is the saved Steam key, empty when there is none.
func (s *Server) steamKey(ctx context.Context) string {
	sealed, err := s.Store.SteamKey(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Log.Error("steam key", "err", err)
		}
		return ""
	}
	key, err := s.Sealer.Open(sealed, sealSteamKey)
	if err != nil {
		s.Log.Error("steam key", "err", err)
		return ""
	}
	return string(key)
}

// steamFor looks players up on Steam, only when a key is saved.
func (s *Server) steamFor(ctx context.Context, ids []string) map[string]*players.SteamInfo {
	key := s.steamKey(ctx)
	if key == "" {
		return nil
	}
	return s.profiles().Lookup(ctx, key, ids)
}

func (s *Server) setSteamKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.Key)
	if !players.ValidSteamKey(key) {
		writeError(w, errSteamKeyShape.Err())
		return
	}
	ctx := r.Context()
	switch err := s.profiles().Check(ctx, key); {
	case errors.Is(err, players.ErrSteamKeyRejected):
		writeError(w, errSteamKeyRejected.Err())
		return
	case err != nil:
		s.Log.Info("steam key check", "err", err)
		writeError(w, errSteamKeyUnreachable.Err())
		return
	}
	if err := s.Store.SetSteamKey(ctx, s.Sealer.Seal([]byte(key), sealSteamKey)); err != nil {
		s.fail(w, "save steam key", err)
		return
	}
	s.profiles().Forget()
	s.audit(ctx, "", auditSteamKeySet, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeSteamKey(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteSteamKey(r.Context()); err != nil {
		s.fail(w, "remove steam key", err)
		return
	}
	s.profiles().Forget()
	s.audit(r.Context(), "", auditSteamKeyRemoved, "")
	w.WriteHeader(http.StatusNoContent)
}
