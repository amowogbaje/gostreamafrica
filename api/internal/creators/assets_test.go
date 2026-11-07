package creators

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const assetA = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

// deadLettered returns an upload whose job ran out of attempts, as the worker leaves it.
func (e *env) deadLettered() Upload {
	e.t.Helper()
	u := e.readyUpload(10 * mib)
	if _, err := e.svc.Complete(context.Background(), e.creator, u.ID); err != nil {
		e.t.Fatal(err)
	}
	e.store.mu.Lock()
	up := e.store.uploads[u.ID]
	up.Status, up.RejectCode, up.RejectReason = StatusRejected, "processing_failed", "We could not process this video"
	e.store.uploads[u.ID] = up
	j := e.store.jobs[u.ID]
	j.State, j.Attempts, j.Error = "failed", 3, "ffmpeg: signal: killed"
	e.store.jobs[u.ID] = j
	e.store.mu.Unlock()
	return up
}

func TestRetry(t *testing.T) {
	ctx := context.Background()
	t.Run("a dead-lettered upload gets a fresh job", func(t *testing.T) {
		e := newEnv(t)
		u := e.deadLettered()
		d, err := e.svc.Retry(ctx, e.creator, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Upload.Status != StatusUploaded || d.Upload.RejectCode != "" || d.Job == nil || d.Job.State != "queued" || d.Job.Attempts != 0 {
			t.Fatalf("%+v %+v", d.Upload, d.Job)
		}
		// asking again while it is queued changes nothing
		if _, err := e.svc.Retry(ctx, e.creator, u.ID); err != nil {
			t.Fatalf("a second retry is harmless: %v", err)
		}
	})
	t.Run("an admin may retry someone's upload", func(t *testing.T) {
		e := newEnv(t)
		u := e.deadLettered()
		if _, err := e.svc.Retry(ctx, e.admin, u.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("someone else cannot see it", func(t *testing.T) {
		e := newEnv(t)
		u := e.deadLettered()
		_, err := e.svc.Retry(ctx, e.other, u.ID)
		wantProblem(t, err, http.StatusNotFound, "upload_not_found")
	})
	t.Run("only processing failures are retryable", func(t *testing.T) {
		for name, mod := range map[string]func(*Upload){
			"refused file": func(u *Upload) { u.RejectCode = "unsupported_codec" },
			"ready":        func(u *Upload) { u.Status, u.RejectCode = StatusReady, "" },
			"initiated":    func(u *Upload) { u.Status, u.RejectCode = StatusInitiated, "" },
			"aborted":      func(u *Upload) { u.Status, u.RejectCode = StatusAborted, "" },
		} {
			e := newEnv(t)
			u := e.deadLettered()
			e.store.mu.Lock()
			up := e.store.uploads[u.ID]
			mod(&up)
			e.store.uploads[u.ID] = up
			e.store.mu.Unlock()
			_, err := e.svc.Retry(ctx, e.creator, u.ID)
			if name == "ready" || name == "refused file" || name == "initiated" || name == "aborted" {
				wantProblem(t, err, http.StatusConflict, "not_retryable")
			}
		}
	})
	t.Run("the original file must still exist", func(t *testing.T) {
		e := newEnv(t)
		u := e.deadLettered()
		delete(e.objs.objects, "media-raw/"+u.ObjectKey)
		_, err := e.svc.Retry(ctx, e.creator, u.ID)
		wantProblem(t, err, http.StatusConflict, "source_missing")
	})
	t.Run("storage trouble is a 502, not a lie", func(t *testing.T) {
		e := newEnv(t)
		u := e.deadLettered()
		e.objs.failNext["Head"] = errors.New("boom")
		_, err := e.svc.Retry(ctx, e.creator, u.ID)
		var se *storageError
		if !errors.As(err, &se) {
			t.Fatalf("%v", err)
		}
		if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusRejected {
			t.Fatalf("nothing may change: %s", got.Status)
		}
	})
	t.Run("another unfinished upload on the title blocks it", func(t *testing.T) {
		e := newEnv(t)
		u := e.deadLettered()
		e.create(10 * mib) // a new upload for the same title, started after the old one died
		_, err := e.svc.Retry(ctx, e.creator, u.ID)
		wantProblem(t, err, http.StatusConflict, "upload_in_progress")
	})
}

func TestSnapshotDoesNotTouchStorage(t *testing.T) {
	e := newEnv(t)
	u := e.create(10 * mib).Upload
	d, err := e.svc.Snapshot(context.Background(), e.creator, u.ID)
	if err != nil || d.Upload.ID != u.ID || d.StoredParts != nil {
		t.Fatalf("%+v %v", d, err)
	}
	if e.objs.called("ListParts") != 0 {
		t.Fatal("polling must not call storage")
	}
	_, err = e.svc.Snapshot(context.Background(), e.other, u.ID)
	wantProblem(t, err, http.StatusNotFound, "upload_not_found")
}

// readyAsset sets up a published title with a finished video, its upload and files in storage.
func (e *env) readyAsset() (Upload, AssetInfo) {
	e.t.Helper()
	u := e.readyUpload(10 * mib)
	if _, err := e.svc.Complete(context.Background(), e.creator, u.ID); err != nil {
		e.t.Fatal(err)
	}
	e.store.mu.Lock()
	up := e.store.uploads[u.ID]
	up.Status = StatusReady
	e.store.uploads[u.ID] = up
	e.store.mu.Unlock()
	a := AssetInfo{ID: assetA, TitleID: titleA, CreatorID: creatorID, Status: "ready", TitleStatus: "published"}
	e.store.assets[assetA] = a
	e.objs.objects["media-hls/"+assetA+"/master.m3u8"] = fakeObj{}
	e.objs.objects["media-hls/"+assetA+"/360p/seg_00000.m4s"] = fakeObj{}
	e.objs.objects["media-public/"+titleA+"/poster.jpg"] = fakeObj{}
	e.objs.objects["media-hls/ffffffff-ffff-4fff-8fff-ffffffffffff/master.m3u8"] = fakeObj{} // someone else's video
	return up, a
}

func TestUnpublishAsset(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, _ = e.readyAsset()
	a, err := e.svc.UnpublishAsset(ctx, e.creator, assetA)
	if err != nil || a.TitleStatus != "unpublished" || a.TitleID != titleA {
		t.Fatalf("%+v %v", a, err)
	}
	if !e.objs.hasObject("media-hls", assetA+"/master.m3u8") {
		t.Fatal("unpublishing keeps the files")
	}
	if a, err := e.svc.UnpublishAsset(ctx, e.creator, assetA); err != nil || a.TitleStatus != "unpublished" {
		t.Fatalf("repeating is fine: %+v %v", a, err)
	}
	if n := len(e.store.unpublished); n != 1 {
		t.Fatalf("the title is only changed when it was published, %d calls", n)
	}
	_, err = e.svc.UnpublishAsset(ctx, e.other, assetA)
	wantProblem(t, err, http.StatusNotFound, "asset_not_found")
	_, err = e.svc.UnpublishAsset(ctx, e.creator, "garbage")
	wantProblem(t, err, http.StatusNotFound, "asset_not_found")
	if _, err := e.svc.UnpublishAsset(ctx, e.admin, assetA); err != nil {
		t.Fatalf("admin: %v", err)
	}
}

func TestDeleteAsset(t *testing.T) {
	ctx := context.Background()
	t.Run("removes the video everywhere and unpublishes first", func(t *testing.T) {
		e := newEnv(t)
		up, _ := e.readyAsset()
		if err := e.svc.DeleteAsset(ctx, e.creator, assetA); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"media-hls/" + assetA + "/master.m3u8", "media-hls/" + assetA + "/360p/seg_00000.m4s", "media-public/" + titleA + "/poster.jpg", "media-raw/" + up.ObjectKey} {
			bucket, key, _ := strings.Cut(k, "/")
			if e.objs.hasObject(bucket, key) {
				t.Errorf("%s survived", k)
			}
		}
		if !e.objs.hasObject("media-hls", "ffffffff-ffff-4fff-8fff-ffffffffffff/master.m3u8") {
			t.Fatal("another video's files must be untouched")
		}
		if _, ok := e.store.assets[assetA]; ok {
			t.Fatal("asset row remains")
		}
		if got, _ := e.store.Get(ctx, up.ID); got.Status != StatusDeleted {
			t.Fatalf("the upload record is kept as 'deleted': %s", got.Status)
		}
		if len(e.store.unpublished) == 0 {
			t.Fatal("the title must be unpublished")
		}
		for _, pf := range e.objs.deleted {
			if strings.HasSuffix(pf, "/") && (pf == "media-hls/" || pf == "media-public/" || pf == "media-raw/") {
				t.Fatalf("an empty prefix would wipe a bucket: %v", e.objs.deleted)
			}
		}
		err := e.svc.DeleteAsset(ctx, e.creator, assetA)
		wantProblem(t, err, http.StatusNotFound, "asset_not_found")
		// the title is free for a new upload again
		if _, err := e.svc.Create(ctx, e.creator, e.req(10*mib)); err != nil {
			t.Fatalf("after deleting: %v", err)
		}
	})
	t.Run("not while it is being processed", func(t *testing.T) {
		e := newEnv(t)
		e.readyAsset()
		a := e.store.assets[assetA]
		a.Status = "processing"
		e.store.assets[assetA] = a
		wantProblem(t, e.svc.DeleteAsset(ctx, e.creator, assetA), http.StatusConflict, "asset_busy")
		if !e.objs.hasObject("media-hls", assetA+"/master.m3u8") {
			t.Fatal("nothing may be deleted")
		}
	})
	t.Run("not someone else's", func(t *testing.T) {
		e := newEnv(t)
		e.readyAsset()
		wantProblem(t, e.svc.DeleteAsset(ctx, e.other, assetA), http.StatusNotFound, "asset_not_found")
		if !e.objs.hasObject("media-hls", assetA+"/master.m3u8") {
			t.Fatal("nothing may be deleted")
		}
		if err := e.svc.DeleteAsset(ctx, e.admin, assetA); err != nil {
			t.Fatalf("an admin may: %v", err)
		}
	})
	t.Run("a storage failure keeps the database record so it can be retried", func(t *testing.T) {
		e := newEnv(t)
		e.readyAsset()
		e.objs.failNext["DeletePrefix"] = errors.New("storage down")
		var se *storageError
		if err := e.svc.DeleteAsset(ctx, e.creator, assetA); !errors.As(err, &se) {
			t.Fatalf("%v", err)
		}
		if _, ok := e.store.assets[assetA]; !ok {
			t.Fatal("the asset record must remain after a failed deletion")
		}
		if err := e.svc.DeleteAsset(ctx, e.creator, assetA); err != nil {
			t.Fatalf("the retry completes the job: %v", err)
		}
		if e.objs.hasObject("media-hls", assetA+"/master.m3u8") {
			t.Fatal("still there")
		}
	})
	t.Run("a failed asset can be cleaned up too", func(t *testing.T) {
		e := newEnv(t)
		e.readyAsset()
		a := e.store.assets[assetA]
		a.Status, a.TitleStatus = "failed", "draft"
		e.store.assets[assetA] = a
		if err := e.svc.DeleteAsset(ctx, e.creator, assetA); err != nil {
			t.Fatal(err)
		}
	})
}

// ---------------------------------------------------------------- HTTP

func TestHTTP_RetryAndAssets(t *testing.T) {
	e := newHTTP(t)
	u := e.deadLettered()
	if got := e.do("POST", "/v1/creator/uploads/"+u.ID+"/retry", nobody, nil); got.code != 401 {
		t.Fatalf("%d", got.code)
	}
	if got := e.do("POST", "/v1/creator/uploads/"+u.ID+"/retry", asViewer, nil); got.code != 403 {
		t.Fatalf("%d", got.code)
	}
	if got := e.do("POST", "/v1/creator/uploads/"+u.ID+"/retry", asOther, nil); got.code != 404 {
		t.Fatalf("%d", got.code)
	}
	got := e.do("POST", "/v1/creator/uploads/"+u.ID+"/retry", asCreator, nil)
	if got.code != 202 || !strings.Contains(string(got.body), `"status":"uploaded"`) || !strings.Contains(string(got.body), `"state":"queued"`) {
		t.Fatalf("%d %s", got.code, got.body)
	}
	e.addTitle(uuidN(5), creatorID, "movie", false)
	in := e.req(10 * mib)
	in.TitleID = uuidN(5)
	fresh := mustCreate(t, e.env, in)
	if got := e.do("POST", "/v1/creator/uploads/"+fresh.ID+"/retry", asCreator, nil); got.code != 409 || got.errCode(t) != "not_retryable" {
		t.Fatalf("an unfinished upload is not retryable: %d %s", got.code, got.body)
	}

	// assets
	e2 := newHTTP(t)
	e2.readyAsset()
	path := "/v1/creator/assets/" + assetA
	for _, r := range []struct{ method, path string }{{"POST", path + "/unpublish"}, {"DELETE", path}} {
		if got := e2.do(r.method, r.path, nobody, nil); got.code != 401 {
			t.Errorf("%s anonymous: %d", r.method, got.code)
		}
		if got := e2.do(r.method, r.path, asViewer, nil); got.code != 403 {
			t.Errorf("%s viewer: %d", r.method, got.code)
		}
		if got := e2.do(r.method, r.path, asOther, nil); got.code != 404 || got.errCode(t) != "asset_not_found" {
			t.Errorf("%s stranger: %d %s", r.method, got.code, got.body)
		}
	}
	got = e2.do("POST", path+"/unpublish", asCreator, nil)
	if got.code != 200 || !strings.Contains(string(got.body), `"title_status":"unpublished"`) || !strings.Contains(string(got.body), assetA) {
		t.Fatalf("%d %s", got.code, got.body)
	}
	got = e2.do("DELETE", path, asCreator, nil)
	if got.code != 204 || len(got.body) != 0 {
		t.Fatalf("%d %s", got.code, got.body)
	}
	if got := e2.do("DELETE", path, asCreator, nil); got.code != 404 {
		t.Fatalf("deleting twice: %d", got.code)
	}
}

func TestHTTP_JobErrorsAreFriendly(t *testing.T) {
	e := newHTTP(t)
	u := e.readyUpload(10 * mib)
	if _, err := e.svc.Complete(context.Background(), e.creator, u.ID); err != nil {
		t.Fatal(err)
	}
	set := func(state string, attempts int, errText string) string {
		e.store.mu.Lock()
		j := e.store.jobs[u.ID]
		j.State, j.Attempts, j.Error = state, attempts, errText
		e.store.jobs[u.ID] = j
		e.store.mu.Unlock()
		return string(e.do("GET", "/v1/creator/uploads/"+u.ID, asCreator, nil).body)
	}
	secret := "ffmpeg failed: exit status 1: /scratch/job-1/source.mp4: dial tcp 10.0.0.5:8333"
	body := set("queued", 1, secret)
	if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "ffmpeg") || !strings.Contains(body, retryingNote) {
		t.Fatalf("a retrying job shows a friendly note only: %s", body)
	}
	if body := set("queued", 0, ""); strings.Contains(body, `"error"`) {
		t.Fatalf("a fresh job has no error: %s", body)
	}
	if body := set("failed", 3, secret); strings.Contains(body, "10.0.0.5") || strings.Contains(body, `"error"`) {
		t.Fatalf("the internal failure never reaches the creator: %s", body)
	}
	if body := set("running", 2, secret); strings.Contains(body, "10.0.0.5") {
		t.Fatalf("%s", body)
	}
}

type sseEvent struct{ name, data string }

// readEvents reads Server-Sent Events from a stream until it ends.
func readEvents(t *testing.T, resp *http.Response, out chan<- sseEvent) {
	t.Helper()
	defer close(out)
	sc := bufio.NewScanner(resp.Body)
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if ev.name != "" {
				out <- ev
			}
			ev = sseEvent{}
		}
	}
}

