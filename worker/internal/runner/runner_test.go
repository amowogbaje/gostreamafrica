package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"streamafrica/worker/internal/jobs"
	"streamafrica/worker/internal/pipeline"
	"streamafrica/worker/internal/transcode"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type event struct {
	kind  string
	jobID string
	extra any
}

type fakeQueue struct {
	mu        sync.Mutex
	pending   []jobs.Job
	events    []event
	hbErr     error
	finishErr error
	deadNext  bool
	claimErr  error
	reaps     atomic.Int32
	progress  []int
}

func (q *fakeQueue) add(j ...jobs.Job) {
	q.mu.Lock()
	q.pending = append(q.pending, j...)
	q.mu.Unlock()
}

func (q *fakeQueue) rec(kind, id string, extra any) {
	q.mu.Lock()
	q.events = append(q.events, event{kind, id, extra})
	q.mu.Unlock()
}

func (q *fakeQueue) count(kind string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, e := range q.events {
		if e.kind == kind {
			n++
		}
	}
	return n
}

func (q *fakeQueue) first(kind string) event {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.events {
		if e.kind == kind {
			return e
		}
	}
	return event{}
}

func (q *fakeQueue) Claim(context.Context) (*jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	if len(q.pending) == 0 {
		return nil, nil
	}
	j := q.pending[0]
	q.pending = q.pending[1:]
	return &j, nil
}

func (q *fakeQueue) Heartbeat(_ context.Context, id string) error {
	q.rec("heartbeat", id, nil)
	return q.hbErr
}

func (q *fakeQueue) Progress(_ context.Context, id, stage string, pct int) error {
	q.mu.Lock()
	q.progress = append(q.progress, pct)
	q.mu.Unlock()
	return nil
}

func (q *fakeQueue) Finish(_ context.Context, j jobs.Job, asset, manifest string, secs int) error {
	q.rec("finish", j.ID, [3]any{asset, manifest, secs})
	return q.finishErr
}

func (q *fakeQueue) Reject(_ context.Context, j jobs.Job, _ string, rj *transcode.Rejection) error {
	q.rec("reject", j.ID, rj.Code)
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, j jobs.Job, cause error, delay time.Duration, _ string) (bool, error) {
	q.rec("retry", j.ID, struct {
		cause error
		delay time.Duration
	}{cause, delay})
	return q.deadNext, nil
}

func (q *fakeQueue) Release(_ context.Context, j jobs.Job) error {
	q.rec("release", j.ID, nil)
	return nil
}

func (q *fakeQueue) Reap(context.Context) (int, error) { q.reaps.Add(1); return 0, nil }

type fakeProc struct {
	run func(ctx context.Context, j jobs.Job, p pipeline.Progress) (pipeline.Result, error)
}

func (f fakeProc) Run(ctx context.Context, j jobs.Job, p pipeline.Progress) (pipeline.Result, error) {
	return f.run(ctx, j, p)
}

func cfg() Config {
	return Config{Concurrency: 1, JobTimeout: 5 * time.Second, Lease: 300 * time.Millisecond, PollInterval: 5 * time.Millisecond,
		BackoffBase: 10 * time.Second, BackoffMax: time.Minute, ShutdownGrace: 200 * time.Millisecond}
}

