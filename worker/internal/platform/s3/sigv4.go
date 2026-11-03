// Request signing copied from the API module's internal/platform/s3 (the two Go modules share no code,
// ADR-0001). Keep the two in step; both are verified against the same AWS test vectors.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	algorithm     = "AWS4-HMAC-SHA256"
	emptySHA256   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedBody  = "UNSIGNED-PAYLOAD"
	amzDateFormat = "20060102T150405Z"
	dateFormat    = "20060102"
)

// awsEscape percent-encodes s the way SigV4 requires: only A-Z a-z 0-9 - _ . ~ stay literal, space
// is %20 (never +), hex digits are upper case. Slashes are kept when keepSlash is true.
func awsEscape(s string, keepSlash bool) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&15])
		}
	}
	return b.String()
}

// canonicalQuery sorts and encodes query parameters for signing and for the request URL.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k, false)+"="+awsEscape(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func signingKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

func scope(date, region string) string { return date + "/" + region + "/s3/aws4_request" }

// signature builds the canonical request, the string to sign and the final signature.
// headers must already be lower-cased and trimmed; they are the signed headers.
func signature(secret, region, method, canonicalPath, canonicalQ string, headers map[string]string, payloadHash string, t time.Time) (sig, signedHeaders string) {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k + ":" + headers[k] + "\n")
	}
	signedHeaders = strings.Join(names, ";")
	canonical := strings.Join([]string{method, canonicalPath, canonicalQ, ch.String(), signedHeaders, payloadHash}, "\n")
	amzDate := t.UTC().Format(amzDateFormat)
	date := t.UTC().Format(dateFormat)
	toSign := strings.Join([]string{algorithm, amzDate, scope(date, region), sha256Hex([]byte(canonical))}, "\n")
	sig = hex.EncodeToString(hmacSHA256(signingKey(secret, date, region), toSign))
	return sig, signedHeaders
}

// signHeaders adds SigV4 Authorization headers to req. Signed: host, x-amz-*, and content-type,
// range and content-md5 when present.
func signHeaders(req *http.Request, accessKey, secret, region, payloadHash string, t time.Time) {
	req.Header.Set("X-Amz-Date", t.UTC().Format(amzDateFormat))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "range" || lk == "content-md5" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
		}
	}
	sig, signed := signature(secret, region, req.Method, req.URL.EscapedPath(), canonicalQuery(req.URL.Query()), headers, payloadHash, t)
	date := t.UTC().Format(dateFormat)
	req.Header.Set("Authorization", algorithm+" Credential="+accessKey+"/"+scope(date, region)+
		", SignedHeaders="+signed+", Signature="+sig)
}
