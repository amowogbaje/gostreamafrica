package playback

import (
	"crypto/md5" //nolint:gosec // nginx's secure_link module only speaks MD5; the secret is a suffix, so length extension does not apply
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Errors from Verify.
var (
	ErrExpired = errors.New("playback: token expired")
	ErrInvalid = errors.New("playback: token invalid")
)

// Signer makes the URLs the media front door accepts.
//
// URL shape (the token sits in the PATH, so every relative URL inside a playlist inherits it):
//
//	{base}/hls/{token}/{expires}/{assetId}/{path}
//
// The token covers the expiry and the asset id, so one token unlocks every file of that asset
// (manifest, renditions, segments, subtitles) until it expires, and nothing else.
type Signer struct {
	key  string
	base string
}

// NewSigner builds a Signer. base is the browser-facing origin of the media front door, key the shared secret.
func NewSigner(base, key string) Signer { return Signer{key: key, base: strings.TrimRight(base, "/")} }

// Token returns the token for an asset that expires at the given Unix time. It is the URL-safe
// base64 of MD5("<expires><assetId> <key>"), exactly what nginx's secure_link_md5 computes.
func (s Signer) Token(assetID string, expires int64) string {
	sum := md5.Sum([]byte(strconv.FormatInt(expires, 10) + assetID + " " + s.key)) //nolint:gosec // see the type comment
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// URL returns the signed URL of one file of an asset, for example "master.m3u8" or "360p/seg_00001.m4s".
func (s Signer) URL(assetID string, expires time.Time, file string) string {
	exp := expires.Unix()
	return s.base + "/hls/" + s.Token(assetID, exp) + "/" + strconv.FormatInt(exp, 10) + "/" + assetID + "/" + strings.TrimLeft(file, "/")
}

// Verify mirrors what nginx does: it reports ErrExpired for a genuine but outdated token and
// ErrInvalid for anything else.
func (s Signer) Verify(token string, expires int64, assetID string, now time.Time) error {
	want := s.Token(assetID, expires)
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return ErrInvalid
	}
	if now.Unix() > expires {
		return ErrExpired
	}
	return nil
}
