package webui

import (
	"strings"
	"testing"
)

func TestPolicyHashesInlineCode(t *testing.T) {
	index := []byte(`<html><head><script>let a = 1;</script></head><body><div style="display: contents"><script>go()</script></div></body></html>`)
	got := policyFor(index)
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self' 'sha256-", // two hashes follow
		"'unsafe-hashes' 'sha256-",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("policy lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(strings.SplitN(strings.SplitN(got, "script-src", 2)[1], ";", 2)[0], "sha256-"); n != 2 {
		t.Errorf("%d script hashes, want 2:\n%s", n, got)
	}
	withBundle := policyFor(index, []byte("t('<div style=\"clip: rect(0 0 0 0)\"></div>')"))
	if strings.Count(withBundle, "sha256-") != strings.Count(got, "sha256-")+1 {
		t.Errorf("style attribute in a bundle not allowed:\n%s", withBundle)
	}
	if strings.Contains(got, "'unsafe-inline'") || strings.Contains(got, "'unsafe-eval'") {
		t.Errorf("policy allows arbitrary inline code:\n%s", got)
	}
}
