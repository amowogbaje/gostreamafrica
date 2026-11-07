package creators

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/s3"
)

const (
	mib = 1 << 20
	gib = 1 << 30
)

type env struct {
	t       *testing.T
	svc     *Service
	store   *fakeStore
	objs    *fakeObjects
	clock   time.Time
	cfg     Config
	scanner *stubScanner
	creator auth.Principal
	other   auth.Principal
	admin   auth.Principal
	titleID string
}

type stubScanner struct {
	threat string
	err    error
}

func (s *stubScanner) Scan(context.Context, ScanTarget) (string, error) { return s.threat, s.err }

const (
	creatorID = "11111111-1111-4111-8111-111111111111"
	otherID   = "22222222-2222-4222-8222-222222222222"
	adminID   = "33333333-3333-4333-8333-333333333333"
	titleA    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func newEnv(t *testing.T, mods ...func(*Config)) *env {
	t.Helper()
	e := &env{t: t, clock: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), store: newFakeStore(), scanner: &stubScanner{},
		creator: auth.Principal{UserID: creatorID, Role: auth.RoleCreator},
		other:   auth.Principal{UserID: otherID, Role: auth.RoleCreator},
		admin:   auth.Principal{UserID: adminID, Role: auth.RoleAdmin},
		titleID: titleA}
	now := func() time.Time { return e.clock }
	e.objs = newFakeObjects(now)
	e.cfg = Config{RawBucket: "media-raw", HLSBucket: "media-hls", PublicBucket: "media-public", MaxBytes: 8 * gib, PartBytes: 64 * mib, URLTTL: 15 * time.Minute,
		AbandonAfter: 24 * time.Hour, MaxActive: 3, DailyBytes: 32 * gib, Policy: DefaultPolicy(4*3600, 3840, 2160)}
	for _, m := range mods {
		m(&e.cfg)
	}
	e.store.titles[titleA] = TitleInfo{ID: titleA, CreatorID: creatorID, Type: "movie"}
	e.svc = NewService(e.store, e.objs, e.scanner, quietLogger(), now, e.cfg)
	return e
}

func (e *env) addTitle(id, creator, typ string, hasAsset bool) {
	e.store.titles[id] = TitleInfo{ID: id, CreatorID: creator, Type: typ, HasAsset: hasAsset}
}

// auth2 is a creator identity for tests that need several of them.
type auth2 struct{ UserID string }

func (a auth2) principal() auth.Principal {
	return auth.Principal{UserID: a.UserID, Role: auth.RoleCreator}
}

func (e *env) asCreator() auth2 { return auth2{UserID: creatorID} }

func uuidN(n int) string { return fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012d", n) }

func (e *env) req(size int64) CreateInput {
	return CreateInput{TitleID: e.titleID, Filename: "My Film.mp4", ContentType: "video/mp4", SizeBytes: size}
}

func (e *env) create(size int64) CreateResult {
	e.t.Helper()
	res, err := e.svc.Create(context.Background(), e.creator, e.req(size))
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return res
}

// upload creates an upload and stores all its parts, ready for Complete.
func (e *env) readyUpload(size int64) Upload {
	e.t.Helper()
	u := e.create(size).Upload
	e.objs.putAllParts(u)
	return u
}

func problemOf(t *testing.T, err error, status int, code string) *Problem {
	t.Helper()
	var p *Problem
	if !errors.As(err, &p) {
		t.Fatalf("want problem %d %s, got %T: %v", status, code, err, err)
	}
	if p.Status != status || p.Code != code {
		t.Fatalf("want %d %s, got %d %s (%s)", status, code, p.Status, p.Code, p.Message)
	}
	return p
}

// wantProblem asserts the error is the given problem.
func wantProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	_ = problemOf(t, err, status, code)
}

// ---------------------------------------------------------------- Create

