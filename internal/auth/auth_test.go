package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash %q", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("right password rejected")
	}
	if CheckPassword(h, "correct horse batterY") {
		t.Error("wrong password accepted")
	}
	if HashPassword("same") == HashPassword("same") {
		t.Error("two hashes of one password are equal; the salt is not random")
	}
	for _, bad := range []string{"", "$argon2i$v=19$m=19456,t=2,p=1$AAAA$AAAA", "$argon2id$v=19$m=99999999,t=2,p=1$AAAA$AAAA"} {
		if CheckPassword(bad, "x") {
			t.Errorf("hash %q accepted", bad)
		}
	}
	if CheckPasswordRules("short") == nil || CheckPasswordRules("long enough pw") != nil {
		t.Error("password rules")
	}
}

// The test vectors from RFC 6238, appendix B, for SHA-1. The RFC uses eight
// digits; six digits are the last six of those.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	} {
		step, ok := CheckTOTP(secret, want, time.Unix(unix, 0))
		if !ok || step != unix/30 {
			t.Errorf("at %d: step %d ok %v", unix, step, ok)
		}
	}
}

func TestTOTPWindow(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	code := totpCode(secret, now.Unix()/30)
	for _, d := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		if _, ok := CheckTOTP(secret, code, now.Add(d)); !ok {
			t.Errorf("code rejected %v away", d)
		}
	}
	if _, ok := CheckTOTP(secret, code, now.Add(90*time.Second)); ok {
		t.Error("code accepted 90s later")
	}
	if _, ok := CheckTOTP(secret, "12345", now); ok {
		t.Error("five digits accepted")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes()
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatal(len(codes), len(hashes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 11 || c[5] != '-' || seen[c] {
			t.Errorf("code %q", c)
		}
		seen[c] = true
		typed := strings.ToUpper(strings.ReplaceAll(c, "-", " "))
		if string(HashRecoveryCode(typed)) != string(hashes[i]) {
			t.Errorf("%q typed as %q does not match", c, typed)
		}
	}
}

func TestTOTPURI(t *testing.T) {
	uri := TOTPURI([]byte("12345678901234567890"), "Zelie", "a@example.com")
	want := "otpauth://totp/Zelie:a@example.com?issuer=Zelie&secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if uri != want {
		t.Errorf("got  %s\nwant %s", uri, want)
	}
}

func TestSFTPChecksHaveTheirOwnPool(t *testing.T) {
	h := HashPassword("correct horse battery")
	// Hold the only SFTP slot: a web check must not wait for it.
	sftpPool.slots <- struct{}{}
	done := make(chan bool)
	go func() { done <- CheckPassword(h, "correct horse battery") }()
	select {
	case ok := <-done:
		if !ok {
			t.Error("right password rejected")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a web check waited for the SFTP pool")
	}
	sftpDone := make(chan bool)
	go func() { sftpDone <- CheckSFTPPassword(h, "correct horse battery") }()
	select {
	case <-sftpDone:
		t.Fatal("an SFTP check ran with the pool full")
	case <-time.After(200 * time.Millisecond):
	}
	<-sftpPool.slots
	if !<-sftpDone {
		t.Error("right password rejected by the SFTP pool")
	}
}
