package creators

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"streamafrica/api/internal/platform/s3"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// isoHead is the start of a plausible MP4; ebmlHead the start of a plausible MKV/WebM.
var (
	isoHead  = append([]byte{0, 0, 0, 24}, []byte("ftypmp42\x00\x00\x00\x00mp42isom")...)
	ebmlHead = []byte{0x1A, 0x45, 0xDF, 0xA3, 0x93, 0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}
)

// ---------------------------------------------------------------- in-memory object store

type fakeObj struct {
	size  int64
	head  []byte
	ctype string
}

type fakeUpload struct {
	bucket, key, ctype string
	started            time.Time
	parts              map[int]int64 // part number -> size
	head               []byte        // bytes the first 4 KiB of the assembled object will have
}

// fakeObjects is an in-memory ObjectStore. Tests "upload" parts by calling putPart, which is what a
// browser PUTting to a presigned URL does to the real store.
type fakeObjects struct {
	mu       sync.Mutex
	now      func() time.Time
	seq      int
	uploads  map[string]*fakeUpload // upload id -> state
	objects  map[string]fakeObj     // bucket/key
	failNext map[string]error       // method name -> error to return once
	headSize int64                  // when > 0, Head reports this size (simulates a truncated or padded file)
	calls    []string
	signed   []string // "bucket/key#part:size" for every presigned URL
	deleted  []string // every DeletePrefix call, as bucket/prefix
}

func newFakeObjects(now func() time.Time) *fakeObjects {
	return &fakeObjects{now: now, uploads: map[string]*fakeUpload{}, objects: map[string]fakeObj{}, failNext: map[string]error{}}
}

func (f *fakeObjects) fail(method string) error {
	f.calls = append(f.calls, method)
	if err := f.failNext[method]; err != nil {
		delete(f.failNext, method)
		return err
	}
	return nil
}

func (f *fakeObjects) CreateMultipartUpload(_ context.Context, bucket, key, ctype string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateMultipartUpload"); err != nil {
		return "", err
	}
	f.seq++
	id := fmt.Sprintf("UP%03d", f.seq)
	f.uploads[id] = &fakeUpload{bucket: bucket, key: key, ctype: ctype, started: f.now(), parts: map[int]int64{}, head: isoHead}
	return id, nil
}

func (f *fakeObjects) PresignUploadPart(bucket, key, uploadID string, n int, size int64, ttl time.Duration) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signed = append(f.signed, fmt.Sprintf("%s/%s#%d:%d", bucket, key, n, size))
	return fmt.Sprintf("http://store.test/%s/%s?uploadId=%s&partNumber=%d&len=%d&ttl=%d", bucket, key, uploadID, n, size, int(ttl.Seconds()))
}

func (f *fakeObjects) ListParts(_ context.Context, bucket, key, uploadID string) ([]s3.Part, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListParts"); err != nil {
		return nil, err
	}
	u, ok := f.uploads[uploadID]
	if !ok {
		return nil, &s3.Error{Status: 404, Code: "NoSuchUpload", Message: "gone"}
	}
	var out []s3.Part
	for n, size := range u.parts {
		out = append(out, s3.Part{Number: n, Size: size, ETag: fmt.Sprintf(`"etag-%d"`, n)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (f *fakeObjects) CompleteMultipartUpload(_ context.Context, bucket, key, uploadID string, parts []s3.Part) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CompleteMultipartUpload"); err != nil {
		return err
	}
	u, ok := f.uploads[uploadID]
	if !ok {
		return &s3.Error{Status: 404, Code: "NoSuchUpload", Message: "gone"}
	}
	var total int64
	for _, p := range parts {
		total += u.parts[p.Number]
	}
	f.objects[bucket+"/"+key] = fakeObj{size: total, head: u.head, ctype: u.ctype}
	delete(f.uploads, uploadID)
	return nil
}

func (f *fakeObjects) AbortMultipartUpload(_ context.Context, bucket, key, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("AbortMultipartUpload"); err != nil {
		return err
	}
	delete(f.uploads, uploadID)
	return nil
}

func (f *fakeObjects) ListMultipartUploads(_ context.Context, bucket, prefix string) ([]s3.MultipartUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListMultipartUploads"); err != nil {
		return nil, err
	}
	var out []s3.MultipartUpload
	for id, u := range f.uploads {
		if u.bucket == bucket {
			out = append(out, s3.MultipartUpload{Key: u.key, UploadID: id, Initiated: u.started})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadID < out[j].UploadID })
	return out, nil
}

