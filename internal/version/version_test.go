package version

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "release build",
			info: Info{Version: "v1.0.0", Commit: "0123456789abcdef", Go: "go1.27.1", Platform: "linux/amd64"},
			want: "zelie v1.0.0 (0123456789ab, go1.27.1, linux/amd64)",
		},
		{
			name: "local build with uncommitted changes",
			info: Info{Version: "dev", Commit: "abc", Modified: true, Go: "go1.27.1", Platform: "linux/arm64"},
			want: "zelie dev (abc-dirty, go1.27.1, linux/arm64)",
		},
		{
			name: "no vcs data",
			info: Info{Version: "dev", Go: "go1.27.1", Platform: "darwin/arm64"},
			want: "zelie dev (unknown, go1.27.1, darwin/arm64)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
