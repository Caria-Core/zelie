package panel

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Caria-Core/zelie/internal/auth"
	"github.com/Caria-Core/zelie/internal/store"
)

type passkeyJSON struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
}

type sessionJSON struct {
	ID        string    `json:"id"`
	Current   bool      `json:"current"`
	Verified  bool      `json:"verified"`
	IP        string    `json:"ip"`
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"created_at"`
	SeenAt    time.Time `json:"seen_at"`
}

// account describes how the signed-in account logs in and where it is
// logged in, for the security settings page.
func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	ctx := r.Context()
	keys, err := s.Store.Passkeys(ctx, l.account.ID)
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	left, err := s.Store.RecoveryCodesLeft(ctx, l.account.ID)
	if err != nil {
		s.fail(w, "count recovery codes", err)
		return
	}
	sessions, err := s.Store.Sessions(ctx, l.account.ID, s.now())
	if err != nil {
		s.fail(w, "load sessions", err)
		return
	}
	out := struct {
		Email        string        `json:"email"`
		TOTP         bool          `json:"totp"`
		RecoveryLeft int           `json:"recovery_left"`
		Passkeys     []passkeyJSON `json:"passkeys"`
		Sessions     []sessionJSON `json:"sessions"`
	}{Email: l.account.Email, TOTP: l.account.TOTPSecret != nil, RecoveryLeft: left, Passkeys: []passkeyJSON{}}
	for _, k := range keys {
		p := passkeyJSON{ID: base64.RawURLEncoding.EncodeToString(k.ID), Name: k.Name, CreatedAt: k.CreatedAt}
		if !k.UsedAt.IsZero() {
			p.UsedAt = &k.UsedAt
		}
		out.Passkeys = append(out.Passkeys, p)
	}
	for _, x := range sessions {
		out.Sessions = append(out.Sessions, sessionJSON{
			// The stored hash names the session without giving away the
			// cookie, which only this browser has.
			ID:      base64.RawURLEncoding.EncodeToString(x.Hash),
			Current: string(x.Hash) == string(l.session.Hash), Verified: x.Verified,
			IP: x.IP, Agent: x.Agent, CreatedAt: x.CreatedAt, SeenAt: x.SeenAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// confirm takes a code from the authenticator app or a recovery code and
// opens confirmWindow. A password is not enough: it is the one thing a
// phishing page is most likely to have.
func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TOTP     string `json:"totp"`
		Recovery string `json:"recovery"`
	}
	if !decode(w, r, &req) {
		return
	}
	l := loginFrom(r.Context())
	if !s.secondStepAllowed(w, l) {
		return
	}
	var ok bool
	var err error
	switch {
	case req.TOTP != "" && l.account.TOTPSecret != nil:
		ok, err = s.useTOTPCode(r, l.account, req.TOTP)
	case req.Recovery != "":
		ok, err = s.Store.UseRecoveryCode(r.Context(), l.account.ID, auth.HashRecoveryCode(req.Recovery))
	}
	if err != nil {
		s.fail(w, "confirm", err)
		return
	}
	if !ok {
		s.secondFailed(w, l)
		return
	}
	s.confirmedNow(w, r, l)
}

func (s *Server) confirmPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	if l := loginFrom(r.Context()); s.secondStepAllowed(w, l) {
		s.beginPasskeyCheck(w, r, l, "passkey-confirm")
	}
}

func (s *Server) confirmPasskey(w http.ResponseWriter, r *http.Request) {
	if l := loginFrom(r.Context()); s.secondStepAllowed(w, l) && s.finishPasskeyCheck(w, r, l, "passkey-confirm") {
		s.confirmedNow(w, r, l)
	}
}

// secondStepAllowed applies the same limit on wrong codes as logging in.
func (s *Server) secondStepAllowed(w http.ResponseWriter, l login) bool {
	if s.guards.second.Blocked(fmt.Sprint(l.account.ID), s.now()) {
		writeError(w, http.StatusTooManyRequests, errTooMany)
		return false
	}
	return true
}

func (s *Server) confirmedNow(w http.ResponseWriter, r *http.Request, l login) {
	s.guards.second.Reset(fmt.Sprint(l.account.ID))
	if err := s.Store.ConfirmSession(r.Context(), l.session.Hash, s.now()); err != nil {
		s.fail(w, "confirm session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// changePassword asks for the current password as well, so a confirmation
// made with a stolen recovery code cannot also take the password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &req) {
		return
	}
	l := loginFrom(r.Context())
	if !s.secondStepAllowed(w, l) {
		return
	}
	if !auth.CheckPassword(l.account.Password, req.Current) {
		s.guards.second.Add(fmt.Sprint(l.account.ID), s.now())
		writeError(w, http.StatusBadRequest, errors.New("the current password is wrong"))
		return
	}
	if err := auth.CheckPasswordRules(req.New); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	if err := s.Store.SetPassword(ctx, l.account.ID, auth.HashPassword(req.New)); err != nil {
		s.fail(w, "change password", err)
		return
	}
	// Whoever knew the old password may be logged in somewhere.
	if err := s.Store.DeleteOtherSessions(ctx, l.account.ID, l.session.Hash); err != nil {
		s.fail(w, "end other sessions", err)
		return
	}
	s.Log.Info("password changed", "user", l.account.ID, "ip", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeTOTP(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	if !s.factorRemoved(w, s.Store.RemoveTOTP(r.Context(), l.account.ID), "remove totp") {
		return
	}
	s.Log.Info("authenticator app removed", "user", l.account.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removePasskey(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such passkey"))
		return
	}
	if !s.factorRemoved(w, s.Store.DeletePasskey(r.Context(), l.account.ID, id), "remove passkey") {
		return
	}
	s.Log.Info("passkey removed", "user", l.account.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) factorRemoved(w http.ResponseWriter, err error, what string) bool {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("it is already gone"))
	case errors.Is(err, store.ErrLastFactor):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		s.fail(w, what, err)
	default:
		return true
	}
	return false
}

// newRecoveryCodes replaces every recovery code, used or not.
func (s *Server) newRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	codes, hashes := auth.NewRecoveryCodes()
	if err := s.Store.SetRecoveryCodes(r.Context(), l.account.ID, hashes); err != nil {
		s.fail(w, "save recovery codes", err)
		return
	}
	s.Log.Info("recovery codes replaced", "user", l.account.ID)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// endSession logs out one of the account's browsers. It needs no
// confirmation: at worst it logs someone out.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	errGone := errors.New("that session has already ended")
	hash, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errGone)
		return
	}
	switch err := s.Store.DeleteUserSession(r.Context(), l.account.ID, hash); {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errGone)
		return
	case err != nil:
		s.fail(w, "end session", err)
		return
	}
	if string(hash) == string(l.session.Hash) {
		clearCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) endOtherSessions(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	if err := s.Store.DeleteOtherSessions(r.Context(), l.account.ID, l.session.Hash); err != nil {
		s.fail(w, "end other sessions", err)
		return
	}
	s.Log.Info("other sessions ended", "user", l.account.ID)
	w.WriteHeader(http.StatusNoContent)
}
