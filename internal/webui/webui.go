// Package webui serves the panel's web interface, which is built from web/
// and embedded into the binary.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
)

//go:embed all:build
var build embed.FS

// Handler serves the interface. Paths that are not files get index.html, and
// the interface's own router takes it from there.
func Handler() http.Handler {
	files, err := fs.Sub(build, "build/ui")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return http.HandlerFunc(notBuilt)
	}
	var bundles [][]byte
	fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(name, ".js") {
			if b, err := fs.ReadFile(files, name); err == nil {
				bundles = append(bundles, b)
			}
		}
		return nil
	})
	csp := policyFor(index, bundles...)
	fileServer := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(files, name); err == nil && !st.IsDir() {
				// Build output under immutable/ has a content hash in its
				// name, so it can be cached for good.
				if strings.HasPrefix(name, "_app/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}

var (
	inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	styleAttr    = regexp.MustCompile(`style="([^"]*)"`)
)

// policyFor builds a Content-Security-Policy that allows the page's own
// files and exactly the inline code the build contains, by hash, and nothing
// else. An injected script tag would not run. Style attributes are collected
// from the bundles too, because Svelte creates elements from HTML templates;
// SvelteKit's screen reader announcer is one.
func policyFor(index []byte, bundles ...[]byte) string {
	var scripts, styles []string
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		scripts = append(scripts, hashSource(m[1]))
	}
	for _, b := range append([][]byte{index}, bundles...) {
		for _, m := range styleAttr.FindAllSubmatch(b, -1) {
			if h := hashSource(m[1]); !slices.Contains(styles, h) {
				styles = append(styles, h)
			}
		}
	}
	styleSrc := "style-src 'self'"
	if len(styles) > 0 {
		styleSrc += " 'unsafe-hashes' " + strings.Join(styles, " ")
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' " + strings.Join(scripts, " "),
		styleSrc,
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"manifest-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

func hashSource(b []byte) string {
	sum := sha256.Sum256(b)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func notBuilt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write(bytes.TrimSpace([]byte(`
This zelie binary was built without its web interface.
Build it with "pnpm --dir web install && pnpm --dir web build", then build zelie again.
`)))
}
