package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, AccessKey: "AK", SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNew_Validation(t *testing.T) {
	for _, cfg := range []Config{{Endpoint: "", AccessKey: "a", SecretKey: "b"}, {Endpoint: "http://x/p", AccessKey: "a", SecretKey: "b"}, {Endpoint: "http://x"}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}

func TestDownload_WritesFileAndChecksLength(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/media-raw/u/source.mp4" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, "hello video")
	})
	dst := filepath.Join(t.TempDir(), "in.mp4")
	n, err := c.Download(context.Background(), "media-raw", "u/source.mp4", dst)
	b, _ := os.ReadFile(dst)
	if err != nil || n != 11 || string(b) != "hello video" {
		t.Fatalf("%d %q %v", n, b, err)
	}
	c2 := newTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "x") })
	if _, err := c2.Download(context.Background(), "b", "k", filepath.Join(t.TempDir(), "no", "dir")); err == nil {
		t.Fatal("an unwritable destination must fail")
	}
}

func TestDownload_NotFound(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>`)
	})
	_, err := c.Download(context.Background(), "b", "k", filepath.Join(t.TempDir(), "x"))
	if !IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
}

func TestUpload_SendsHeadersAndSignedHash(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || r.URL.Path != "/media-hls/a/360p/seg_00000.m4s" || string(b) != "SEGMENT" || r.ContentLength != 7 {
			t.Errorf("unexpected %s %s %q", r.Method, r.URL, b)
		}
		if r.Header.Get("Content-Type") != "video/mp4" || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("headers: %v", r.Header)
		}
		if got := r.Header.Get("X-Amz-Content-Sha256"); got != sha256Hex([]byte("SEGMENT")) {
			t.Errorf("payload hash %q", got)
		}
	})
	p := filepath.Join(t.TempDir(), "seg")
	_ = os.WriteFile(p, []byte("SEGMENT"), 0o600)
	if err := c.Upload(context.Background(), "media-hls", "a/360p/seg_00000.m4s", p, "video/mp4", "public, max-age=31536000, immutable"); err != nil {
		t.Fatal(err)
	}
}

func TestList_Paginates(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("list-type") != "2" || q.Get("prefix") != "a/" {
			t.Errorf("query %v", q)
		}
		if q.Get("continuation-token") == "" {
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>T</NextContinuationToken><Contents><Key>a/1</Key></Contents></ListBucketResult>`)
			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>a/2</Key></Contents></ListBucketResult>`)
	})
	keys, err := c.List(context.Background(), "b", "a/")
	if err != nil || len(keys) != 2 || keys[0] != "a/1" || keys[1] != "a/2" {
		t.Fatalf("%v %v", keys, err)
	}
}

func TestDeletePrefix(t *testing.T) {
	var mu sync.Mutex
	deleted := map[string]bool{}
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>x/1</Key></Contents><Contents><Key>x/2</Key></Contents><Contents><Key>x/3</Key></Contents></ListBucketResult>`)
			return
		}
		mu.Lock()
		deleted[r.URL.Path] = true
		mu.Unlock()
		w.WriteHeader(204)
	})
	n, err := c.DeletePrefix(context.Background(), "b", "x/")
	if err != nil || n != 3 || len(deleted) != 3 || !deleted["/b/x/2"] {
		t.Fatalf("%d %v %v", n, deleted, err)
	}
}

func TestDelete_MissingIsFine_OtherErrorsAreNot(t *testing.T) {
	code := 404
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
	if err := c.Delete(context.Background(), "b", "k"); err != nil {
		t.Fatalf("missing: %v", err)
	}
	code = 500
	if err := c.Delete(context.Background(), "b", "k"); err == nil {
		t.Fatal("a 500 must be an error")
	}
}

func TestGetBytes_Limit(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "0123456789") })
	if b, err := c.GetBytes(context.Background(), "b", "k", 10); err != nil || string(b) != "0123456789" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := c.GetBytes(context.Background(), "b", "k", 5); err == nil {
		t.Fatal("over the limit must fail")
	}
}