func (f *fakeObjects) Head(_ context.Context, bucket, key string) (s3.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Head"); err != nil {
		return s3.ObjectInfo{}, err
	}
	o, ok := f.objects[bucket+"/"+key]
	if !ok {
		return s3.ObjectInfo{}, &s3.Error{Status: 404, Code: "NotFound"}
	}
	if f.headSize > 0 {
		o.size = f.headSize
	}
	return s3.ObjectInfo{Size: o.size, ContentType: o.ctype}, nil
}

func (f *fakeObjects) GetRange(_ context.Context, bucket, key string, offset, length int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetRange"); err != nil {
		return nil, err
	}
	o, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, &s3.Error{Status: 404, Code: "NoSuchKey"}
	}
	end := offset + length
	if end > int64(len(o.head)) {
		end = int64(len(o.head))
	}
	if offset >= end {
		return nil, nil
	}
	return o.head[offset:end], nil
}

func (f *fakeObjects) Delete(_ context.Context, bucket, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Delete"); err != nil {
		return err
	}
	delete(f.objects, bucket+"/"+key)
	return nil
}

func (f *fakeObjects) DeletePrefix(_ context.Context, bucket, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeletePrefix"); err != nil {
		return 0, err
	}
	n := 0
	for k := range f.objects {
		if strings.HasPrefix(k, bucket+"/"+prefix) {
			delete(f.objects, k)
			n++
		}
	}
	f.deleted = append(f.deleted, bucket+"/"+prefix)
	return n, nil
}

// ---- helpers for tests (not part of ObjectStore)

// putPart stores a part as a presigned PUT would.
func (f *fakeObjects) putPart(uploadID string, n int, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[uploadID].parts[n] = size
}

// putAllParts stores every part of u with its exact size.
func (f *fakeObjects) putAllParts(u Upload) {
	for n := 1; n <= u.PartCount; n++ {
		f.putPart(u.S3UploadID, n, u.PartSize(n))
	}
}

func (f *fakeObjects) setHead(uploadID string, head []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[uploadID].head = head
}

func (f *fakeObjects) hasObject(bucket, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[bucket+"/"+key]
	return ok
}

func (f *fakeObjects) openUploads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

func (f *fakeObjects) called(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- in-memory Store

type fakeStore struct {
	mu          sync.Mutex
	titles      map[string]TitleInfo
	assets      map[string]AssetInfo
	unpublished []string
	uploads     map[string]Upload
	jobs        map[string]Job // upload id -> job
	policy      map[string]Policy
	lockHeld    bool
	jobSeq      int
}

func newFakeStore() *fakeStore {
	return &fakeStore{titles: map[string]TitleInfo{}, assets: map[string]AssetInfo{}, uploads: map[string]Upload{}, jobs: map[string]Job{}, policy: map[string]Policy{}}
}

func active(s Status) bool {
	return s == StatusInitiated || s == StatusUploaded || s == StatusProcessing
}

func (f *fakeStore) Title(_ context.Context, id string) (TitleInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.titles[id]
	if !ok {
		return TitleInfo{}, ErrTitleNotFound
	}
	return t, nil
}

func (f *fakeStore) CountActive(_ context.Context, creator string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.uploads {
		if u.CreatorID == creator && active(u.Status) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) BytesSince(_ context.Context, creator string, since time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, u := range f.uploads {
		if u.CreatorID == creator && u.CreatedAt.After(since) && u.Status != StatusAborted && u.Status != StatusExpired {
			n += u.SizeBytes
		}
	}
	return n, nil
}

func (f *fakeStore) Create(_ context.Context, u Upload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.uploads {
		if o.TitleID == u.TitleID && active(o.Status) {
			return ErrTitleBusy
		}
	}
	f.uploads[u.ID] = u
	return nil
}

func (f *fakeStore) Get(_ context.Context, id string) (Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[id]
	if !ok {
		return Upload{}, ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) MarkUploaded(_ context.Context, id string, p Policy, now time.Time) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if u.Status != StatusInitiated {
		return Job{}, ErrConflict
	}
	u.Status, u.S3UploadID, u.CompletedAt, u.UpdatedAt = StatusUploaded, "", &now, now
	f.uploads[id] = u
	f.jobSeq++
	j := Job{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.jobSeq), UploadID: id, State: "queued"}
	f.jobs[id] = j
	f.policy[id] = p
	return j, nil
}

func (f *fakeStore) JobFor(_ context.Context, id string) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return j, nil
}

