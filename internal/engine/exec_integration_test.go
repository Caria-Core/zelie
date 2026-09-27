//go:build integration

package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExec(t *testing.T) {
	e := connect(t)
	run(t, e, Spec{ID: "it-exec", Image: testImage, Env: []string{"SECRET=from-the-container"},
		Args: []string{"sleep", "600"}, MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32})
	ctx := context.Background()

	var out, errOut bytes.Buffer
	code, err := e.Exec(ctx, "it-exec", []string{"sh", "-c", `echo "$SECRET $(id -u)"; echo oops >&2; exit 3`}, nil, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || out.String() != "from-the-container 0\n" || errOut.String() != "oops\n" {
		t.Errorf("got code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}

	// Large output arrives whole.
	out.Reset()
	if _, err := e.Exec(ctx, "it-exec", []string{"sh", "-c", "head -c 20000000 /dev/zero"}, nil, &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 20000000 {
		t.Errorf("got %d bytes of output, want 20000000", out.Len())
	}

	// A short input ends before the process starts. It must still end.
	out.Reset()
	ctx5, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := e.Exec(ctx5, "it-exec", []string{"cat"}, strings.NewReader("short"), &out, nil); err != nil || out.String() != "short" {
		t.Fatalf("short input: %q, %v", out.String(), err)
	}

	// Input: how much of it arrives is what a restore depends on.
	in := make([]byte, 30<<20+12345)
	rand.Read(in)
	sum := sha256.Sum256(in)
	for i := range 3 {
		out.Reset()
		code, err := e.Exec(ctx, "it-exec", []string{"sh", "-c", "cat > /tmp/in && wc -c < /tmp/in && sha256sum /tmp/in"}, bytes.NewReader(in), &out, nil)
		if err != nil || code != 0 {
			t.Fatalf("run %d: code %d, %v", i, code, err)
		}
		f := strings.Fields(out.String())
		if len(f) < 2 || f[0] != strconv.Itoa(len(in)) || f[1] != hex.EncodeToString(sum[:]) {
			t.Errorf("run %d: the container got %q, want %d bytes with sum %x", i, out.String(), len(in), sum)
		}
	}
}
