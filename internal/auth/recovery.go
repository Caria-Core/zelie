package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"strings"
)

// Recovery codes are shown once and let an administrator in when their
// phone or security key is lost. Each one works once.
const recoveryCodes = 10

// recoveryAlphabet leaves out characters that are easy to misread.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// NewRecoveryCodes returns codes such as "k7m2p-x9qfa" and the hashes to
// store.
func NewRecoveryCodes() (codes []string, hashes [][]byte) {
	for range recoveryCodes {
		b := make([]byte, 10)
		rand.Read(b)
		var sb strings.Builder
		for i, c := range b {
			if i == 5 {
				sb.WriteByte('-')
			}
			// 256 is not a multiple of the alphabet size; the bias this
			// leaves is far too small to matter for about 50 bits of randomness.
			sb.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		codes = append(codes, sb.String())
		hashes = append(hashes, HashRecoveryCode(sb.String()))
	}
	return codes, hashes
}

// HashRecoveryCode ignores case, spaces and dashes, since people type these
// by hand.
func HashRecoveryCode(code string) []byte {
	code = strings.ToLower(code)
	code = strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, code)
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}
