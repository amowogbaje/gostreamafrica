// Command worker turns uploaded videos into adaptive-bitrate HLS. It claims transcode jobs from
// PostgreSQL, runs FFmpeg, publishes the result to object storage, and serves a health endpoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/worker/internal/jobs"
	"streamafrica/worker/internal/pipeline"
	"streamafrica/worker/internal/platform/config"
	"streamafrica/worker/internal/platform/health"
	"streamafrica/worker/internal/platform/s3"
	"streamafrica/worker/internal/runner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})).
		With("service", "worker", "env", cfg.Env, "worker_id", cfg.WorkerID)

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return fmt.Errorf("ffprobe not found in PATH: %w", err)
	}
	logger.Info("ffmpeg found", "ffmpeg", ffmpeg, "ffprobe", ffprobe)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 15*time.Second)
	err = pool.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("database not reachable: %w", err)
	}

	objects, err := s3.New(s3.Config{Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey})
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}

	cleanStaleTemp(logger, cfg.TempDir)

	store := jobs.NewStore(pool, cfg.WorkerID, cfg.Lease)
	pipe := pipeline.New(pipeline.Config{
		TempDir: cfg.TempDir, MinFreeBytes: cfg.MinFreeBytes, BucketHLS: cfg.BucketHLS, BucketPublic: cfg.BucketPublic,
		FFmpeg: ffmpeg, FFprobe: ffprobe, Threads: cfg.FFmpegThreads, Preset: cfg.X264Preset,
	}, store, objects, logger)
	run := runner.New(runner.Config{
		Concurrency: cfg.Concurrency, JobTimeout: cfg.JobTimeout, Lease: cfg.Lease, PollInterval: cfg.PollInterval,
		BackoffBase: cfg.BackoffBase, BackoffMax: cfg.BackoffMax, ShutdownGrace: cfg.ShutdownGrace,
	}, store, pipe, logger)

	mux := http.NewServeMux()
	mux.Handle("/healthz", health.Handler())
	srv := &http.Server{Addr: cfg.HealthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("worker health listening", "addr", cfg.HealthAddr)
		errCh <- srv.ListenAndServe()
	}()

	runDone := make(chan struct{})
	go func() {
		logger.Info("worker started", "concurrency", cfg.Concurrency, "ffmpeg_threads", cfg.FFmpegThreads, "preset", cfg.X264Preset)
		run.Run(ctx)
		close(runDone)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}
	<-runDone // Run returns after running jobs finish or are handed back

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// cleanStaleTemp removes job directories a crashed process left behind.
func cleanStaleTemp(logger *slog.Logger, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "job-") {
			continue
		}
		info, err := e.Info()
		if err == nil && time.Since(info.ModTime()) > time.Hour {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err == nil {
				logger.Info("removed a stale scratch directory", "dir", e.Name())
			}
		}
	}
}
