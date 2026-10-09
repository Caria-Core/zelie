package panel

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

// Audit actions. They are short codes: the interface words them.
const (
	auditKick            = "kick"
	auditBan             = "ban"
	auditUnban           = "unban"
	auditBanExpired      = "ban_expired"
	auditOp              = "op"
	auditDeop            = "deop"
	auditWhitelistAdd    = "whitelist_add"
	auditWhitelistRemove = "whitelist_remove"
	auditNoteAdd         = "note_add"
	auditNoteDelete      = "note_delete"
	auditSteamKeySet     = "steam_key_set"
	auditSteamKeyRemoved = "steam_key_removed"
)

const (
	// maxBanMinutes is ten years.
	maxBanMinutes = 10 * 365 * 24 * 60
	maxNoteLen    = 1000
)

var (
	errNoPlayer      = msg.Define(http.StatusNotFound, "players.not_found", "There is no such player.")
	errBadPlayerID   = msg.Define(http.StatusBadRequest, "players.bad_id", "That is not a valid player id for this game.")
	errBadPlayerName = msg.Define(http.StatusBadRequest, "players.bad_name", "This player's name cannot be used in a console command.")
	errBadBanLength  = msg.Define(http.StatusBadRequest, "players.bad_ban_length", "The ban length must be 0, for good, or at most {max} minutes.")
	errAlreadyBanned = msg.Define(http.StatusConflict, "players.already_banned", "This player is banned already.")
	errNameReused    = msg.Define(http.StatusConflict, "players.name_reused", "Another player has used this name since, so the command would reach them instead.")
	errNoBan         = msg.Define(http.StatusNotFound, "players.ban_not_found", "There is no such ban.")
	errBanOver       = msg.Define(http.StatusConflict, "players.ban_over", "This ban is over already.")
	errBadNote       = msg.Define(http.StatusBadRequest, "players.bad_note", "A note takes 1 to 1000 characters, and a tag of watch, suspect, alt, vip or other, or none.")
	errNoNote        = msg.Define(http.StatusNotFound, "players.note_not_found", "There is no such note.")
)

var minecraftName = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

type playerJSON struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	FirstSeen   time.Time          `json:"first_seen"`
	LastSeen    time.Time          `json:"last_seen"`
	LastIP      string             `json:"last_ip"`
	PlaySeconds int64              `json:"play_seconds"`
	Online      bool               `json:"online"`
	Banned      bool               `json:"banned"`
	Notes       int                `json:"notes"`
	Steam       *players.SteamInfo `json:"steam"`
}

// playersOut adds the bans, notes and Steam profiles to a page of players.
func (s *Server) playersOut(ctx context.Context, app string, list []store.Player) ([]playerJSON, error) {
	ids := make([]string, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	flags, err := s.Store.PlayerFlagsFor(ctx, app, ids, s.now())
	if err != nil {
		return nil, err
	}
	steam := s.steamFor(ctx, ids)
	out := make([]playerJSON, 0, len(list))
	for _, p := range list {
		f := flags[p.ID]
		out = append(out, playerJSON{ID: p.ID, Name: p.Name, FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, LastIP: p.LastIP,
			PlaySeconds: p.PlaySeconds, Online: p.Online, Banned: f.Banned, Notes: f.Notes, Steam: steam[p.ID]})
	}
	return out, nil
}

// pageOf reads limit and offset from the query.
func pageOf(r *http.Request, defLimit, maxLimit int) (limit, offset int) {
	limit, offset = defLimit, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, maxLimit)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = n
	}
	return limit, offset
}

func (s *Server) listPlayers(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	search := r.URL.Query().Get("search")
	limit, offset := pageOf(r, 50, 200)
	total, err := s.Store.CountPlayers(ctx, a.ID, search)
	if err != nil {
		s.fail(w, "count players", err)
		return
	}
	list, err := s.Store.Players(ctx, a.ID, search, limit, offset)
	if err != nil {
		s.fail(w, "list players", err)
		return
	}
	out, err := s.playersOut(ctx, a.ID, list)
	if err != nil {
		s.fail(w, "list players", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "players": out})
}

type playerSessionJSON struct {
	JoinedAt time.Time  `json:"joined_at"`
	LeftAt   *time.Time `json:"left_at"`
	IP       string     `json:"ip"`
	Reason   string     `json:"reason"`
}

