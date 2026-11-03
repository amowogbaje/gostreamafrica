// Package runner is the worker's main loop: claim jobs, run them with a lease that is kept alive by a
// heartbeat, and record each outcome exactly once (success, permanent refusal, retry with backoff, or
// hand-back on shutdown).
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"streamafrica/worker/internal/jobs"
	"streamafrica/worker/internal/pipeline"
	"streamafrica/worker/internal/transcode"
)

// Queue is the job store the runner uses.
type Queue interface {
	Claim(ctx context.Context) (*jobs.Job, error)
	Heartbeat(ctx context.Context, jobID string) error
	Progress(ctx context.Context, jobID, stage string, percent int) error
	Finish(ctx context.Context, job jobs.Job, assetID, manifestKey string, durationSecs int) error
	Reject(ctx context.Context, job jobs.Job, assetID string, rj *transcode.Rejection) error
	Retry(ctx context.Context, job jobs.Job, cause error, delay time.Duration, assetID string) (bool, error)
	Release(ctx context.Context, job jobs.Job) error
	Reap(ctx context.Context) (int, error)
}

// Processor runs one job.
type Processor interface {
	Run(ctx context.Context, job jobs.Job, progress pipeline.Progress) (pipeline.Result, error)
}

// Config tunes the runner.
type Config struct {
	Concurrency   int
	JobTimeout    time.Duration
	Lease         time.Duration
	PollInterval  time.Duration
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	ShutdownGrace time.Duration
}

// Runner processes jobs until stopped.
type Runner struct {
	cfg   Config
	q     Queue
	p     Processor
	log   *slog.Logger
	mu    sync.Mutex
	busy  int
	total int
}

// New builds a Runner.
func New(cfg Config, q Queue, p Processor, log *slog.Logger) *Runner {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Runner{cfg: cfg, q: q, p: p, log: log}
}

// Stats reports how many jobs are running now and have finished since start (for logs and tests).
func (r *Runner) Stats() (busy, finished int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy, r.total
}

// Run works until ctx is cancelled. Then it stops claiming, gives running jobs ShutdownGrace to finish,
// cancels the rest (their jobs go back to the queue without losing an attempt) and returns.
func (r *Runner) Run(ctx context.Context) {
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	var wg sync.WaitGroup
	for i := 0; i < r.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.loop(ctx, work)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	r.log.Info("shutting down: no new jobs; waiting for running jobs", "grace", r.cfg.ShutdownGrace.String())
	select {
	case <-done:
	case <-time.After(r.cfg.ShutdownGrace):
		r.log.Warn("grace period over: cancelling running jobs")
		cancelWork()
		<-done
	}
}

func (r *Runner) loop(stop, work context.Context) {
	for stop.Err() == nil {
		reapCtx, cancel := context.WithTimeout(stop, 30*time.Second)
		if n, err := r.q.Reap(reapCtx); err != nil && stop.Err() == nil {
			r.log.Error("reaping stale jobs failed", "error", err.Error())
		} else if n > 0 {
			r.log.Warn("re-queued jobs whose worker stopped responding", "jobs", n)
		}
		job, err := r.q.Claim(reapCtx)
		cancel()
		if err != nil {
			if stop.Err() == nil {
				r.log.Error("claiming a job failed", "error", err.Error())
			}
			sleep(stop, r.cfg.PollInterval)
			continue
		}
		if job == nil {
			sleep(stop, r.cfg.PollInterval)
			continue
		}
		r.process(work, *job)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Backoff is the retry delay for the given attempt.
func (r *Runner) backoff(attempt int) time.Duration {
	return jobs.Backoff(attempt, r.cfg.BackoffBase, r.cfg.BackoffMax)
}

var errTimeout = errors.New("the job took longer than the time limit")

func (r *Runner) process(work context.Context, job jobs.Job) {
	log := r.log.With("job_id", job.ID, "upload_id", job.UploadID, "attempt", job.Attempts)
	r.mu.Lock()
	r.busy++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.busy--
		r.total++
		r.mu.Unlock()
	}()
	log.Info("job started", "max_attempts", job.MaxAttempts)
	started := time.Now()

	ctx, cancel := context.WithCancelCause(work)
	defer cancel(nil)
	timer := time.AfterFunc(r.cfg.JobTimeout, func() { cancel(errTimeout) })
	defer timer.Stop()

	// The heartbeat keeps the lease; if it is lost another worker may own the job, so stop at once.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(r.cfg.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := r.q.Heartbeat(ctx, job.ID); errors.Is(err, jobs.ErrLeaseLost) {
					cancel(jobs.ErrLeaseLost)
					return
				} else if err != nil && ctx.Err() == nil {
					log.Warn("heartbeat failed", "error", err.Error())
				}
			}
		}
	}()

	lastPct := -1
	res, err := r.p.Run(ctx, job, func(stage string, pct int) {
		if pct == lastPct || ctx.Err() != nil {
			return
		}
		lastPct = pct
		if perr := r.q.Progress(ctx, job.ID, stage, pct); perr != nil && ctx.Err() == nil {
			log.Warn("progress update failed", "error", perr.Error())
		}
	})
	cancel(nil)
	<-hbDone
	cause := context.Cause(ctx)

	// Final writes use their own context: the job context may already be cancelled.
	fin, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	var rj *transcode.Rejection
	switch {
	case err == nil:
		if ferr := r.q.Finish(fin, job, res.AssetID, res.ManifestKey, res.DurationSecs); errors.Is(ferr, jobs.ErrLeaseLost) {
			log.Warn("job finished but the lease was lost; another worker owns it now")
		} else if ferr != nil {
			log.Error("recording the result failed", "error", ferr.Error())
		} else {
			log.Info("job done", "seconds", time.Since(started).Round(time.Second).Seconds(), "rungs", res.Rungs)
		}
	case errors.Is(cause, jobs.ErrLeaseLost):
		log.Warn("lease lost; abandoning the job without writing anything")
	case errors.As(err, &rj):
		log.Info("job refused", "code", rj.Code)
		if rerr := r.q.Reject(fin, job, res.AssetID, rj); rerr != nil {
			log.Error("recording the refusal failed", "error", rerr.Error())
		}
	case errors.Is(cause, errTimeout):
		r.retry(fin, log, job, res.AssetID, fmt.Errorf("%w (%s)", errTimeout, r.cfg.JobTimeout))
	case work.Err() != nil: // the worker is shutting down hard: hand the job back untouched
		if rerr := r.q.Release(fin, job); rerr != nil {
			log.Error("releasing the job failed", "error", rerr.Error())
		} else {
			log.Info("job released back to the queue (worker shutting down)")
		}
	default:
		r.retry(fin, log, job, res.AssetID, err)
	}
}

func (r *Runner) retry(ctx context.Context, log *slog.Logger, job jobs.Job, assetID string, cause error) {
	delay := r.backoff(job.Attempts)
	dead, err := r.q.Retry(ctx, job, cause, delay, assetID)
	switch {
	case errors.Is(err, jobs.ErrLeaseLost):
		log.Warn("lease lost while recording a failure")
	case err != nil:
		log.Error("recording the failure failed", "error", err.Error())
	case dead:
		log.Error("job failed for good: out of attempts", "error", cause.Error(), "attempts", job.Attempts)
	default:
		log.Warn("job failed; will retry", "error", cause.Error(), "retry_in", delay.String(), "attempt", job.Attempts, "of", job.MaxAttempts)
	}
}
