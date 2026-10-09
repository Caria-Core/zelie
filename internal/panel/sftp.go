package panel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/auth"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/sftpd"
	"github.com/Caria-Core/zelie/internal/store"
)

// SFTP for game servers and files apps. The service itself is a separate process (package
// sftpd) that holds the connections and knows nothing of accounts: it asks
// the panel, over a socket only its own user can use, who may log in to
// which server. A login is either an SSH key of an account that manages
// the server, or the server's own SFTP password. The password of an account
// is never taken, so SFTP is no way around its second step.

const (
	maxSSHKeys       = 20
	minRSABits       = 2048
	sftpPasswordLen  = 24
	sftpLoginWindow  = 15 * time.Minute
	sftpMaxBodyBytes = 16 << 10
)

// No l, o, 0 or 1, which are read wrong.
const sftpAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

var (
	errSSHKeyInvalid = msg.Define(http.StatusBadRequest, "sshkey.invalid", "That is not a public SSH key. Paste the line from a file ending in .pub.")
	errSSHKeyType    = msg.Define(http.StatusBadRequest, "sshkey.type", "Only ed25519, ECDSA and RSA keys are accepted.")
	errSSHKeyWeak    = msg.Define(http.StatusBadRequest, "sshkey.weak", "RSA keys need at least {bits} bits.")
	errSSHKeyName    = msg.Define(http.StatusBadRequest, "sshkey.name", "Give the key a name of up to 64 characters.")
	errSSHKeyExists  = msg.Define(http.StatusConflict, "sshkey.exists", "That key is already added.")
	errSSHKeyMany    = msg.Define(http.StatusConflict, "sshkey.too_many", "An account can have {max} keys. Remove one first.")
	errSSHKeyGone    = msg.Define(http.StatusNotFound, "sshkey.gone", "That key is already gone.")
	errSFTPPort      = msg.Define(http.StatusBadRequest, "sftp.bad_port", "Give a port from 1024 to 65535.")
	errSFTPPortTaken = msg.Define(http.StatusConflict, "sftp.port_taken", "Port {port} is in the pool of game server ports. Choose another.")
	errSFTPDenied    = msg.Define(http.StatusForbidden, "sftp.denied", "The login was refused.")
	errSFTPPortFail  = msg.Define(http.StatusBadGateway, "sftp.port_failed", "Port {port} could not be opened; another program may be using it. SFTP stays on port {old}.")
	errSFTPWait      = msg.Define(http.StatusTooManyRequests, "sftp.wait", "Too many wrong passwords. Try again later.")
)

// sftpState holds what limits wrong SFTP passwords.
type sftpState struct {
	once   sync.Once
	byIP   *failures
	byGame *failures
}

// sftpLimits counts wrong SFTP passwords, like the login page does for the
// panel's. Keys are not counted: nobody can guess one.
func (s *Server) sftpLimits() *sftpState {
	s.sftp.once.Do(func() {
		s.sftp.byIP = newFailures(s.Store, s.Log, "sftp-ip", 30, sftpLoginWindow)
		s.sftp.byGame = newFailures(s.Store, s.Log, "sftp-server", 10, sftpLoginWindow)
	})
	return &s.sftp
}

// The keys of an account.

type sshKeyJSON struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
}

func sshKeyOut(k store.SSHKey) sshKeyJSON {
	out := sshKeyJSON{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint, CreatedAt: k.CreatedAt}
	if pub, err := ssh.ParsePublicKey(k.PublicKey); err == nil {
		out.Type = pub.Type()
	}
	return out
}

func (s *Server) listSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.SSHKeys(r.Context(), loginFrom(r.Context()).account.ID)
	if err != nil {
		s.fail(w, "list ssh keys", err)
		return
	}
	out := make([]sshKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, sshKeyOut(k))
	}
	writeJSON(w, http.StatusOK, out)
}