func TestCreate_StartsAMultipartUpload(t *testing.T) {
	e := newEnv(t)
	res := e.create(8 * gib)
	u := res.Upload
	if u.Status != StatusInitiated || u.CreatorID != creatorID || u.TitleID != titleA || u.Container != "mp4" ||
		u.ContentType != "video/mp4" || u.Filename != "My Film.mp4" || u.Bucket != "media-raw" {
		t.Fatalf("unexpected upload: %+v", u)
	}
	if want := u.ID + "/source.mp4"; u.ObjectKey != want {
		t.Fatalf("object key %q, want %q (never derived from the file name)", u.ObjectKey, want)
	}
	if u.PartCount != 128 || u.PartSizeBytes != 64*mib {
		t.Fatalf("8 GiB must be 128 parts of 64 MiB, got %d x %d", u.PartCount, u.PartSizeBytes)
	}
	if !u.ExpiresAt.Equal(e.clock.Add(24 * time.Hour)) {
		t.Fatalf("expires_at %v", u.ExpiresAt)
	}
	if len(res.Parts) != 4 {
		t.Fatalf("only the first 4 part URLs are issued up front, got %d", len(res.Parts))
	}
	for i, p := range res.Parts {
		if p.Number != i+1 || p.SizeBytes != 64*mib || !strings.Contains(p.URL, "ttl=900") || !strings.Contains(p.URL, fmt.Sprintf("len=%d", 64*mib)) {
			t.Fatalf("part %d: %+v", i+1, p)
		}
	}
	if !res.URLsExpireAt.Equal(e.clock.Add(15 * time.Minute)) {
		t.Fatalf("urls expire at %v", res.URLsExpireAt)
	}
	if e.objs.openUploads() != 1 {
		t.Fatalf("expected one open multipart upload, got %d", e.objs.openUploads())
	}
	got, err := e.store.Get(context.Background(), u.ID)
	if err != nil || got.S3UploadID == "" || got.S3UploadID != u.S3UploadID {
		t.Fatalf("row not stored with its storage upload id: %+v, %v", got, err)
	}
}

func TestPartPlanning(t *testing.T) {
	cases := []struct {
		size  int64
		parts int
		last  int64
	}{
		{1, 1, 1},
		{64 * mib, 1, 64 * mib},
		{64*mib + 1, 2, 1},
		{200 * mib, 4, 8 * mib},
		{8*gib - 1, 128, 64*mib - 1},
		{8 * gib, 128, 64 * mib},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.size), func(t *testing.T) {
			e := newEnv(t, func(c *Config) { c.DailyBytes = 1 << 50 })
			u := e.create(c.size).Upload
			if u.PartCount != c.parts || u.PartSize(u.PartCount) != c.last {
				t.Fatalf("parts=%d last=%d, want %d / %d", u.PartCount, u.PartSize(u.PartCount), c.parts, c.last)
			}
			var sum int64
			for n := 1; n <= u.PartCount; n++ {
				sz := u.PartSize(n)
				if sz <= 0 || (n < u.PartCount && sz != u.PartSizeBytes) {
					t.Fatalf("part %d has size %d", n, sz)
				}
				if n < u.PartCount && u.PartCount > 1 && sz < 5*mib {
					t.Fatalf("part %d is below the 5 MiB S3 minimum", n)
				}
				sum += sz
			}
			if sum != c.size {
				t.Fatalf("parts add up to %d, want %d", sum, c.size)
			}
		})
	}
	u := Upload{SizeBytes: 100, PartSizeBytes: 40, PartCount: 3}
	if u.PartSize(0) != 0 || u.PartSize(4) != 0 || u.PartSize(3) != 20 {
		t.Fatalf("out-of-range part sizes must be 0")
	}
}

func TestCreate_Validation(t *testing.T) {
	long := strings.Repeat("a", 252) + ".mp4"
	cases := []struct {
		name  string
		mod   func(*CreateInput)
		field string
	}{
		{"title id not a uuid", func(r *CreateInput) { r.TitleID = "nope" }, "title_id"},
		{"empty filename", func(r *CreateInput) { r.Filename = "  " }, "filename"},
		{"filename too long", func(r *CreateInput) { r.Filename = long }, "filename"},
		{"path in filename", func(r *CreateInput) { r.Filename = "../x.mp4" }, "filename"},
		{"backslash in filename", func(r *CreateInput) { r.Filename = `a\b.mp4` }, "filename"},
		{"control character", func(r *CreateInput) { r.Filename = "a\x00b.mp4" }, "filename"},
		{"unsupported extension", func(r *CreateInput) { r.Filename = "film.avi" }, "filename"},
		{"no extension", func(r *CreateInput) { r.Filename = "film" }, "filename"},
		{"zero size", func(r *CreateInput) { r.SizeBytes = 0 }, "size_bytes"},
		{"negative size", func(r *CreateInput) { r.SizeBytes = -5 }, "size_bytes"},
		{"over the limit", func(r *CreateInput) { r.SizeBytes = 8*gib + 1 }, "size_bytes"},
		{"not a video type", func(r *CreateInput) { r.ContentType = "image/png" }, "content_type"},
		{"type contradicts extension", func(r *CreateInput) { r.ContentType = "video/webm" }, "content_type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			in := e.req(10 * mib)
			c.mod(&in)
			_, err := e.svc.Create(context.Background(), e.creator, in)
			p := problemOf(t, err, http.StatusUnprocessableEntity, "validation_failed")
			if p.Fields[c.field] == "" {
				t.Fatalf("expected an error on %q, got %v", c.field, p.Fields)
			}
			if e.objs.openUploads() != 0 || len(e.store.uploads) != 0 {
				t.Fatal("a rejected request must not start anything")
			}
		})
	}
}

