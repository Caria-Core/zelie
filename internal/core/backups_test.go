package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
)

const pgDump = "CREATE TABLE t (x int);\n-- PostgreSQL database dump complete\n"

func backupServer(t *testing.T) (*Server, *fakeEngine, *[]string) {
	t.Helper()
	s, f := newServer()
	dir := t.TempDir()
	key, err := backup.LoadOrCreateKey(filepath.Join(dir, "backup.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Backups = &backup.Dir{Root: filepath.Join(dir, "backups"), Key: key}
	f.containers = []engine.Status{
		{ID: "db-1", App: "db", State: "running"},
		{ID: "web-1", App: "web", State: "running"},
		{ID: "old-1", App: "old", State: "stopped"},
	}
	var ran []string
	f.exec = func(args []string, stdin io.Reader, stdout io.Writer) uint32 {
		cmd := strings.Join(args, " ")
		ran = append(ran, cmd)
		switch {
		case stdin != nil:
			b, _ := io.ReadAll(stdin)
			sum := sha256.Sum256(b)
			fmt.Fprintf(stdout, "%d\n%s  /tmp/zelie-restore\n", len(b), hex.EncodeToString(sum[:]))
		case strings.HasPrefix(cmd, "pg_dump"):
			io.WriteString(stdout, pgDump)
		}
		return 0
	}
	return s, f, &ran
}

func TestBackupAndRestore(t *testing.T) {
	s, _, ran := backupServer(t)
	root := &peer.Peer{UID: 0}

	rec := request(t, s, root, "POST", "/v1/backups", `{"app":"db","container":"db-1","kind":"postgres"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("backup: %d %s", rec.Code, rec.Body)
	}
	var info backup.Info
	json.Unmarshal(rec.Body.Bytes(), &info)
	if !strings.HasSuffix(info.Name, ".sql.zst.age") || info.Bytes == 0 {
		t.Fatalf("info %+v", info)
	}

	rec = request(t, s, root, "GET", "/v1/backups/db", "")
	var list []backup.Info
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != info.Name {
		t.Fatalf("list %s", rec.Body)
	}

	// The download is the encrypted file, not the dump.
	rec = request(t, s, root, "GET", "/v1/backups/db/"+info.Name, "")
	if rec.Code != http.StatusOK || int64(rec.Body.Len()) != info.Bytes || bytes.Contains(rec.Body.Bytes(), []byte("CREATE TABLE")) {
		t.Fatalf("download: %d, %d bytes", rec.Code, rec.Body.Len())
	}

	*ran = nil
	rec = request(t, s, root, "POST", "/v1/backups/db/"+info.Name+"/restore", `{"kind":"postgres","container":"db-1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if len(*ran) != 2 || !strings.Contains((*ran)[1], "DROP DATABASE") {
		t.Errorf("ran %q", *ran)
	}

	rec = request(t, s, root, "DELETE", "/v1/backups/db/"+info.Name, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", rec.Code)
	}
}

func TestBackupRefusals(t *testing.T) {
	s, f, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"another app's container", "POST", "/v1/backups", `{"app":"db","container":"web-1","kind":"postgres"}`, http.StatusBadRequest},
		{"stopped container", "POST", "/v1/backups", `{"app":"old","container":"old-1","kind":"postgres"}`, http.StatusConflict},
		{"missing container", "POST", "/v1/backups", `{"app":"db","container":"db-9","kind":"postgres"}`, http.StatusNotFound},
		// The panel names a kind; a command is not part of the request.
		{"unknown kind", "POST", "/v1/backups", `{"app":"db","container":"db-1","kind":"shell"}`, http.StatusBadRequest},
		{"command", "POST", "/v1/backups", `{"app":"db","container":"db-1","kind":"postgres","args":["sh"]}`, http.StatusBadRequest},
		{"bad app", "GET", "/v1/backups/..%2Fetc", "", http.StatusBadRequest},
		{"path in name", "GET", "/v1/backups/db/..%2F..%2Fbackup.key", "", http.StatusBadRequest},
		{"wrong kind for file", "POST", "/v1/backups/db/20260101T000000Z-abcd.rdb.zst.age/restore", `{"kind":"postgres","container":"db-1"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := request(t, s, root, c.method, c.path, c.body); rec.Code != c.want {
				t.Errorf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}

	// A dump that stops early is not kept, and the database's words reach
	// the panel.
	f.exec = func([]string, io.Reader, io.Writer) uint32 { return 1 }
	rec := request(t, s, root, "POST", "/v1/backups", `{"app":"db","container":"db-1","kind":"postgres"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dump failed") {
		t.Errorf("failed dump: %d %s", rec.Code, rec.Body)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Backups.Root, "db"))
	if len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestRestoreRedisVolume(t *testing.T) {
	s, f, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	f.volumeDir = t.TempDir()
	f.exec = func(_ []string, _ io.Reader, stdout io.Writer) uint32 {
		io.WriteString(stdout, "REDIS0012 data")
		return 0
	}
	f.containers = append(f.containers, engine.Status{ID: "cache-1", App: "cache", State: "running"})
	rec := request(t, s, root, "POST", "/v1/backups", `{"app":"cache","container":"cache-1","kind":"redis"}`)
	var info backup.Info
	json.Unmarshal(rec.Body.Bytes(), &info)
	rec = request(t, s, root, "POST", "/v1/backups/cache/"+info.Name+"/restore", `{"kind":"redis","volume":"data"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	got, _ := os.ReadFile(filepath.Join(f.volumeDir, "appendonlydir", "appendonly.aof.1.base.rdb"))
	if string(got) != "REDIS0012 data" {
		t.Errorf("volume has %q", got)
	}
}

func TestRecoveryKey(t *testing.T) {
	s, _, _ := backupServer(t)
	rec := request(t, s, &peer.Peer{UID: 999}, "GET", "/v1/backups-key?host=example.com", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AGE-SECRET-KEY-1") || !strings.Contains(rec.Body.String(), "example.com") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
