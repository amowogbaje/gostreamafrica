package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, PublicEndpoint: "http://localhost:8333", Region: "us-east-1",
		AccessKey: "AK", SecretKey: "SK", Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNew_Validation(t *testing.T) {
	bad := []Config{
		{Endpoint: "", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "ftp://x", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "http://x/path", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "http://x", PublicEndpoint: "nonsense", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "http://x", AccessKey: "", SecretKey: "b"},
	}
	for i, c := range bad {
		if _, err := New(c); err == nil {
			t.Errorf("case %d: expected an error", i)
		}
	}
	if _, err := New(Config{Endpoint: "http://x:8333/", AccessKey: "a", SecretKey: "b"}); err != nil {
		t.Fatalf("a trailing slash must be accepted: %v", err)
	}
}

func TestEveryRequestIsSigned(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AK/20261101/us-east-1/s3/aws4_request") ||
			!strings.Contains(a, "SignedHeaders=host;") || r.Header.Get("X-Amz-Date") != "20261101T120000Z" ||
			r.Header.Get("X-Amz-Content-Sha256") == "" {
			t.Errorf("request is not properly signed: %q", a)
		}
		w.Header().Set("Content-Length", "0")
	})
	_ = c.Delete(context.Background(), "b", "k")
}

func TestCreateMultipartUpload(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/media-raw/abc/source.mp4" || r.URL.RawQuery != "uploads=" {
			t.Errorf("unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Content-Type") != "video/mp4" {
			t.Errorf("Content-Type %q not forwarded", r.Header.Get("Content-Type"))
		}
		_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>media-raw</Bucket><Key>abc/source.mp4</Key><UploadId>UP123</UploadId></InitiateMultipartUploadResult>`)
	})
	id, err := c.CreateMultipartUpload(context.Background(), "media-raw", "abc/source.mp4", "video/mp4")
	if err != nil || id != "UP123" {
		t.Fatalf("got %q, %v", id, err)
	}
}

func TestPresignUploadPart_UsesPublicHostAndNoNetwork(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("presigning must not call the network") })
	raw := c.PresignUploadPart("media-raw", "abc/source.mp4", "UP123", 7, 0, 15*time.Minute)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Host != "localhost:8333" || u.Path != "/media-raw/abc/source.mp4" {
		t.Fatalf("wrong target: %s", raw)
	}
	q := u.Query()
	if q.Get("X-Amz-SignedHeaders") != "host" {
		t.Fatalf("without a length only host is signed: %v", q)
	}
	pinned := c.PresignUploadPart("media-raw", "abc/source.mp4", "UP123", 7, 67108864, 15*time.Minute)
	pu, _ := url.Parse(pinned)
	if pu.Query().Get("X-Amz-SignedHeaders") != "content-length;host" {
		t.Fatalf("content-length must be signed when given: %s", pinned)
	}
	if q.Get("partNumber") != "7" || q.Get("uploadId") != "UP123" || q.Get("X-Amz-Expires") != "900" ||
		q.Get("X-Amz-Date") != "20261101T120000Z" || q.Get("X-Amz-SignedHeaders") != "host" || len(q.Get("X-Amz-Signature")) != 64 {
		t.Fatalf("wrong query: %v", q)
	}
}

func TestListParts_Paginates(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("uploadId") != "UP" {
			t.Errorf("missing uploadId: %v", q)
		}
		if q.Get("part-number-marker") == "" {
			_, _ = io.WriteString(w, `<ListPartsResult><IsTruncated>true</IsTruncated><NextPartNumberMarker>2</NextPartNumberMarker>
				<Part><PartNumber>2</PartNumber><ETag>"b"</ETag><Size>20</Size></Part>
				<Part><PartNumber>1</PartNumber><ETag>"a"</ETag><Size>10</Size></Part></ListPartsResult>`)
			return
		}
		if q.Get("part-number-marker") != "2" {
			t.Errorf("wrong marker %q", q.Get("part-number-marker"))
		}
		_, _ = io.WriteString(w, `<ListPartsResult><IsTruncated>false</IsTruncated><Part><PartNumber>3</PartNumber><ETag>"c"</ETag><Size>5</Size></Part></ListPartsResult>`)
	})
	parts, err := c.ListParts(context.Background(), "b", "k", "UP")
	if err != nil || calls != 2 || len(parts) != 3 {
		t.Fatalf("calls=%d parts=%v err=%v", calls, parts, err)
	}
	for i, want := range []Part{{1, `"a"`, 10}, {2, `"b"`, 20}, {3, `"c"`, 5}} {
		if parts[i] != want {
			t.Errorf("part %d = %+v, want %+v", i, parts[i], want)
		}
	}
}

func TestCompleteMultipartUpload_BodyAndErrorInside200(t *testing.T) {
	var gotBody string
	var respond string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Method != "POST" || r.URL.Query().Get("uploadId") != "UP" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, respond)
	})
	respond = `<CompleteMultipartUploadResult><ETag>"x-2"</ETag></CompleteMultipartUploadResult>`
	err := c.CompleteMultipartUpload(context.Background(), "b", "k", "UP", []Part{{Number: 1, ETag: `"a"`}, {Number: 2, ETag: `"b"`}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<PartNumber>1</PartNumber>", "<PartNumber>2</PartNumber>", "&#34;a&#34;"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q: %s", want, gotBody)
		}
	}
	respond = `<Error><Code>InternalError</Code><Message>We encountered an internal error.</Message></Error>`
	err = c.CompleteMultipartUpload(context.Background(), "b", "k", "UP", []Part{{Number: 1, ETag: "a"}})
	var e *Error
	if err == nil || !strings.Contains(err.Error(), "InternalError") {
		t.Fatalf("an <Error> inside a 200 must be an error, got %v", err)
	}
	_ = e
}

func TestAbort_And_Delete_TreatMissingAsSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchUpload</Code><Message>gone</Message></Error>`)
	})
	if err := c.AbortMultipartUpload(context.Background(), "b", "k", "UP"); err != nil {
		t.Fatalf("abort of a missing upload: %v", err)
	}
	if err := c.Delete(context.Background(), "b", "k"); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
}

