package panel

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/auth"
	"github.com/Caria-Core/zelie/internal/store"
)

// Purposes for Sealer, so a sealed value cannot be moved to another field.
const sealTOTP = "totp secret"

var (
	errTooMany     = errors.New("too many attempts; wait a few minutes and try again")
	errBadLogin    = errors.New("wrong email or password")
	errBadCode     = errors.New("that code did not work")
	errNoCeremony  = errors.New("this step has expired; start again")
	errHalfLogin   = errors.New("log in with your password first")
	errNoFactor    = errors.New("this account has no authenticator app")
	errAlreadyDone = errors.New("already logged in")
)

// guards holds the in-memory state of logging in. None of it needs to
// survive a restart: at worst a half-finished login starts over.
type guards struct {
	byIP      *failures
	byAccount *failures
	second    *failures
	pending   pending
}

func newGuards() *guards {
	return &guards{
		byIP:      newFailures(30, 15*time.Minute),
		byAccount: newFailures(10, 15*time.Minute),
		second:    newFailures(10, 15*time.Minute),
		pending:   pending{m: map[string]pendingItem{}},
	}
}

// pending keeps what a two-step ceremony, such as adding a passkey, needs
// between its two requests. Each item is keyed to one session and can be
// taken once.
type pending struct {
	mu sync.Mutex
	m  map[string]pendingItem
}

type pendingItem struct {
	value   any
	expires time.Time
}

func (p *pending) put(session []byte, kind string, v any, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, it := range p.m {
		if now.After(it.expires) {
			delete(p.m, k)
		}
	}
	p.m[kind+string(session)] = pendingItem{value: v, expires: now.Add(halfLoginTTL)}
}

func (p *pending) take(session []byte, kind string, now time.Time) (any, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	it, ok := p.m[kind+string(session)]
	delete(p.m, kind+string(session))
	if !ok || now.After(it.expires) {
		return nil, false
	}
	return it.value, true
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		credentials
	}
	if !decode(w, r, &req) {
		return
	}
	now, ip := s.now(), clientIP(r)
	if s.guards.byIP.Blocked(ip, now) {
		writeError(w, http.StatusTooManyRequests, errTooMany)
		return
	}
	email, err := checkEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := auth.CheckPasswordRules(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token, _ := base64.RawURLEncoding.DecodeString(req.Token)
	hash := sha256.Sum256(token)
	u, err := s.Store.CreateFirstAdmin(r.Context(), hash[:], email, auth.HashPassword(req.Password), now)
	switch {
	case errors.Is(err, store.ErrBadSetupToken):
		s.guards.byIP.Add(ip, now)
		writeError(w, http.StatusForbidden, err)
		return
	case errors.Is(err, store.ErrSetupDone):
		writeError(w, http.StatusConflict, err)
		return
	case err != nil:
		s.fail(w, "setup", err)
		return
	}
	s.Log.Info("first administrator created", "user", u.ID, "ip", ip)
	if err := s.startSession(w, r, u.ID, false); err != nil {
		s.fail(w, "start session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enroll": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if !decode(w, r, &req) {
		return
	}
	now, ip := s.now(), clientIP(r)
	account := strings.ToLower(req.Email)
	if s.guards.byIP.Blocked(ip, now) || s.guards.byAccount.Blocked(account, now) {
		writeError(w, http.StatusTooManyRequests, errTooMany)
		return
	}
	a, err := s.Store.AccountByEmail(r.Context(), req.Email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.DummyCheck(req.Password)
	case err != nil:
		s.fail(w, "login", err)
		return
	}
	if err != nil || !auth.CheckPassword(a.Password, req.Password) {
		s.guards.byIP.Add(ip, now)
		s.guards.byAccount.Add(account, now)
		s.Log.Warn("failed login", "ip", ip)
		writeError(w, http.StatusUnauthorized, errBadLogin)
		return
	}
	s.guards.byAccount.Reset(account)
	if err := s.startSession(w, r, a.ID, false); err != nil {
		s.fail(w, "start session", err)
		return
	}
	writeJSON(w, http.StatusOK, meFor(a, false))
}

// me tells the interface where the visitor stands: logged out, halfway
// through logging in, or in.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	l, err := s.currentLogin(r)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusOK, map[string]bool{"logged_in": false})
		return
	case err != nil:
		s.fail(w, "load session", err)
		return
	}
	writeJSON(w, http.StatusOK, meFor(l.account, l.session.Verified))
}

type meJSON struct {
	LoggedIn bool     `json:"logged_in"`
	Email    string   `json:"email"`
	Admin    bool     `json:"admin"`
	Verified bool     `json:"verified"`
	Enroll   bool     `json:"enroll"`            // must set up a second factor first
	Methods  []string `json:"methods,omitempty"` // second factors the account can use
}