type sharedIPJSON struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	LastSeen time.Time `json:"last_seen"`
}

type playerNoteJSON struct {
	ID   int64     `json:"id"`
	Tag  string    `json:"tag"`
	Note string    `json:"note"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

type banJSON struct {
	ID        int64      `json:"id"`
	PlayerID  string     `json:"player_id"`
	Name      string     `json:"name"`
	Reason    string     `json:"reason"`
	By        string     `json:"by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	LiftedAt  *time.Time `json:"lifted_at"`
	LiftedBy  *string    `json:"lifted_by"`
	Active    bool       `json:"active"`
}

func (s *Server) banOut(b store.PlayerBan) banJSON {
	out := banJSON{ID: b.ID, PlayerID: b.PlayerID, Name: b.Name, Reason: b.Reason, By: b.By, CreatedAt: b.CreatedAt,
		ExpiresAt: timePtr(b.ExpiresAt), LiftedAt: timePtr(b.LiftedAt), Active: b.Active(s.now())}
	if !b.LiftedAt.IsZero() && b.LiftedBy != "" {
		out.LiftedBy = &b.LiftedBy
	}
	return out
}

type reportJSON struct {
	ReporterID   string    `json:"reporter_id"`
	ReporterName string    `json:"reporter_name"`
	Subject      string    `json:"subject"`
	Message      string    `json:"message"`
	At           time.Time `json:"at"`
}

func reportOut(r store.PlayerReport) reportJSON {
	return reportJSON{ReporterID: r.ReporterID, ReporterName: r.ReporterName, Subject: r.Subject, Message: r.Message, At: r.At}
}

