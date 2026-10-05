package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoad_OK(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://u:p@postgres:5432/db?sslmode=disable",
		"REDIS_URL":    "redis://:pw@redis:6379/0",
		"S3_ENDPOINT":  "http://minio:9000",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PostgresAddr != "postgres:5432" || cfg.RedisAddr != "redis:6379" || cfg.S3Addr != "minio:9000" {
		t.Fatalf("bad addrs: %+v", cfg)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Env != "local" || cfg.LogLevel != "info" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
}

func TestLoad_DefaultPorts(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "postgres://u:p@db/x",
		"REDIS_URL":    "redis://cache",
		"S3_ENDPOINT":  "https://r2.example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PostgresAddr != "db:5432" || cfg.RedisAddr != "cache:6379" || cfg.S3Addr != "r2.example.com:443" {
		t.Fatalf("bad addrs: %+v", cfg)
	}
}

func TestLoad_MissingAndInvalid(t *testing.T) {
	_, err := Load(env(map[string]string{"DATABASE_URL": "postgres://u:secretpw@:5432/db", "S3_ENDPOINT": "http://minio:9000"}))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL must be", "REDIS_URL is required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
	if strings.Contains(msg, "secretpw") {
		t.Error("error leaks password")
	}
}