func TestErrors_AreTyped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `<Error><Code>SignatureDoesNotMatch</Code><Message>bad</Message></Error>`)
	})
	_, err := c.GetRange(context.Background(), "b", "k", 0, 10) // HEAD responses carry no body, so use a GET
	var e *Error
	if err == nil || IsNotFound(err) {
		t.Fatalf("expected a non-not-found error, got %v", err)
	}
	if !strings.Contains(err.Error(), "SignatureDoesNotMatch") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error text: %v", err)
	}
	_ = e
}

func TestHead_And_GetRange(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			w.Header().Set("Content-Length", "12345")
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("ETag", `"abc"`)
		case "GET":
			if r.Header.Get("Range") != "bytes=4-11" {
				t.Errorf("Range %q", r.Header.Get("Range"))
			}
			if !strings.Contains(r.Header.Get("Authorization"), "range") {
				t.Errorf("Range header must be signed: %s", r.Header.Get("Authorization"))
			}
			w.WriteHeader(206)
			_, _ = io.WriteString(w, "ftypisom")
		}
	})
	info, err := c.Head(context.Background(), "b", "k")
	if err != nil || info.Size != 12345 || info.ContentType != "video/mp4" || info.ETag != `"abc"` {
		t.Fatalf("got %+v, %v", info, err)
	}
	b, err := c.GetRange(context.Background(), "b", "k", 4, 8)
	if err != nil || string(b) != "ftypisom" {
		t.Fatalf("got %q, %v", b, err)
	}
	if _, err := c.GetRange(context.Background(), "b", "k", 0, 0); err == nil {
		t.Fatal("a zero-length range must be rejected")
	}
}

func TestHead_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }) // HEAD has no body
	_, err := c.Head(context.Background(), "b", "k")
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestListMultipartUploads_Paginates(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if _, ok := q["uploads"]; !ok || q.Get("prefix") != "p/" {
			t.Errorf("unexpected query %v", q)
		}
		if q.Get("key-marker") == "" {
			_, _ = io.WriteString(w, `<ListMultipartUploadsResult><IsTruncated>true</IsTruncated><NextKeyMarker>p/a</NextKeyMarker><NextUploadIdMarker>U1</NextUploadIdMarker>
				<Upload><Key>p/a</Key><UploadId>U1</UploadId><Initiated>2026-11-01T10:00:00.000Z</Initiated></Upload></ListMultipartUploadsResult>`)
			return
		}
		if q.Get("key-marker") != "p/a" || q.Get("upload-id-marker") != "U1" {
			t.Errorf("markers %v", q)
		}
		_, _ = io.WriteString(w, `<ListMultipartUploadsResult><IsTruncated>false</IsTruncated>
			<Upload><Key>p/b</Key><UploadId>U2</UploadId><Initiated>2026-10-31T09:30:00Z</Initiated></Upload></ListMultipartUploadsResult>`)
	})
	ups, err := c.ListMultipartUploads(context.Background(), "b", "p/")
	if err != nil || len(ups) != 2 || ups[0].UploadID != "U1" || ups[1].Key != "p/b" ||
		!ups[1].Initiated.Equal(time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("got %+v, %v", ups, err)
	}
}

func TestObjectPath_EncodesKeys(t *testing.T) {
	if got := objectPath("media-raw", "a b/ü.mp4"); got != "/media-raw/a%20b/%C3%BC.mp4" {
		t.Fatalf("got %s", got)
	}
	if got := objectPath("media-raw", ""); got != "/media-raw" {
		t.Fatalf("got %s", got)
	}
}

func TestPresignUploadPart_SignsContentLengthWhenAsked(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	with, _ := url.Parse(c.PresignUploadPart("b", "k", "UP", 1, 5242880, time.Minute))
	if got := with.Query().Get("X-Amz-SignedHeaders"); got != "content-length;host" {
		t.Fatalf("signed headers = %q", got)
	}
	without, _ := url.Parse(c.PresignUploadPart("b", "k", "UP", 1, 0, time.Minute))
	if got := without.Query().Get("X-Amz-SignedHeaders"); got != "host" {
		t.Fatalf("signed headers = %q", got)
	}
	if with.Query().Get("X-Amz-Signature") == without.Query().Get("X-Amz-Signature") {
		t.Fatal("the signature must depend on the signed content length")
	}
}