// parseSSHKey reads one public key line and accepts the kinds worth
// trusting.
func parseSSHKey(line string) (ssh.PublicKey, string, *msg.Error) {
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil || len(options) > 0 || len(bytes.TrimSpace(rest)) > 0 {
		return nil, "", errSSHKeyInvalid.Err()
	}
	if ck, ok := pub.(ssh.CryptoPublicKey); ok {
		switch k := ck.CryptoPublicKey().(type) {
		case ed25519.PublicKey, *ecdsa.PublicKey:
			return pub, comment, nil
		case *rsa.PublicKey:
			if k.N.BitLen() < minRSABits {
				return nil, "", errSSHKeyWeak.Err("bits", minRSABits)
			}
			return pub, comment, nil
		}
	}
	return nil, "", errSSHKeyType.Err()
}

func (s *Server) addSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Key) > 8<<10 {
		writeError(w, errSSHKeyInvalid.Err())
		return
	}
	pub, comment, bad := parseSSHKey(req.Key)
	if bad != nil {
		writeError(w, bad)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(comment)
	}
	if name == "" || len([]rune(name)) > 64 {
		writeError(w, errSSHKeyName.Err())
		return
	}
	ctx := r.Context()
	l := loginFrom(ctx)
	have, err := s.Store.SSHKeys(ctx, l.account.ID)
	if err != nil {
		s.fail(w, "list ssh keys", err)
		return
	}
	if len(have) >= maxSSHKeys {
		writeError(w, errSSHKeyMany.Err("max", maxSSHKeys))
		return
	}
	k := store.SSHKey{UserID: l.account.ID, Name: name, Fingerprint: ssh.FingerprintSHA256(pub), PublicKey: pub.Marshal(), CreatedAt: s.now()}
	id, err := s.Store.AddSSHKey(ctx, k)
	switch {
	case errors.Is(err, store.ErrExists):
		writeError(w, errSSHKeyExists.Err())
		return
	case err != nil:
		s.fail(w, "add ssh key", err)
		return
	}
	k.ID = id
	s.Log.Info("ssh key added", "user", l.account.ID, "fingerprint", k.Fingerprint, "ip", clientIP(r))
	writeJSON(w, http.StatusCreated, sshKeyOut(k))
}

func (s *Server) deleteSSHKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, errSSHKeyGone.Err())
		return
	}
	l := loginFrom(r.Context())
	switch err := s.Store.DeleteSSHKey(r.Context(), l.account.ID, id); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errSSHKeyGone.Err())
		return
	case err != nil:
		s.fail(w, "delete ssh key", err)
		return
	}
	s.Log.Info("ssh key removed", "user", l.account.ID, "key", id)
	w.WriteHeader(http.StatusNoContent)
}

// The service and a server's password, for the interface.

type sftpServerJSON struct {
	Port int `json:"port"`
	// HostKey is the fingerprint of the host key; empty if the core could
	// not be asked.
	HostKey string `json:"host_key"`
	// Running is whether the port is open. The server behind it starts
	// with the first connection and stops when idle.
	Running bool `json:"running"`
}

func (s *Server) sftpInfo(ctx context.Context) (sftpServerJSON, error) {
	port, err := s.Store.SFTPPort(ctx, store.ThisNode)
	if err != nil {
		return sftpServerJSON{}, err
	}
	out := sftpServerJSON{Port: port}
	st, err := s.Core.SFTP(ctx)
	if err != nil {
		s.Log.Warn("ask the core about SFTP", "err", err)
		return out, nil
	}
	out.HostKey, out.Running = st.HostKey, st.Listening
	return out, nil
}

