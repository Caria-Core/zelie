package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{args: []string{"version"}, wantCode: 0, wantStdout: "zelie "},
		{args: []string{"help"}, wantCode: 0, wantStdout: "Usage: zelie"},
		{args: nil, wantCode: 2, wantStderr: "Usage: zelie"},
		// What the SFTP unit's ExecCondition runs; an old binary answers
		// "unknown command" with 2 instead.
		{args: []string{"sftp", "--check"}, wantCode: 0},
		{args: []string{"sftp", "extra"}, wantCode: 2, wantStderr: "usage: zelie sftp"},
		{args: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
