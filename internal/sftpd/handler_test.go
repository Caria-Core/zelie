package sftpd

import (
	"context"
	"io"
	"testing"

	"github.com/Caria-Core/zelie/internal/core"
)

// drain is a core that reads uploads to the end and keeps nothing.
type drain struct{ Files }

func (drain) UploadFile(_ context.Context, _ core.FileRef, _ string, _ int64, body io.Reader) error {
	_, err := io.Copy(io.Discard, body)
	return err
}

func TestPendingBytesAreCappedPerConnection(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	ctx := context.Background()
	a := newUpload(ctx, drain{}, core.FileRef{}, "/a", b)
	c := newUpload(ctx, drain{}, core.FileRef{}, "/c", b)
	if _, err := a.WriteAt(make([]byte, 60), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteAt(make([]byte, 60), 1000); err == nil {
		t.Fatal("pending data over the connection's cap was kept")
	}
	// Failing the second file gives nothing back that it did not hold, and
	// closing the first returns its share.
	a.Close()
	d := newUpload(ctx, drain{}, core.FileRef{}, "/d", b)
	if _, err := d.WriteAt(make([]byte, 60), 1000); err != nil {
		t.Errorf("room was not given back: %v", err)
	}
	d.Close()
	if b.bytes != 0 {
		t.Errorf("%d bytes still counted", b.bytes)
	}
}

func TestPendingBytesAreGivenBackWhenTheGapFills(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	u := newUpload(context.Background(), drain{}, core.FileRef{}, "/a", b)
	if _, err := u.WriteAt(make([]byte, 10), 10); err != nil {
		t.Fatal(err)
	}
	if b.bytes != 10 {
		t.Fatalf("held %d, want 10", b.bytes)
	}
	if _, err := u.WriteAt(make([]byte, 10), 0); err != nil {
		t.Fatal(err)
	}
	if b.bytes != 0 {
		t.Errorf("held %d after the gap filled", b.bytes)
	}
	if err := u.Close(); err != nil {
		t.Error(err)
	}
}

func TestEmptyOutOfOrderWriteIsNotStored(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	u := newUpload(context.Background(), drain{}, core.FileRef{}, "/a", b)
	for range 1000 {
		if _, err := u.WriteAt(nil, 50); err != nil {
			t.Fatal(err)
		}
	}
	if len(u.pending) != 0 || b.bytes != 0 {
		t.Errorf("%d empty blocks stored", len(u.pending))
	}
	u.Close()
}

func TestWriteHandlesAreCapped(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 2}
	if !b.openFile() || !b.openFile() {
		t.Fatal("refused under the cap")
	}
	if b.openFile() {
		t.Error("a third write handle was given")
	}
	b.closeFile()
	if !b.openFile() {
		t.Error("a closed handle was not given back")
	}
}