func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the stream ended early")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

func TestHTTP_ProgressStream(t *testing.T) {
	e := newHTTP(t)
	handler := e.h
	srv := httptest.NewServer(handler)
	defer srv.Close()
	// speed the stream up: find the handler and shorten its poll interval
	mux := http.NewServeMux()
	h := NewHandler(e.svc, testAuth, &memLimiter{}, quietLogger())
	h.pollEvery = 10 * time.Millisecond
	h.Mount(mux)
	fast := httptest.NewServer(mux)
	defer fast.Close()

	u := e.readyUpload(10 * mib)
	if _, err := e.svc.Complete(context.Background(), e.creator, u.ID); err != nil {
		t.Fatal(err)
	}
	open := func(base, who, id string) *http.Response {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/v1/creator/uploads/"+id+"/events", nil)
		req.Header.Set("X-Test-User", who)
		req.Header.Set("X-Test-Role", "creator")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := open(fast.URL, otherID, u.ID); resp.StatusCode != 404 {
		t.Fatalf("a stranger cannot watch someone's upload: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := open(fast.URL, creatorID, u.ID)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		resp.Header.Get("X-Accel-Buffering") != "no" || !strings.Contains(resp.Header.Get("Cache-Control"), "no-cache") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	events := make(chan sseEvent, 16)
	go readEvents(t, resp, events)

	first := next(t, events)
	if first.name != "status" || !strings.Contains(first.data, `"status":"uploaded"`) || !strings.Contains(first.data, `"state":"queued"`) {
		t.Fatalf("%+v", first)
	}
	update := func(status Status, jobState, stage string, pct int) {
		e.store.mu.Lock()
		up := e.store.uploads[u.ID]
		up.Status = status
		e.store.uploads[u.ID] = up
		j := e.store.jobs[u.ID]
		j.State, j.Stage, j.Percent = jobState, stage, &pct
		e.store.jobs[u.ID] = j
		e.store.mu.Unlock()
	}
	update(StatusProcessing, "running", "transcode", 40)
	if ev := next(t, events); ev.name != "status" || !strings.Contains(ev.data, `"stage":"transcode"`) || !strings.Contains(ev.data, `"percent":40`) {
		t.Fatalf("%+v", ev)
	}
	// nothing changes, so nothing is sent; then the next change arrives
	time.Sleep(60 * time.Millisecond)
	update(StatusProcessing, "running", "transcode", 70)
	if ev := next(t, events); !strings.Contains(ev.data, `"percent":70`) {
		t.Fatalf("unchanged states must not be repeated: %+v", ev)
	}
	update(StatusReady, "succeeded", "done", 100)
	if ev := next(t, events); ev.name != "status" || !strings.Contains(ev.data, `"status":"ready"`) {
		t.Fatalf("%+v", ev)
	}
	if ev := next(t, events); ev.name != "end" || !strings.Contains(ev.data, "ready") {
		t.Fatalf("a final state ends the stream: %+v", ev)
	}
	if _, ok := <-events; ok {
		t.Fatal("the stream must close after the end event")
	}

	// an already finished upload: one status, then end, immediately
	resp2 := open(fast.URL, creatorID, u.ID)
	defer resp2.Body.Close()
	ev2 := make(chan sseEvent, 4)
	go readEvents(t, resp2, ev2)
	if a, b := next(t, ev2), next(t, ev2); a.name != "status" || b.name != "end" {
		t.Fatalf("%+v %+v", a, b)
	}

	// a client that goes away stops the stream: the handler returns, so the server can close
	e.store.mu.Lock()
	up := e.store.uploads[u.ID]
	up.Status = StatusProcessing
	e.store.uploads[u.ID] = up
	e.store.mu.Unlock()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fast.URL+"/v1/creator/uploads/"+u.ID+"/events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	req.Header.Set("X-Test-User", creatorID)
	req.Header.Set("X-Test-Role", "creator")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	ev3 := make(chan sseEvent, 4)
	go readEvents(t, resp3, ev3)
	next(t, ev3)
	cancel()
	done := make(chan struct{})
	go func() { fast.Close(); close(done) }() // Close waits for every handler to return
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream handler did not stop when the client disconnected")
	}
	_ = json.Valid
}
