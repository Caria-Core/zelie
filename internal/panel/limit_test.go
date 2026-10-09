package panel

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

// hashBytes is the memory one argon2id hash takes. Counting what a stretch
// of work allocates tells whether it hashed a password, without timing it.
const hashBytes = 19 << 20

func hashesDuring(f func()) int {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return int((after.TotalAlloc - before.TotalAlloc) / hashBytes)
}

// inParallel runs f for 0 to n-1 all at once and returns what each gave.
func inParallel[T any](n int, f func(i int) T) []T {
	out := make([]T, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i] = f(i)
		}()
	}
	close(start)
	wg.Wait()
	return out
}

type answer struct {
	status int
	code   string
}

func answerOf(code int, out map[string]any) answer {
	c, _ := out["code"].(string)
	return answer{code, c}
}

func tally(answers []answer) map[answer]int {
	m := map[answer]int{}
	for _, a := range answers {
		m[a]++
	}
	return m
}

func TestKeyLocks(t *testing.T) {
	var k keyLocks
	ctx := context.Background()
	unlock, err := k.lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Another key does not wait.
	other, err := k.lock(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	other()

	// The same key waits until it is let go, and gives up when asked to.
	waiting, cancel := context.WithCancel(ctx)
	gone := make(chan error)
	go func() {
		_, err := k.lock(waiting, "a")
		gone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-gone; err == nil {
		t.Error("a waiter was let in while the lock was held")
	}
	unlock()
	again, err := k.lock(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	again()

	// Nothing is kept for keys nobody holds or waits for.
	if len(k.m) != 0 {
		t.Errorf("%d locks left over", len(k.m))
	}
}

// Guessers on many addresses reach the account at once. Only a few get
// their password checked; the rest are asked for a puzzle, as they would be
// one after another.
func TestParallelWrongPasswordsOfAnAccountAreCounted(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)

	// From many addresses, so only the account's count matters.
	wrong := map[string]string{"email": "a@example.com", "password": "wrong password"}
	got := tally(inParallel(200, func(i int) answer {
		guesser := &browser{t: t, h: h, ip: fmt.Sprintf("203.0.%d.%d", 113+i/250, 1+i%250)}
		return answerOf(guesser.do("POST", "/api/login", wrong))
	}))
	if want := (map[answer]int{{http.StatusUnauthorized, "login.wrong"}: powAfter, {http.StatusForbidden, "login.pow"}: 200 - powAfter}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("answers %v, want %v", got, want)
	}
}

func TestParallelWrongPasswordsFromOneAddress(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)

	// Only the first few get a plain refusal; the rest are asked for the
	// puzzle, as one after another they would be.
	got := tally(inParallel(30, func(int) answer {
		guesser := &browser{t: t, h: h, ip: "203.0.113.9"}
		return answerOf(guesser.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "wrong password"}))
	}))
	if want := (map[answer]int{{http.StatusUnauthorized, "login.wrong"}: powAfter, {http.StatusForbidden, "login.pow"}: 30 - powAfter}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("answers %v, want %v", got, want)
	}
}

func TestParallelWrongCodesAreCounted(t *testing.T) {
	_, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	secret := out["secret"].(string)
	b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, secret, *now)})
	b.do("POST", "/api/logout", nil)
	if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"}); code != http.StatusOK {
		t.Fatalf("password step: %d", code)
	}

	// Codes that cannot be right, whichever step the clock is in.
	valid := map[string]bool{}
	for _, d := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		valid[totpNow(t, secret, now.Add(d))] = true
	}
	var wrong []string
	for i := 0; len(wrong) < 60; i++ {
		if c := fmt.Sprintf("%06d", i); !valid[c] {
			wrong = append(wrong, c)
		}
	}
	got := tally(inParallel(len(wrong), func(i int) answer {
		guesser := &browser{t: t, h: h, ip: "198.51.100.7", cookie: b.cookie}
		return answerOf(guesser.do("POST", "/api/login/totp", map[string]string{"code": wrong[i]}))
	}))
	if want := (map[answer]int{{http.StatusUnauthorized, "login.bad_code"}: 10, {http.StatusTooManyRequests, "login.too_many"}: len(wrong) - 10}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("answers %v, want %v", got, want)
	}
}