func (s *Server) getPlayer(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	p, err := s.Store.Player(ctx, a.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errNoPlayer.Err())
		return
	}
	if err != nil {
		s.fail(w, "load player", err)
		return
	}
	one, err := s.playersOut(ctx, a.ID, []store.Player{p})
	if err != nil {
		s.fail(w, "load player", err)
		return
	}
	sessions, err1 := s.Store.PlayerSessions(ctx, a.ID, id, 50)
	links, err2 := s.Store.PlayersSharingIP(ctx, a.ID, id, 20)
	notes, err3 := s.Store.PlayerNotes(ctx, a.ID, id)
	bans, err4 := s.Store.PlayerBans(ctx, a.ID, id, true, s.now())
	reports, err5 := s.Store.PlayerReports(ctx, a.ID, id, 50)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		s.fail(w, "load player", err)
		return
	}
	out := struct {
		Player   playerJSON          `json:"player"`
		Sessions []playerSessionJSON `json:"sessions"`
		SharedIP []sharedIPJSON      `json:"shared_ip"`
		Notes    []playerNoteJSON    `json:"notes"`
		Bans     []banJSON           `json:"bans"`
		Reports  []reportJSON        `json:"reports"`
	}{Player: one[0], Sessions: []playerSessionJSON{}, SharedIP: []sharedIPJSON{}, Notes: []playerNoteJSON{}, Bans: []banJSON{}, Reports: []reportJSON{}}
	for _, x := range sessions {
		out.Sessions = append(out.Sessions, playerSessionJSON{JoinedAt: x.JoinedAt, LeftAt: timePtr(x.LeftAt), IP: x.IP, Reason: x.Reason})
	}
	for _, l := range links {
		out.SharedIP = append(out.SharedIP, sharedIPJSON{ID: l.PlayerID, Name: l.Name, IP: l.IP, LastSeen: l.LastSeen})
	}
	for _, n := range notes {
		out.Notes = append(out.Notes, playerNoteJSON{ID: n.ID, Tag: n.Tag, Note: n.Note, By: n.By, At: n.At})
	}
	for _, b := range bans {
		out.Bans = append(out.Bans, s.banOut(b))
	}
	for _, x := range reports {
		out.Reports = append(out.Reports, reportOut(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) playerChat(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := pageOf(r, 100, 200)
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	channel := q.Get("channel")
	if channel != "global" && channel != "team" {
		channel = ""
	}
	list, err := s.Store.SearchPlayerChat(r.Context(), a.ID, store.ChatFilter{Player: q.Get("player"), Channel: channel, Search: q.Get("search"), Before: max(before, 0), Limit: limit})
	if err != nil {
		s.fail(w, "list chat", err)
		return
	}
	type chatJSON struct {
		ID       int64     `json:"id"`
		PlayerID string    `json:"player_id"`
		Name     string    `json:"name"`
		Channel  string    `json:"channel"`
		Text     string    `json:"text"`
		At       time.Time `json:"at"`
	}
	out := make([]chatJSON, 0, len(list))
	for _, c := range list {
		out = append(out, chatJSON{c.ID, c.PlayerID, c.Name, c.Channel, c.Text, c.At})
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out})
}

const (
	reportsRead     = 1000
	reportsPerGroup = 20
)

// playerReports groups the recent reports by who they are about.
func (s *Server) playerReports(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Store.PlayerReports(r.Context(), a.ID, "", reportsRead)
	if err != nil {
		s.fail(w, "list reports", err)
		return
	}
	type target struct {
		TargetID   string       `json:"target_id"`
		TargetName string       `json:"target_name"`
		Count      int          `json:"count"`
		Reporters  int          `json:"reporters"`
		LastAt     time.Time    `json:"last_at"`
		Items      []reportJSON `json:"items"`
	}
	var groups []*target
	byID := map[string]*target{}
	reporters := map[string]map[string]bool{}
	// Newest first, so the first name seen is the latest one.
	for _, x := range list {
		t := byID[x.TargetID]
		if t == nil {
			t = &target{TargetID: x.TargetID, LastAt: x.At, Items: []reportJSON{}}
			byID[x.TargetID], reporters[x.TargetID] = t, map[string]bool{}
			groups = append(groups, t)
		}
		if t.TargetName == "" {
			t.TargetName = x.TargetName
		}
		t.Count++
		reporters[x.TargetID][x.ReporterID] = true
		if len(t.Items) < reportsPerGroup {
			t.Items = append(t.Items, reportOut(x))
		}
	}
	slices.SortStableFunc(groups, func(a, b *target) int { return b.LastAt.Compare(a.LastAt) })
	for _, t := range groups {
		t.Reporters = len(reporters[t.TargetID])
	}
	if groups == nil {
		groups = []*target{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": groups})
}

func (s *Server) listBans(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Store.PlayerBans(r.Context(), a.ID, "", r.URL.Query().Get("all") == "1", s.now())
	if err != nil {
		s.fail(w, "list bans", err)
		return
	}
	out := make([]banJSON, 0, len(list))
	for _, b := range list {
		out = append(out, s.banOut(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{"bans": out})
}

func (s *Server) gameAudit(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	limit, _ := pageOf(r, 50, 200)
	list, err := s.Store.Audit(r.Context(), a.ID, limit)
	if err != nil {
		s.fail(w, "list audit", err)
		return
	}
	type entryJSON struct {
		At     time.Time `json:"at"`
		By     string    `json:"by"`
		Action string    `json:"action"`
		Target string    `json:"target"`
		Detail string    `json:"detail"`
	}
	out := make([]entryJSON, 0, len(list))
	for _, e := range list {
		out = append(out, entryJSON{e.At, e.By, e.Action, e.Target, e.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// audit records what the signed-in account did. A failure is logged and
// does not undo what was done.
func (s *Server) audit(ctx context.Context, app, action, target string, detail ...string) {
	var parts []string
	for _, d := range detail {
		if d != "" {
			parts = append(parts, d)
		}
	}
	if err := s.Store.AddAudit(context.WithoutCancel(ctx), s.now(), loginFrom(ctx).account.ID, app, action, target, strings.Join(parts, " | ")); err != nil {
		s.Log.Error("audit", "action", action, "err", err)
	}
}

// playerAct is what an action on a player works on.
type playerAct struct {
	pg        playerGame
	player    store.Player
	container string
	ip        netip.Addr
}

// actOn finds the game, the player and the running server for an action
// that reaches the game. family limits it to one game family when set.
func (s *Server) actOn(w http.ResponseWriter, r *http.Request, family string) (playerAct, bool) {
	pg, ok := s.playerGameFrom(w, r)
	if !ok {
		return playerAct{}, false
	}
	if pg.cap != "full" || (family != "" && pg.family != family) {
		writeError(w, errPlayerNoSupport.Err())
		return playerAct{}, false
	}
	ctx, id := r.Context(), r.PathValue("id")
	p, err := s.Store.Player(ctx, pg.app.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound) && pg.family == players.GameRust && players.IsSteamID64(id):
		// Listed by the server a moment before the console log was read.
		p, err = store.Player{ID: id}, nil
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoPlayer.Err())
		return playerAct{}, false
	}
	if err != nil {
		s.fail(w, "load player", err)
		return playerAct{}, false
	}
	switch pg.family {
	case players.GameRust:
		if !players.IsSteamID64(p.ID) {
			writeError(w, errBadPlayerID.Err())
			return playerAct{}, false
		}
	case players.GameMinecraft:
		if !minecraftName.MatchString(p.Name) {
			writeError(w, errBadPlayerName.Err())
			return playerAct{}, false
		}
		// Commands go by name, and Mojang gives a name to someone else once
		// its owner has changed it. A player not seen since may have a name
		// that is another account's now. Only another id proves that: a
		// record keyed by the name is often the same person, whose id the
		// parser had forgotten.
		if !strings.HasPrefix(p.ID, "name:") {
			latest, err := s.Store.AccountByName(ctx, pg.app.ID, p.Name)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.fail(w, "load player", err)
				return playerAct{}, false
			}
			if err == nil && latest.ID != p.ID {
				writeError(w, errNameReused.Err())
				return playerAct{}, false
			}
		}
	}
	container, ip, running := s.runningAddr(ctx, pg.app.ID)
	if !running {
		writeError(w, errPlayerNotRunning.Err())
		return playerAct{}, false
	}
	return playerAct{pg: pg, player: p, container: container, ip: ip}, true
}

// run sends the commands and answers the request if they fail.
func (s *Server) runAct(w http.ResponseWriter, r *http.Request, act playerAct, cmds ...string) bool {
	if err := s.sendPlayerCommands(r.Context(), act.pg, act.container, act.ip, cmds...); err != nil {
		s.failWith(w, "player command", err)
		return false
	}
	return true
}

// kickCommand and the ones after build a console command for the game.
// Every text goes through CleanArg: it came from a person or from a player's
// name, and must not end its quotes or its line early.
func kickCommand(family, id, name, reason string) string {
	reason = cleanReason(reason)
	if family == players.GameRust {
		if reason == "" {
			return `kick "` + id + `"`
		}
		return `kick "` + id + `" "` + reason + `"`
	}
	return strings.TrimSpace("kick " + name + " " + reason)
}

func banCommands(family, id, name, reason string) []string {
	reason = cleanReason(reason)
	if family == players.GameRust {
		return []string{
			`banid "` + id + `" "` + players.CleanArg(name, 64) + `" "` + reason + `"`,
			kickCommand(family, id, name, reason),
			"server.writecfg",
		}
	}
	return []string{strings.TrimSpace("ban " + name + " " + reason)}
}

func unbanCommands(family, id, name string) []string {
	if family == players.GameRust {
		return []string{`unban "` + id + `"`, "server.writecfg"}
	}
	return []string{"pardon " + name}
}

func (s *Server) kickPlayer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	act, ok := s.actOn(w, r, "")
	if !ok || !s.runAct(w, r, act, kickCommand(act.pg.family, act.player.ID, act.player.Name, req.Reason)) {
		return
	}
	s.audit(r.Context(), act.pg.app.ID, auditKick, act.player.ID, act.player.Name, cleanReason(req.Reason))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) banPlayer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Minutes < 0 || req.Minutes > maxBanMinutes {
		writeError(w, errBadBanLength.Err("max", maxBanMinutes))
		return
	}
	act, ok := s.actOn(w, r, "")
	if !ok {
		return
	}
	ctx, app := r.Context(), act.pg.app.ID
	defer s.banLocks.lock(app)()
	now := s.now()
	held, err := s.Store.PlayerBans(ctx, app, act.player.ID, false, now)
	if err != nil {
		s.fail(w, "list bans", err)
		return
	}
	if len(held) > 0 {
		writeError(w, errAlreadyBanned.Err())
		return
	}
	reason := cleanReason(req.Reason)
	if !s.runAct(w, r, act, banCommands(act.pg.family, act.player.ID, act.player.Name, reason)...) {
		return
	}
	b := store.PlayerBan{PlayerID: act.player.ID, Name: act.player.Name, Reason: reason, CreatedBy: loginFrom(ctx).account.ID, CreatedAt: now}
	length := "permanent"
	if req.Minutes > 0 {
		b.ExpiresAt = now.Add(time.Duration(req.Minutes) * time.Minute)
		length = strconv.Itoa(req.Minutes) + " min"
	}
	if _, err := s.Store.AddPlayerBan(ctx, app, b); err != nil {
		s.fail(w, "save ban", err)
		return
	}
	s.audit(ctx, app, auditBan, act.player.ID, act.player.Name, length, reason)
	if req.Minutes > 0 {
		s.watchers.Add(1)
		go func() {
			defer s.watchers.Done()
			s.keepBans(app, act.container)
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unbanPlayer(w http.ResponseWriter, r *http.Request) {
	pg, ok := s.playerGameFrom(w, r)
	if !ok {
		return
	}
	if pg.cap != "full" {
		writeError(w, errPlayerNoSupport.Err())
		return
	}
	ctx, app := r.Context(), pg.app.ID
	id, _ := strconv.ParseInt(r.PathValue("ban"), 10, 64)
	b, err := s.Store.PlayerBan(ctx, app, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errNoBan.Err())
		return
	}
	if err != nil {
		s.fail(w, "load ban", err)
		return
	}
	if !b.LiftedAt.IsZero() {
		writeError(w, errBanOver.Err())
		return
	}
	if pg.family == players.GameMinecraft && !minecraftName.MatchString(b.Name) {
		writeError(w, errBadPlayerName.Err())
		return
	}
	container, ip, running := s.runningAddr(ctx, app)
	if !running {
		writeError(w, errPlayerNotRunning.Err())
		return
	}
	defer s.banLocks.lock(app)()
	if err := s.sendPlayerCommands(ctx, pg, container, ip, unbanCommands(pg.family, b.PlayerID, b.Name)...); err != nil {
		s.failWith(w, "player command", err)
		return
	}
	if _, err := s.Store.LiftPlayerBan(ctx, app, b.ID, loginFrom(ctx).account.ID, s.now()); err != nil {
		s.fail(w, "lift ban", err)
		return
	}
	s.audit(ctx, app, auditUnban, b.PlayerID, b.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) opPlayer(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, auditOp, auditDeop, func(on bool, name string) string {
		if on {
			return "op " + name
		}
		return "deop " + name
	})
}

func (s *Server) whitelistPlayer(w http.ResponseWriter, r *http.Request) {
	s.toggle(w, r, auditWhitelistAdd, auditWhitelistRemove, func(on bool, name string) string {
		if on {
			return "whitelist add " + name
		}
		return "whitelist remove " + name
	})
}

// toggle is the Minecraft-only actions that turn something on or off for a
// player.
func (s *Server) toggle(w http.ResponseWriter, r *http.Request, onAction, offAction string, command func(on bool, name string) string) {
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	act, ok := s.actOn(w, r, players.GameMinecraft)
	if !ok || !s.runAct(w, r, act, command(req.On, act.player.Name)) {
		return
	}
	action := offAction
	if req.On {
		action = onAction
	}
	s.audit(r.Context(), act.pg.app.ID, action, act.player.ID, act.player.Name)
	w.WriteHeader(http.StatusNoContent)
}

var noteTags = []string{"", "watch", "suspect", "alt", "vip", "other"}

func (s *Server) addPlayerNote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag  string `json:"tag"`
		Note string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(req.Note)
	if note == "" || utf8.RuneCountInString(note) > maxNoteLen || !slices.Contains(noteTags, req.Tag) {
		writeError(w, errBadNote.Err())
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	p, err := s.Store.Player(ctx, a.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errNoPlayer.Err())
		return
	}
	if err == nil {
		_, err = s.Store.AddPlayerNote(ctx, a.ID, id, req.Tag, note, loginFrom(ctx).account.ID, s.now())
	}
	if err != nil {
		s.fail(w, "save note", err)
		return
	}
	s.audit(ctx, a.ID, auditNoteAdd, id, p.Name, req.Tag)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deletePlayerNote(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	noteID, _ := strconv.ParseInt(r.PathValue("note"), 10, 64)
	err := s.Store.DeletePlayerNote(ctx, a.ID, id, noteID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errNoNote.Err())
		return
	}
	if err != nil {
		s.fail(w, "delete note", err)
		return
	}
	s.audit(ctx, a.ID, auditNoteDelete, id, strconv.FormatInt(noteID, 10))
	w.WriteHeader(http.StatusNoContent)
}
