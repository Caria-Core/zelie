// Package s3 is a small client for S3 and the services that speak its
// protocol, such as Cloudflare R2, Backblaze B2, Wasabi and MinIO. It does
// only what off-site backups need: put, get, delete and list objects.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config says where a bucket is and how to sign in to it.
type Config struct {
	// Endpoint is the service's address, such as
	// https://s3.eu-central-1.amazonaws.com.
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

var (
	validBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	validRegion = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// DefaultRegion is used when none is given. Services with no regions of
// their own, R2 among them, accept it.
const DefaultRegion = "us-east-1"

const (
	// S3 takes at most 10,000 parts in one upload.
	maxParts = 10000
	partSize = 64 << 20
	attempts = 3
)

// Client talks to one bucket.
type Client struct {
	cfg       Config
	scheme    string
	host      string
	pathStyle bool
	http      *http.Client
	now       func() time.Time
	partSize  int64
	wait      func(attempt int) time.Duration
}

// New checks cfg and returns a client for its bucket. Nothing is sent yet.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("the endpoint must be an address like https://s3.example.com")
	}
	if !validBucket.MatchString(cfg.Bucket) {
		return nil, errors.New("the bucket name is not valid")
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if !validRegion.MatchString(cfg.Region) {
		return nil, errors.New("the region is not valid")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("both keys are needed")
	}
	host := strings.ToLower(u.Host)
	// AWS wants buckets addressed by host name; most of the others take
	// the bucket in the path. A name with dots would not match the
	// certificate as a host name, so it goes in the path everywhere.
	pathStyle := !strings.HasSuffix(u.Hostname(), ".amazonaws.com") || strings.Contains(cfg.Bucket, ".")
	if !pathStyle {
		host = cfg.Bucket + "." + host
	}
	return &Client{
		cfg: cfg, scheme: u.Scheme, host: host, pathStyle: pathStyle,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 2 * time.Minute,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
			},
			// A redirect would take the signed request somewhere the user
			// did not name.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		partSize: partSize,
	}, nil
}

// Error is a failure the service reported.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	case e.Code != "":
		return e.Code
	}
	return fmt.Sprintf("the service answered %d %s", e.Status, http.StatusText(e.Status))
}

// Object is one entry of a listing.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Put uploads size bytes of r as key. A large file goes up in parts, each
// sent again on its own if it fails.
func (c *Client) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	part := c.partSize
	for size > part*maxParts {
		part *= 2
	}
	if size <= part {
		hash, err := hashOf(r, 0, size)
		if err != nil {
			return err
		}
		resp, err := c.send(ctx, http.MethodPut, key, nil, &section{r, 0, size}, hash)
		if err != nil {
			return err
		}
		return drain(resp)
	}
	return c.putParts(ctx, key, r, size, part)
}

func (c *Client) putParts(ctx context.Context, key string, r io.ReaderAt, size, part int64) (err error) {
	resp, err := c.send(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, emptyHash)
	if err != nil {
		return err
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	if err := decodeXML(resp, &started); err != nil {
		return err
	}
	if started.UploadID == "" {
		return errors.New("the service started no upload")
	}
	upload := url.Values{"uploadId": {started.UploadID}}
	defer func() {
		if err != nil {
			// Unfinished parts are kept, and billed, until the upload
			// is aborted.
			ctx := context.WithoutCancel(ctx)
			if resp, aerr := c.send(ctx, http.MethodDelete, key, upload, nil, emptyHash); aerr == nil {
				drain(resp)
			}
		}
	}()
	type donePart struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var done []donePart
	for off, n := int64(0), 1; off < size; off, n = off+part, n+1 {
		length := min(part, size-off)
		hash, err := hashOf(r, off, length)
		if err != nil {
			return err
		}
		q := url.Values{"uploadId": {started.UploadID}, "partNumber": {strconv.Itoa(n)}}
		resp, err := c.send(ctx, http.MethodPut, key, q, &section{r, off, length}, hash)
		if err != nil {
			return err
		}
		etag := resp.Header.Get("ETag")
		if err := drain(resp); err != nil {
			return err
		}
		if etag == "" {
			return fmt.Errorf("the service gave part %d no ETag", n)
		}
		done = append(done, donePart{n, etag})
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name   `xml:"CompleteMultipartUpload"`
		Parts   []donePart `xml:"Part"`
	}{Parts: done})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	resp, err = c.send(ctx, http.MethodPost, key, upload, &section{bytes.NewReader(body), 0, int64(len(body))}, hex.EncodeToString(sum[:]))
	if err != nil {
		return err
	}
	// The answer comes as soon as the service starts joining the parts,
	// so a failure to join is reported in a 200.
	var result struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := decodeXML(resp, &result); err != nil {
		return err
	}
	if result.XMLName.Local == "Error" {
		return &Error{Status: http.StatusOK, Code: result.Code, Message: result.Message}
	}
	return nil
}

