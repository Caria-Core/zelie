package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/s3"
	"github.com/Caria-Core/zelie/internal/s3/s3test"
)

func codeOf(t *testing.T, body *bytes.Buffer) string {
	t.Helper()
	var e errorJSON
	json.Unmarshal(body.Bytes(), &e)
	return e.Code
}

func TestOffsite(t *testing.T) {
	s, _, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	path := filepath.Join(t.TempDir(), "offsite.json")
	o, err := LoadOffsite(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Offsite = o
	fake := s3test.NewFake(t)

	rec := request(t, s, root, "GET", "/v1/offsite", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"set":false}` {
		t.Fatalf("get before set: %d %s", rec.Code, rec.Body)
	}

	rec = request(t, s, root, "POST", "/v1/backups", `{"app":"db","container":"db-1","kind":"postgres"}`)
	var info backup.Info
	json.Unmarshal(rec.Body.Bytes(), &info)
	rec = request(t, s, root, "POST", "/v1/backups/db/"+info.Name+"/offsite", "")
	if rec.Code != http.StatusConflict || codeOf(t, rec.Body) != "offsite.not_set" {
		t.Fatalf("upload before set: %d %s", rec.Code, rec.Body)
	}

	good := map[string]string{"endpoint": fake.URL + "/", "bucket": "bucket", "prefix": "/zelie/test/", "access_key": "a", "secret_key": "s"}
	set := func(change map[string]string) (int, string, string) {
		t.Helper()
		body := map[string]string{}
		for k, v := range good {
			body[k] = v
		}
		for k, v := range change {
			body[k] = v
		}
		b, _ := json.Marshal(body)
		rec := request(t, s, root, "PUT", "/v1/offsite", string(b))
		return rec.Code, codeOf(t, rec.Body), rec.Body.String()
	}
	for _, tc := range []struct {
		change map[string]string
		code   string
	}{
		{map[string]string{"endpoint": "s3.example.com"}, "offsite.bad_endpoint"},
		{map[string]string{"endpoint": fake.URL + "/other"}, "offsite.bad_endpoint"},
		{map[string]string{"bucket": "Bucket"}, "offsite.bad_bucket"},
		{map[string]string{"region": "eu west"}, "offsite.bad_region"},
		{map[string]string{"prefix": "a/../b"}, "offsite.bad_prefix"},
		{map[string]string{"prefix": ""}, "offsite.bad_prefix"},
		{map[string]string{"secret_key": ""}, "offsite.need_keys"},
		{map[string]string{"bucket": "missing"}, "offsite.no_bucket"},
		{map[string]string{"endpoint": "http://127.0.0.1:1"}, "offsite.unreachable"},
	} {
		if status, code, body := set(tc.change); code != tc.code {
			t.Errorf("%v: %d %s, want %s", tc.change, status, body, tc.code)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a refused destination was saved")
	}

	fake.Fail["PUT"] = []int{http.StatusForbidden}
	if _, code, body := set(nil); code != "offsite.failed" || !strings.Contains(body, "Injected") {
		t.Errorf("storage refusing: %s", body)
	}

	status, _, body := set(nil)
	if status != http.StatusOK || strings.Contains(body, `"s"`) || strings.Contains(body, "secret") {
		t.Fatalf("set: %d %s", status, body)
	}
	for k := range fake.Objects {
		if strings.Contains(k, "zelie-check") {
			t.Errorf("check file left behind: %s", k)
		}
	}
	if note := string(fake.Objects["zelie/test/zelie-key.txt"]); !strings.Contains(note, s.Backups.Key.Public()) {
		t.Errorf("key note: %q", note)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", st, err)
	}
	rec = request(t, s, root, "GET", "/v1/offsite", "")
	if !strings.Contains(rec.Body.String(), `"prefix":"zelie/test"`) || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("get: %s", rec.Body)
	}

	// The secret may be left out to change the folder, but not to send it
	// somewhere else.
	if status, _, body := set(map[string]string{"secret_key": "", "prefix": "zelie/test"}); status != http.StatusOK {
		t.Errorf("keeping the secret: %s", body)
	}
	if _, code, _ := set(map[string]string{"secret_key": "", "endpoint": "http://localhost:1"}); code != "offsite.need_keys" {
		t.Errorf("secret sent to a new endpoint: %s", code)
	}

	rec = request(t, s, root, "POST", "/v1/backups/db/"+info.Name+"/offsite", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	local, _ := os.ReadFile(filepath.Join(s.Backups.Root, "db", info.Name))
	if !bytes.Equal(fake.Objects["zelie/test/db/"+info.Name], local) {
		t.Error("the uploaded object differs from the file")
	}

	fake.Objects["zelie/test/db/notes.txt"] = []byte("x")
	fake.Objects["zelie/test/Bad App/"+info.Name] = []byte("x")
	fake.Objects["zelie/other/db/"+info.Name] = []byte("x")
	rec = request(t, s, root, "GET", "/v1/offsite/backups", "")
	var list []OffsiteBackup
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].App != "db" || list[0].Name != info.Name || list[0].Bytes != int64(len(local)) || list[0].Created.IsZero() {
		t.Fatalf("list: %s", rec.Body)
	}

	// Lost here, brought back from the bucket, and restored.
	request(t, s, root, "DELETE", "/v1/backups/db/"+info.Name, "")
	rec = request(t, s, root, "POST", "/v1/offsite/backups/db/"+info.Name+"/fetch", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("fetch: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, s, root, "POST", "/v1/backups/db/"+info.Name+"/restore", `{"kind":"postgres","container":"db-1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("restore after fetch: %d %s", rec.Code, rec.Body)
	}

	rec = request(t, s, root, "DELETE", "/v1/offsite/backups/db/"+info.Name, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", rec.Code)
	}
	rec = request(t, s, root, "POST", "/v1/offsite/backups/db/"+strings.Replace(info.Name, info.Name[:8], "20200101", 1)+"/fetch", "")
	if rec.Code != http.StatusNotFound || codeOf(t, rec.Body) != "offsite.gone" {
		t.Errorf("fetch missing: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/v1/offsite/backups/db/..%2Fx/fetch", "/v1/offsite/backups/Bad/" + info.Name + "/fetch"} {
		if rec := request(t, s, root, "POST", p, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}

	// A restarted core finds the same destination.
	again, err := LoadOffsite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, cfg, ok := again.current(); !ok || cfg.Prefix != "zelie/test" || cfg.SecretKey != "s" {
		t.Errorf("reloaded: %+v", cfg)
	}

	rec = request(t, s, root, "DELETE", "/v1/offsite", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove destination: %d", rec.Code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("file kept")
	}
	if _, ok := fake.Objects["zelie/test/zelie-key.txt"]; !ok {
		t.Error("forgetting the destination touched the bucket")
	}
}

func TestOffsiteErrors(t *testing.T) {
	c := OffsiteConfig{Endpoint: "https://s3.example.com", Bucket: "b"}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{&s3.Error{Status: 403, Code: "InvalidAccessKeyId"}, "offsite.wrong_keys"},
		{&s3.Error{Status: 403, Code: "SignatureDoesNotMatch"}, "offsite.wrong_keys"},
		{&s3.Error{Status: 403, Code: "AccessDenied"}, "offsite.denied"},
		{&s3.Error{Status: 404, Code: "NoSuchBucket"}, "offsite.no_bucket"},
		{&s3.Error{Status: 404, Code: "NoSuchKey"}, "offsite.gone"},
		{&s3.Error{Status: 400, Code: "AuthorizationHeaderMalformed"}, "offsite.failed"},
		{fmt.Errorf("put: %w", &url.Error{Op: "Put", URL: "x", Err: errors.New("no such host")}), "offsite.unreachable"},
	} {
		var m *msg.Error
		if err := offsiteError(tc.err, c); !errors.As(err, &m) || m.Code != tc.code {
			t.Errorf("%v: got %v, want %s", tc.err, err, tc.code)
		}
	}
}
