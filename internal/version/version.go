// Package version reports which build of Zelie is running.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is set at release time with -ldflags "-X ...version.Version=v1.2.3".
// Local builds keep the default.
var Version = "dev"

// Info describes the running binary.
type Info struct {
	Version  string
	Commit   string
	Modified bool
	Go       string
	Platform string
}

// Get returns the build information. The commit comes from the VCS data that
// the Go toolchain embeds, so it is correct even for local builds.
func Get() Info {
	info := Info{
		Version:  Version,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	return info
}

func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if commit == "" {
		commit = "unknown"
	}
	if i.Modified {
		commit += "-dirty"
	}
	return fmt.Sprintf("zelie %s (%s, %s, %s)", i.Version, commit, i.Go, i.Platform)
}
