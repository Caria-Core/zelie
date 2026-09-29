// Package secret seals the values of secret environment variables. The
// panel seals them with the core's public key and stores only the result;
// the private key never leaves the core, which opens them when it starts a
// container. A copy of the panel's database, or a bug that lets someone read
// it, therefore reveals no secret.
//
// It does not protect against someone who controls the panel outright: they
// could deploy a version of the app that prints its environment.
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// PublicKey is what the panel seals with.
type PublicKey [32]byte

func (k PublicKey) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// ParsePublicKey reads a key written by String.
func ParsePublicKey(s string) (PublicKey, error) {
	var k PublicKey
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != len(k) {
		return k, errors.New("invalid public key")
	}
	copy(k[:], b)
	return k, nil
}

// Keys is the core's key pair.
type Keys struct {
	public  PublicKey
	private [32]byte
}

// LoadOrCreate reads the private key at path, creating it the first time.
func LoadOrCreate(path string) (*Keys, error) {
	k := &Keys{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		rand.Read(k.private[:])
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(k.private[:])
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			return nil, err
		}
	case err != nil:
		return nil, err
	case len(b) != len(k.private):
		return nil, fmt.Errorf("%s is not a secret key", path)
	default:
		copy(k.private[:], b)
	}
	pub, err := curve25519.X25519(k.private[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	copy(k.public[:], pub)
	return k, nil
}

func (k *Keys) Public() PublicKey { return k.public }

var validName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`) })

// ValidName reports whether name can be an environment variable.
func ValidName(name string) bool { return validName().MatchString(name) }

// The sealed text names the app, so a value sealed for one app does not
// open for another.
const prefix = "zelie-env\x00"

// Seal seals name=value for app.
func Seal(key PublicKey, app, name, value string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("%q is not a valid variable name", name)
	}
	msg := prefix + app + "\x00" + name + "\x00" + value
	out, err := box.SealAnonymous(nil, []byte(msg), (*[32]byte)(&key), rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// ErrWrongApp is returned for a value sealed for another app.
var ErrWrongApp = errors.New("the value was sealed for another app")

// Open opens a value sealed for app and returns it as NAME=value.
func (k *Keys) Open(sealed, app string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", errors.New("the sealed value is not valid")
	}
	msg, ok := box.OpenAnonymous(nil, b, (*[32]byte)(&k.public), &k.private)
	if !ok || !strings.HasPrefix(string(msg), prefix) {
		return "", errors.New("the sealed value does not open with this key")
	}
	parts := strings.SplitN(strings.TrimPrefix(string(msg), prefix), "\x00", 3)
	if len(parts) != 3 || !ValidName(parts[1]) {
		return "", errors.New("the sealed value is not valid")
	}
	if parts[0] != app {
		return "", ErrWrongApp
	}
	return parts[1] + "=" + parts[2], nil
}