// start runs the runner until the returned stop function is called, which waits for it to return.
func start(t *testing.T, r *Runner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func job(id string, attempt int) jobs.Job {
	return jobs.Job{ID: id, UploadID: "up-" + id, Attempts: attempt, MaxAttempts: 3}
}

func TestSuccess(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1))
	r := New(cfg(), q, fakeProc{func(ctx context.Context, j jobs.Job, p pipeline.Progress) (pipeline.Result, error) {
		p("download", 2)
		p("download", 2) // repeated values are not written twice
		p("transcode", 50)
		return pipeline.Result{AssetID: "asset1", ManifestKey: "asset1/master.m3u8", DurationSecs: 61, Rungs: []string{"360p"}}, nil
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "finish", func() bool { return q.count("finish") == 1 })
	if e := q.first("finish"); e.jobID != "j1" || e.extra != [3]any{"asset1", "asset1/master.m3u8", 61} {
		t.Fatalf("%+v", e)
	}
	q.mu.Lock()
	got := append([]int(nil), q.progress...)
	q.mu.Unlock()
	if len(got) != 2 || got[0] != 2 || got[1] != 50 {
		t.Fatalf("progress written: %v", got)
	}
	if q.count("retry")+q.count("reject")+q.count("release") != 0 {
		t.Fatal("a successful job records nothing else")
	}
	if q.reaps.Load() == 0 {
		t.Fatal("the loop reaps stale jobs")
	}
}

func TestRejectionIsFinal(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1))
	r := New(cfg(), q, fakeProc{func(context.Context, jobs.Job, pipeline.Progress) (pipeline.Result, error) {
		return pipeline.Result{AssetID: "a"}, &transcode.Rejection{Code: transcode.CodeTooLong, Reason: "too long"}
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "reject", func() bool { return q.count("reject") == 1 })
	if q.first("reject").extra != transcode.CodeTooLong || q.count("retry") != 0 || q.count("finish") != 0 {
		t.Fatalf("%+v", q.events)
	}
}

func TestFailureRetriesWithGrowingBackoff(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1), job("j2", 3))
	r := New(cfg(), q, fakeProc{func(context.Context, jobs.Job, pipeline.Progress) (pipeline.Result, error) {
		return pipeline.Result{}, errors.New("ffmpeg crashed")
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "two retries", func() bool { return q.count("retry") == 2 })
	delays := map[string]time.Duration{}
	q.mu.Lock()
	for _, e := range q.events {
		if e.kind == "retry" {
			delays[e.jobID] = e.extra.(struct {
				cause error
				delay time.Duration
			}).delay
		}
	}
	q.mu.Unlock()
	if delays["j1"] != 10*time.Second || delays["j2"] != 40*time.Second {
		t.Fatalf("the delay doubles with each attempt: %v", delays)
	}
}

func TestTimeoutIsRetried(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1))
	c := cfg()
	c.JobTimeout = 50 * time.Millisecond
	r := New(c, q, fakeProc{func(ctx context.Context, _ jobs.Job, _ pipeline.Progress) (pipeline.Result, error) {
		<-ctx.Done()
		return pipeline.Result{}, ctx.Err()
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "retry", func() bool { return q.count("retry") == 1 })
	cause := q.first("retry").extra.(struct {
		cause error
		delay time.Duration
	}).cause
	if !errors.Is(cause, errTimeout) {
		t.Fatalf("%v", cause)
	}
	if q.count("release") != 0 {
		t.Fatal("a timeout is a failure, not a release")
	}
}

func TestLeaseLostStopsWithoutWriting(t *testing.T) {
	q := &fakeQueue{hbErr: jobs.ErrLeaseLost}
	q.add(job("j1", 1))
	var cancelled atomic.Bool
	r := New(cfg(), q, fakeProc{func(ctx context.Context, _ jobs.Job, _ pipeline.Progress) (pipeline.Result, error) {
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return pipeline.Result{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return pipeline.Result{}, nil
		}
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "the work to be cancelled", func() bool { return cancelled.Load() })
	waitFor(t, "the job to be abandoned", func() bool { _, n := r.Stats(); return n == 1 })
	if q.count("finish")+q.count("retry")+q.count("reject")+q.count("release") != 0 {
		t.Fatalf("a worker that lost its lease must write nothing: %+v", q.events)
	}
}

func TestFinishLeaseLostIsTolerated(t *testing.T) {
	q := &fakeQueue{finishErr: jobs.ErrLeaseLost}
	q.add(job("j1", 1))
	r := New(cfg(), q, fakeProc{func(context.Context, jobs.Job, pipeline.Progress) (pipeline.Result, error) {
		return pipeline.Result{AssetID: "a"}, nil
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "finish attempt", func() bool { return q.count("finish") == 1 })
	waitFor(t, "loop to continue", func() bool { return q.reaps.Load() > 3 })
}

func TestHeartbeatKeepsTheLeaseAlive(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1))
	r := New(cfg(), q, fakeProc{func(ctx context.Context, _ jobs.Job, _ pipeline.Progress) (pipeline.Result, error) {
		time.Sleep(450 * time.Millisecond) // 4+ lease/3 intervals
		return pipeline.Result{AssetID: "a"}, nil
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "finish", func() bool { return q.count("finish") == 1 })
	if n := q.count("heartbeat"); n < 3 {
		t.Fatalf("only %d heartbeats during a 450 ms job with a 300 ms lease", n)
	}
}

func TestShutdown_FinishesJobsWithinGrace(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1))
	started := make(chan struct{})
	r := New(cfg(), q, fakeProc{func(ctx context.Context, _ jobs.Job, _ pipeline.Progress) (pipeline.Result, error) {
		close(started)
		time.Sleep(80 * time.Millisecond) // shorter than the 200 ms grace; ignores the stop signal
		return pipeline.Result{AssetID: "a"}, nil
	}}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	if q.count("finish") != 1 || q.count("release") != 0 {
		t.Fatalf("a job that completes within the grace period is finished normally: %+v", q.events)
	}
}

func TestShutdown_ReleasesJobsThatOutlastTheGrace(t *testing.T) {
	q := &fakeQueue{}
	q.add(job("j1", 1), job("j2", 1))
	var started sync.WaitGroup
	started.Add(2)
	c := cfg()
	c.Concurrency = 2
	r := New(c, q, fakeProc{func(ctx context.Context, _ jobs.Job, _ pipeline.Progress) (pipeline.Result, error) {
		started.Done()
		<-ctx.Done() // only returns when cancelled after the grace period
		return pipeline.Result{}, ctx.Err()
	}}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	started.Wait()
	t0 := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the grace period")
	}
	if waited := time.Since(t0); waited < 150*time.Millisecond {
		t.Fatalf("running jobs get the whole grace period first, returned after %v", waited)
	}
	if q.count("release") != 2 || q.count("retry") != 0 || q.count("finish") != 0 {
		t.Fatalf("both jobs go back to the queue untouched: %+v", q.events)
	}
}

func TestShutdown_NoNewJobsAfterStop(t *testing.T) {
	q := &fakeQueue{}
	r := New(cfg(), q, fakeProc{func(context.Context, jobs.Job, pipeline.Progress) (pipeline.Result, error) {
		return pipeline.Result{}, nil
	}}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, "idle loop", func() bool { return q.reaps.Load() > 1 })
	cancel()
	<-done
	q.add(job("late", 1))
	time.Sleep(30 * time.Millisecond)
	if busy, total := r.Stats(); busy != 0 || total != 0 {
		t.Fatalf("a stopped runner must not claim: %d %d", busy, total)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	q := &fakeQueue{}
	for i := 0; i < 8; i++ {
		q.add(job(string(rune('a'+i)), 1))
	}
	var cur, peak atomic.Int32
	c := cfg()
	c.Concurrency = 3
	r := New(c, q, fakeProc{func(context.Context, jobs.Job, pipeline.Progress) (pipeline.Result, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		return pipeline.Result{AssetID: "a"}, nil
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "all jobs", func() bool { return q.count("finish") == 8 })
	if peak.Load() != 3 {
		t.Fatalf("peak concurrency %d, want exactly 3", peak.Load())
	}
}

func TestClaimErrorsDoNotKillTheLoop(t *testing.T) {
	q := &fakeQueue{claimErr: errors.New("database is restarting")}
	r := New(cfg(), q, fakeProc{func(context.Context, jobs.Job, pipeline.Progress) (pipeline.Result, error) {
		return pipeline.Result{AssetID: "a"}, nil
	}}, quiet())
	stop := start(t, r)
	defer stop()
	waitFor(t, "several loops", func() bool { return q.reaps.Load() > 3 })
	q.mu.Lock()
	q.claimErr = nil
	q.pending = append(q.pending, job("j1", 1))
	q.mu.Unlock()
	waitFor(t, "recovery", func() bool { return q.count("finish") == 1 })
}
