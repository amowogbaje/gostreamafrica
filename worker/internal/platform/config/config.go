// Package config loads worker configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds worker settings.
type Config struct {
	Env        string
	HealthAddr string
	LogLevel   string

	DatabaseURL string // secret

	S3Endpoint   string
	S3Region     string
	S3AccessKey  string // secret
	S3SecretKey  string // secret
	BucketRaw    string
	BucketHLS    string
	BucketPublic string

	Concurrency   int           // jobs processed at once
	FFmpegThreads int           // threads per FFmpeg process (0 = let FFmpeg decide)
	X264Preset    string        // speed/quality trade-off for libx264
	JobTimeout    time.Duration // hard limit for one attempt
	TempDir       string
	MinFreeBytes  int64 // disk that must stay free beyond what a job needs
	Lease         time.Duration
	PollInterval  time.Duration
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	ShutdownGrace time.Duration
	WorkerID      string
}

// Load reads configuration using the supplied getenv and validates it.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def, lo, hi int) int {
		n, err := strconv.Atoi(get(k, strconv.Itoa(def)))
		if err != nil || n < lo || n > hi {
			errs = append(errs, fmt.Errorf("%s must be an integer from %d to %d", k, lo, hi))
			return def
		}
		return n
	}
	host, _ := os.Hostname()
	cfg := Config{
		Env:          get("APP_ENV", "local"),
		HealthAddr:   get("WORKER_HEALTH_ADDR", ":8081"),
		LogLevel:     get("WORKER_LOG_LEVEL", "info"),
		DatabaseURL:  strings.TrimSpace(getenv("DATABASE_URL")),
		S3Endpoint:   strings.TrimRight(strings.TrimSpace(getenv("S3_ENDPOINT")), "/"),
		S3Region:     get("S3_REGION", "us-east-1"),
		S3AccessKey:  strings.TrimSpace(getenv("S3_ACCESS_KEY")),
		S3SecretKey:  strings.TrimSpace(getenv("S3_SECRET_KEY")),
		BucketRaw:    get("S3_BUCKET_RAW", "media-raw"),
		BucketHLS:    get("S3_BUCKET_HLS", "media-hls"),
		BucketPublic: get("S3_BUCKET_PUBLIC", "media-public"),
		X264Preset:   get("WORKER_X264_PRESET", "veryfast"),
		TempDir:      get("WORKER_TEMP_DIR", "/tmp/streamafrica-worker"),
		WorkerID:     get("WORKER_ID", fmt.Sprintf("%s-%d", host, os.Getpid())),
	}
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if u, err := url.Parse(cfg.S3Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		errs = append(errs, errors.New("S3_ENDPOINT must be an http(s) URL without a path"))
	}
	if cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		errs = append(errs, errors.New("S3_ACCESS_KEY and S3_SECRET_KEY are required"))
	}
	switch cfg.X264Preset {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow":
	default:
		errs = append(errs, errors.New("WORKER_X264_PRESET must be one of ultrafast, superfast, veryfast, faster, fast, medium, slow"))
	}
	cfg.Concurrency = num("WORKER_CONCURRENCY", 1, 1, 16)
	cfg.FFmpegThreads = num("WORKER_FFMPEG_THREADS", 2, 0, 64)
	cfg.JobTimeout = time.Duration(num("WORKER_JOB_TIMEOUT_MINUTES", 240, 1, 2880)) * time.Minute
	cfg.MinFreeBytes = int64(num("WORKER_MIN_FREE_MB", 1024, 0, 1<<20)) << 20
	cfg.Lease = time.Duration(num("WORKER_LEASE_SECONDS", 120, 10, 3600)) * time.Second
	cfg.PollInterval = time.Duration(num("WORKER_POLL_SECONDS", 2, 1, 60)) * time.Second
	cfg.BackoffBase = time.Duration(num("WORKER_BACKOFF_BASE_SECONDS", 30, 1, 3600)) * time.Second
	cfg.BackoffMax = time.Duration(num("WORKER_BACKOFF_MAX_MINUTES", 15, 1, 1440)) * time.Minute
	cfg.ShutdownGrace = time.Duration(num("WORKER_SHUTDOWN_GRACE_SECONDS", 60, 1, 3600)) * time.Second
	return cfg, errors.Join(errs...)
}
