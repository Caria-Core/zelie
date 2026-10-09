package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Caria-Core/zelie/internal/auth"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/sftpd"
	"github.com/Caria-Core/zelie/internal/store"
)

// A reset link lets someone with root on the server back into the panel:
// root can read everything anyway, so the panel trusts the server's own
// sign-in to it. It works once, for an hour, and leaves the account with a
// new password and no second step, which it must set up again at once.
const resetLinkTTL = time.Hour

var (
	errBadReset      = msg.Define(http.StatusForbidden, "reset.bad_token", "This reset link is not valid. Run zelie reset-login on the server for a new one.")
	errResetNoUser   = msg.Define(http.StatusNotFound, "reset.no_account", "There is no account with that email.")
	errResetWhichOne = msg.Define(http.StatusConflict, "reset.which_account", "There is more than one administrator. Name one: zelie reset-login <email>.")
)

// resetLink is for zelie reset-login, over the panel's local socket.
func (s *Server) resetLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &req) {
		return
	}
	token := make([]byte, 32)
	rand.Read(token)
	hash := sha256.Sum256(token)
	u, err := s.Store.SetResetToken(r.Context(), req.Email, hash[:], s.now().Add(resetLinkTTL))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errResetNoUser.Err())
		return
	case errors.Is(err, store.ErrWhichAccount):
		writeError(w, errResetWhichOne.Err())
		return
	case err != nil:
		s.fail(w, "reset link", err)
		return
	}
	s.Log.Warn("login reset link made on the server", "user", u.ID)
	writeJSON(w, http.StatusOK, map[string]string{"token": base64.RawURLEncoding.EncodeToString(token), "email": u.Email})
}

// resetLogin spends a reset link: a new password, and a session that must
// set up a second step before it can do anything else.
func (s *Server) resetLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
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
	if bad := auth.CheckPasswordRules(req.Password); bad != nil {
		writeError(w, bad)
		return
	}
	token, _ := base64.RawURLEncoding.DecodeString(req.Token)
	hash := sha256.Sum256(token)
	// As in setup, the password is hashed only for a token that is good.
	var u store.User
	err := s.Store.CheckResetToken(r.Context(), hash[:], now)
	if err == nil {
		u, err = s.Store.ResetLogin(r.Context(), hash[:], auth.HashPassword(req.Password), now)
	}
	switch {
	case errors.Is(err, store.ErrBadResetToken):
		if err := s.guards.byIP.Add(ipKey, now); err != nil {
			s.fail(w, "reset login", err)
			return
		}
		writeError(w, errBadReset.Err())
		return
	case err != nil:
		s.fail(w, "reset login", err)
		return
	}
	s.Log.Warn("login reset: new password, second steps and SSH keys removed", "user", u.ID, "ip", ip)
	if err := s.startSession(w, r, u.ID, false); err != nil {
		s.fail(w, "start session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enroll": true})
}

// ResetLink asks the panel for a reset link, for the account with email or
// the only administrator. It returns the token and whose it is.
func (c *Client) ResetLink(ctx context.Context, email string) (token, account string, err error) {
	b, _ := json.Marshal(map[string]string{"email": email})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://panel/local/reset-link", bytes.NewReader(b))
	if err != nil {
		return "", "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("reach the Zelie panel: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
		Email string `json:"email"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("panel answered %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New(out.Error)
	}
	return out.Token, out.Email, nil
}
