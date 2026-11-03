// Package s3 is the worker's S3 client: download the raw upload, upload the HLS output, list and
// delete by prefix. Standard library only (see sigv4.go for the signing).
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Config describes the object store.
type Config struct {
	Endpoint, Region, AccessKey, SecretKey string
	HTTPClient                             *http.Client
}

// Client talks to the object store. Safe for concurrent use.
type Client struct {
	base           *url.URL
	region         string
	access, secret string
	http           *http.Client
}

// New validates cfg.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("s3: endpoint must be an http(s) URL without a path")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: access key and secret key are required")
	}
	c := &Client{base: &url.URL{Scheme: u.Scheme, Host: u.Host}, region: cfg.Region, access: cfg.AccessKey, secret: cfg.SecretKey, http: cfg.HTTPClient}
	if c.region == "" {
		c.region = "us-east-1"
	}
	if c.http == nil {
		// No overall timeout: a source file can be many gigabytes. Callers pass a context.
		c.http = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Minute, MaxIdleConnsPerHost: 8}}
	}
	return c, nil
}

// Error is an S3 error response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("s3: %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// IsNotFound reports whether err means the object does not exist.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Code == "NoSuchKey")
}

func objectPath(bucket, key string) string {
	if key == "" {
		return "/" + awsEscape(bucket, false)
	}
	return "/" + awsEscape(bucket, false) + "/" + awsEscape(key, true)
}

func parseError(status int, body []byte) error {
	var e struct{ Code, Message string }
	if xml.Unmarshal(body, &e) != nil || e.Code == "" {
		b := body
		if len(b) > 200 {
			b = b[:200]
		}
		return &Error{Status: status, Code: http.StatusText(status), Message: strings.TrimSpace(string(b))}
	}
	return &Error{Status: status, Code: e.Code, Message: e.Message}
}

// request builds and signs a request; the caller closes the response body.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, header http.Header, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	u := c.base.Scheme + "://" + c.base.Host + path
	if len(q) > 0 {
		u += "?" + canonicalQuery(q)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, v := range header {
		req.Header[k] = v
	}
	signHeaders(req, c.access, c.secret, c.region, payloadHash, time.Now())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, parseError(resp.StatusCode, data)
	}
	return resp, nil
}

// Download streams an object to a file and checks the length.
func (c *Client) Download(ctx context.Context, bucket, key, dest string) (int64, error) {
	resp, err := c.request(ctx, http.MethodGet, objectPath(bucket, key), nil, nil, nil, 0, emptySHA256)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("s3: downloading %s/%s: %w", bucket, key, err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, fmt.Errorf("s3: downloading %s/%s: got %d of %d bytes", bucket, key, n, resp.ContentLength)
	}
	return n, nil
}

// GetBytes reads a small object into memory (at most limit bytes).
func (c *Client) GetBytes(ctx context.Context, bucket, key string, limit int64) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodGet, objectPath(bucket, key), nil, nil, nil, 0, emptySHA256)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("s3: %s/%s is larger than %d bytes", bucket, key, limit)
	}
	return b, nil
}

// Upload sends a file as one object. The payload is hashed first so the request is fully signed.
func (c *Client) Upload(ctx context.Context, bucket, key, path, contentType, cacheControl string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hdr := http.Header{}
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	if cacheControl != "" {
		hdr.Set("Cache-Control", cacheControl)
	}
	resp, err := c.request(ctx, http.MethodPut, objectPath(bucket, key), nil, hdr, f, size, hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// List returns every key under prefix.
func (c *Client) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.request(ctx, http.MethodGet, objectPath(bucket, ""), q, nil, nil, 0, emptySHA256)
		if err != nil {
			return nil, err
		}
		var out struct {
			Truncated bool     `xml:"IsTruncated"`
			Next      string   `xml:"NextContinuationToken"`
			Keys      []string `xml:"Contents>Key"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: bad list response: %w", err)
		}
		keys = append(keys, out.Keys...)
		if !out.Truncated || out.Next == "" {
			return keys, nil
		}
		token = out.Next
	}
}

// Delete removes one object; a missing object is fine.
func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	resp, err := c.request(ctx, http.MethodDelete, objectPath(bucket, key), nil, nil, nil, 0, emptySHA256)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// DeletePrefix removes every object under prefix and returns how many it found.
func (c *Client) DeletePrefix(ctx context.Context, bucket, prefix string) (int, error) {
	keys, err := c.List(ctx, bucket, prefix)
	if err != nil {
		return 0, err
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sem   = make(chan struct{}, 8)
	)
	for _, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func(k string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.Delete(ctx, bucket, k); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(k)
	}
	wg.Wait()
	return len(keys), first
}
