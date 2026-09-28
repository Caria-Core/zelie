package core

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/peer"
)

func TestUploadAndImport(t *testing.T) {
	s, _, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	dump := "USE `old`;\nCREATE TABLE t (x int);\nINSERT INTO t VALUES (1);\n"

	rec := request(t, s, root, "POST", "/v1/uploads", `{"app":"db","size":`+strconv.Itoa(len(dump))+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var u Upload
	json.Unmarshal(rec.Body.Bytes(), &u)

	// Importing before the whole file arrived is refused.
	rec = request(t, s, root, "POST", "/v1/uploads/"+u.ID+"/import", `{"kind":"mariadb"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "upload.incomplete") {
		t.Fatalf("early import: %d %s", rec.Code, rec.Body)
	}

	rec = request(t, s, root, "PUT", "/v1/uploads/"+u.ID+"?offset=0", dump[:20])
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"received":20`) {
		t.Fatalf("first piece: %d %s", rec.Code, rec.Body)
	}
	// The same piece again, as after a lost answer: nothing is written, and
	// the sender learns where to go on from.
	rec = request(t, s, root, "PUT", "/v1/uploads/"+u.ID+"?offset=0", dump[:20])
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"received":20`) {
		t.Fatalf("repeat: %d %s", rec.Code, rec.Body)
	}
	// More than it said it would be.
	rec = request(t, s, root, "PUT", "/v1/uploads/"+u.ID+"?offset=20", dump[20:]+"extra")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "upload.too_long") {
		t.Fatalf("too long: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, s, root, "PUT", "/v1/uploads/"+u.ID+"?offset=20", dump[20:])
	if rec.Code != http.StatusOK {
		t.Fatalf("second piece: %d %s", rec.Code, rec.Body)
	}

	rec = request(t, s, root, "POST", "/v1/uploads/"+u.ID+"/import", `{"kind":"mariadb"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	var info backup.Info
	json.Unmarshal(rec.Body.Bytes(), &info)
	if !strings.HasSuffix(info.Name, ".sql.zst.age") || info.Adapted["USE"] != 1 {
		t.Fatalf("info %+v", info)
	}
	r, err := s.Backups.Open("db", info.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != dump[len("USE `old`;\n"):] {
		t.Errorf("backup holds %q", got)
	}
	// The upload is gone.
	if rec = request(t, s, root, "GET", "/v1/uploads/"+u.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("upload after import: %d", rec.Code)
	}
}

// A file that is not what the database takes is refused with a reason, and
// thrown away: sending it again would not help.
func TestImportRefused(t *testing.T) {
	s, _, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	rec := request(t, s, root, "POST", "/v1/uploads", `{"app":"db","size":5}`)
	var u Upload
	json.Unmarshal(rec.Body.Bytes(), &u)
	request(t, s, root, "PUT", "/v1/uploads/"+u.ID+"?offset=0", "PGDMP")
	rec = request(t, s, root, "POST", "/v1/uploads/"+u.ID+"/import", `{"kind":"postgres"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "import.pg_custom") {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if rec = request(t, s, root, "GET", "/v1/uploads/"+u.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("upload after refusal: %d", rec.Code)
	}
	if list, _ := s.Backups.List("db"); len(list) != 0 {
		t.Errorf("backups %v", list)
	}
}

func TestUploadChecks(t *testing.T) {
	s, _, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	for _, body := range []string{`{"app":"../x","size":5}`, `{"app":"db","size":0}`} {
		if rec := request(t, s, root, "POST", "/v1/uploads", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if rec := request(t, s, root, "POST", "/v1/uploads", `{"app":"db","size":1099511627776000}`); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "upload.no_room") {
		t.Errorf("huge: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/uploads/../../etc", "/v1/uploads/ABC"} {
		if rec := request(t, s, root, "PUT", path+"?offset=0", "x"); rec.Code == http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}

	// One left for a day is deleted when the next one starts.
	rec := request(t, s, root, "POST", "/v1/uploads", `{"app":"db","size":5}`)
	var old Upload
	json.Unmarshal(rec.Body.Bytes(), &old)
	day := time.Now().Add(-25 * time.Hour)
	os.Chtimes(filepath.Join(s.uploadsDir(), old.ID+".json"), day, day)
	request(t, s, root, "POST", "/v1/uploads", `{"app":"db","size":5}`)
	if _, err := os.Stat(filepath.Join(s.uploadsDir(), old.ID)); !os.IsNotExist(err) {
		t.Errorf("old upload: %v", err)
	}
}
