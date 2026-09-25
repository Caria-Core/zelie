package panel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/Caria-Core/zelie/internal/store"
)

var errPasskeyNeedsDomain = errors.New("passkeys need the panel on a domain name; use an authenticator app on a bare IP address")

// relyingParty ties passkeys to the host name the panel is served on. The
// proxy forwards only the configured panel host, so r.Host can be trusted
// here.
func relyingParty(r *http.Request) (*webauthn.WebAuthn, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return nil, errPasskeyNeedsDomain
	}
	return webauthn.New(&webauthn.Config{
		RPID:          host,
		RPDisplayName: "Zelie",
		// The origin keeps the port, if the visitor used one.
		RPOrigins: []string{"https://" + strings.ToLower(r.Host)},
	})
}

// passkeyUser adapts an account to what the WebAuthn library expects.
type passkeyUser struct {
	account store.Account
	creds   []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(u.account.ID))
}
func (u passkeyUser) WebAuthnName() string                       { return u.account.Email }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.account.Email }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (s *Server) passkeyUser(r *http.Request, a store.Account) (passkeyUser, error) {
	keys, err := s.Store.Passkeys(r.Context(), a.ID)
	if err != nil {
		return passkeyUser{}, err
	}
	u := passkeyUser{account: a}
	for _, k := range keys {
		var c webauthn.Credential
		if err := json.Unmarshal(k.Credential, &c); err != nil {
			return passkeyUser{}, err
		}
		u.creds = append(u.creds, c)
	}
	return u, nil
}

func (s *Server) passkeyOptions(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	rp, err := relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.passkeyUser(r, l.account)
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	opts, data, err := rp.BeginRegistration(u,
		webauthn.WithExclusions(webauthn.Credentials(u.creds).CredentialDescriptors()),
		// A resident key lets a later version log in with the passkey alone.
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
	)
	if err != nil {
		s.fail(w, "begin passkey registration", err)
		return
	}
	s.guards.pending.put(l.session.Hash, "passkey-add", data, s.now())
	writeJSON(w, http.StatusOK, opts)
}

// addPasskey expects the browser's credential as the body and the name the
// user gave it in ?name=.
func (s *Server) addPasskey(w http.ResponseWriter, r *http.Request) {
	l := loginFrom(r.Context())
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 60 {
		writeError(w, http.StatusBadRequest, errors.New("the name is too long"))
		return
	}
	v, ok := s.guards.pending.take(l.session.Hash, "passkey-add", s.now())
	if !ok {
		writeError(w, http.StatusBadRequest, errNoCeremony)
		return
	}
	rp, err := relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.passkeyUser(r, l.account)
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	cred, err := rp.FinishRegistration(u, *v.(*webauthn.SessionData), r)
	if err != nil {
		s.Log.Warn("passkey registration failed", "user", l.account.ID, "err", err)
		writeError(w, http.StatusBadRequest, errors.New("the passkey could not be added"))
		return
	}
	b, err := json.Marshal(cred)
	if err != nil {
		s.fail(w, "encode passkey", err)
		return
	}
	err = s.Store.AddPasskey(r.Context(), store.Passkey{ID: cred.ID, UserID: l.account.ID, Name: name, Credential: b, CreatedAt: s.now()})
	if err != nil {
		s.fail(w, "save passkey", err)
		return
	}
	s.Log.Info("passkey added", "user", l.account.ID)
	s.factorAdded(w, r, l)
}

func (s *Server) loginPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	l, ok := s.halfLogin(w, r)
	if !ok {
		return
	}
	rp, err := relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.passkeyUser(r, l.account)
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	if len(u.creds) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("this account has no passkey"))
		return
	}
	opts, data, err := rp.BeginLogin(u)
	if err != nil {
		s.fail(w, "begin passkey login", err)
		return
	}
	s.guards.pending.put(l.session.Hash, "passkey-login", data, s.now())
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) loginPasskey(w http.ResponseWriter, r *http.Request) {
	l, ok := s.halfLogin(w, r)
	if !ok {
		return
	}
	v, ok := s.guards.pending.take(l.session.Hash, "passkey-login", s.now())
	if !ok {
		writeError(w, http.StatusBadRequest, errNoCeremony)
		return
	}
	rp, err := relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.passkeyUser(r, l.account)
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	cred, err := rp.FinishLogin(u, *v.(*webauthn.SessionData), r)
	if err != nil {
		s.Log.Warn("passkey login failed", "user", l.account.ID, "err", err)
		s.secondFailed(w, l)
		return
	}
	b, err := json.Marshal(cred)
	if err != nil {
		s.fail(w, "encode passkey", err)
		return
	}
	if err := s.Store.UpdatePasskey(r.Context(), cred.ID, b, s.now()); err != nil {
		s.fail(w, "update passkey", err)
		return
	}
	s.verified(w, r, l, "passkey")
}