func meFor(a store.Account, verified bool) meJSON {
	m := meJSON{LoggedIn: true, Email: a.Email, Admin: a.Admin, Verified: verified, Enroll: !a.HasSecondFactor()}
	if a.Passkeys > 0 {
		m.Methods = append(m.Methods, "passkey")
	}
	if a.TOTPSecret != nil {
		m.Methods = append(m.Methods, "totp")
	}
	if a.HasSecondFactor() {
		m.Methods = append(m.Methods, "recovery")
	}
	return m
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if hash := sessionHash(r); hash != nil {
		if err := s.Store.DeleteSession(r.Context(), hash); err != nil {
			s.fail(w, "logout", err)
			return
		}
	}
	clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// halfLogin loads a session that has passed the password but not the second
// factor. It writes the error response itself when there is none.
func (s *Server) halfLogin(w http.ResponseWriter, r *http.Request) (login, bool) {
	l, err := s.currentLogin(r)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusUnauthorized, errHalfLogin)
		return l, false
	case err != nil:
		s.fail(w, "load session", err)
		return l, false
	case l.session.Verified:
		writeError(w, http.StatusConflict, errAlreadyDone)
		return l, false
	}
	key := fmt.Sprint(l.account.ID)
	if s.guards.second.Blocked(key, s.now()) {
		writeError(w, http.StatusTooManyRequests, errTooMany)
		return l, false
	}
	return l, true
}

// verified finishes a login after a correct second factor.
func (s *Server) verified(w http.ResponseWriter, r *http.Request, l login, how string) {
	s.guards.second.Reset(fmt.Sprint(l.account.ID))
	now := s.now()
	if err := s.Store.VerifySession(r.Context(), l.session.Hash, sessionExpiry(l.session.CreatedAt, now)); err != nil {
		s.fail(w, "verify session", err)
		return
	}
	s.Log.Info("logged in", "user", l.account.ID, "with", how, "ip", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) secondFailed(w http.ResponseWriter, l login) {
	s.guards.second.Add(fmt.Sprint(l.account.ID), s.now())
	writeError(w, http.StatusUnauthorized, errBadCode)
}

func (s *Server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	l, ok := s.halfLogin(w, r)
	if !ok {
		return
	}
	if l.account.TOTPSecret == nil {
		writeError(w, http.StatusBadRequest, errNoFactor)
		return
	}
	secret, err := s.Sealer.Open(l.account.TOTPSecret, sealTOTP)
	if err != nil {
		s.fail(w, "open totp secret", err)
		return
	}
	step, ok := auth.CheckTOTP(secret, req.Code, s.now())
	if ok {
		if ok, err = s.Store.UseTOTPStep(r.Context(), l.account.ID, step); err != nil {
			s.fail(w, "use totp", err)
			return
		}
	}
	if !ok {
		s.secondFailed(w, l)
		return
	}
	s.verified(w, r, l, "authenticator app")
}

func (s *Server) loginRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	l, ok := s.halfLogin(w, r)
	if !ok {
		return
	}
	ok, err := s.Store.UseRecoveryCode(r.Context(), l.account.ID, auth.HashRecoveryCode(req.Code))
	if err != nil {
		s.fail(w, "use recovery code", err)
		return
	}
	if !ok {
		s.secondFailed(w, l)
		return
	}
	s.verified(w, r, l, "recovery code")
}

// newTOTP starts adding an authenticator app. The secret is kept in memory
// until a code from the app confirms it was scanned correctly.
func (s *Server) newTOTP(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	secret := auth.NewTOTPSecret()
	s.guards.pending.put(l.session.Hash, "totp", secret, s.now())
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": auth.TOTPSecretText(secret),
		"uri":    auth.TOTPURI(secret, "Zelie", l.account.Email),
	})
}

func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	l := loginFrom(r.Context())
	now := s.now()
	v, ok := s.guards.pending.take(l.session.Hash, "totp", now)
	if !ok {
		writeError(w, http.StatusBadRequest, errNoCeremony)
		return
	}
	secret := v.([]byte)
	step, ok := auth.CheckTOTP(secret, req.Code, now)
	if !ok {
		// Keep the secret so a typo does not mean scanning again.
		s.guards.pending.put(l.session.Hash, "totp", secret, now)
		writeError(w, http.StatusBadRequest, errBadCode)
		return
	}
	if err := s.Store.SetTOTP(r.Context(), l.account.ID, s.Sealer.Seal(secret, sealTOTP), step); err != nil {
		s.fail(w, "save totp", err)
		return
	}
	s.Log.Info("authenticator app added", "user", l.account.ID)
	s.factorAdded(w, r, l)
}

// factorAdded finishes adding a second factor. The first one also brings the
// recovery codes and completes the login it was added in.
func (s *Server) factorAdded(w http.ResponseWriter, r *http.Request, l login) {
	out := map[string]any{}
	if !l.account.HasSecondFactor() {
		codes, hashes := auth.NewRecoveryCodes()
		if err := s.Store.SetRecoveryCodes(r.Context(), l.account.ID, hashes); err != nil {
			s.fail(w, "save recovery codes", err)
			return
		}
		out["recovery_codes"] = codes
	}
	if !l.session.Verified {
		now := s.now()
		if err := s.Store.VerifySession(r.Context(), l.session.Hash, sessionExpiry(l.session.CreatedAt, now)); err != nil {
			s.fail(w, "verify session", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func checkEmail(s string) (string, error) {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return "", errors.New("that does not look like an email address")
	}
	return s, nil
}

// decode reads a small JSON body and rejects unknown fields. On failure it
// has already written the response.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return false
	}
	return true
}
