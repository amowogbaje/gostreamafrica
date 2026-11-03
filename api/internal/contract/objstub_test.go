package contract

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"streamafrica/api/internal/platform/s3"
)

// objStub is a minimal in-memory object store for the contract test: it implements just enough of
// the multipart life cycle to drive the upload endpoints. (The real store is covered by the S3
// client's own tests and by `make s3-check`.)
type objStub struct {
	seq     int
	uploads map[string]*stubUpload
	objects map[string][]byte // bucket/key -> first bytes
	down    bool              // simulate an outage
}

type stubUpload struct {
	bucket, key string
	parts       map[int]int64
	head        []byte
}

var mp4Head = append([]byte{0, 0, 0, 24}, []byte("ftypmp42\x00\x00\x00\x00mp42isom")...)

func newObjStub() *objStub {
	return &objStub{uploads: map[string]*stubUpload{}, objects: map[string][]byte{}}
}

func (o *objStub) err() error {
	if o.down {
		return errors.New("connection refused")
	}
	return nil
}

func (o *objStub) CreateMultipartUpload(_ context.Context, bucket, key, _ string) (string, error) {
	if err := o.err(); err != nil {
		return "", err
	}
	o.seq++
	id := fmt.Sprintf("STUB%04d", o.seq)
	o.uploads[id] = &stubUpload{bucket: bucket, key: key, parts: map[int]int64{}, head: mp4Head}
	return id, nil
}

func (o *objStub) PresignUploadPart(bucket, key, uploadID string, n int, size int64, ttl time.Duration) string {
	return fmt.Sprintf("http://localhost:8333/%s/%s?partNumber=%d&uploadId=%s&X-Amz-Expires=%d&len=%d", bucket, key, n, uploadID, int(ttl.Seconds()), size)
}

func (o *objStub) ListParts(_ context.Context, _, _, uploadID string) ([]s3.Part, error) {
	if err := o.err(); err != nil {
		return nil, err
	}
	u, ok := o.uploads[uploadID]
	if !ok {
		return nil, &s3.Error{Status: 404, Code: "NoSuchUpload"}
	}
	var out []s3.Part
	for n, sz := range u.parts {
		out = append(out, s3.Part{Number: n, Size: sz, ETag: fmt.Sprintf(`"e%d"`, n)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (o *objStub) CompleteMultipartUpload(_ context.Context, bucket, key, uploadID string, parts []s3.Part) error {
	u, ok := o.uploads[uploadID]
	if !ok {
		return &s3.Error{Status: 404, Code: "NoSuchUpload"}
	}
	var total int64
	for _, p := range parts {
		total += u.parts[p.Number]
	}
	o.objects[bucket+"/"+key] = append([]byte(fmt.Sprintf("%020d", total)), u.head...)
	delete(o.uploads, uploadID)
	return nil
}

func (o *objStub) AbortMultipartUpload(_ context.Context, _, _, uploadID string) error {
	delete(o.uploads, uploadID)
	return nil
}

func (o *objStub) ListMultipartUploads(context.Context, string, string) ([]s3.MultipartUpload, error) {
	return nil, nil
}

func (o *objStub) Head(_ context.Context, bucket, key string) (s3.ObjectInfo, error) {
	b, ok := o.objects[bucket+"/"+key]
	if !ok {
		return s3.ObjectInfo{}, &s3.Error{Status: 404, Code: "NotFound"}
	}
	var size int64
	_, _ = fmt.Sscanf(string(b[:20]), "%d", &size)
	return s3.ObjectInfo{Size: size}, nil
}

func (o *objStub) GetRange(_ context.Context, bucket, key string, offset, length int64) ([]byte, error) {
	b, ok := o.objects[bucket+"/"+key]
	if !ok {
		return nil, &s3.Error{Status: 404, Code: "NoSuchKey"}
	}
	body := b[20:]
	end := offset + length
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	return body[offset:end], nil
}

func (o *objStub) Delete(_ context.Context, bucket, key string) error {
	delete(o.objects, bucket+"/"+key)
	return nil
}

// store puts every part of the upload with this object key in place, as a browser would.
func (o *objStub) store(key string, sizes ...int64) {
	for _, u := range o.uploads {
		if strings.HasSuffix(u.key, key) {
			for i, sz := range sizes {
				u.parts[i+1] = sz
			}
			return
		}
	}
	panic("no open storage upload for " + key)
}

func (o *objStub) setHead(key string, head []byte) {
	for _, u := range o.uploads {
		if strings.HasSuffix(u.key, key) {
			u.head = head
			return
		}
	}
	panic("no open storage upload for " + key)
}
