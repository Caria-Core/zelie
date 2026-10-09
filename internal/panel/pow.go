package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/bits"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

// After this many failed logins from an address, or for an account from
// anywhere, each further attempt must come with a solved puzzle: a hash
// with some leading zero bits. At 16 bits a browser on a laptop takes about
// 0.3 s and a phone about a second; every third failure doubles it, up to
// 20 bits. A guesser pays that for every guess. The account's count is what
// slows a guesser who spreads over many addresses, since no one address has
// many failures. Nothing goes to a third party.
const (
	powAfter   = 3
	powMinBits = 16
	powMaxBits = 20
	powTTL     = 2 * time.Minute

	// powTopAt is the count of failures at which the puzzle is as big as it
	// gets. The account's counter uses it as its maximum, so when the count
	// cannot be read the puzzle is the biggest.
	powTopAt = powAfter + 3*(powMaxBits-powMinBits)
)

var errPow = msg.Define(http.StatusForbidden, "login.pow", "Your browser has to solve a short check first.")

// powBits grows with the failures, so guessing gets slower as it goes on.
func powBits(failures int) int {
	return min(powMinBits+(failures-powAfter)/3, powMaxBits)
}

// powNeeded is the size of the puzzle a login must solve, or zero when it
// need not yet, given the failures from its address and for its account.
func powNeeded(fromAddress, forAccount int) int {
	n := max(fromAddress, forAccount)
	if n < powAfter {
		return 0
	}
	return powBits(n)
}

type powChallenge struct {
	IP      string `json:"ip"`
	Bits    int    `json:"bits"`
	Expires int64  `json:"exp"`
	Nonce   []byte `json:"n"`
}

type powAnswer struct {
	Challenge string `json:"challenge"`
	Nonce     string `json:"nonce"`
}

// powUsed keeps the puzzles already solved until they expire, so one
// solution is not good for many guesses.
type powUsed struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (u *powUsed) take(challenge string, expires, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[string]time.Time{}
	}
	for c, exp := range u.m {
		if now.After(exp) {
			delete(u.m, c)
		}
	}
	if _, ok := u.m[challenge]; ok {
		return false
	}
	u.m[challenge] = expires
	return true
}

// newPow makes a puzzle for ip, sealed so it cannot be made easier.
func (s *Server) newPow(ip string, bits int, now time.Time) *msg.Error {
	c := powChallenge{IP: ip, Bits: bits, Expires: now.Add(powTTL).Unix(), Nonce: make([]byte, 16)}
	rand.Read(c.Nonce)
	b, _ := json.Marshal(c)
	sealed := base64.RawURLEncoding.EncodeToString(s.Sealer.Seal(b, "login-pow"))
	return errPow.Err("challenge", sealed, "bits", bits)
}

// powSolved checks an answer to a puzzle made for ip with at least bits.
func (s *Server) powSolved(a *powAnswer, ip string, bits int, now time.Time) bool {
	if a == nil || len(a.Challenge) > 1024 || len(a.Nonce) > 20 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(a.Challenge)
	if err != nil {
		return false
	}
	plain, err := s.Sealer.Open(raw, "login-pow")
	if err != nil {
		return false
	}
	var c powChallenge
	if json.Unmarshal(plain, &c) != nil || c.IP != ip || c.Bits < bits || now.Unix() > c.Expires {
		return false
	}
	if _, err := strconv.ParseUint(a.Nonce, 10, 64); err != nil {
		return false
	}
	if zeroBits(sha256.Sum256([]byte(a.Challenge+":"+a.Nonce))) < c.Bits {
		return false
	}
	return s.powUsed.take(a.Challenge, time.Unix(c.Expires, 0), now)
}

func zeroBits(h [32]byte) int {
	n := bits.LeadingZeros64(binary.BigEndian.Uint64(h[:8]))
	if n == 64 {
		n += bits.LeadingZeros64(binary.BigEndian.Uint64(h[8:16]))
	}
	return n
}
