package build

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// suggestTest looks for a Node project's test script and returns the
// command that runs it with the project's package manager. The source is
// untrusted: files are opened without following symlinks and read with a
// limit.
func suggestTest(src string) string {
	f, err := os.OpenFile(filepath.Join(src, "package.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&pkg); err != nil {
		return ""
	}
	test := strings.TrimSpace(pkg.Scripts["test"])
	// npm init writes a test script that only fails.
	if test == "" || strings.Contains(test, "no test specified") {
		return ""
	}
	for _, m := range []struct{ lock, cmd string }{
		{"pnpm-lock.yaml", "pnpm test"},
		{"yarn.lock", "yarn test"},
		{"bun.lock", "bun run test"},
		{"bun.lockb", "bun run test"},
	} {
		if _, err := os.Lstat(filepath.Join(src, m.lock)); err == nil {
			return m.cmd
		}
	}
	return "npm test"
}

// planCommands reads the build and start commands Railpack chose from its
// plan. The plan comes out of a container that read untrusted code, so it
// is read with the same care as package.json.
func planCommands(dir string) (build, start string) {
	f, err := os.OpenFile(filepath.Join(dir, "railpack-plan.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	var plan struct {
		Steps []struct {
			Name     string `json:"name"`
			Commands []struct {
				Cmd string `json:"cmd"`
			} `json:"commands"`
		} `json:"steps"`
		Deploy struct {
			StartCommand string `json:"startCommand"`
		} `json:"deploy"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&plan); err != nil {
		return "", ""
	}
	var cmds []string
	for _, s := range plan.Steps {
		if s.Name != "build" {
			continue
		}
		for _, c := range s.Commands {
			if c.Cmd != "" {
				cmds = append(cmds, c.Cmd)
			}
		}
	}
	return clip(strings.Join(cmds, " && ")), clip(plan.Deploy.StartCommand)
}

// clip keeps only a command that fits on one line and in the database.
func clip(s string) string {
	if len(s) > 500 || strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return ""
	}
	return s
}