func TestCreate_AcceptsEquivalentInputs(t *testing.T) {
	cases := []struct {
		filename, ctype, container, stored string
	}{
		{"film.MP4", "video/mp4", "mp4", "video/mp4"},
		{"film.mp4", "", "mp4", "video/mp4"},
		{"film.mp4", "application/octet-stream", "mp4", "video/mp4"},
		{"film.mp4", "video/mp4; codecs=avc1", "mp4", "video/mp4"},
		{"film.mp4", "video/x-m4v", "mp4", "video/mp4"},
		{"film.mov", "video/quicktime", "mov", "video/quicktime"},
		{"film.mp4", "video/quicktime", "mp4", "video/mp4"}, // same ISO family: browsers disagree on .mov/.mp4
		{"film.mkv", "", "mkv", "video/x-matroska"},
		{"film.mkv", "video/x-matroska", "mkv", "video/x-matroska"},
		{"film.webm", "video/webm", "webm", "video/webm"},
		{"  spaced name.webm  ", "video/webm", "webm", "video/webm"},
	}
	for _, c := range cases {
		t.Run(c.filename+"|"+c.ctype, func(t *testing.T) {
			e := newEnv(t)
			in := e.req(10 * mib)
			in.Filename, in.ContentType = c.filename, c.ctype
			res, err := e.svc.Create(context.Background(), e.creator, in)
			if err != nil {
				t.Fatal(err)
			}
			if res.Upload.Container != c.container || res.Upload.ContentType != c.stored ||
				!strings.HasSuffix(res.Upload.ObjectKey, "/source."+c.container) {
				t.Fatalf("got %+v", res.Upload)
			}
		})
	}
}

func TestCreate_Authorization(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addTitle(uuidN(1), otherID, "movie", false)
	e.addTitle(uuidN(2), creatorID, "series", false)
	e.addTitle(uuidN(3), creatorID, "movie", true)

	cases := []struct {
		name   string
		who    auth.Principal
		title  string
		status int
		code   string
	}{
		{"someone else's title looks like a missing one", e.creator, uuidN(1), 404, "title_not_found"},
		{"unknown title", e.creator, uuidN(99), 404, "title_not_found"},
		{"series are not supported yet", e.creator, uuidN(2), 422, "validation_failed"},
		{"title that already has a video", e.creator, uuidN(3), 409, "video_exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := e.req(10 * mib)
			in.TitleID = c.title
			_, err := e.svc.Create(ctx, c.who, in)
			wantProblem(t, err, c.status, c.code)
		})
	}
	t.Run("an admin may upload for any title", func(t *testing.T) {
		in := e.req(10 * mib)
		in.TitleID = uuidN(1)
		res, err := e.svc.Create(ctx, e.admin, in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Upload.CreatorID != adminID {
			t.Fatalf("the upload belongs to who started it: %+v", res.Upload)
		}
	})
	if e.objs.openUploads() != 1 {
		t.Fatalf("refused requests opened storage uploads: %d", e.objs.openUploads())
	}
}

