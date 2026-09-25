package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/store"
)

// The __Host- prefix makes the browser refuse the cookie unless it is Secure,
// has no Domain and has Path=/, so no other site or subdomain can set it.
const cookieName = "__Host-zelie"

const (
	// halfLoginTTL is how long someone has to give their second factor
	// after the password.
	halfLoginTTL = 10 * time.Minute
	sessionIdle  = 7 * 24 * time.Hour
	sessionMax   = 30 * 24 * time.Hour
	// confirmWindow is how long proving it is you again lasts. Changing how
	// the account logs in needs it, so a browser left open or a stolen
	// cookie is not enough to take the account over.
	confirmWindow = 15 * time.Minute
)

// login is the session and account behind a request.
type login struct {
	session store.Session
	account store.Account
}

type loginKey struct{}

func loginFrom(ctx context.Context) login {
	l, _ := ctx.Value(loginKey{}).(login)
	return l
}

// startSession logs the account in on this browser. The session is only
// fully usable once verified is true.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64, verified bool) error {
	token := make([]byte, 32)
	rand.Read(token)
	hash := sha256.Sum256(token)
	now := s.now()
	x := store.Session{
		Hash: hash[:], UserID: userID, Verified: verified,
		CreatedAt: now, SeenAt: now, ExpiresAt: now.Add(halfLoginTTL),
		IP: clientIP(r), Agent: truncate(r.UserAgent(), 200),
	}
	if verified {
		x.ExpiresAt = now.Add(sessionIdle)
	}
	if err := s.Store.CreateSession(r.Context(), x); err != nil {
		return err
	}
	// Logging in replaces whatever session this browser had.
	if old := sessionHash(r); old != nil {
		s.Store.DeleteSession(r.Context(), old)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    base64.RawURLEncoding.EncodeToString(token),
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionMax.Seconds()),
	})
	return nil
}

// sessionExpiry moves an idle timeout forward, up to a fixed limit from when
// the session started.
func sessionExpiry(created, now time.Time) time.Time {
	idle, limit := now.Add(sessionIdle), created.Add(sessionMax)
	if idle.Before(limit) {
		return idle
	}
	return limit
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func sessionHash(r *http.Request) []byte {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	token, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(token) != 32 {
		return nil
	}
	h := sha256.Sum256(token)
	return h[:]
}

// currentLogin loads the session and account for the request, if any.
func (s *Server) currentLogin(r *http.Request) (login, error) {
	hash := sessionHash(r)
	if hash == nil {
		return login{}, store.ErrNotFound
	}
	now := s.now()
	x, err := s.Store.Session(r.Context(), hash, now)
	if err != nil {
		return login{}, err
	}
	a, err := s.Store.AccountByID(r.Context(), x.UserID)
	if err != nil {
		return login{}, err
	}
	// Writing on every request would be wasted work; a minute is precise
	// enough for an idle timeout counted in days.
	if x.Verified && now.Sub(x.SeenAt) > time.Minute {
		expires := sessionExpiry(x.CreatedAt, now)
		if err := s.Store.TouchSession(r.Context(), hash, now, expires); err != nil {
			return login{}, err
		}
	}
	return login{session: x, account: a}, nil
}

// signedIn lets a request through only with a fully verified session.
func (s *Server) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return s.withLogin(false, next)
}

// confirmed lets a request through only if the user also proved it was them
// within confirmWindow.
func (s *Server) confirmed(next http.HandlerFunc) http.HandlerFunc {
	return s.withLogin(false, func(w http.ResponseWriter, r *http.Request) {
		if !s.recentlyConfirmed(loginFrom(r.Context())) {
			writeConfirmFirst(w)
			return
		}
		next(w, r)
	})
}

// enrolling is for adding a second factor. It also lets through a
// half-finished login whose account has none yet, so it can set one up;
// nothing else is reachable until it does. An account that already has one
// must have confirmed recently.
func (s *Server) enrolling(next http.HandlerFunc) http.HandlerFunc {
	return s.withLogin(true, func(w http.ResponseWriter, r *http.Request) {
		if l := loginFrom(r.Context()); l.account.HasSecondFactor() && !s.recentlyConfirmed(l) {
			writeConfirmFirst(w)
			return
		}
		next(w, r)
	})
}

func (s *Server) recentlyConfirmed(l login) bool {
	return l.session.Verified && s.now().Sub(l.session.ConfirmedAt) <= confirmWindow
}

// writeConfirmFirst tells the interface to ask the user to confirm and then
// try again.
func writeConfirmFirst(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "confirm it is you first", "confirm": true})
}

func (s *Server) withLogin(allowEnrolling bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l, err := s.currentLogin(r)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusUnauthorized, errors.New("not logged in"))
			return
		case err != nil:
			s.fail(w, "load session", err)
			return
		}
		if !l.session.Verified && !(allowEnrolling && !l.account.HasSecondFactor()) {
			writeError(w, http.StatusUnauthorized, errors.New("the second step of logging in is not done"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), loginKey{}, l)))
	}
}

// clientIP is the visitor's address. Behind the proxy it comes from
// X-Forwarded-For, which the proxy always overwrites; a visitor cannot set it.
func clientIP(r *http.Request) string {
	if p, _ := peer.From(r.Context()); p.UID == 0 {
		return "local"
	}
	ip := r.Header.Get("X-Forwarded-For")
	if net.ParseIP(ip) == nil {
		return "unknown"
	}
	return ip
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