func (f *fakeStore) Reject(_ context.Context, id, code, reason string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if u.Status != StatusInitiated && u.Status != StatusUploaded {
		return ErrConflict
	}
	u.Status, u.RejectCode, u.RejectReason, u.S3UploadID, u.UpdatedAt = StatusRejected, code, reason, "", now
	f.uploads[id] = u
	return nil
}

func (f *fakeStore) Close(_ context.Context, id string, to Status, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if u.Status != StatusInitiated {
		return ErrConflict
	}
	u.Status, u.S3UploadID, u.UpdatedAt = to, "", now
	f.uploads[id] = u
	return nil
}

func (f *fakeStore) Expired(_ context.Context, now time.Time, limit int) ([]Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Upload
	for _, u := range f.uploads {
		if u.Status == StatusInitiated && !u.ExpiresAt.After(now) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) Requeue(_ context.Context, id string, now time.Time) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.uploads[id]
	if u.Status != StatusRejected || u.RejectCode != "processing_failed" {
		return Job{}, ErrConflict
	}
	for _, o := range f.uploads {
		if o.ID != id && o.TitleID == u.TitleID && active(o.Status) {
			return Job{}, ErrTitleBusy
		}
	}
	u.Status, u.RejectCode, u.RejectReason, u.UpdatedAt = StatusUploaded, "", "", now
	f.uploads[id] = u
	j := f.jobs[id]
	j.State, j.Attempts, j.Error = "queued", 0, ""
	f.jobs[id] = j
	return j, nil
}

func (f *fakeStore) Asset(_ context.Context, id string) (AssetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.assets[id]
	if !ok {
		return AssetInfo{}, ErrNotFound
	}
	return a, nil
}

func (f *fakeStore) ReadyUploadIDs(_ context.Context, title string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, u := range f.uploads {
		if u.TitleID == title && u.Status == StatusReady {
			ids = append(ids, u.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (f *fakeStore) UnpublishTitle(_ context.Context, title string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, a := range f.assets {
		if a.TitleID == title && a.TitleStatus == "published" {
			a.TitleStatus = "unpublished"
			f.assets[id] = a
		}
	}
	f.unpublished = append(f.unpublished, title)
	return nil
}

func (f *fakeStore) DeleteAsset(_ context.Context, assetID string, uploadIDs []string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.assets, assetID)
	for _, id := range uploadIDs {
		if u, ok := f.uploads[id]; ok && u.Status == StatusReady {
			u.Status, u.UpdatedAt = StatusDeleted, now
			f.uploads[id] = u
		}
	}
	return nil
}

func (f *fakeStore) WithLock(ctx context.Context, _ int64, fn func(context.Context) error) (bool, error) {
	f.mu.Lock()
	if f.lockHeld {
		f.mu.Unlock()
		return false, nil
	}
	f.lockHeld = true
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.lockHeld = false; f.mu.Unlock() }()
	return true, fn(ctx)
}
