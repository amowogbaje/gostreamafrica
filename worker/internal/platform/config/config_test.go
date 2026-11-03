package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{"DATABASE_URL": "postgres://u:p@db/x", "S3_ENDPOINT": "http://seaweedfs:8333", "S3_ACCESS_KEY": "k", "S3_SECRET_KEY": "s"}
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthAddr != ":8081" || cfg.Env != "local" || cfg.LogLevel != "info" || cfg.Concurrency != 1 || cfg.FFmpegThreads != 2 ||
		cfg.X264Preset != "veryfast" || cfg.JobTimeout != 4*time.Hour || cfg.Lease != 2*time.Minute || cfg.BucketHLS != "media-hls" ||
		cfg.MinFreeBytes != 1<<30 || cfg.WorkerID == "" {
		t.Fatalf("%+v", cfg)
	}
}

func TestLoad_Overrides(t *testing.T) {
	m := base()
	m["WORKER_HEALTH_ADDR"], m["WORKER_CONCURRENCY"], m["WORKER_X264_PRESET"] = ":9", "4", "medium"
	cfg, err := Load(env(m))
	if err != nil || cfg.HealthAddr != ":9" || cfg.Concurrency != 4 || cfg.X264Preset != "medium" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestLoad_Validation(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"no database":      {map[string]string{"DATABASE_URL": ""}, "DATABASE_URL is required"},
		"bad endpoint":     {map[string]string{"S3_ENDPOINT": "nope"}, "S3_ENDPOINT must be"},
		"no keys":          {map[string]string{"S3_ACCESS_KEY": ""}, "S3_ACCESS_KEY and S3_SECRET_KEY"},
		"bad preset":       {map[string]string{"WORKER_X264_PRESET": "placebo"}, "WORKER_X264_PRESET"},
		"zero concurrency": {map[string]string{"WORKER_CONCURRENCY": "0"}, "WORKER_CONCURRENCY"},
		"not a number":     {map[string]string{"WORKER_LEASE_SECONDS": "soon"}, "WORKER_LEASE_SECONDS"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := base()
			for k, v := range c.set {
				m[k] = v
			}
			if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