// Get opens key for reading. The caller closes the body.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.send(ctx, http.MethodGet, key, nil, nil, emptyHash)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete removes key. A key that is not there is no error.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.send(ctx, http.MethodDelete, key, nil, nil, emptyHash)
	if err != nil {
		return err
	}
	return drain(resp)
}

// List returns every object whose key starts with prefix.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.send(ctx, http.MethodGet, "", q, nil, emptyHash)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		if err := decodeXML(resp, &page); err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, Modified: o.LastModified})
		}
		if !page.IsTruncated || page.Next == "" || page.Next == token {
			return out, nil
		}
		token = page.Next
	}
}

type section struct {
	r      io.ReaderAt
	off, n int64
}

func hashOf(r io.ReaderAt, off, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(r, off, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// send makes a signed request and returns the answer when it succeeded.
// Failures that may pass, such as a dropped connection or a busy service,
// are tried again; the body is read from its source each time.
func (c *Client) send(ctx context.Context, method, key string, query url.Values, body *section, hash string) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		req, err := c.request(ctx, method, key, query, body)
		if err != nil {
			return nil, err
		}
		c.sign(req, hash)
		resp, err := c.http.Do(req)
		if err == nil && resp.StatusCode < 300 {
			return resp, nil
		}
		again := true
		if err == nil {
			err = readError(resp)
			again = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		}
		if !again || attempt == attempts || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-time.After(c.backoff(attempt)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	if c.wait != nil {
		return c.wait(attempt)
	}
	return time.Duration(attempt*attempt) * time.Second
}

func (c *Client) request(ctx context.Context, method, key string, query url.Values, body *section) (*http.Request, error) {
	p := "/"
	if c.pathStyle {
		p += c.cfg.Bucket
		if key != "" {
			p += "/"
		}
	}
	p += key
	u := &url.URL{Scheme: c.scheme, Host: c.host, Path: p, RawPath: escape(p, false), RawQuery: canonicalQuery(query)}
	var r io.Reader
	if body != nil {
		r = io.NewSectionReader(body.r, body.off, body.n)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = body.n
	}
	return req, nil
}

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds an AWS Signature Version 4 to req, over its host and every
// header already set on it.
func (c *Client) sign(req *http.Request, payloadHash string) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	stamp := now().UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	values := map[string]string{"host": req.URL.Host}
	names := []string{"host"}
	for k, vs := range req.Header {
		k = strings.ToLower(k)
		trimmed := make([]string, len(vs))
		for i, v := range vs {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		values[k] = strings.Join(trimmed, ",")
		names = append(names, k)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, n := range names {
		headers.WriteString(n + ":" + values[n] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method, req.URL.EscapedPath(), req.URL.RawQuery, headers.String(), signed, payloadHash,
	}, "\n")
	scope := day + "/" + c.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := mac([]byte("AWS4"+c.cfg.SecretKey), day)
	key = mac(key, c.cfg.Region)
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey, scope, signed, hex.EncodeToString(mac(key, toSign))))
}

func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

// canonicalQuery is the query in the one form both sides sign: sorted, and
// escaped the way SigV4 wants.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, escape(k, true)+"="+escape(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// escape percent-encodes everything but letters, digits and -._~, and the
// slash unless slash is set.
func escape(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' || ch == '/' && !slash {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func readError(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode}
	var parsed struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(b, &parsed) == nil {
		e.Code, e.Message = parsed.Code, parsed.Message
	}
	return e
}

func decodeXML(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v); err != nil {
		return fmt.Errorf("read the service's answer: %w", err)
	}
	return nil
}

func drain(resp *http.Response) error {
	defer resp.Body.Close()
	_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}
