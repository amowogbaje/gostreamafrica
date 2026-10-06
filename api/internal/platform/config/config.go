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

// Config holds the settings the API needs today. Fields marked secret must
// never be logged.
type Config struct {
	Env              string
	HTTPAddr         string
	LogLevel         string
	PostgresAddr     string // host:port derived from DATABASE_URL
	RedisAddr        string // host:port derived from REDIS_URL
	S3Addr           string // host:port derived from S3_ENDPOINT
	DatabaseURL      string // secret
	RedisURL         string // secret
	DBMaxConns       int32
	TrustedProxyHops int // reverse proxies in front of the API (0 = none)
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
	intIn := func(key, def string, lo, hi int) int {
		n, err := strconv.Atoi(get(key, def))
		if err != nil || n < lo || n > hi {
			errs = append(errs, fmt.Errorf("%s must be an integer from %d to %d", key, lo, hi))
			n, _ = strconv.Atoi(def)
		}
		return n
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
	cfg.RedisURL = strings.TrimSpace(getenv("REDIS_URL"))
	cfg.DBMaxConns = int32(intIn("DB_MAX_CONNS", "10", 1, 100))
	cfg.TrustedProxyHops = intIn("TRUSTED_PROXY_HOPS", "0", 0, 5)
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
