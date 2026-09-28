//go:build integration

package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
)

// TestService runs against a real S3 service, such as a MinIO started for
// the test: ZELIE_S3_ENDPOINT, ZELIE_S3_ACCESS_KEY and ZELIE_S3_SECRET_KEY.
// It makes the bucket zelie-test.
func TestService(t *testing.T) {
	endpoint := os.Getenv("ZELIE_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ZELIE_S3_ENDPOINT is not set")
	}
	c, err := New(Config{Endpoint: endpoint, Bucket: "zelie-test",
		AccessKey: os.Getenv("ZELIE_S3_ACCESS_KEY"), SecretKey: os.Getenv("ZELIE_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if resp, err := c.send(ctx, http.MethodPut, "", nil, nil, emptyHash); err == nil {
		drain(resp)
	} else if se := (*Error)(nil); !errors.As(err, &se) || se.Code != "BucketAlreadyOwnedByYou" {
		t.Fatal(err)
	}

	// Parts at the smallest size S3 takes, the last one short.
	c.partSize = 5 << 20
	large := make([]byte, 12<<20+123)
	rand.Read(large)
	small := []byte("hello")
	for key, data := range map[string][]byte{"it/large": large, "it/small": small, "it/with space+plus": small} {
		if err := c.Put(ctx, key, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		rc, n, err := c.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data) || n != int64(len(data)) {
			t.Fatalf("%s: read back %d bytes (said %d), want %d", key, len(got), n, len(data))
		}
	}
	list, err := c.List(ctx, "it/")
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, o := range list {
		if err := c.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := c.List(ctx, "it/"); len(list) != 0 {
		t.Errorf("left after delete: %+v", list)
	}

	wrong, _ := New(Config{Endpoint: endpoint, Bucket: "zelie-test", AccessKey: os.Getenv("ZELIE_S3_ACCESS_KEY"), SecretKey: "wrong"})
	var se *Error
	if _, err := wrong.List(ctx, ""); !errors.As(err, &se) || se.Code != "SignatureDoesNotMatch" {
		t.Errorf("wrong secret: %v", err)
	}
	missing, _ := New(Config{Endpoint: endpoint, Bucket: "zelie-missing", AccessKey: os.Getenv("ZELIE_S3_ACCESS_KEY"), SecretKey: os.Getenv("ZELIE_S3_SECRET_KEY")})
	if _, err := missing.List(ctx, ""); !errors.As(err, &se) || se.Code != "NoSuchBucket" {
		t.Errorf("missing bucket: %v", err)
	}
}
