package panel

import (
	"crypto/rand"
	"strconv"
	"strings"

	"github.com/Caria-Core/zelie/internal/egg"
)

// Eggs ship with an empty or made-up value for the passwords that guard a
// server's administration, and every server made from the egg would share
// it. A new server gets a random one instead. What players type to join
// stays as the egg has it: that password is the owner's to choose.

const (
	secretLength    = 24
	secretMinLength = 8
	secretAlphabet  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// placeholders are values that mean "put a real one here". They are compared
// without case and without the separators people put in them.
var placeholders = map[string]bool{
	"changeme": true, "changethis": true, "changeit": true, "password": true,
	"pass": true, "passwd": true, "admin": true, "secret": true, "somepassword": true,
	"yourpassword": true, "rcon": true, "test": true, "default": true, "letmein": true,
	"qwerty": true, "12345": true, "123456": true, "1234567": true, "12345678": true,
	"123456789": true,
}

func isPlaceholder(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	v = strings.NewReplacer("_", "", "-", "", " ", "").Replace(v)
	return placeholders[v]
}

// otherService marks variables that hold a login for a service the egg's
// game does not run. A made-up value would only make that login fail.
var otherService = map[string]bool{"STEAM": true, "GIT": true, "GITHUB": true, "GITLAB": true, "DISCORD": true}

// joinWords mark a password the players of the game type.
var joinWords = map[string]bool{
	"SERVER": true, "JOIN": true, "PLAYER": true, "GAME": true, "WORLD": true,
	"SESSION": true, "CLIENT": true, "PUBLIC": true, "LOBBY": true, "GUEST": true,
}

// secretKind says what a variable's name says about its value.
type secretKind int

const (
	notSecret secretKind = iota
	// joinSecret is a password players give to join: it is only filled in
	// when the egg requires one and has none.
	joinSecret
	// adminSecret guards the server's administration.
	adminSecret
)

func secretKindOf(env string) secretKind {
	name := strings.ToUpper(env)
	words := strings.Split(name, "_")
	for _, w := range words {
		if otherService[w] {
			return notSecret
		}
	}
	hasPass := strings.Contains(name, "PASS") || words[len(words)-1] == "PW"
	admin := strings.Contains(name, "RCON") || strings.Contains(name, "ADMIN")
	if admin && strings.Contains(name, "PASS") {
		return adminSecret
	}
	join := false
	for _, w := range words {
		join = join || joinWords[w]
	}
	last := words[len(words)-1]
	switch {
	case len(words) > 1 && !join && (last == "PASSWORD" || last == "PASS" || last == "PASSWD" || last == "TOKEN" || last == "SECRET"):
		return adminSecret
	case hasPass:
		return joinSecret
	}
	return notSecret
}

// secretValue is the value a new server gets for the variable, given the
// value the egg has for it: the same one, or a random one where the egg
// leaves an administrative secret empty or as a placeholder. The random
// value uses letters and digits only and fits the variable's rules; when it
// cannot, the egg's value stays.
func secretValue(v egg.Variable, value string) string {
	kind := secretKindOf(v.Env)
	if kind == notSecret {
		return value
	}
	empty := strings.TrimSpace(value) == ""
	switch {
	case kind == adminSecret && (empty || isPlaceholder(value)):
	case kind == joinSecret && empty && hasRule(v, "required"):
	default:
		return value
	}
	if s, ok := randomSecret(v); ok {
		return s
	}
	return value
}

func hasRule(v egg.Variable, name string) bool {
	for _, r := range v.Rules {
		if r == name {
			return true
		}
	}
	return false
}

// randomSecret makes a value of secretLength characters, or as many as the
// rules allow. ok is false when the rules ask for something else, such as a
// number, or leave no room for a strong value.
func randomSecret(v egg.Variable) (string, bool) {
	n := secretLength
	lo := 0
	for _, r := range v.Rules {
		name, arg, _ := strings.Cut(r, ":")
		if name == "integer" || name == "numeric" || name == "in" || name == "boolean" || name == "url" {
			return "", false
		}
		nums := []int{}
		for _, part := range strings.Split(arg, ",") {
			if x, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				nums = append(nums, x)
			}
		}
		switch {
		case name == "max" && len(nums) == 1:
			n = min(n, nums[0])
		case name == "min" && len(nums) == 1:
			lo = max(lo, nums[0])
		case name == "between" && len(nums) == 2:
			lo = max(lo, nums[0])
			n = min(n, nums[1])
		case name == "size" && len(nums) == 1:
			lo, n = max(lo, nums[0]), min(n, nums[0])
		}
	}
	n = max(n, lo)
	if n < secretMinLength {
		return "", false
	}
	buf := make([]byte, 0, n)
	for len(buf) < n {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", false
		}
		for _, c := range b {
			// 62 does not divide 256; values from 248 up would favour the
			// first letters.
			if c < 248 && len(buf) < n {
				buf = append(buf, secretAlphabet[int(c)%len(secretAlphabet)])
			}
		}
	}
	s := string(buf)
	// Rules such as a regex are not read above; the value must pass them.
	if v.Check(s) != nil {
		return "", false
	}
	return s, true
}

// initialValue is what a variable holds on a new server when nothing sets it.
func initialValue(v egg.Variable) string { return secretValue(v, v.Default) }
