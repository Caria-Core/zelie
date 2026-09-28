package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/s3"
)

// OffsiteConfig is where backups are copied off this server: a folder in
// an S3 bucket, and the keys to it.
type OffsiteConfig struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	// SecretKey may be left out when changing the folder or the region;
	// the one already saved is kept.
	SecretKey string `json:"secret_key,omitempty"`
}

// OffsiteInfo is the destination as the panel may see it: everything but
// the secret key.
type OffsiteInfo struct {
	Set       bool   `json:"set"`
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
}

// OffsiteBackup is a backup file found in the bucket.
type OffsiteBackup struct {
	App     string    `json:"app"`
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	Created time.Time `json:"created"`
}

// Offsite holds the destination. Its keys stay in a file only root reads:
// the panel can set them but never read them back, so a panel taken over
// cannot hand them out. Pointing the backups at another bucket gains it
// nothing either, since they are encrypted.
type Offsite struct {
	path   string
	mu     sync.Mutex
	cfg    OffsiteConfig
	client *s3.Client
}

// LoadOffsite reads the destination saved at path, if there is one.
func LoadOffsite(path string) (*Offsite, error) {
	o := &Offsite{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg OffsiteConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c, err := s3.New(cfg.s3())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	o.cfg, o.client = cfg, c
	return o, nil
}

func (c OffsiteConfig) s3() s3.Config {
	return s3.Config{Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, AccessKey: c.AccessKey, SecretKey: c.SecretKey}
}

func (c OffsiteConfig) info() OffsiteInfo {
	return OffsiteInfo{Set: true, Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.Prefix, AccessKey: c.AccessKey}
}

// current returns the client and the destination, or false when there is
// none.
func (o *Offsite) current() (*s3.Client, OffsiteConfig, bool) {
	if o == nil {
		return nil, OffsiteConfig{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.client, o.cfg, o.client != nil
}

func (o *Offsite) save(cfg OffsiteConfig, c *s3.Client) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := writeFileAtomic(o.path, b); err != nil {
		return err
	}
	o.cfg, o.client = cfg, c
	return nil
}

func (o *Offsite) clear() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.Remove(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	o.cfg, o.client = OffsiteConfig{}, nil
	return nil
}

// writeFileAtomic replaces path with b, readable by root alone, so a crash
// leaves either the old file or the new one.
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

var (
	errOffsiteNotSet      = msg.Define(http.StatusConflict, "offsite.not_set", "No off-site storage is set up.")
	errOffsiteEndpoint    = msg.Define(http.StatusBadRequest, "offsite.bad_endpoint", "The address must look like https://s3.example.com, with nothing after the host name.")
	errOffsiteBucket      = msg.Define(http.StatusBadRequest, "offsite.bad_bucket", "A bucket name has 3 to 63 lowercase letters, digits, dots and dashes.")
	errOffsiteRegion      = msg.Define(http.StatusBadRequest, "offsite.bad_region", "A region has lowercase letters, digits and dashes, like eu-central-1.")
	errOffsitePrefix      = msg.Define(http.StatusBadRequest, "offsite.bad_prefix", "The folder has letters, digits, dots, dashes and underscores, with a / between its parts.")
	errOffsiteKeys        = msg.Define(http.StatusBadRequest, "offsite.need_keys", "Enter both the access key and the secret key.")
	errOffsiteWrongKeys   = msg.Define(http.StatusUnprocessableEntity, "offsite.wrong_keys", "The storage did not accept the access key or the secret key.")
	errOffsiteNoBucket    = msg.Define(http.StatusUnprocessableEntity, "offsite.no_bucket", "There is no bucket named {bucket} at {endpoint}.")
	errOffsiteDenied      = msg.Define(http.StatusUnprocessableEntity, "offsite.denied", "The keys work, but they may not write, read and delete in {bucket}.")
	errOffsiteAnswer      = msg.Define(http.StatusUnprocessableEntity, "offsite.failed", "The storage answered: {detail}")
	errOffsiteUnreachable = msg.Define(http.StatusUnprocessableEntity, "offsite.unreachable", "Could not reach {endpoint}: {detail}")
	errOffsiteMismatch    = msg.Define(http.StatusUnprocessableEntity, "offsite.mismatch", "The storage gave back something other than what was written to it.")
	errOffsiteGone        = msg.Define(http.StatusNotFound, "offsite.gone", "This backup is no longer in the off-site storage.")
	errOffsiteNoRoom      = msg.Define(http.StatusUnprocessableEntity, "offsite.no_room", "Not enough disk space to bring the backup back: it needs {need} and {free} is free, and Zelie keeps 1 GB free for the apps.")
)

var (
	validPrefix   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}(/[A-Za-z0-9][A-Za-z0-9._-]{0,62}){0,7}$`)
	validBucket   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	validRegionID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

func checkOffsite(c OffsiteConfig) error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" ||
		u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return errOffsiteEndpoint.Err()
	}
	if !validBucket.MatchString(c.Bucket) {
		return errOffsiteBucket.Err()
	}
	if c.Region != "" && !validRegionID.MatchString(c.Region) {
		return errOffsiteRegion.Err()
	}
	if !validPrefix.MatchString(c.Prefix) {
		return errOffsitePrefix.Err()
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return errOffsiteKeys.Err()
	}
	return nil
}

// offsiteError turns what the storage said into a message for the user.
func offsiteError(err error, c OffsiteConfig) error {
	var se *s3.Error
	if errors.As(err, &se) {
		switch se.Code {
		case "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return errOffsiteWrongKeys.Err()
		case "NoSuchBucket":
			return errOffsiteNoBucket.Err("bucket", c.Bucket, "endpoint", c.Endpoint)
		case "NoSuchKey":
			return errOffsiteGone.Err()
		case "AccessDenied":
			return errOffsiteDenied.Err("bucket", c.Bucket)
		}
		return errOffsiteAnswer.Err("detail", se.Error())
	}
	var ue *url.Error
	if errors.As(err, &ue) && !errors.Is(err, context.Canceled) {
		return errOffsiteUnreachable.Err("endpoint", c.Endpoint, "detail", ue.Err.Error())
	}
	return err
}

func (s *Server) offsiteFailed(w http.ResponseWriter, op, app string, err error, c OffsiteConfig) {
	s.backupFailed(w, op, app, offsiteError(err, c))
}

func objectKey(c OffsiteConfig, app, name string) string {
	return c.Prefix + "/" + app + "/" + name
}

// keyNote is left next to the backups, so whoever finds them later knows
// which recovery file opens them.
func keyNote(public string) string {
	return "# The Zelie backups in this folder are encrypted for the key below.\n" +
		"# The recovery file of the server that made them opens them.\n" + public + "\n"
}

func (s *Server) getOffsite(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.Offsite.current()
	if !ok {
		writeJSON(w, http.StatusOK, OffsiteInfo{})
		return
	}
	writeJSON(w, http.StatusOK, c.info())
}

// setOffsite saves a destination once it has shown that it takes a file,
// gives it back unchanged and deletes it.
func (s *Server) setOffsite(w http.ResponseWriter, r *http.Request) {
	if s.Offsite == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this core keeps no off-site settings"))
		return
	}
	var req OffsiteConfig
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.Endpoint = strings.TrimSuffix(strings.TrimSpace(req.Endpoint), "/")
	req.Region = strings.TrimSpace(req.Region)
	req.Bucket = strings.TrimSpace(req.Bucket)
	req.Prefix = strings.Trim(strings.TrimSpace(req.Prefix), "/")
	req.AccessKey = strings.TrimSpace(req.AccessKey)
	req.SecretKey = strings.TrimSpace(req.SecretKey)
	// The saved secret goes only where it went before.
	if _, old, ok := s.Offsite.current(); ok && req.SecretKey == "" && req.Endpoint == old.Endpoint && req.AccessKey == old.AccessKey {
		req.SecretKey = old.SecretKey
	}
	if err := checkOffsite(req); err != nil {
		s.backupFailed(w, "set off-site storage", "", err)
		return
	}
	client, err := s3.New(req.s3())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.tryOffsite(ctx, client, req); err != nil {
		s.offsiteFailed(w, "check off-site storage", "", err, req)
		return
	}
	if err := s.Offsite.save(req, client); err != nil {
		s.fail(w, "save off-site storage", "", err)
		return
	}
	s.Log.Info("off-site storage set", "endpoint", req.Endpoint, "bucket", req.Bucket, "prefix", req.Prefix)
	writeJSON(w, http.StatusOK, req.info())
}

func (s *Server) tryOffsite(ctx context.Context, c *s3.Client, cfg OffsiteConfig) error {
	var tag [6]byte
	rand.Read(tag[:])
	probe := cfg.Prefix + "/zelie-check-" + hex.EncodeToString(tag[:])
	data := []byte("Zelie checks that it can write, read and delete here. This file is removed right away.\n")
	if err := c.Put(ctx, probe, bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	rc, _, err := c.Get(ctx, probe)
	if err == nil {
		var got []byte
		got, err = io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err == nil && !bytes.Equal(got, data) {
			err = errOffsiteMismatch.Err()
		}
	}
	if derr := c.Delete(ctx, probe); err == nil {
		err = derr
	}
	if err != nil {
		return err
	}
	note := []byte(keyNote(s.Backups.Key.Public()))
	return c.Put(ctx, cfg.Prefix+"/zelie-key.txt", bytes.NewReader(note), int64(len(note)))
}

// removeOffsite forgets the destination. What is in the bucket stays: it
// is the user's.
func (s *Server) removeOffsite(w http.ResponseWriter, r *http.Request) {
	if s.Offsite == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.Offsite.clear(); err != nil {
		s.fail(w, "remove off-site storage", "", err)
		return
	}
	s.Log.Info("off-site storage removed")
	w.WriteHeader(http.StatusNoContent)
}

// uploadBackup copies a backup file to the bucket as it is: encrypted.
func (s *Server) uploadBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	client, cfg, ok := s.Offsite.current()
	if !ok {
		s.backupFailed(w, "upload backup", app, errOffsiteNotSet.Err())
		return
	}
	f, err := s.Backups.OpenRaw(app, name)
	if err != nil {
		s.backupFailed(w, "upload backup", app, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.backupFailed(w, "upload backup", app, err)
		return
	}
	start := time.Now()
	if err := client.Put(r.Context(), objectKey(cfg, app, name), f, st.Size()); err != nil {
		s.offsiteFailed(w, "upload backup", app, err, cfg)
		return
	}
	s.Log.Info("backup uploaded", "app", app, "backup", name, "bytes", st.Size(), "took", time.Since(start).Round(time.Millisecond))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeOffsiteBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	if !backup.ValidApp(app) || !backup.ValidName(name) {
		writeError(w, http.StatusBadRequest, backup.ErrInvalid)
		return
	}
	client, cfg, ok := s.Offsite.current()
	if !ok {
		s.backupFailed(w, "remove off-site backup", app, errOffsiteNotSet.Err())
		return
	}
	if err := client.Delete(r.Context(), objectKey(cfg, app, name)); err != nil {
		s.offsiteFailed(w, "remove off-site backup", app, err, cfg)
		return
	}
	s.Log.Info("off-site backup removed", "app", app, "backup", name)
	w.WriteHeader(http.StatusNoContent)
}

// listOffsite lists the backup files in the destination's folder, of
// every app, newest first within each. Anything else there is left out.
func (s *Server) listOffsite(w http.ResponseWriter, r *http.Request) {
	client, cfg, ok := s.Offsite.current()
	if !ok {
		s.backupFailed(w, "list off-site backups", "", errOffsiteNotSet.Err())
		return
	}
	objects, err := client.List(r.Context(), cfg.Prefix+"/")
	if err != nil {
		s.offsiteFailed(w, "list off-site backups", "", err, cfg)
		return
	}
	out := []OffsiteBackup{}
	for _, o := range objects {
		app, name, ok := strings.Cut(strings.TrimPrefix(o.Key, cfg.Prefix+"/"), "/")
		if !ok || !backup.ValidApp(app) || !backup.ValidName(name) {
			continue
		}
		created, _ := time.Parse("20060102T150405Z", name[:16])
		out = append(out, OffsiteBackup{App: app, Name: name, Bytes: o.Size, Created: created})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Name > out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

// fetchBackup brings a backup back from the bucket into this server's
// backups, where it can be restored or downloaded like any other.
func (s *Server) fetchBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	if !backup.ValidApp(app) || !backup.ValidName(name) {
		writeError(w, http.StatusBadRequest, backup.ErrInvalid)
		return
	}
	client, cfg, ok := s.Offsite.current()
	if !ok {
		s.backupFailed(w, "fetch backup", app, errOffsiteNotSet.Err())
		return
	}
	start := time.Now()
	body, size, err := client.Get(r.Context(), objectKey(cfg, app, name))
	if err != nil {
		s.offsiteFailed(w, "fetch backup", app, err, cfg)
		return
	}
	defer body.Close()
	if size > 0 {
		if err := s.roomFor(size, errOffsiteNoRoom); err != nil {
			s.backupFailed(w, "fetch backup", app, err)
			return
		}
	}
	if err := s.Backups.Import(app, name, body); err != nil {
		s.offsiteFailed(w, "fetch backup", app, err, cfg)
		return
	}
	s.Log.Info("backup fetched", "app", app, "backup", name, "bytes", size, "took", time.Since(start).Round(time.Millisecond))
	w.WriteHeader(http.StatusNoContent)
}

// Offsite returns the destination backups are copied to.
func (c *Client) Offsite(ctx context.Context) (OffsiteInfo, error) {
	var out OffsiteInfo
	err := c.do(ctx, http.MethodGet, "/v1/offsite", nil, &out)
	return out, err
}

// SetOffsite checks a destination and saves it.
func (c *Client) SetOffsite(ctx context.Context, cfg OffsiteConfig) (OffsiteInfo, error) {
	var out OffsiteInfo
	err := c.do(ctx, http.MethodPut, "/v1/offsite", cfg, &out)
	return out, err
}

// RemoveOffsite forgets the destination; the bucket is left as it is.
func (c *Client) RemoveOffsite(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/v1/offsite", nil, nil)
}

// UploadBackup copies a backup to the destination.
func (c *Client) UploadBackup(ctx context.Context, app, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name)+"/offsite", nil, nil)
}

// OffsiteBackups lists the backups in the destination.
func (c *Client) OffsiteBackups(ctx context.Context) ([]OffsiteBackup, error) {
	var out []OffsiteBackup
	err := c.do(ctx, http.MethodGet, "/v1/offsite/backups", nil, &out)
	return out, err
}

// RemoveOffsiteBackup deletes a backup's copy in the destination.
func (c *Client) RemoveOffsiteBackup(ctx context.Context, app, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/offsite/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name), nil, nil)
}

// FetchBackup brings a backup back from the destination onto this server.
func (c *Client) FetchBackup(ctx context.Context, app, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/offsite/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name)+"/fetch", nil, nil)
}
