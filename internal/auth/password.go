// Package auth holds the pieces of logging in that do not depend on the web:
// password hashing, one-time codes and recovery codes.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters from the OWASP password storage guidance. Each hash
// takes about 19 MB of memory, so the panel also limits how many run at once.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// MinPasswordLength follows NIST SP 800-63B for passwords used together with
// a second factor.
const MinPasswordLength = 10

// maxPasswordLength keeps a huge request body from turning into a huge hash.
const maxPasswordLength = 512

var hashSlots = make(chan struct{}, 2)

// CheckPasswordRules reports why a new password is not acceptable.
func CheckPasswordRules(pw string) error {
	switch n := len([]rune(pw)); {
	case n < MinPasswordLength:
		return fmt.Errorf("the password needs at least %d characters", MinPasswordLength)
	case len(pw) > maxPasswordLength:
		return errors.New("the password is too long")
	}
	return nil
}

// HashPassword returns an argon2id hash in the PHC string format.
func HashPassword(pw string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key := derive([]byte(pw), salt, argonTime, argonMemory, argonThreads)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key))
}

// CheckPassword reports whether pw matches the hash. It reads the parameters
// from the hash, so older hashes keep working if the defaults change.
func CheckPassword(hash, pw string) bool {
	if len(pw) > maxPasswordLength {
		return false
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m > 1<<20 || t > 16 || p == 0 {
		return false
	}
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := derive([]byte(pw), salt, t, m, p)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DummyCheck costs as much as CheckPassword. It is used when an account does
// not exist, so response times do not tell who has one.
func DummyCheck(pw string) {
	CheckPassword(dummyHash(), pw)
}

// dummyHash is made on first use so other zelie commands do not pay for it.
var dummyHash = sync.OnceValue(func() string { return HashPassword("not a real password") })

func derive(pw, salt []byte, t, m uint32, p uint8) []byte {
	hashSlots <- struct{}{}
	key := argon2.IDKey(pw, salt, t, m, p, argonKeyLen)
	<-hashSlots
	// The memory argon2id just used would otherwise stay with the process
	// until the next garbage collection, which an idle panel rarely needs.
	// Logins are rare enough that handing it back at once costs nothing.
	if len(hashSlots) == 0 {
		debug.FreeOSMemory()
	}
	return key
}

var b64 = base64.RawStdEncoding