func TestCreate_Limits(t *testing.T) {
	ctx := context.Background()
	t.Run("unfinished uploads per creator", func(t *testing.T) {
		e := newEnv(t)
		for i := 1; i <= 3; i++ {
			e.addTitle(uuidN(i), creatorID, "movie", false)
			in := e.req(10 * mib)
			in.TitleID = uuidN(i)
			if _, err := e.svc.Create(ctx, e.creator, in); err != nil {
				t.Fatalf("upload %d: %v", i, err)
			}
		}
		e.addTitle(uuidN(4), creatorID, "movie", false)
		in := e.req(10 * mib)
		in.TitleID = uuidN(4)
		_, err := e.svc.Create(ctx, e.creator, in)
		wantProblem(t, err, http.StatusTooManyRequests, "too_many_uploads")
		if e.objs.openUploads() != 3 {
			t.Fatalf("the refused request opened a storage upload")
		}
		// cancelling one frees a slot
		var first Upload
		for _, u := range e.store.uploads {
			first = u
			break
		}
		if err := e.svc.Abort(ctx, e.creator, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Create(ctx, e.creator, in); err != nil {
			t.Fatalf("after cancelling: %v", err)
		}
	})
	t.Run("another creator is not affected", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.MaxActive = 1 })
		e.create(10 * mib)
		e.addTitle(uuidN(1), otherID, "movie", false)
		in := e.req(10 * mib)
		in.TitleID = uuidN(1)
		if _, err := e.svc.Create(ctx, e.other, in); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("daily byte quota, rolling 24 hours, ignoring cancelled uploads", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.DailyBytes = 9 * gib; c.MaxActive = 10 })
		e.addTitle(uuidN(1), creatorID, "movie", false)
		e.addTitle(uuidN(2), creatorID, "movie", false)
		first := e.create(6 * gib).Upload
		in := e.req(5 * gib)
		in.TitleID = uuidN(1)
		_, err := e.svc.Create(ctx, e.creator, in)
		wantProblem(t, err, http.StatusTooManyRequests, "daily_quota_exceeded")
		if err := e.svc.Abort(ctx, e.creator, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Create(ctx, e.creator, in); err != nil {
			t.Fatalf("a cancelled upload must not count against the quota: %v", err)
		}
		in2 := e.req(5 * gib)
		in2.TitleID = uuidN(2)
		_, err = e.svc.Create(ctx, e.creator, in2)
		wantProblem(t, err, http.StatusTooManyRequests, "daily_quota_exceeded")
		e.clock = e.clock.Add(24*time.Hour + time.Minute)
		if _, err := e.svc.Create(ctx, e.creator, in2); err != nil {
			t.Fatalf("after 24 hours the quota window has moved on: %v", err)
		}
	})
}

func TestCreate_OneUnfinishedUploadPerTitle(t *testing.T) {
	e := newEnv(t)
	e.create(10 * mib)
	_, err := e.svc.Create(context.Background(), e.creator, e.req(10*mib))
	wantProblem(t, err, http.StatusConflict, "upload_in_progress")
	if e.objs.openUploads() != 1 {
		t.Fatalf("the losing request must abort the storage upload it opened, %d open", e.objs.openUploads())
	}
}

type createFails struct {
	Store
	err error
}

func (c createFails) Create(context.Context, Upload) error { return c.err }

