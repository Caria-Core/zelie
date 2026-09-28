package msg_test

import (
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/msg"

	// Every package that defines messages, so they are all known here.
	_ "github.com/Caria-Core/zelie/internal/auth"
	_ "github.com/Caria-Core/zelie/internal/backup"
	_ "github.com/Caria-Core/zelie/internal/core"
	_ "github.com/Caria-Core/zelie/internal/panel"
	_ "github.com/Caria-Core/zelie/internal/store"
)

var update = flag.Bool("update", false, "rewrite the web interface's English messages")

const localeFile = "../../web/src/lib/locales/messages.en.ts"

// TestLocale keeps the web interface's English messages the same as the
// ones defined in Go. Run it with -update after adding or changing one.
func TestLocale(t *testing.T) {
	var b strings.Builder
	b.WriteString("// Generated from the messages defined in the Go code (internal/msg).\n")
	b.WriteString("// Do not edit: change the Go code, then run\n")
	b.WriteString("//   go test ./internal/msg -run TestLocale -update\n")
	b.WriteString("export default {\n")
	for _, tm := range msg.All() {
		b.WriteString("\t'msg." + tm.Code + "': " + quote(tm.English) + ",\n")
	}
	b.WriteString("} as const;\n")
	want := b.String()
	if *update {
		if err := os.WriteFile(localeFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(localeFile)
	if err != nil || string(got) != want {
		t.Errorf("%s is not up to date; run go test ./internal/msg -run TestLocale -update", localeFile)
	}
}

func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`).Replace(s) + "'"
}

// TestMessagesRead checks what every message says in English: whole
// sentences, and placeholders that are plain names.
func TestMessagesRead(t *testing.T) {
	stray := regexp.MustCompile(`[{}]`)
	for _, tm := range msg.All() {
		if tm.Code == msg.Other.Code {
			continue
		}
		if first := tm.English[0]; first >= 'a' && first <= 'z' && !strings.HasPrefix(tm.English, "{") {
			t.Errorf("%s does not start with a capital: %q", tm.Code, tm.English)
		}
		if !strings.HasSuffix(tm.English, ".") && !strings.HasSuffix(tm.English, "}") {
			t.Errorf("%s does not end a sentence: %q", tm.Code, tm.English)
		}
		rest := tm.English
		for _, n := range tm.Names() {
			rest = strings.ReplaceAll(rest, "{"+n+"}", "")
		}
		if stray.MatchString(rest) {
			t.Errorf("%s has a placeholder that is not a plain name: %q", tm.Code, tm.English)
		}
	}
}
