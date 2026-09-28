package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/s3/s3test"
)

// The examples from the S3 documentation, "Signature Calculations for the
// Authorization Header".
func TestSignExamples(t *testing.T) {
	c, err := New(Config{
		Endpoint: "https://s3.amazonaws.com", Bucket: "examplebucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name, method, key string
		query             url.Values
		headers           map[string]string
		hash, signed, sig string
	}{
		{
			name: "get object", method: "GET", key: "test.txt",
			headers: map[string]string{"Range": "bytes=0-9"}, hash: emptyHash,
			signed: "host;range;x-amz-content-sha256;x-amz-date",
			sig:    "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "put object", method: "PUT", key: "test$file.text",
			headers: map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			hash:    "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			signed:  "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			sig:     "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name: "get lifecycle", method: "GET", query: url.Values{"lifecycle": {""}}, hash: emptyHash,
			signed: "host;x-amz-content-sha256;x-amz-date",
			sig:    "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "list objects", method: "GET", query: url.Values{"max-keys": {"2"}, "prefix": {"J"}}, hash: emptyHash,
			signed: "host;x-amz-content-sha256;x-amz-date",
			sig:    "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := c.request(context.Background(), tc.method, tc.key, tc.query, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			c.sign(req, tc.hash)
			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" +
				tc.signed + ", Signature=" + tc.sig
			if got := req.Header.Get("Authorization"); got != want {
				t.Errorf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	ok := Config{Endpoint: "https://s3.example.com", Bucket: "backups", AccessKey: "a", SecretKey: "s"}
	for name, change := range map[string]func(*Config){
		"no scheme":     func(c *Config) { c.Endpoint = "s3.example.com" },
		"ftp":           func(c *Config) { c.Endpoint = "ftp://s3.example.com" },
		"path":          func(c *Config) { c.Endpoint = "https://s3.example.com/x" },
		"user":          func(c *Config) { c.Endpoint = "https://me@s3.example.com" },
		"query":         func(c *Config) { c.Endpoint = "https://s3.example.com?x=1" },
		"bucket upper":  func(c *Config) { c.Bucket = "Backups" },
		"bucket slash":  func(c *Config) { c.Bucket = "a/b" },
		"bucket short":  func(c *Config) { c.Bucket = "ab" },
		"region spaces": func(c *Config) { c.Region = "eu west" },
		"no secret":     func(c *Config) { c.SecretKey = "" },
	} {
		cfg := ok
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	c, err := New(ok)
	if err != nil {
		t.Fatal(err)
	}
	if !c.pathStyle || c.host != "s3.example.com" || c.cfg.Region != DefaultRegion {
		t.Errorf("other services: path style %v, host %s, region %s", c.pathStyle, c.host, c.cfg.Region)
	}
	aws := ok
	aws.Endpoint = "https://s3.eu-central-1.amazonaws.com"
	if c, _ := New(aws); c.pathStyle || c.host != "backups.s3.eu-central-1.amazonaws.com" {
		t.Errorf("AWS: path style %v, host %s", c.pathStyle, c.host)
	}
	aws.Bucket = "my.backups"
	if c, _ := New(aws); !c.pathStyle {
		t.Error("AWS bucket with dots: not path style")
	}
}

func newFake(t *testing.T) (*s3test.Fake, *Client) {
	f := s3test.NewFake(t)
	c, err := New(Config{Endpoint: f.URL, Bucket: "bucket", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	c.wait = func(int) time.Duration { return 0 }
	return f, c
}

func TestRoundTrip(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	c.partSize = 1000
	small := []byte("hello")
	large := bytes.Repeat([]byte("0123456789"), 350) // four parts, the last short
	if err := c.Put(ctx, "zelie/a/small", bytes.NewReader(small), int64(len(small))); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "zelie/a/large", bytes.NewReader(large), int64(len(large))); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "zelie/b/one", bytes.NewReader(small), int64(len(small))); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "other/x", bytes.NewReader(small), int64(len(small))); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]byte{"zelie/a/small": small, "zelie/a/large": large} {
		rc, n, err := c.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, want) || n != int64(len(want)) {
			t.Errorf("%s: got %d bytes (said %d), want %d", key, len(got), n, len(want))
		}
	}
	parts := 0
	for _, call := range f.Calls {
		if call == "PUT uploadId partNumber" {
			parts++
		}
	}
	if parts != 4 {
		t.Errorf("large file went up in %d parts, want 4", parts)
	}

	list, err := c.List(ctx, "zelie/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range list {
		keys = append(keys, fmt.Sprintf("%s:%d", o.Key, o.Size))
	}
	if got := strings.Join(keys, " "); got != "zelie/a/large:3500 zelie/a/small:5 zelie/b/one:5" {
		t.Errorf("list: %s", got)
	}
	if !list[0].Modified.Equal(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("modified: %v", list[0].Modified)
	}

	if err := c.Delete(ctx, "zelie/a/small"); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Get(ctx, "zelie/a/small")
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Code != "NoSuchKey" {
		t.Errorf("get after delete: %v", err)
	}
}

func TestFailures(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 2500)

	t.Run("passing failures are tried again", func(t *testing.T) {
		f, c := newFake(t)
		c.partSize = 1000
		f.Fail["PUT uploadId partNumber"] = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
		if err := c.Put(ctx, "k", bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(f.Objects["k"], data) {
			t.Error("object differs")
		}
	})

	t.Run("refusals are not", func(t *testing.T) {
		f, c := newFake(t)
		f.Fail["PUT"] = []int{http.StatusForbidden}
		err := c.Put(ctx, "k", bytes.NewReader(data), int64(len(data)))
		var se *Error
		if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Code != "Injected" {
			t.Fatalf("got %v", err)
		}
		if len(f.Calls) != 1 {
			t.Errorf("%d calls, want 1", len(f.Calls))
		}
	})

	t.Run("gives up after three", func(t *testing.T) {
		f, c := newFake(t)
		f.Fail["GET"] = []int{500, 500, 500, 500}
		if _, _, err := c.Get(ctx, "k"); err == nil {
			t.Fatal("no error")
		}
		if len(f.Calls) != attempts {
			t.Errorf("%d calls, want %d", len(f.Calls), attempts)
		}
	})

	t.Run("failed part aborts the upload", func(t *testing.T) {
		f, c := newFake(t)
		c.partSize = 1000
		f.Fail["PUT uploadId partNumber"] = []int{0: http.StatusForbidden}
		if err := c.Put(ctx, "k", bytes.NewReader(data), int64(len(data))); err == nil {
			t.Fatal("no error")
		}
		if f.Aborted != 1 {
			t.Errorf("aborted %d times", f.Aborted)
		}
		if _, ok := f.Objects["k"]; ok {
			t.Error("object exists")
		}
	})

	t.Run("failure to join in a 200", func(t *testing.T) {
		f, c := newFake(t)
		c.partSize = 1000
		f.JoinErr = true
		err := c.Put(ctx, "k", bytes.NewReader(data), int64(len(data)))
		var se *Error
		if !errors.As(err, &se) || se.Code != "InternalError" {
			t.Fatalf("got %v", err)
		}
		if f.Aborted != 1 {
			t.Errorf("aborted %d times", f.Aborted)
		}
	})

	t.Run("no redirects", func(t *testing.T) {
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("followed the redirect")
		}))
		defer elsewhere.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
		}))
		defer srv.Close()
		c, _ := New(Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "a", SecretKey: "s"})
		if err := c.Delete(ctx, "k"); err == nil {
			t.Error("no error")
		}
	})
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"/bucket/zelie/a b.txt": "/bucket/zelie/a%20b.txt",
		"/ü~_-.":                "/%C3%BC~_-.",
	} {
		if got := escape(in, false); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escape("a/b=c", true); got != "a%2Fb%3Dc" {
		t.Errorf("query escape: %q", got)
	}
}