func TestCreate_DatabaseFailureAbortsTheStorageUpload(t *testing.T) {
	e := newEnv(t)
	e.svc.store = createFails{Store: e.store, err: errors.New("db down")}
	if _, err := e.svc.Create(context.Background(), e.creator, e.req(10*mib)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
	if e.objs.openUploads() != 0 {
		t.Fatal("a storage upload was left behind")
	}
}

func TestCreate_StorageDown(t *testing.T) {
	e := newEnv(t)
	e.objs.failNext["CreateMultipartUpload"] = errors.New("connection refused")
	_, err := e.svc.Create(context.Background(), e.creator, e.req(10*mib))
	var se *storageError
	if !errors.As(err, &se) {
		t.Fatalf("a storage failure must surface as a storageError (502), got %T: %v", err, err)
	}
	if len(e.store.uploads) != 0 {
		t.Fatal("no row may be written when storage failed")
	}
}

// ---------------------------------------------------------------- PartURLs

func TestPartURLs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.create(200 * mib).Upload // 4 parts, last 8 MiB

	res, err := e.svc.PartURLs(ctx, e.creator, u.ID, []int{4, 2, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Parts) != 3 || res.Parts[0].Number != 2 || res.Parts[1].Number != 3 || res.Parts[2].Number != 4 {
		t.Fatalf("parts must be unique and ordered: %+v", res.Parts)
	}
	if res.Parts[2].SizeBytes != 8*mib || res.Parts[0].SizeBytes != 64*mib {
		t.Fatalf("each URL is pinned to its own part's size: %+v", res.Parts)
	}
	if !res.URLsExpireAt.Equal(e.clock.Add(15 * time.Minute)) {
		t.Fatalf("expiry %v", res.URLsExpireAt)
	}

	bad := map[string][]int{
		"none":             {},
		"too many":         make([]int, 21),
		"zero":             {0},
		"negative":         {-1},
		"past the last":    {5},
		"one bad among ok": {1, 2, 9},
	}
	for name, nums := range bad {
		if name == "too many" {
			for i := range nums {
				nums[i] = 1
			}
		}
		_, err := e.svc.PartURLs(ctx, e.creator, u.ID, nums)
		p := problemOf(t, err, http.StatusUnprocessableEntity, "validation_failed")
		if p.Fields["part_numbers"] == "" {
			t.Errorf("%s: no field error: %+v", name, p)
		}
	}

	_, err = e.svc.PartURLs(ctx, e.other, u.ID, []int{1})
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")
	_, err = e.svc.PartURLs(ctx, e.creator, "not-a-uuid", []int{1})
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")
	if _, err := e.svc.PartURLs(ctx, e.admin, u.ID, []int{1}); err != nil {
		t.Fatalf("an admin may fetch part URLs: %v", err)
	}

	e.clock = e.clock.Add(25 * time.Hour)
	_, err = e.svc.PartURLs(ctx, e.creator, u.ID, []int{1})
	wantProblem(t, err, http.StatusConflict, "upload_expired")
}

func TestPartURLs_NotWhenFinished(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.readyUpload(10 * mib)
	if _, err := e.svc.Complete(ctx, e.creator, u.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.PartURLs(ctx, e.creator, u.ID, []int{1})
	wantProblem(t, err, http.StatusConflict, "upload_not_open")
}

// ---------------------------------------------------------------- Get

func TestGet(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.create(200 * mib).Upload
	e.objs.putPart(u.S3UploadID, 1, 64*mib)
	e.objs.putPart(u.S3UploadID, 3, 64*mib)

	d, err := e.svc.Get(ctx, e.creator, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Upload.Status != StatusInitiated || d.Job != nil || len(d.StoredParts) != 2 || d.StoredParts[0].Number != 1 || d.StoredParts[1].Number != 3 {
		t.Fatalf("an unfinished upload reports the parts already stored so a client can resume: %+v", d)
	}
	if _, err := e.svc.Get(ctx, e.admin, u.ID); err != nil {
		t.Fatalf("admin: %v", err)
	}
	for _, id := range []string{"garbage", uuidN(404)} {
		_, err = e.svc.Get(ctx, e.creator, id)
		wantProblem(t, err, http.StatusNotFound, "upload_not_found")
	}
	_, err = e.svc.Get(ctx, e.other, u.ID)
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")

	// the storage upload vanished (assembled elsewhere, or swept): not an error, just no parts
	delete(e.objs.uploads, u.S3UploadID)
	d, err = e.svc.Get(ctx, e.creator, u.ID)
	if err != nil || len(d.StoredParts) != 0 {
		t.Fatalf("got %+v, %v", d, err)
	}

	e.objs.failNext["ListParts"] = errors.New("boom")
	var se *storageError
	if _, err = e.svc.Get(ctx, e.creator, u.ID); err != nil && !errors.As(err, &se) {
		t.Logf("storage failures while listing parts surface as: %T", err)
	}
}

// ---------------------------------------------------------------- Complete

func TestComplete_Happy(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.readyUpload(200 * mib)

	d, err := e.svc.Complete(ctx, e.creator, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Upload.Status != StatusUploaded || d.Upload.S3UploadID != "" || d.Upload.CompletedAt == nil || d.Job == nil || d.Job.State != "queued" {
		t.Fatalf("unexpected result: %+v", d)
	}
	if !e.objs.hasObject("media-raw", u.ObjectKey) {
		t.Fatal("the assembled object is missing")
	}
	if e.store.policy[u.ID].MaxDurationSeconds != 4*3600 || e.store.policy[u.ID].Version != 1 || len(e.store.policy[u.ID].VideoCodecs) == 0 {
		t.Fatalf("the job must carry the validation policy: %+v", e.store.policy[u.ID])
	}
	stored, _ := e.store.Get(ctx, u.ID)
	if stored.Status != StatusUploaded {
		t.Fatalf("row status %s", stored.Status)
	}

	// calling again is harmless and does not queue a second job
	d2, err := e.svc.Complete(ctx, e.creator, u.ID)
	if err != nil || d2.Job == nil || d2.Job.ID != d.Job.ID || e.store.jobSeq != 1 {
		t.Fatalf("not idempotent: %+v, %v, jobs=%d", d2, err, e.store.jobSeq)
	}
	if e.objs.called("CompleteMultipartUpload") != 1 {
		t.Fatalf("storage was asked to assemble %d times", e.objs.called("CompleteMultipartUpload"))
	}
}

func TestComplete_SinglePartFile(t *testing.T) {
	e := newEnv(t)
	u := e.readyUpload(3 * mib) // below the 5 MiB part minimum: one part holds everything
	if u.PartCount != 1 || u.PartSizeBytes != 3*mib {
		t.Fatalf("%+v", u)
	}
	if _, err := e.svc.Complete(context.Background(), e.creator, u.ID); err != nil {
		t.Fatal(err)
	}
}

func TestComplete_IncompleteThenResume(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.create(200 * mib).Upload // 4 parts
	e.objs.putPart(u.S3UploadID, 1, 64*mib)
	e.objs.putPart(u.S3UploadID, 3, 64*mib)

	_, err := e.svc.Complete(ctx, e.creator, u.ID)
	p := problemOf(t, err, http.StatusUnprocessableEntity, "upload_incomplete")
	if !strings.Contains(p.Message, "2, 4") || !strings.Contains(p.Message, "of 4") {
		t.Fatalf("the message must name the missing parts: %q", p.Message)
	}
	if e.objs.called("CompleteMultipartUpload") != 0 {
		t.Fatal("an incomplete upload must not be assembled")
	}
	if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusInitiated {
		t.Fatalf("status must stay initiated, got %s", got.Status)
	}

	e.objs.putPart(u.S3UploadID, 2, 64*mib)
	e.objs.putPart(u.S3UploadID, 4, 8*mib)
	if _, err := e.svc.Complete(ctx, e.creator, u.ID); err != nil {
		t.Fatalf("after sending the missing parts: %v", err)
	}
}

func TestComplete_WrongPartSizes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.create(200 * mib).Upload
	e.objs.putAllParts(u)
	e.objs.putPart(u.S3UploadID, 2, 10*mib) // too small
	_, err := e.svc.Complete(ctx, e.creator, u.ID)
	p := problemOf(t, err, http.StatusUnprocessableEntity, "upload_incomplete")
	if !strings.Contains(p.Message, "wrong size: 2") {
		t.Fatalf("message %q", p.Message)
	}
	e.objs.putPart(u.S3UploadID, 2, 64*mib)
	e.objs.putPart(u.S3UploadID, 4, 64*mib) // last part too big
	_, err = e.svc.Complete(ctx, e.creator, u.ID)
	p = problemOf(t, err, http.StatusUnprocessableEntity, "upload_incomplete")
	if !strings.Contains(p.Message, "wrong size: 4") {
		t.Fatalf("message %q", p.Message)
	}
}

func TestComplete_ExtraParts(t *testing.T) {
	e := newEnv(t)
	u := e.readyUpload(200 * mib)
	e.objs.putPart(u.S3UploadID, 5, 64*mib)
	_, err := e.svc.Complete(context.Background(), e.creator, u.ID)
	p := problemOf(t, err, http.StatusUnprocessableEntity, "upload_incomplete")
	if !strings.Contains(p.Message, "extra parts") {
		t.Fatalf("message %q", p.Message)
	}
}

func TestComplete_RejectsBadFiles(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		head     []byte
		code     string
	}{
		{"an executable renamed to .mp4", "film.mp4", []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff\x00\x00"), RejectUnsupportedContainer},
		{"plain text", "film.mkv", []byte("this is not a video at all, sorry!"), RejectUnsupportedContainer},
		{"too short to be a video", "film.mp4", []byte{0, 0, 0}, RejectUnsupportedContainer},
		{"matroska named .mp4", "film.mp4", ebmlHead, RejectContainerMismatch},
		{"mp4 named .mkv", "film.mkv", isoHead, RejectContainerMismatch},
		{"mp4 named .webm", "film.webm", isoHead, RejectContainerMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			in := e.req(10 * mib)
			in.Filename, in.ContentType = c.filename, ""
			u := mustCreate(t, e, in)
			e.objs.putAllParts(u)
			e.objs.setHead(u.S3UploadID, c.head)

			_, err := e.svc.Complete(ctx, e.creator, u.ID)
			wantProblem(t, err, http.StatusUnprocessableEntity, c.code)
			got, _ := e.store.Get(ctx, u.ID)
			if got.Status != StatusRejected || got.RejectCode != c.code || got.RejectReason == "" {
				t.Fatalf("row: %+v", got)
			}
			if e.objs.hasObject("media-raw", u.ObjectKey) {
				t.Fatal("a rejected file must be deleted from storage")
			}
			if _, ok := e.store.jobs[u.ID]; ok {
				t.Fatal("no job may be queued for a rejected file")
			}
			// the creator can see why, and the slot is free again
			d, err := e.svc.Get(ctx, e.creator, u.ID)
			if err != nil || d.Upload.RejectCode != c.code {
				t.Fatalf("GET after rejection: %+v, %v", d, err)
			}
			_, err = e.svc.Complete(ctx, e.creator, u.ID)
			wantProblem(t, err, http.StatusConflict, "upload_rejected")
			if n, _ := e.store.CountActive(ctx, creatorID); n != 0 {
				t.Fatalf("a rejected upload still counts as active")
			}
		})
	}
}

func mustCreate(t *testing.T, e *env, in CreateInput) Upload {
	t.Helper()
	res, err := e.svc.Create(context.Background(), e.creator, in)
	if err != nil {
		t.Fatal(err)
	}
	return res.Upload
}

func TestComplete_AcceptsEachContainer(t *testing.T) {
	cases := []struct {
		filename string
		head     []byte
	}{
		{"a.mp4", isoHead}, {"a.mov", isoHead}, {"a.mkv", ebmlHead}, {"a.webm", ebmlHead},
		{"a.mp4", append([]byte{0, 0, 0, 8}, []byte("moov\x00\x00\x00\x00")...)},
		{"a.mp4", append([]byte{0, 0, 0, 8}, []byte("free\x00\x00\x00\x00")...)},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i, c.filename), func(t *testing.T) {
			e := newEnv(t)
			in := e.req(10 * mib)
			in.Filename, in.ContentType = c.filename, ""
			u := mustCreate(t, e, in)
			e.objs.putAllParts(u)
			e.objs.setHead(u.S3UploadID, c.head)
			if _, err := e.svc.Complete(context.Background(), e.creator, u.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestComplete_SizeMismatch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.readyUpload(10 * mib)
	e.objs.headSize = 10*mib + 7 // storage reports a different size than was declared
	_, err := e.svc.Complete(ctx, e.creator, u.ID)
	wantProblem(t, err, http.StatusUnprocessableEntity, RejectSizeMismatch)
	if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusRejected || got.RejectCode != RejectSizeMismatch {
		t.Fatalf("%+v", got)
	}
}

func TestComplete_Scanner(t *testing.T) {
	ctx := context.Background()
	t.Run("a threat rejects and deletes the file", func(t *testing.T) {
		e := newEnv(t)
		e.scanner.threat = "Eicar-Test-Signature"
		u := e.readyUpload(10 * mib)
		_, err := e.svc.Complete(ctx, e.creator, u.ID)
		p := problemOf(t, err, http.StatusUnprocessableEntity, RejectMalware)
		if strings.Contains(p.Message, "Eicar") {
			t.Fatal("the threat name must not be shown to the creator")
		}
		if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusRejected || e.objs.hasObject("media-raw", u.ObjectKey) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a scanner outage fails closed and the upload can be retried", func(t *testing.T) {
		e := newEnv(t)
		e.scanner.err = errors.New("clamd unreachable")
		u := e.readyUpload(10 * mib)
		_, err := e.svc.Complete(ctx, e.creator, u.ID)
		wantProblem(t, err, http.StatusServiceUnavailable, "scan_unavailable")
		if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusInitiated {
			t.Fatalf("status must stay initiated so the creator can retry, got %s", got.Status)
		}
		if _, ok := e.store.jobs[u.ID]; ok {
			t.Fatal("no job may be queued before the scan passes")
		}
		e.scanner.err = nil
		d, err := e.svc.Complete(ctx, e.creator, u.ID) // the storage upload is already assembled: retry must cope
		if err != nil || d.Upload.Status != StatusUploaded || d.Job == nil {
			t.Fatalf("retry: %+v, %v", d, err)
		}
	})
}

func TestComplete_StorageFailures(t *testing.T) {
	for _, method := range []string{"ListParts", "CompleteMultipartUpload", "Head", "GetRange"} {
		t.Run(method, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			u := e.readyUpload(10 * mib)
			e.objs.failNext[method] = errors.New("storage exploded")
			_, err := e.svc.Complete(ctx, e.creator, u.ID)
			var se *storageError
			if !errors.As(err, &se) {
				t.Fatalf("want a storageError, got %T: %v", err, err)
			}
			if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusInitiated {
				t.Fatalf("a storage hiccup must not change the upload, status %s", got.Status)
			}
			// and the next attempt works
			if _, err := e.svc.Complete(ctx, e.creator, u.ID); err != nil {
				t.Fatalf("retry after a transient failure: %v", err)
			}
		})
	}
}

func TestComplete_TheStoredUploadIsGone(t *testing.T) {
	e := newEnv(t)
	u := e.create(10 * mib).Upload
	delete(e.objs.uploads, u.S3UploadID) // swept or lost, and no assembled object exists
	_, err := e.svc.Complete(context.Background(), e.creator, u.ID)
	wantProblem(t, err, http.StatusConflict, "upload_incomplete")
}

func TestComplete_Guards(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.readyUpload(10 * mib)
	_, err := e.svc.Complete(ctx, e.other, u.ID)
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")
	_, err = e.svc.Complete(ctx, e.creator, "garbage")
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")

	e.clock = e.clock.Add(25 * time.Hour)
	_, err = e.svc.Complete(ctx, e.creator, u.ID)
	wantProblem(t, err, http.StatusConflict, "upload_expired")

	e2 := newEnv(t)
	u2 := e2.create(10 * mib).Upload
	if err := e2.svc.Abort(ctx, e2.creator, u2.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e2.svc.Complete(ctx, e2.creator, u2.ID)
	wantProblem(t, err, http.StatusConflict, "upload_closed")
}

func TestComplete_ConcurrentCallsQueueOneJob(t *testing.T) {
	e := newEnv(t)
	u := e.readyUpload(200 * mib)
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := e.svc.Complete(context.Background(), e.creator, u.ID)
			errs[i] = err
			if err == nil && d.Job != nil {
				ids[i] = d.Job.ID
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("call %d returned job %q, call 0 returned %q", i, ids[i], ids[0])
		}
	}
	if e.store.jobSeq != 1 {
		t.Fatalf("%d jobs were queued", e.store.jobSeq)
	}
}

// ---------------------------------------------------------------- Abort

func TestAbort(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.create(10 * mib).Upload
	e.objs.putAllParts(u)

	_, err := e.svc.Get(ctx, e.other, u.ID)
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")
	wantProblem(t, e.svc.Abort(ctx, e.other, u.ID), http.StatusNotFound, "upload_not_found")

	if err := e.svc.Abort(ctx, e.creator, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusAborted || got.S3UploadID != "" {
		t.Fatalf("%+v", got)
	}
	if e.objs.openUploads() != 0 {
		t.Fatal("the storage upload and its parts must be discarded")
	}
	if err := e.svc.Abort(ctx, e.creator, u.ID); err != nil {
		t.Fatalf("cancelling twice must be fine: %v", err)
	}
	// the title is free for a new upload
	if _, err := e.svc.Create(ctx, e.creator, e.req(10*mib)); err != nil {
		t.Fatalf("after cancelling: %v", err)
	}
}

func TestAbort_AfterCompletionIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.readyUpload(10 * mib)
	if _, err := e.svc.Complete(ctx, e.creator, u.ID); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, e.svc.Abort(ctx, e.creator, u.ID), http.StatusConflict, "upload_not_cancellable")
}

func TestAbort_StorageFailureLeavesTheUploadOpen(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u := e.create(10 * mib).Upload
	e.objs.failNext["AbortMultipartUpload"] = errors.New("boom")
	var se *storageError
	if err := e.svc.Abort(ctx, e.creator, u.ID); !errors.As(err, &se) {
		t.Fatalf("got %v", err)
	}
	if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusInitiated {
		t.Fatalf("status %s", got.Status)
	}
	if err := e.svc.Abort(ctx, e.creator, u.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// ---------------------------------------------------------------- small pieces

func TestSniffFamily(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"mp4", isoHead, "iso"},
		{"matroska", ebmlHead, "ebml"},
		{"empty", nil, ""},
		{"too short", []byte{0x1A, 0x45}, ""},
		{"ebml magic only", []byte{0x1A, 0x45, 0xDF, 0xA3}, "ebml"},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), ""},
		{"avi", []byte("RIFF\x00\x00\x00\x00AVI LIST"), ""},
		{"zip", []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00\x00\x00"), ""},
	}
	for _, c := range cases {
		if got := sniffFamily(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestContainerFamily(t *testing.T) {
	for ext, want := range map[string]string{"mp4": "iso", "mov": "iso", "mkv": "ebml", "webm": "ebml", "avi": ""} {
		if got := containerFamily(ext); got != want {
			t.Errorf("%s: %q want %q", ext, got, want)
		}
	}
	for ext := range containerTypes {
		if containerFamily(ext) == "" {
			t.Errorf("%s has a content type but no family", ext)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	if humanBytes(8*gib) != "8 GiB" || humanBytes(gib+gib/2) != "1.5 GiB" {
		t.Fatalf("%q %q", humanBytes(8*gib), humanBytes(gib+gib/2))
	}
}

func TestNewUUID(t *testing.T) {
	a, err := newUUID()
	b, _ := newUUID()
	if err != nil || a == b || !uuidRE.MatchString(a) || a[14] != '4' {
		t.Fatalf("%q %q %v", a, b, err)
	}
}

func TestSatisfiesInterfaces(t *testing.T) {
	var _ ObjectStore = (*s3.Client)(nil)
	var _ Store = (*PGStore)(nil)
	var _ Scanner = NoopScanner{}
}
