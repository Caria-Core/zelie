package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/auth"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/sftpd"
	"github.com/Caria-Core/zelie/internal/store"
)

// Purposes for Sealer, so a sealed value cannot be moved to another field.
const sealTOTP = "totp secret"

var (
	errTooMany     = msg.Define(http.StatusTooManyRequests, "login.too_many", "Too many attempts. Wait {minutes} min and try again.")
	errBadLogin    = msg.Define(http.StatusUnauthorized, "login.wrong", "Wrong email or password.")
	errBadCode     = msg.Define(http.StatusUnauthorized, "login.bad_code", "That code did not work.")
	errNoCeremony  = msg.Define(http.StatusBadRequest, "login.expired", "This step has expired. Start again.")
	errHalfLogin   = msg.Define(http.StatusUnauthorized, "login.password_first", "Log in with your password first.")
	errNoFactor    = msg.Define(http.StatusBadRequest, "login.no_totp", "This account has no authenticator app.")
	errAlreadyDone = msg.Define(http.StatusConflict, "login.already", "You are already logged in.")
	errBadEmail    = msg.Define(http.StatusBadRequest, "login.bad_email", "That does not look like an email address.")
	errBadToken    = msg.Define(http.StatusForbidden, "setup.bad_token", "This setup link is not valid. Run zelie setup-link on the server for a new one.")
)

// guards holds the state of logging in. The limits are kept in the
// database; what is pending is not, since at worst a half-finished login
// starts over.
//
// An account is never locked for everyone: that would let a stranger who
// knows an email keep its owner out. Its wrong passwords block only the
// address they came from (byAccountIP), and from anywhere they make the
// puzzle bigger (byAccount), which is what slows a guesser with many
// addresses.
type guards struct {
	byIP        *failures // from one address, for any account
	byAccountIP *failures // for one account, from one address
	byAccount   *failures // for one account, from anywhere; it never blocks
	second      *failures
	pending     pending
}

