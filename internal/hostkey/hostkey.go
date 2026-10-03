// Package hostkey keeps the SSH host key of the SFTP server. The core, which
// is root, makes it before anyone has connected, so the panel can show its
// fingerprint. The server only reads it.
package hostkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/crypto/ssh"
)

// File is the name of the key inside its folder, and of the old one inside
// the SFTP server's state folder.
const File = "host_key"

const (
	dirMode  = 0o750
	fileMode = 0o640
	// A private key is a few hundred bytes. The cap keeps a swapped-out
	// file from making root read something huge.
	maxSize = 64 << 10
	tmpName = File + ".tmp"
)

// Load reads the key at path. It never writes, so the unprivileged SFTP
// server can use it on a folder it cannot change.
func Load(path string) (ssh.Signer, error) {
	pemBytes, err := readLimited(path)
	if err != nil {
		return nil, err
	}
	key, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}
	return key, nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readRegular(f)
}

func readRegular(f *os.File) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxSize {
		return nil, fmt.Errorf("%s is not a plain key file", f.Name())
	}
	return io.ReadAll(io.LimitReader(f, maxSize))
}

// Ensure makes sure dir holds the host key and returns it. dir must be
// writable by root alone: the key belongs to root and the group gid, which
// may read it. If there is no key yet and legacyDir holds an ed25519 one, it
// is copied over so the fingerprint stays what clients have trusted;
// otherwise a new key is made. The old file is left alone, since an update
// may roll back to a binary that reads it.
//
// legacyDir is the SFTP user's own folder, so nothing in it is trusted:
// the file is opened without following links and must be a small regular
// file. Nothing is ever written there.
func Ensure(dir string, gid int, legacyDir string) (ssh.Signer, error) {
	if err := prepareDir(dir, gid); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	f, err := root.OpenFile(File, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	switch {
	case err == nil:
		defer f.Close()
		pemBytes, err := readRegular(f)
		if err != nil {
			return nil, err
		}
		key, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("read host key %s: %w", f.Name(), err)
		}
		// Put right whatever an earlier version or a person left behind.
		if err := f.Chown(os.Geteuid(), gid); err != nil {
			return nil, err
		}
		if err := f.Chmod(fileMode); err != nil {
			return nil, err
		}
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	pemBytes := readLegacy(legacyDir)
	if pemBytes == nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, "zelie sftp host key")
		if err != nil {
			return nil, err
		}
		pemBytes = pem.EncodeToMemory(block)
	}
	key, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	if err := write(root, pemBytes, gid); err != nil {
		return nil, err
	}
	return key, nil
}

// prepareDir makes dir if needed and checks that it is a real folder that
// root owns, so a link or someone else's folder is never trusted.
func prepareDir(dir string, gid int) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	if owner(st) != os.Geteuid() {
		return fmt.Errorf("%s is not owned by the user running the core", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Chown(".", os.Geteuid(), gid); err != nil {
		return err
	}
	return root.Chmod(".", dirMode)
}

func owner(st os.FileInfo) int {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid)
	}
	return -1
}

// write puts the key in place through a temporary file, so a reader never
// sees half of it.
func write(root *os.Root, pemBytes []byte, gid int) error {
	// A leftover from an earlier crash. Remove does not follow a link.
	if err := root.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		root.Remove(tmpName)
		return err
	}
	if err := f.Chown(os.Geteuid(), gid); err != nil {
		return fail(err)
	}
	if err := f.Chmod(fileMode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(pemBytes); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		root.Remove(tmpName)
		return err
	}
	if err := root.Rename(tmpName, File); err != nil {
		root.Remove(tmpName)
		return err
	}
	return nil
}

// readLegacy returns the old key's bytes if there is a usable ed25519 key,
// and nil if not for any reason.
func readLegacy(dir string) []byte {
	if dir == "" {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	// O_NONBLOCK: opening a FIFO planted under that name must not hang root.
	f, err := root.OpenFile(File, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := readRegular(f)
	if err != nil {
		return nil
	}
	key, err := ssh.ParsePrivateKey(b)
	if err != nil || key.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil
	}
	return b
}
