// Package config loads service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DevJWTSecret is the placeholder used in .env.example. It is refused outside APP_ENV=local.
const DevJWTSecret = "dev-only-secret-change-me-0123456789abcdef"

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

	JWTSecret          []byte // secret
	AccessTTL          time.Duration
	RefreshTTL         time.Duration
	RefreshAbsoluteTTL time.Duration
	RefreshCookiePath  string
	CookieSecure       bool // true everywhere except APP_ENV=local
	AllowedOrigins     []string
	WebBaseURL         string
	Argon2MemoryKiB    uint32
	Argon2Time         uint32
	Argon2Parallelism  uint8

	SMTPHost       string
	SMTPPort       int
	SMTPFrom       string
	SMTPUsername   string
	SMTPPassword   string // secret
	SMTPRequireTLS bool

	CatalogHomeTTL   time.Duration
	CatalogGenresTTL time.Duration
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
	cfg.CookieSecure = cfg.Env != "local"

	// --- auth
	secret := strings.TrimSpace(getenv("AUTH_JWT_SECRET"))
	switch {
	case secret == "":
		errs = append(errs, errors.New("AUTH_JWT_SECRET is required"))
	case len(secret) < 32:
		errs = append(errs, errors.New("AUTH_JWT_SECRET must be at least 32 characters"))
	case cfg.Env != "local" && secret == DevJWTSecret:
		errs = append(errs, errors.New("AUTH_JWT_SECRET must not be the example value outside APP_ENV=local"))
	}
	cfg.JWTSecret = []byte(secret)
	cfg.AccessTTL = time.Duration(intIn("AUTH_ACCESS_TTL_MINUTES", "15", 1, 60)) * time.Minute
	cfg.RefreshTTL = time.Duration(intIn("AUTH_REFRESH_TTL_DAYS", "30", 1, 90)) * 24 * time.Hour
	cfg.RefreshAbsoluteTTL = time.Duration(intIn("AUTH_REFRESH_ABSOLUTE_DAYS", "90", 1, 365)) * 24 * time.Hour
	if cfg.RefreshAbsoluteTTL < cfg.RefreshTTL {
		errs = append(errs, errors.New("AUTH_REFRESH_ABSOLUTE_DAYS must not be smaller than AUTH_REFRESH_TTL_DAYS"))
	}
	cfg.RefreshCookiePath = get("AUTH_REFRESH_COOKIE_PATH", "/api/v1/auth")
	if !strings.HasPrefix(cfg.RefreshCookiePath, "/") {
		errs = append(errs, errors.New("AUTH_REFRESH_COOKIE_PATH must start with /"))
	}
	for _, o := range strings.Split(getenv("AUTH_ALLOWED_ORIGINS"), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			errs = append(errs, errors.New("AUTH_ALLOWED_ORIGINS must be comma-separated origins like https://example.com"))
			break
		}
		cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
	}
	cfg.WebBaseURL = strings.TrimRight(get("WEB_BASE_URL", "http://localhost:8088"), "/")
	if u, err := url.Parse(cfg.WebBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("WEB_BASE_URL must be an http(s) URL"))
	}
	cfg.Argon2MemoryKiB = uint32(intIn("AUTH_ARGON2_MEMORY_KIB", "19456", 8192, 262144))
	cfg.Argon2Time = uint32(intIn("AUTH_ARGON2_TIME", "2", 1, 10))
	cfg.Argon2Parallelism = uint8(intIn("AUTH_ARGON2_PARALLELISM", "1", 1, 8))

	// --- email
	cfg.SMTPHost = strings.TrimSpace(getenv("SMTP_HOST"))
	if cfg.SMTPHost == "" {
		errs = append(errs, errors.New("SMTP_HOST is required"))
	}
	cfg.SMTPPort = intIn("SMTP_PORT", "1025", 1, 65535)
	cfg.SMTPFrom = get("SMTP_FROM", "StreamAfrica <no-reply@streamafrica.local>")
	cfg.SMTPUsername = strings.TrimSpace(getenv("SMTP_USERNAME"))
	cfg.SMTPPassword = getenv("SMTP_PASSWORD")
	cfg.SMTPRequireTLS = cfg.Env != "local"
	if v := strings.TrimSpace(getenv("SMTP_REQUIRE_TLS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, errors.New("SMTP_REQUIRE_TLS must be true or false"))
		} else {
			cfg.SMTPRequireTLS = b
		}
	}

	// --- catalog cache
	cfg.CatalogHomeTTL = time.Duration(intIn("CATALOG_HOME_TTL_SECONDS", "300", 10, 3600)) * time.Second
	cfg.CatalogGenresTTL = time.Duration(intIn("CATALOG_GENRES_TTL_SECONDS", "600", 10, 86400)) * time.Second
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
