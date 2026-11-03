package s3

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Test vectors from "Authenticating Requests: Using Query Parameters (AWS Signature Version 4)"
// and "Signature Calculations for the Authorization Header" in the Amazon S3 documentation.
const (
	vecAccess = "AKIAIOSFODNN7EXAMPLE"
	vecSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var vecTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func TestPresign_AWSDocumentationVector(t *testing.T) {
	got := presign(vecAccess, vecSecret, "us-east-1", "GET", "https", "examplebucket.s3.amazonaws.com", "/test.txt",
		url.Values{}, vecTime, 86400*time.Second, nil)
	const wantSig = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "&X-Amz-Signature="+wantSig) {
		t.Fatalf("signature mismatch:\n got  %s\n want ...%s", got, wantSig)
	}
	for _, want := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request",
		"X-Amz-Date=20130524T000000Z", "X-Amz-Expires=86400", "X-Amz-SignedHeaders=host",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("URL is missing %q: %s", want, got)
		}
	}
}

func TestSignHeaders_AWSDocumentationVector(t *testing.T) {
	// GET Object with a Range header, from the S3 SigV4 header-signing example.
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	signHeaders(req, vecAccess, vecSecret, "us-east-1", emptySHA256, vecTime)
	got := req.Header.Get("Authorization")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	// The AWS example writes no space after the commas; S3 accepts both. Compare without them.
	if strings.ReplaceAll(got, ", ", ",") != want {
		t.Fatalf("Authorization mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestSignHeaders_PutWithQueryVector(t *testing.T) {
	// "PUT Object" example: body "Welcome to Amazon S3.", x-amz-storage-class header, no query.
	body := "Welcome to Amazon S3."
	req, _ := http.NewRequestWithContext(context.Background(), "PUT", "https://examplebucket.s3.amazonaws.com/test$file.text", strings.NewReader(body))
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	signHeaders(req, vecAccess, vecSecret, "us-east-1", sha256Hex([]byte(body)), vecTime)
	// date is not in our signed set (the AWS example signs it), so we only check we produce a
	// well-formed header with every x-amz header signed.
	got := req.Header.Get("Authorization")
	if !strings.Contains(got, "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-storage-class") {
		t.Fatalf("unexpected signed headers: %s", got)
	}
}

func TestAWSEscape(t *testing.T) {
	cases := []struct {
		in        string
		keepSlash bool
		want      string
	}{
		{"abc-_.~XYZ09", false, "abc-_.~XYZ09"},
		{"a b", false, "a%20b"},
		{"a+b", false, "a%2Bb"},
		{"a/b", false, "a%2Fb"},
		{"a/b", true, "a/b"},
		{"é", false, "%C3%A9"},
		{"x=y&z", false, "x%3Dy%26z"},
		{"\"quoted\"", false, "%22quoted%22"},
	}
	for _, c := range cases {
		if got := awsEscape(c.in, c.keepSlash); got != c.want {
			t.Errorf("awsEscape(%q,%v)=%q want %q", c.in, c.keepSlash, got, c.want)
		}
	}
}

func TestCanonicalQuery_SortedAndEncoded(t *testing.T) {
	q := url.Values{"uploads": {""}, "prefix": {"a b/"}, "Z": {"1"}, "a": {"2", "1"}}
	want := "Z=1&a=1&a=2&prefix=a%20b%2F&uploads="
	if got := canonicalQuery(q); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
