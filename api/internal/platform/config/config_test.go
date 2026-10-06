package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://u:p@postgres:5432/db?sslmode=disable",
		"REDIS_URL":    "redis://:pw@redis:6379/0",
		"S3_ENDPOINT":  "http://minio:9000",
	}
}

func TestLoad_OK(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PostgresAddr != "postgres:5432" || cfg.RedisAddr != "redis:6379" || cfg.S3Addr != "minio:9000" {
		t.Fatalf("bad addrs: %+v", cfg)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Env != "local" || cfg.LogLevel != "info" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if cfg.DBMaxConns != 10 || cfg.TrustedProxyHops != 0 || cfg.DatabaseURL == "" || cfg.RedisURL == "" {
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

func TestLoad_IntegerSettings(t *testing.T) {
	m := base()
	m["DB_MAX_CONNS"] = "25"
	m["TRUSTED_PROXY_HOPS"] = "1"
	cfg, err := Load(env(m))
	if err != nil || cfg.DBMaxConns != 25 || cfg.TrustedProxyHops != 1 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	for key, bad := range map[string]string{"DB_MAX_CONNS": "0", "TRUSTED_PROXY_HOPS": "9"} {
		m := base()
		m[key] = bad
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s=%s: expected error naming the key, got %v", key, bad, err)
		}
	}
	m = base()
	m["DB_MAX_CONNS"] = "abc"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error for non-numeric")
	}
}