func newGuards(st *store.Store, log *slog.Logger) *guards {
	return &guards{
		byIP:        newFailures(st, log, "ip", 30, 15*time.Minute),
		byAccountIP: newFailures(st, log, "account-ip", 10, 15*time.Minute),
		byAccount:   newFailures(st, log, "account", powTopAt, 15*time.Minute),
		second:      newFailures(st, log, "second", 10, 15*time.Minute),
		pending:     pending{m: map[string]pendingItem{}},
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

// tooMany tells how long to wait, in whole minutes rounded up.
func tooMany(wait time.Duration) *msg.Error {
	return errTooMany.Err("minutes", int((wait+time.Minute-1)/time.Minute))
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
	ipKey := sftpd.LimitKey(ip)
	if wait := s.guards.byIP.Wait(ipKey, now); wait > 0 {
		writeError(w, tooMany(wait))
		return
	}
	email, bad := checkEmail(req.Email)
	if bad != nil {
		writeError(w, bad)
		return
	}
	if bad := auth.CheckPasswordRules(req.Password); bad != nil {
		writeError(w, bad)
		return
	}
	token, _ := base64.RawURLEncoding.DecodeString(req.Token)
	hash := sha256.Sum256(token)
	// Hashing the password is the costly part, so it waits until the token
	// is known to be good. CreateFirstAdmin checks again, in its transaction.
	var u store.User
	err := s.Store.CheckSetupToken(r.Context(), hash[:], now)
	if err == nil {
		u, err = s.Store.CreateFirstAdmin(r.Context(), hash[:], email, auth.HashPassword(req.Password), now)
	}
	switch {
	case errors.Is(err, store.ErrBadSetupToken):
		if err := s.guards.byIP.Add(ipKey, now); err != nil {
			s.fail(w, "setup", err)
			return
		}
		writeError(w, errBadToken.Err())
		return
	case errors.Is(err, store.ErrSetupDone):
		writeError(w, errSetupDone.Err())
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
	var req struct {
		credentials
		Pow *powAnswer `json:"pow"`
	}
	if !decode(w, r, &req) {
		return
	}
	ip := clientIP(r)
	ipKey := sftpd.LimitKey(ip)
	account := strings.ToLower(req.Email)
	// The address's lock also covers this pair, since every request of the
	// pair comes from that address.
	pair := account + "\x00" + ipKey
	unlock, err := lockBoth(r.Context(), s.guards.byIP, ipKey, s.guards.byAccount, account)
	if err != nil {
		return // the client has gone
	}
	defer unlock()
	now := s.now()
	if wait := max(s.guards.byIP.Wait(ipKey, now), s.guards.byAccountIP.Wait(pair, now)); wait > 0 {
		writeError(w, tooMany(wait))
		return
	}
	bits := powNeeded(s.guards.byIP.Count(ipKey, now), s.guards.byAccount.Count(account, now))
	if bits > 0 && !s.powSolved(req.Pow, ipKey, bits, now) {
		writeError(w, s.newPow(ipKey, bits, now))
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
		s.Log.Warn("failed login", "ip", ip)
		err := errors.Join(s.guards.byIP.Add(ipKey, now), s.guards.byAccountIP.Add(pair, now), s.guards.byAccount.Add(account, now))
		if err != nil {
			s.fail(w, "login", err)
			return
		}
		writeError(w, errBadLogin.Err())
		return
	}
	s.guards.byAccountIP.Reset(pair)
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
// factor. It writes the error response itself when there is none. On
// success the caller holds the account's second-step lock until it calls
// unlock, which is long enough to add the failure of a wrong code.
func (s *Server) halfLogin(w http.ResponseWriter, r *http.Request) (l login, unlock func(), ok bool) {
	l, err := s.currentLogin(r)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errHalfLogin.Err())
		return l, nil, false
	case err != nil:
		s.fail(w, "load session", err)
		return l, nil, false
	case l.session.Verified:
		writeError(w, errAlreadyDone.Err())
		return l, nil, false
	}
	unlock, ok = s.secondStepAllowed(w, r, l)
	return l, unlock, ok
}

// verified finishes a login after a correct second factor.
func (s *Server) verified(w http.ResponseWriter, r *http.Request, l login, how string) {
	s.guards.second.Reset(fmt.Sprint(l.account.ID))
	now := s.now()
	if err := s.Store.VerifySession(r.Context(), l.session.Hash, now, sessionExpiry(l.session.CreatedAt, now)); err != nil {
		s.fail(w, "verify session", err)
		return
	}
	s.Log.Info("logged in", "user", l.account.ID, "with", how, "ip", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) secondFailed(w http.ResponseWriter, l login) {
	if err := s.guards.second.Add(fmt.Sprint(l.account.ID), s.now()); err != nil {
		s.fail(w, "second step", err)
		return
	}
	writeError(w, errBadCode.Err())
}

func (s *Server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	l, unlock, ok := s.halfLogin(w, r)
	if !ok {
		return
	}
	defer unlock()
	if l.account.TOTPSecret == nil {
		writeError(w, errNoFactor.Err())
		return
	}
	ok, err := s.useTOTPCode(r, l.account, req.Code)
	if err != nil {
		s.fail(w, "check totp", err)
		return
	}
	if !ok {
		s.secondFailed(w, l)
		return
	}
	s.verified(w, r, l, "authenticator app")
}

// useTOTPCode checks a code from the account's authenticator app. A correct
// code is used up.
func (s *Server) useTOTPCode(r *http.Request, a store.Account, code string) (bool, error) {
	secret, err := s.Sealer.Open(a.TOTPSecret, sealTOTP)
	if err != nil {
		return false, err
	}
	step, ok := auth.CheckTOTP(secret, code, s.now())
	if !ok {
		return false, nil
	}
	return s.Store.UseTOTPStep(r.Context(), a.ID, step)
}

func (s *Server) loginRecovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	l, unlock, ok := s.halfLogin(w, r)
	if !ok {
		return
	}
	defer unlock()
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
	uri := auth.TOTPURI(secret, "Zelie", l.account.Email)
	qr, err := qrDataURL(uri)
	if err != nil {
		s.fail(w, "draw qr code", err)
		return
	}
	s.guards.pending.put(l.session.Hash, "totp", secret, s.now())
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": auth.TOTPSecretText(secret),
		"uri":    uri,
		"qr":     qr,
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
		writeError(w, errNoCeremony.Err())
		return
	}
	secret := v.([]byte)
	step, ok := auth.CheckTOTP(secret, req.Code, now)
	if !ok {
		// Keep the secret so a typo does not mean scanning again.
		s.guards.pending.put(l.session.Hash, "totp", secret, now)
		writeError(w, errBadCode.Err().WithStatus(http.StatusBadRequest))
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
		if err := s.Store.VerifySession(r.Context(), l.session.Hash, now, sessionExpiry(l.session.CreatedAt, now)); err != nil {
			s.fail(w, "verify session", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func checkEmail(s string) (string, *msg.Error) {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return "", errBadEmail.Err()
	}
	return s, nil
}

// readBody reads a small body into memory and puts it back for whoever
// reads it next. Taking a lock only after this means a client that sends
// its body slowly is waited for without holding anyone up. On failure it
// has written the response.
func readBody(w http.ResponseWriter, r *http.Request) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, errBadBody.Err())
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return true
}

// decode reads a small JSON body and rejects unknown fields. On failure it
// has already written the response.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, errBadBody.Err())
		return false
	}
	return true
}
