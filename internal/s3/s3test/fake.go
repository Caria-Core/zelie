// Package s3test has a fake S3 bucket for tests.
package s3test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Fake is a bucket named "bucket" in memory, speaking just enough of S3
// for tests. Fields are safe to read once the requests are done.
type Fake struct {
	mu      sync.Mutex
	Objects map[string][]byte
	uploads map[string]map[int][]byte
	// Fail answers the next requests of a kind, such as "PUT" or
	// "PUT uploadId partNumber", with these statuses, one each.
	Fail map[string][]int
	// Aborted counts aborted multipart uploads.
	Aborted int
	// JoinErr makes joining the parts fail, reported in a 200.
	JoinErr bool
	// Calls lists the kinds of every request, in order.
	Calls []string
	URL   string
}

// NewFake starts a fake; it stops when the test ends.
func NewFake(t testing.TB) *Fake {
	f := &Fake{Objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, Fail: map[string][]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// ServeHTTP answers one request.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	what := r.Method
	for _, k := range []string{"uploads", "uploadId", "partNumber", "list-type"} {
		if q.Has(k) {
			what += " " + k
		}
	}
	f.Calls = append(f.Calls, what)
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	if s := f.Fail[what]; len(s) > 0 {
		f.Fail[what] = s[1:]
		w.WriteHeader(s[0])
		fmt.Fprintf(w, "<Error><Code>Injected</Code><Message>failed on purpose</Message></Error>")
		return
	}
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "<Error><Code>XAmzContentSHA256Mismatch</Code></Error>")
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/bucket/")
	switch {
	case what == "GET list-type" && r.URL.Path == "/bucket":
		f.list(w, q)
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<Error><Code>NoSuchBucket</Code></Error>")
	case what == "PUT":
		f.Objects[key] = body
	case what == "GET":
		b, ok := f.Objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	case what == "DELETE":
		delete(f.Objects, key)
		w.WriteHeader(http.StatusNoContent)
	case what == "POST uploads":
		id := strconv.Itoa(len(f.uploads) + 1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case what == "PUT uploadId partNumber":
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case what == "POST uploadId":
		var done struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		xml.Unmarshal(body, &done)
		if f.JoinErr {
			fmt.Fprint(w, "<Error><Code>InternalError</Code><Message>could not join</Message></Error>")
			return
		}
		var all []byte
		for i, p := range done.Parts {
			if p.Number != i+1 || p.ETag != fmt.Sprintf(`"etag-%d"`, p.Number) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, "<Error><Code>InvalidPart</Code></Error>")
				return
			}
			all = append(all, f.uploads[q.Get("uploadId")][p.Number]...)
		}
		f.Objects[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult></CompleteMultipartUploadResult>")
	case what == "DELETE uploadId":
		f.Aborted++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// list answers two keys a page, to make the client follow the pages.
func (f *Fake) list(w http.ResponseWriter, q url.Values) {
	var keys []string
	for k := range f.Objects {
		if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("continuation-token") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	more := len(keys) > 2
	if more {
		keys = keys[:2]
	}
	fmt.Fprint(w, "<ListBucketResult>")
	for _, k := range keys {
		fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-09-28T10:00:00.000Z</LastModified></Contents>", k, len(f.Objects[k]))
	}
	if more {
		fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", keys[1])
	}
	fmt.Fprint(w, "</ListBucketResult>")
}
