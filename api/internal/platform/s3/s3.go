// Package s3 is a small S3 client for exactly what StreamAfrica needs: multipart uploads with
// presigned part URLs, object inspection, and cleanup. It speaks plain S3 (SigV4, path-style),
// so the same code runs against SeaweedFS locally and Cloudflare R2 or AWS S3 in the cloud
// (ADR-0006, ADR-0014). Standard library only: no SDK, no extra dependencies.
package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config describes the object store.
type Config struct {
	Endpoint       string // what this process connects to, e.g. http://seaweedfs:8333
	PublicEndpoint string // what browsers connect to; presigned URLs are signed for this host. Defaults to Endpoint.
	Region         string
	AccessKey      string
	SecretKey      string
	HTTPClient     *http.Client     // optional
	Now            func() time.Time // optional, for tests
}

// Client talks to the object store. It is safe for concurrent use.
type Client struct {
	endpoint, public *url.URL
	region           string
	access, secret   string
	http             *http.Client
	now              func() time.Time
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	parse := func(name, raw string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("s3: %s must be an http(s) URL without a path", name)
		}
		return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
	}
	ep, err := parse("endpoint", cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	pub := ep
	if cfg.PublicEndpoint != "" {
		if pub, err = parse("public endpoint", cfg.PublicEndpoint); err != nil {
			return nil, err
		}
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: access key and secret key are required")
	}
	c := &Client{endpoint: ep, public: pub, region: cfg.Region, access: cfg.AccessKey, secret: cfg.SecretKey,
		http: cfg.HTTPClient, now: cfg.Now}
	if c.region == "" {
		c.region = "us-east-1"
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
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

// IsNotFound reports whether err means the object, upload or bucket does not exist.
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case "NoSuchKey", "NoSuchUpload", "NoSuchBucket", "NotFound":
		return true
	}
	return e.Status == http.StatusNotFound
}

// Part is one uploaded part of a multipart upload.
type Part struct {
	Number int
	ETag   string
	Size   int64
}

// MultipartUpload is an unfinished multipart upload.
type MultipartUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// ObjectInfo is the result of Head.
type ObjectInfo struct {
	Size        int64
	ContentType string
	ETag        string
}

func objectPath(bucket, key string) string {
	if key == "" {
		return "/" + awsEscape(bucket, false)
	}
	return "/" + awsEscape(bucket, false) + "/" + awsEscape(key, true)
}

// do sends one signed request and returns the body of a 2xx answer.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, header http.Header, body []byte) ([]byte, http.Header, error) {
	u := c.endpoint.Scheme + "://" + c.endpoint.Host + path
	if len(q) > 0 {
		u += "?" + canonicalQuery(q)
	}
	var rdr io.Reader
	payload := emptySHA256
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
		payload = sha256Hex(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	signHeaders(req, c.access, c.secret, c.region, payload, c.now())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("s3: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("s3: reading response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.Header, parseError(resp.StatusCode, data)
	}
	return data, resp.Header, nil
}

