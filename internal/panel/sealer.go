package panel

import (
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
)

// Sealer encrypts the panel's own secrets, such as authenticator app keys,
// before they go into the database. A copy of the database alone, from a
// backup for example, then does not give them away.
type Sealer struct{ aead cipher.AEAD }

// LoadSealer reads the key at path, creating it on first start.
func LoadSealer(path string) (*Sealer, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, chacha20poly1305.KeySize)
		rand.Read(key)
		// O_EXCL: if two panels ever start at once, one fails instead of
		// both writing different keys.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("%s: key has the wrong length", path)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plain. purpose is bound to the result, so a value sealed for
// one use cannot be passed off as another.
func (s *Sealer) Seal(plain []byte, purpose string) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plain, []byte(purpose))
}

func (s *Sealer) Open(sealed []byte, purpose string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value is too short")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], []byte(purpose))
}
