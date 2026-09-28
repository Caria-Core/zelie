package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

// One-time codes follow RFC 6238 with the settings every authenticator app
// understands: SHA-1, six digits, a new code every 30 seconds.
const (
	totpPeriod = 30
	totpDigits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret, as RFC 4226 recommends.
func NewTOTPSecret() []byte {
	secret := make([]byte, 20)
	rand.Read(secret)
	return secret
}

// TOTPURI is what the QR code holds, so an authenticator app can add the
// account in one scan.
func TOTPURI(secret []byte, issuer, account string) string {
	v := url.Values{}
	v.Set("secret", b32.EncodeToString(secret))
	v.Set("issuer", issuer)
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// TOTPSecretText is the secret for typing into an app by hand.
func TOTPSecretText(secret []byte) string { return b32.EncodeToString(secret) }

// CheckTOTP looks for code in the current time step and one step either side,
// to allow for clocks that are slightly off. It returns the step that matched,
// which the caller stores so the same code cannot be used twice.
func CheckTOTP(secret []byte, code string, now time.Time) (step int64, ok bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	for s := cur - 1; s <= cur+1; s++ {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// TOTPCode is the code an authenticator app shows in a time step, for
// tools that log in the way a person would.
func TOTPCode(secret []byte, step int64) string { return totpCode(secret, step) }

func totpCode(secret []byte, step int64) string {
	mac := hmac.New(sha1.New, secret)
	binary.Write(mac, binary.BigEndian, uint64(step))
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, n%1_000_000)
}