// A failure that cannot be saved must not pass as an ordinary wrong answer:
// it would go uncounted, and the limits would stop limiting.
func TestFailureThatCannotBeSavedIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{Store: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ProxyUID: proxyUID}
	sealer, err := LoadSealer(filepath.Join(t.TempDir(), "panel.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Sealer = sealer
	now := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return now }
	h := s.Handler()

	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, out["secret"].(string), now)})
	b.do("POST", "/api/logout", nil)
	if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"}); code != http.StatusOK {
		t.Fatalf("password step: %d", code)
	}

	// Reads keep working while every write of a failure is refused, as when
	// the disk is full.
	other, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`CREATE TRIGGER no_failures BEFORE INSERT ON login_failures BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}

	if code, out := b.do("POST", "/api/login/totp", map[string]string{"code": "000000"}); code != http.StatusInternalServerError || out["code"] != "server.failed" {
		t.Errorf("a wrong code that was not counted: %d %v", code, out)
	}
	guesser := &browser{t: t, h: h, ip: "198.51.100.8"}
	if code, out := guesser.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "wrong password"}); code != http.StatusInternalServerError || out["code"] != "server.failed" {
		t.Errorf("a wrong password that was not counted: %d %v", code, out)
	}
}

func TestLoginLimitsCountAnIPv6PrefixAsOne(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)
	// Each try is for another account, so only the address is counted.
	tries := 0
	login := func(ip string) (int, map[string]any) {
		b.ip = ip
		tries++
		return b.do("POST", "/api/login", map[string]string{"email": fmt.Sprintf("guess%d@example.com", tries), "password": "wrong password"})
	}

	// A customer with a /64 can use any address in it; each one is the same
	// visitor, so the puzzle comes after the same few failures.
	for i := range powAfter {
		if code, _ := login(fmt.Sprintf("2001:db8:1:2::%x", i+1)); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code, out := login("2001:db8:1:2:ffff::1"); code != http.StatusForbidden || out["code"] != "login.pow" {
		t.Errorf("another address of the same /64: %d %v", code, out)
	}
	if code, _ := login("2001:db8:1:3::1"); code != http.StatusUnauthorized {
		t.Errorf("another /64: %d", code)
	}
}

func TestSetupLimitCountsAnIPv6PrefixAsOne(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	setup := func(ip string) int {
		b.ip = ip
		code, _ := b.do("POST", "/api/setup", map[string]string{"token": "wrong", "email": "a@example.com", "password": "long enough pw"})
		return code
	}
	for i := range 30 {
		if code := setup(fmt.Sprintf("2001:db8:1:2::%x", i+1)); code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code := setup("2001:db8:1:2:ffff::1"); code != http.StatusTooManyRequests {
		t.Errorf("the same /64: %d", code)
	}
	if code := setup("2001:db8:1:3::1"); code != http.StatusForbidden {
		t.Errorf("another /64: %d", code)
	}
}

// Hashing a password costs the panel 19 MB and a slot that logins wait for,
// so a stranger's request must not cause it unless the link is good.
func TestSetupAndResetHashOnlyForAGoodLink(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	token := setupToken(t, h)
	setup := func(token string) int {
		code, _ := b.do("POST", "/api/setup", map[string]string{"token": token, "email": "a@example.com", "password": "long enough pw"})
		return code
	}

	if n := hashesDuring(func() { setup("wrong") }); n != 0 {
		t.Errorf("a wrong setup link was hashed for: %d", n)
	}
	if n := hashesDuring(func() { setup(token) }); n == 0 {
		t.Fatal("hashes were not seen, so this test shows nothing")
	}
	if n := hashesDuring(func() {
		for range 20 {
			if code := setup(token); code != http.StatusConflict {
				t.Errorf("setup after it was done: %d", code)
			}
		}
	}); n != 0 {
		t.Errorf("setup after it was done hashed %d passwords", n)
	}

	_, out := resetLink(t, h, "")
	link := out["token"].(string)
	reset := func(token string) int {
		code, _ := b.do("POST", "/api/reset", map[string]string{"token": token, "password": "a new password"})
		return code
	}
	if n := hashesDuring(func() { reset(link + "x") }); n != 0 {
		t.Errorf("a wrong reset link was hashed for: %d", n)
	}
	if n := hashesDuring(func() { reset(link) }); n == 0 {
		t.Error("the good reset link was not hashed for")
	}
	if n := hashesDuring(func() { reset(link) }); n != 0 {
		t.Errorf("a spent reset link was hashed for: %d", n)
	}
}
