// Package config loads service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Config holds the settings the API needs today. Secrets are never stored
// here; only dial addresses derived from connection URLs.
type Config struct {
	Env          string
	HTTPAddr     string
	LogLevel     string
	PostgresAddr string // host:port derived from DATABASE_URL
	RedisAddr    string // host:port derived from REDIS_URL
	S3Addr       string // host:port derived from S3_ENDPOINT
	DatabaseURL  string // secret: never log
	DBMaxConns   int32
}

// Load reads configuration using the supplied getenv (os.Getenv in main).
// All problems are reported together.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	addr := func(key, defPort string) string {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
			return ""
		}
		a, err := hostPort(raw, defPort)
		if err != nil {
			// Deliberately do not wrap err: url.Parse echoes the raw URL, which may contain a password.
			errs = append(errs, fmt.Errorf("%s must be a URL with a host", key))
			return ""
		}
		return a
	}

	cfg := Config{
		Env:      get("APP_ENV", "local"),
		HTTPAddr: get("API_HTTP_ADDR", ":8080"),
		LogLevel: get("API_LOG_LEVEL", "info"),
	}
	cfg.PostgresAddr = addr("DATABASE_URL", "5432")
	cfg.RedisAddr = addr("REDIS_URL", "6379")
	cfg.S3Addr = addr("S3_ENDPOINT", "")
	cfg.DatabaseURL = strings.TrimSpace(getenv("DATABASE_URL"))
	n, convErr := strconv.Atoi(get("DB_MAX_CONNS", "10"))
	if convErr != nil || n < 1 || n > 100 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be an integer from 1 to 100"))
		n = 10
	}
	cfg.DBMaxConns = int32(n)
	return cfg, errors.Join(errs...)
}

func hostPort(raw, defPort string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid url")
	}
	port := u.Port()
	if port == "" {
		port = defPort
	}
	if port == "" { // scheme default
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