func parseError(status int, body []byte) error {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) != nil || e.Code == "" {
		return &Error{Status: status, Code: http.StatusText(status), Message: strings.TrimSpace(string(truncate(body, 200)))}
	}
	return &Error{Status: status, Code: e.Code, Message: e.Message}
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// CreateMultipartUpload starts an upload and returns its upload id. contentType is stored on
// the finished object.
func (c *Client) CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (string, error) {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	body, _, err := c.do(ctx, http.MethodPost, objectPath(bucket, key), url.Values{"uploads": {""}}, h, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(body, &out); err != nil || out.UploadID == "" {
		return "", errors.New("s3: CreateMultipartUpload returned no upload id")
	}
	return out.UploadID, nil
}

// PresignUploadPart returns a URL a browser can PUT one part to. It is signed for the PUBLIC
// endpoint and valid for ttl. No network call is made. When contentLength is positive it is
// part of the signature: the store refuses any request whose Content-Length differs, so a client
// cannot push more bytes than the part it was given (browsers set Content-Length from the body).
func (c *Client) PresignUploadPart(bucket, key, uploadID string, partNumber int, contentLength int64, ttl time.Duration) string {
	q := url.Values{"partNumber": {strconv.Itoa(partNumber)}, "uploadId": {uploadID}}
	var extra map[string]string
	if contentLength > 0 {
		extra = map[string]string{"content-length": strconv.FormatInt(contentLength, 10)}
	}
	return presign(c.access, c.secret, c.region, http.MethodPut, c.public.Scheme, c.public.Host,
		objectPath(bucket, key), q, c.now(), ttl, extra)
}

// ListParts returns every part stored so far for an upload, in part order.
func (c *Client) ListParts(ctx context.Context, bucket, key, uploadID string) ([]Part, error) {
	var parts []Part
	marker := 0
	for {
		q := url.Values{"uploadId": {uploadID}, "max-parts": {"1000"}}
		if marker > 0 {
			q.Set("part-number-marker", strconv.Itoa(marker))
		}
		body, _, err := c.do(ctx, http.MethodGet, objectPath(bucket, key), q, nil, nil)
		if err != nil {
			return nil, err
		}
		var out struct {
			Truncated  bool `xml:"IsTruncated"`
			NextMarker int  `xml:"NextPartNumberMarker"`
			Parts      []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
				Size   int64  `xml:"Size"`
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("s3: bad ListParts response: %w", err)
		}
		for _, p := range out.Parts {
			parts = append(parts, Part{Number: p.Number, ETag: p.ETag, Size: p.Size})
		}
		if !out.Truncated || out.NextMarker <= marker {
			break
		}
		marker = out.NextMarker
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// CompleteMultipartUpload assembles the parts into the final object.
func (c *Client) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []Part) error {
	var b bytes.Buffer
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		b.WriteString("<Part><PartNumber>" + strconv.Itoa(p.Number) + "</PartNumber><ETag>")
		_ = xml.EscapeText(&b, []byte(p.ETag))
		b.WriteString("</ETag></Part>")
	}
	b.WriteString("</CompleteMultipartUpload>")
	h := http.Header{"Content-Type": {"application/xml"}}
	body, _, err := c.do(ctx, http.MethodPost, objectPath(bucket, key), url.Values{"uploadId": {uploadID}}, h, b.Bytes())
	if err != nil {
		return err
	}
	// S3 can answer 200 and still put an <Error> in the body.
	if bytes.Contains(truncate(body, 512), []byte("<Error>")) {
		return parseError(http.StatusOK, body)
	}
	return nil
}

// AbortMultipartUpload discards an upload and its parts. An unknown upload is not an error.
func (c *Client) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	_, _, err := c.do(ctx, http.MethodDelete, objectPath(bucket, key), url.Values{"uploadId": {uploadID}}, nil, nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// ListMultipartUploads returns every unfinished upload in bucket whose key starts with prefix.
func (c *Client) ListMultipartUploads(ctx context.Context, bucket, prefix string) ([]MultipartUpload, error) {
	var all []MultipartUpload
	keyMarker, idMarker := "", ""
	for {
		q := url.Values{"uploads": {""}, "max-uploads": {"1000"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
		}
		if idMarker != "" {
			q.Set("upload-id-marker", idMarker)
		}
		body, _, err := c.do(ctx, http.MethodGet, objectPath(bucket, ""), q, nil, nil)
		if err != nil {
			return nil, err
		}
		var out struct {
			Truncated  bool   `xml:"IsTruncated"`
			NextKey    string `xml:"NextKeyMarker"`
			NextUpload string `xml:"NextUploadIdMarker"`
			Uploads    []struct {
				Key       string `xml:"Key"`
				UploadID  string `xml:"UploadId"`
				Initiated string `xml:"Initiated"`
			} `xml:"Upload"`
		}
		if err := xml.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("s3: bad ListMultipartUploads response: %w", err)
		}
		for _, u := range out.Uploads {
			ts, err := time.Parse(time.RFC3339, u.Initiated)
			if err != nil {
				return nil, fmt.Errorf("s3: bad Initiated timestamp %q", u.Initiated)
			}
			all = append(all, MultipartUpload{Key: u.Key, UploadID: u.UploadID, Initiated: ts})
		}
		if !out.Truncated || (out.NextKey == keyMarker && out.NextUpload == idMarker) {
			return all, nil
		}
		keyMarker, idMarker = out.NextKey, out.NextUpload
	}
}

// Head returns the size and content type of an object, or an error for which IsNotFound is true.
func (c *Client) Head(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	_, h, err := c.do(ctx, http.MethodHead, objectPath(bucket, key), nil, nil, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	size, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	if err != nil {
		return ObjectInfo{}, errors.New("s3: HEAD response has no Content-Length")
	}
	return ObjectInfo{Size: size, ContentType: h.Get("Content-Type"), ETag: h.Get("ETag")}, nil
}

// GetRange reads length bytes starting at offset (fewer if the object ends sooner).
func (c *Client) GetRange(ctx context.Context, bucket, key string, offset, length int64) ([]byte, error) {
	if offset < 0 || length <= 0 {
		return nil, errors.New("s3: invalid range")
	}
	h := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)}}
	body, _, err := c.do(ctx, http.MethodGet, objectPath(bucket, key), nil, h, nil)
	return body, err
}

// Delete removes an object. A missing object is not an error.
func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	_, _, err := c.do(ctx, http.MethodDelete, objectPath(bucket, key), nil, nil, nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
