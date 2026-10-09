package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSFTPVolumesAreSavedAndLoadedAgain(t *testing.T) {
	list := filepath.Join(t.TempDir(), "state", "sftp-volumes.json")
	v, err := LoadSFTPVolumes(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set([]string{"vol-b", "vol-a", "vol-a"}); err != nil {
		t.Fatal(err)
	}
	again, err := LoadSFTPVolumes(list)
	if err != nil || !again.Has("vol-a") || !again.Has("vol-b") || again.Has("vol-c") {
		t.Errorf("loaded %v, %v", again, err)
	}
	// Written whole under a temporary name, so nothing is left beside it.
	entries, _ := os.ReadDir(filepath.Dir(list))
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the folder holds %v", names)
	}
	if st, _ := os.Stat(list); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}

// A file that came back empty or cut short after a crash must not keep the
// core from starting: the panel sends the list again.
func TestDamagedSFTPVolumesListStartsEmpty(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "cut short": `["vol-a","vo`, "wrong kind": `{"vol-a":true}`} {
		list := filepath.Join(t.TempDir(), "sftp-volumes.json")
		os.WriteFile(list, []byte(content), 0o600)
		v, err := LoadSFTPVolumes(list)
		if v == nil {
			t.Errorf("%s: no list to start with: %v", name, err)
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "damaged") {
			t.Errorf("%s: the damage was not reported: %v", name, err)
		}
		if v.Has("vol-a") {
			t.Errorf("%s: a volume came out of a damaged file", name)
		}
		if err := v.Set([]string{"vol-a"}); err != nil {
			t.Errorf("%s: the list could not be set again: %v", name, err)
		}
		if again, err := LoadSFTPVolumes(list); err != nil || !again.Has("vol-a") {
			t.Errorf("%s: after setting it: %v", name, err)
		}
	}
}
