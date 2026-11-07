package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs against a real S3-compatible server (SeaweedFS from `make up`). Skipped unless
// TEST_S3_ENDPOINT is set. Needs TEST_S3_ACCESS_KEY, TEST_S3_SECRET_KEY and an existing
// bucket named by TEST_S3_BUCKET (default media-raw). Everything it creates is removed.
func TestAgainstRealServer(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		skipOrFail(t, "TEST_S3_ENDPOINT not set")
	}
	bucket := os.Getenv("TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "media-raw"
	}
	c, err := New(Config{Endpoint: endpoint, Region: "us-east-1",
		AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "_s3pkgtest/" + time.Now().Format("150405.000") + "/source.mp4"
	t.Cleanup(func() { _ = c.Delete(ctx, bucket, key) })

	uploadID, err := c.CreateMultipartUpload(ctx, bucket, key, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	aborted := false
	t.Cleanup(func() {
		if !aborted {
			_ = c.AbortMultipartUpload(ctx, bucket, key, uploadID)
		}
	})

	ups, err := c.ListMultipartUploads(ctx, bucket, "_s3pkgtest/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range ups {
		if u.UploadID == uploadID {
			found = true
			if d := time.Since(u.Initiated); d < -time.Minute || d > time.Minute {
				t.Errorf("Initiated looks wrong: %v ago", d)
			}
		}
	}
	if !found {
		t.Fatalf("new upload not listed: %+v", ups)
	}

	// Two parts sent through presigned URLs, exactly like a browser would.
	const partSize = 5 << 20
	data := make([]byte, partSize+1000)
	_, _ = rand.Read(data)
	copy(data[4:8], "ftyp")
	for n, chunk := range [][]byte{data[:partSize], data[partSize:]} {
		url := c.PresignUploadPart(bucket, key, uploadID, n+1, int64(len(chunk)), 5*time.Minute)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(chunk))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("part %d PUT -> %d", n+1, resp.StatusCode)
		}
	}

	// A pinned URL must refuse a body of a different size.
	wrong := c.PresignUploadPart(bucket, key, uploadID, 3, 1000, 5*time.Minute)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, wrong, bytes.NewReader(make([]byte, 2000)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("a PUT larger than the signed Content-Length must be refused with 403, got %d", resp.StatusCode)
	}

	parts, err := c.ListParts(ctx, bucket, key, uploadID)
	if err != nil || len(parts) != 2 || parts[0].Size != partSize || parts[1].Size != 1000 || parts[0].ETag == "" {
		t.Fatalf("ListParts: %+v, %v", parts, err)
	}
	if err := c.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts); err != nil {
		t.Fatal(err)
	}
	aborted = true

	info, err := c.Head(ctx, bucket, key)
	if err != nil || info.Size != int64(len(data)) || !strings.HasPrefix(info.ContentType, "video/mp4") {
		t.Fatalf("Head: %+v, %v", info, err)
	}
	head, err := c.GetRange(ctx, bucket, key, 0, 16)
	if err != nil || !bytes.Equal(head, data[:16]) {
		t.Fatalf("GetRange: %x, %v", head, err)
	}
	if _, err := c.ListParts(ctx, bucket, key, uploadID); !IsNotFound(err) {
		t.Errorf("ListParts after completion should be not-found, got %v", err)
	}

	// Abort path: a second upload is discarded and disappears from the listing.
	id2, err := c.CreateMultipartUpload(ctx, bucket, key+".b", "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AbortMultipartUpload(ctx, bucket, key+".b", id2); err != nil {
		t.Fatal(err)
	}
	if err := c.AbortMultipartUpload(ctx, bucket, key+".b", id2); err != nil {
		t.Errorf("aborting twice must be harmless: %v", err)
	}
	ups, _ = c.ListMultipartUploads(ctx, bucket, "_s3pkgtest/")
	for _, u := range ups {
		if u.UploadID == id2 {
			t.Errorf("aborted upload is still listed")
		}
	}

	prefix := key[:strings.LastIndex(key, "/")+1]
	keys, err := c.List(ctx, bucket, prefix)
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("List: %v %v", keys, err)
	}
	if n, err := c.DeletePrefix(ctx, bucket, prefix); err != nil || n != 1 {
		t.Fatalf("DeletePrefix: %d %v", n, err)
	}
	if _, err := c.Head(ctx, bucket, key); !IsNotFound(err) {
		t.Errorf("object should be gone, got %v", err)
	}
	if n, err := c.DeletePrefix(ctx, bucket, prefix); err != nil || n != 0 {
		t.Errorf("deleting twice must be harmless: %d %v", n, err)
	}
}

// skipOrFail skips an integration test whose service is not available, unless REQUIRE_INTEGRATION=1 (set by
// `make test` and CI), when a missing service is a failure: tests must not be silently skipped there.
func skipOrFail(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("REQUIRE_INTEGRATION") == "1" {
		t.Fatalf("%s, but REQUIRE_INTEGRATION=1: integration tests may not be skipped (start the stack with `make up`)", msg)
	}
	t.Skip(msg)
}
