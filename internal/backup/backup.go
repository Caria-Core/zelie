// Package backup keeps backups as files on this server. Every backup is
// compressed with zstd and then encrypted with age, locally too, so a copy
// of a backup file on its own gives nothing away. The key lives with the
// core; its owner can download it as a recovery file and open any backup
// with the standard age and zstd tools, without Zelie.
package backup

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Key encrypts and opens backups.
type Key struct {
	id *age.X25519Identity
}

// LoadOrCreateKey reads the key at path, creating it the first time.
func LoadOrCreateKey(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString(id.String() + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			return nil, err
		}
		return &Key{id: id}, nil
	}
	if err != nil {
		return nil, err
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("%s is not a backup key", path)
	}
	return &Key{id: id}, nil
}

// Recovery is the key as a file its owner keeps off the server. The age
// tool reads it as an identity file; the lines starting with # are for the
// person who finds it later.
func (k *Key) Recovery(host string, now time.Time) string {
	return fmt.Sprintf(`# Zelie backup recovery key for %s, saved %s.
# It opens every backup this server makes. Keep it somewhere safe and
# off the server. To open a backup without Zelie:
#   age -d -i zelie-recovery.txt BACKUP.zst.age | zstd -d > BACKUP
%s
`, host, now.UTC().Format("2006-01-02"), k.id)
}

// encrypt returns a writer that compresses and encrypts into w. Close
// finishes both; without it the result cannot be opened.
func (k *Key) encrypt(w io.Writer) (io.WriteCloser, error) {
	enc, err := age.Encrypt(w, k.id.Recipient())
	if err != nil {
		return nil, err
	}
	z, err := zstd.NewWriter(enc, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	return &chain{w: z, closers: []io.Closer{z, enc}}, nil
}

// decrypt reads what encrypt wrote.
func (k *Key) decrypt(r io.Reader) (io.ReadCloser, error) {
	dec, err := age.Decrypt(r, k.id)
	if err != nil {
		return nil, err
	}
	z, err := zstd.NewReader(dec)
	if err != nil {
		return nil, err
	}
	return z.IOReadCloser(), nil
}

type chain struct {
	w       io.Writer
	closers []io.Closer
}

func (c *chain) Write(b []byte) (int, error) { return c.w.Write(b) }

func (c *chain) Close() error {
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Dir holds the backups, one directory per app.
type Dir struct {
	Root string
	Key  *Key
	now  func() time.Time
}

// Info describes a backup file.
type Info struct {
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	Created time.Time `json:"created"`
	// For volume backups: the size of the files inside, and how many
	// changed while they were copied.
	Size    int64 `json:"size,omitempty"`
	Changed int   `json:"changed,omitempty"`
}

var (
	validApp  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	validName = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9]+\.[a-z]+\.zst\.age$`)
)

// ValidName reports whether name is a backup file's name. Names come from
// the panel and end up in paths, so nothing else is let through.
func ValidName(name string) bool { return validName.MatchString(name) }

// ErrInvalid is returned for an app or backup name that is not allowed.
var ErrInvalid = errors.New("invalid name")

func (d *Dir) path(app, name string) (string, error) {
	if !validApp.MatchString(app) {
		return "", fmt.Errorf("app %q: %w", app, ErrInvalid)
	}
	if name != "" && !validName.MatchString(name) {
		return "", fmt.Errorf("backup %q: %w", name, ErrInvalid)
	}
	return filepath.Join(d.Root, app, name), nil
}

func (d *Dir) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// Writer is a backup being written. Nothing appears under its name until
// Commit; Abort, or a crash, leaves only a temporary file behind.
type Writer struct {
	io.Writer
	file  *os.File
	enc   io.WriteCloser
	final string
	info  Info
}

// Create starts a backup of app. ext says what is inside: sql, rdb, tar.
func (d *Dir) Create(app, ext string) (*Writer, error) {
	now := d.clock().UTC()
	name := fmt.Sprintf("%s-%s.%s.zst.age", now.Format("20060102T150405Z"), randomTag(), ext)
	final, err := d.path(app, name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(final), ".partial-*")
	if err != nil {
		return nil, err
	}
	buf := bufio.NewWriterSize(f, 1<<20)
	enc, err := d.Key.encrypt(buf)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &Writer{Writer: enc, file: f, enc: &flushing{enc, buf}, final: final, info: Info{Name: name, Created: now}}, nil
}

// randomTag keeps two backups made in the same second apart.
func randomTag() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type flushing struct {
	io.WriteCloser
	buf *bufio.Writer
}

func (f *flushing) Close() error {
	if err := f.WriteCloser.Close(); err != nil {
		return err
	}
	return f.buf.Flush()
}

// Commit finishes the file and gives it its name.
func (w *Writer) Commit() (Info, error) {
	err := w.enc.Close()
	if err == nil {
		err = w.file.Sync()
	}
	if cerr := w.file.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(w.file.Name(), w.final)
	}
	if err != nil {
		os.Remove(w.file.Name())
		return Info{}, err
	}
	st, err := os.Stat(w.final)
	if err != nil {
		return Info{}, err
	}
	w.info.Bytes = st.Size()
	return w.info, nil
}

// Abort throws the file away.
func (w *Writer) Abort() {
	// Closing stops the compressor's goroutines; what it writes is lost.
	w.enc.Close()
	w.file.Close()
	os.Remove(w.file.Name())
}

// Open reads a backup back as it was before it was compressed.
func (d *Dir) Open(app, name string) (io.ReadCloser, error) {
	f, err := d.OpenRaw(app, name)
	if err != nil {
		return nil, err
	}
	r, err := d.Key.decrypt(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &both{r, f}, nil
}

type both struct {
	io.ReadCloser
	file *os.File
}

func (b *both) Close() error {
	b.ReadCloser.Close()
	return b.file.Close()
}

// OpenRaw opens the encrypted file, for downloads.
func (d *Dir) OpenRaw(app, name string) (*os.File, error) {
	p, err := d.path(app, name)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("no backup named: %w", ErrInvalid)
	}
	return os.Open(p)
}

// List returns the backups of app, newest first.
func (d *Dir) List(app string) ([]Info, error) {
	p, err := d.path(app, "")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !validName.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		created, _ := time.Parse("20060102T150405Z", e.Name()[:16])
		out = append(out, Info{Name: e.Name(), Bytes: fi.Size(), Created: created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Remove deletes one backup.
func (d *Dir) Remove(app, name string) error {
	if name == "" {
		return fmt.Errorf("no backup named: %w", ErrInvalid)
	}
	p, err := d.path(app, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
