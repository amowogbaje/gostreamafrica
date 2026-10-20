package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{
		"DATABASE_URL":    "postgres://u:p@postgres:5432/db?sslmode=disable",
		"REDIS_URL":       "redis://:pw@redis:6379/0",
		"S3_ENDPOINT":     "http://minio:9000",
		"AUTH_JWT_SECRET": DevJWTSecret,
		"SMTP_HOST":       "mailpit",
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
	if cfg.AccessTTL != 15*time.Minute || cfg.RefreshTTL != 30*24*time.Hour || cfg.RefreshAbsoluteTTL != 90*24*time.Hour ||
		cfg.CookieSecure || cfg.RefreshCookiePath != "/api/v1/auth" || cfg.WebBaseURL != "http://localhost:8088" ||
		cfg.Argon2MemoryKiB != 19456 || cfg.Argon2Time != 2 || cfg.Argon2Parallelism != 1 {
		t.Fatalf("bad auth defaults: %+v", cfg)
	}
	if cfg.CatalogHomeTTL != 5*time.Minute || cfg.CatalogGenresTTL != 10*time.Minute {
		t.Fatalf("bad cache defaults: %+v", cfg)
	}
	if cfg.SMTPHost != "mailpit" || cfg.SMTPPort != 1025 || cfg.SMTPRequireTLS || len(cfg.AllowedOrigins) != 0 {
		t.Fatalf("bad smtp defaults: %+v", cfg)
	}
}

func TestLoad_NonLocalHardening(t *testing.T) {
	m := base()
	m["APP_ENV"] = "production"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "must not be the example value") {
		t.Fatalf("example secret accepted in production: %v", err)
	}
	m["AUTH_JWT_SECRET"] = strings.Repeat("k", 40)
	cfg, err := Load(env(m))
	if err != nil || !cfg.CookieSecure || !cfg.SMTPRequireTLS {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestLoad_AuthValidation(t *testing.T) {
	bad := map[string]map[string]string{
		"AUTH_JWT_SECRET":            {"AUTH_JWT_SECRET": "short"},
		"AUTH_ACCESS_TTL_MINUTES":    {"AUTH_ACCESS_TTL_MINUTES": "0"},
		"AUTH_REFRESH_TTL_DAYS":      {"AUTH_REFRESH_TTL_DAYS": "999"},
		"AUTH_REFRESH_ABSOLUTE_DAYS": {"AUTH_REFRESH_TTL_DAYS": "60", "AUTH_REFRESH_ABSOLUTE_DAYS": "30"},
		"AUTH_REFRESH_COOKIE_PATH":   {"AUTH_REFRESH_COOKIE_PATH": "api"},
		"AUTH_ALLOWED_ORIGINS":       {"AUTH_ALLOWED_ORIGINS": "localhost:3000"},
		"WEB_BASE_URL":               {"WEB_BASE_URL": "ftp://x"},
		"AUTH_ARGON2_MEMORY_KIB":     {"AUTH_ARGON2_MEMORY_KIB": "1024"},
		"SMTP_HOST":                  {"SMTP_HOST": " "},
		"SMTP_PORT":                  {"SMTP_PORT": "70000"},
		"SMTP_REQUIRE_TLS":           {"SMTP_REQUIRE_TLS": "maybe"},
		"CATALOG_HOME_TTL_SECONDS":   {"CATALOG_HOME_TTL_SECONDS": "1"},
		"CATALOG_GENRES_TTL_SECONDS": {"CATALOG_GENRES_TTL_SECONDS": "abc"},
	}
	for key, overrides := range bad {
		m := base()
		for k, v := range overrides {
			m[k] = v
		}
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: expected an error naming the key, got %v", key, err)
		}
	}
}

func TestLoad_Origins(t *testing.T) {
	m := base()
	m["AUTH_ALLOWED_ORIGINS"] = "http://localhost:3000, https://app.example.com"
	cfg, err := Load(env(m))
	if err != nil || len(cfg.AllowedOrigins) != 2 || cfg.AllowedOrigins[1] != "https://app.example.com" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestLoad_DefaultPorts(t *testing.T) {
	m := base()
	m["DATABASE_URL"] = "postgres://u:p@db/x"
	m["REDIS_URL"] = "redis://cache"
	m["S3_ENDPOINT"] = "https://r2.example.com"
	cfg, err := Load(env(m))
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
	for _, want := range []string{"DATABASE_URL must be", "REDIS_URL is required", "AUTH_JWT_SECRET is required", "SMTP_HOST is required"} {
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
	for key, val := range map[string]string{"DB_MAX_CONNS": "0", "TRUSTED_PROXY_HOPS": "9", "DB_MAX_CONNS ": "abc"} {
		m := base()
		m[strings.TrimSpace(key)] = val
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(key)) {
			t.Fatalf("%s=%s: expected error naming the key, got %v", key, val, err)
		}
	}
}

func TestLoad_APIDocsToggle(t *testing.T) {
	cases := []struct {
		env, value string
		want       bool
		wantErr    bool
	}{
		{"local", "", true, false},       // on by default for local development
		{"production", "", false, false}, // off by default everywhere else
		{"staging", "", false, false},
		{"production", "true", true, false}, // explicit opt-in
		{"local", "false", false, false},    // explicit opt-out
		{"local", "maybe", true, true},
	}
	for _, c := range cases {
		m := base()
		m["APP_ENV"] = c.env
		if c.env != "local" {
			m["AUTH_JWT_SECRET"] = strings.Repeat("k", 40)
		}
		if c.value != "" {
			m["API_DOCS_ENABLED"] = c.value
		}
		cfg, err := Load(env(m))
		if (err != nil) != c.wantErr || (err == nil && cfg.APIDocsEnabled != c.want) {
			t.Errorf("env=%s value=%q: enabled=%v err=%v, want enabled=%v wantErr=%v", c.env, c.value, cfg.APIDocsEnabled, err, c.want, c.wantErr)
		}
		if c.wantErr && !strings.Contains(err.Error(), "API_DOCS_ENABLED") {
			t.Errorf("error must name the variable: %v", err)
		}
	}
}