func (s *Server) getSFTP(w http.ResponseWriter, r *http.Request) {
	out, err := s.sftpInfo(r.Context())
	if err != nil {
		s.fail(w, "describe sftp", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setSFTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Port < lowestAllocation || req.Port > highestPort {
		writeError(w, errSFTPPort.Err())
		return
	}
	ctx := r.Context()
	pool, err := s.Store.Allocations(ctx, store.ThisNode)
	if err != nil {
		s.fail(w, "list allocations", err)
		return
	}
	for _, a := range pool {
		if a.Port == req.Port {
			writeError(w, errSFTPPortTaken.Err("port", req.Port))
			return
		}
	}
	old, err := s.Store.SFTPPort(ctx, store.ThisNode)
	if err != nil {
		s.fail(w, "read sftp port", err)
		return
	}
	// The core goes back to the old port when the new one will not open, so
	// the stored port follows it.
	if err := s.Core.SetSFTPPort(ctx, req.Port); err != nil {
		s.Log.Error("set the sftp port in the core", "port", req.Port, "err", err)
		writeError(w, errSFTPPortFail.Err("port", req.Port, "old", old))
		return
	}
	if err := s.Store.SetSFTPPort(ctx, store.ThisNode, req.Port); err != nil {
		s.fail(w, "set sftp port", err)
		return
	}
	s.Log.Info("sftp port set", "port", req.Port, "user", loginFrom(ctx).account.ID)
	s.getSFTP(w, r)
}

type gameSFTPJSON struct {
	sftpServerJSON
	// Host is the address to connect to, and User the name to log in with.
	Host string `json:"host"`
	User string `json:"user"`
	// PasswordSet says whether the server has an SFTP password. The
	// password itself is shown once, when it is made.
	PasswordSet bool `json:"password_set"`
}

func (s *Server) getGameSFTP(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	info, err := s.sftpInfo(ctx)
	if err != nil {
		s.fail(w, "describe sftp", err)
		return
	}
	hash, err := s.Store.SFTPPassword(ctx, a.ID)
	if err != nil {
		s.fail(w, "read sftp password", err)
		return
	}
	out := gameSFTPJSON{sftpServerJSON: info, User: a.ID, PasswordSet: hash != ""}
	if n, err := s.Store.Node(ctx, store.ThisNode); err == nil {
		out.Host = s.nodeOut(ctx, n).Address
	}
	writeJSON(w, http.StatusOK, out)
}

func newSFTPPassword() string {
	b := make([]byte, sftpPasswordLen)
	rand.Read(b)
	for i := range b {
		b[i] = sftpAlphabet[int(b[i])%len(sftpAlphabet)]
	}
	return string(b)
}

// newGameSFTPPassword makes a password, or replaces the one there is.
func (s *Server) newGameSFTPPassword(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	pw := newSFTPPassword()
	if err := s.Store.SetSFTPPassword(r.Context(), a.ID, auth.HashPassword(pw), s.now()); err != nil {
		s.fail(w, "save sftp password", err)
		return
	}
	s.sftpLimits().byGame.Reset(a.ID)
	s.Log.Info("sftp password made", "server", a.ID, "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) removeGameSFTPPassword(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	if err := s.Store.SetSFTPPassword(r.Context(), a.ID, "", s.now()); err != nil {
		s.fail(w, "remove sftp password", err)
		return
	}
	s.Log.Info("sftp password removed", "server", a.ID, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// What the SFTP service asks. These routes are for its user only.

func (s *Server) sftpRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /local/sftp/auth", s.sftpAuth)
	mux.HandleFunc("POST /local/sftp/check", s.sftpCheck)
	mux.HandleFunc("POST /local/sftp/room", s.sftpRoom)
	return mux
}

func decodeSmall(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, sftpMaxBodyBytes)
	return decode(w, r, v)
}

// mayManageGame says whether an account may work on a game server's files.
// It follows managesGame: for now, administrators.
func mayManageGame(a store.Account) bool { return a.Admin }

// credOf is what a grant remembers of the password it was given for. A
// fingerprint of the hash is enough to see that the password was changed or
// removed, and cannot be used to log in.
func credOf(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return "p:" + hex.EncodeToString(sum[:12])
}

// gameGrant is the grant for a server and the credential it was made from,
// or false when the server has no game volume.
func (s *Server) gameGrant(ctx context.Context, app store.App, account int64, cred string) (sftpd.Grant, bool, error) {
	vols, err := s.Store.Volumes(ctx, app.ID)
	if err != nil {
		return sftpd.Grant{}, false, err
	}
	for _, v := range vols {
		if v.Path == gameVolumePath {
			return sftpd.Grant{Server: app.ID, Volume: v.Name, UID: gameUID, GID: gameGID, Account: account, Cred: cred}, true, nil
		}
	}
	return sftpd.Grant{}, false, nil
}

// sftpGrant works out what a login may use, or that it may use nothing.
func (s *Server) sftpGrant(ctx context.Context, req sftpd.AuthRequest) (sftpd.Grant, bool, error) {
	var none sftpd.Grant
	app, err := s.Store.App(ctx, req.Server)
	if err == nil && !app.RunsEgg() {
		err = store.ErrNotFound
	}
	var hash string
	if err == nil && req.Key == nil {
		hash, err = s.Store.SFTPPassword(ctx, app.ID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		// A key request is cheap whether or not the server exists, so only
		// a password needs the fake hash to look the same. Hashing for keys
		// too would let anyone tie up the hash slot, uncounted.
		if req.Key == nil {
			auth.DummySFTPCheck(req.Password)
		}
		return none, false, nil
	case err != nil:
		return none, false, err
	}

	var account int64
	var cred string
	if req.Key != nil {
		pub, err := ssh.ParsePublicKey(req.Key)
		if err != nil {
			return none, false, nil
		}
		fp := ssh.FingerprintSHA256(pub)
		acct, key, err := s.Store.SSHKeyOwner(ctx, fp)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return none, false, nil
		case err != nil:
			return none, false, err
		}
		if !bytes.Equal(key.PublicKey, pub.Marshal()) || !mayManageGame(acct) {
			return none, false, nil
		}
		account, cred = acct.ID, "k:"+fp
	} else if hash == "" {
		auth.DummySFTPCheck(req.Password)
		return none, false, nil
	} else if !auth.CheckSFTPPassword(hash, req.Password) {
		return none, false, nil
	} else {
		cred = credOf(hash)
	}
	return s.gameGrant(ctx, app, account, cred)
}

// sftpStillGranted asks the same questions as a login, without a password:
// the grant is whole only if what it was made from is still there.
func (s *Server) sftpStillGranted(ctx context.Context, g sftpd.Grant) (bool, error) {
	app, err := s.Store.App(ctx, g.Server)
	if err == nil && !app.RunsEgg() {
		err = store.ErrNotFound
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	switch {
	case strings.HasPrefix(g.Cred, "p:"):
		hash, err := s.Store.SFTPPassword(ctx, app.ID)
		if err != nil || hash == "" || credOf(hash) != g.Cred {
			return false, err
		}
	case strings.HasPrefix(g.Cred, "k:"):
		acct, key, err := s.Store.SSHKeyOwner(ctx, strings.TrimPrefix(g.Cred, "k:"))
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if acct.ID != g.Account || key.UserID != g.Account || !mayManageGame(acct) {
			return false, nil
		}
	default:
		return false, nil
	}
	now, ok, err := s.gameGrant(ctx, app, g.Account, g.Cred)
	return ok && now == g, err
}

func (s *Server) sftpCheck(w http.ResponseWriter, r *http.Request) {
	var g sftpd.Grant
	if !decodeSmall(w, r, &g) {
		return
	}
	ok, err := s.sftpStillGranted(r.Context(), g)
	switch {
	case err != nil:
		s.fail(w, "check an sftp grant", err)
	case !ok:
		writeError(w, errSFTPDenied.Err())
	default:
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

func (s *Server) sftpAuth(w http.ResponseWriter, r *http.Request) {
	var req sftpd.AuthRequest
	if !decodeSmall(w, r, &req) {
		return
	}
	ip := req.IP
	if net.ParseIP(ip) == nil {
		ip = "unknown"
	}
	lim := s.sftpLimits()
	password := req.Key == nil
	// One /64 is one client, as in the service.
	ipKey := sftpd.LimitKey(ip)
	if password {
		// Wrong passwords are counted only after the hash is checked, so
		// parallel guesses are let in one at a time.
		unlock, err := lockBoth(r.Context(), lim.byIP, ipKey, lim.byGame, req.Server)
		if err != nil {
			return // the service has gone
		}
		defer unlock()
	}
	now := s.now()
	if password && lim.byIP.Wait(ipKey, now) > 0 {
		writeError(w, errSFTPWait.Err())
		return
	}
	// Past the server's limit the password is still checked, so strangers
	// guessing cannot lock out the owner. A wrong one is told to wait.
	serverBusy := password && lim.byGame.Wait(req.Server, now) > 0
	g, ok, err := s.sftpGrant(r.Context(), req)
	switch {
	case err != nil:
		s.fail(w, "check an sftp login", err)
		return
	case !ok:
		if password {
			if err := errors.Join(lim.byIP.Add(ipKey, now), lim.byGame.Add(req.Server, now)); err != nil {
				s.fail(w, "check an sftp login", err)
				return
			}
		}
		s.Log.Info("sftp login refused", "server", truncate(req.Server, 64), "ip", ip, "key", !password)
		if serverBusy {
			writeError(w, errSFTPWait.Err())
			return
		}
		writeError(w, errSFTPDenied.Err())
		return
	}
	if password {
		lim.byGame.Reset(req.Server)
	}
	s.Log.Info("sftp login", "server", g.Server, "user", g.Account, "ip", ip, "key", !password)
	writeJSON(w, http.StatusOK, g)
}

// sftpRoom says how much room the server's disk limit leaves, from the last
// measurement, as the file manager does.
func (s *Server) sftpRoom(w http.ResponseWriter, r *http.Request) {
	var req sftpd.RoomRequest
	if !decodeSmall(w, r, &req) {
		return
	}
	var out sftpd.RoomResponse
	vols, err := s.Store.Volumes(r.Context(), req.Server)
	if err != nil {
		s.fail(w, "load volumes", err)
		return
	}
	for _, v := range vols {
		if v.Path != gameVolumePath {
			continue
		}
		if used, ok := s.sizes.get(v.Name); ok {
			room := max(v.LimitMB<<20-used, 0)
			out.Room = &room
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// syncSFTPVolumes tells the core which volumes SFTP may use: the files of
// game servers and files apps, and nothing else. The core keeps the list and holds the
// SFTP user to it, so a login can never reach a database's volume.
func (s *Server) syncSFTPVolumes(ctx context.Context) {
	games, err := s.Store.GameServers(ctx)
	if err != nil {
		s.Log.Error("sftp volumes: list servers", "err", err)
		return
	}
	names := []string{}
	for _, g := range games {
		vols, err := s.Store.Volumes(ctx, g.AppID)
		if err != nil {
			s.Log.Error("sftp volumes: list volumes", "server", g.AppID, "err", err)
			return
		}
		for _, v := range vols {
			if v.Path == gameVolumePath {
				names = append(names, v.Name)
			}
		}
	}
	if err := s.Core.SetSFTPVolumes(ctx, names); err != nil {
		s.Log.Error("sftp volumes: sync", "err", err)
	}
}

// syncSFTPPort makes the socket listen on the port the database holds, in
// case the two differ, such as after a restore or a failed change.
func (s *Server) syncSFTPPort(ctx context.Context) {
	port, err := s.Store.SFTPPort(ctx, store.ThisNode)
	if err != nil {
		s.Log.Error("sftp port: read", "err", err)
		return
	}
	// Nothing is moved here: that would take a port from a running game.
	// The core refuses to listen on a forwarded port anyway, so SFTP stays
	// where it is until the administrator frees the port or picks another.
	if pool, err := s.Store.Allocations(ctx, store.ThisNode); err == nil {
		for _, a := range pool {
			if a.Port == port && a.AppID != "" {
				s.Log.Error("sftp port: the stored port is held by a game server; change one of them", "port", port, "server", a.AppID)
				return
			}
		}
	}
	if err := s.Core.SetSFTPPort(ctx, port); err != nil {
		s.Log.Error("sftp port: sync", "err", err)
	}
}
