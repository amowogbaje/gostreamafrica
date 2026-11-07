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

	// --- object storage (plain S3 API; SeaweedFS locally, R2 or S3 in the cloud: ADR-0006, ADR-0014)
	S3Endpoint       string // URL the API itself uses, for example http://seaweedfs:8333
	S3PublicEndpoint string // URL the browser uses; presigned URLs are signed for this host
	S3Region         string
	S3AccessKey      string // secret
	S3SecretKey      string // secret
	S3BucketRaw      string
	S3BucketHLS      string
	S3BucketPublic   string

	// --- playback (Phase 2.3)
	PlaybackSigningKey string        // secret: signs short-lived media URLs; nginx holds the same key
	PlaybackBaseURL    string        // where browsers reach the media front door (nginx locally, the CDN in the cloud)
	PlaybackTokenTTL   time.Duration // how long a signed manifest URL stays valid

	// --- creator uploads (Phase 2)
	UploadMaxBytes        int64         // largest accepted source file
	UploadPartBytes       int64         // multipart part size handed to clients
	UploadURLTTL          time.Duration // lifetime of one presigned part URL
	UploadAbandonAfter    time.Duration // an unfinished upload is aborted after this long
	UploadSweepEvery      time.Duration
	UploadMaxActive       int   // unfinished uploads one creator may have at once
	UploadDailyBytes      int64 // bytes one creator may start uploading per rolling 24 hours
	UploadMaxDurationSecs int   // longest accepted video, enforced by the worker probe
	UploadMaxWidth        int
	UploadMaxHeight       int

	// APIDocsEnabled serves Swagger UI and the spec at /docs/. On by default only for APP_ENV=local.
	APIDocsEnabled bool
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

	// --- object storage
	cfg.S3Endpoint = strings.TrimRight(strings.TrimSpace(getenv("S3_ENDPOINT")), "/")
	cfg.S3PublicEndpoint = strings.TrimRight(get("S3_PUBLIC_ENDPOINT", cfg.S3Endpoint), "/")
	for _, kv := range [][2]string{{"S3_ENDPOINT", cfg.S3Endpoint}, {"S3_PUBLIC_ENDPOINT", cfg.S3PublicEndpoint}} {
		if u, err := url.Parse(kv[1]); kv[1] != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "") {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL without a path", kv[0]))
		}
	}
	cfg.S3Region = get("S3_REGION", "us-east-1")
	cfg.S3AccessKey = strings.TrimSpace(getenv("S3_ACCESS_KEY"))
	cfg.S3SecretKey = strings.TrimSpace(getenv("S3_SECRET_KEY"))
	if cfg.S3AccessKey == "" {
		errs = append(errs, errors.New("S3_ACCESS_KEY is required"))
	}
	if cfg.S3SecretKey == "" {
		errs = append(errs, errors.New("S3_SECRET_KEY is required"))
	}
	cfg.S3BucketRaw = get("S3_BUCKET_RAW", "media-raw")
	cfg.S3BucketHLS = get("S3_BUCKET_HLS", "media-hls")
	cfg.S3BucketPublic = get("S3_BUCKET_PUBLIC", "media-public")

	// --- playback
	cfg.PlaybackSigningKey = strings.TrimSpace(getenv("PLAYBACK_SIGNING_KEY"))
	if len(cfg.PlaybackSigningKey) < 32 || strings.ContainsAny(cfg.PlaybackSigningKey, " \t\r\n\"'$") {
		errs = append(errs, errors.New("PLAYBACK_SIGNING_KEY is required: at least 32 characters, no spaces, quotes or $"))
	}
	cfg.PlaybackBaseURL = strings.TrimRight(strings.TrimSpace(getenv("PLAYBACK_BASE_URL")), "/")
	if u, err := url.Parse(cfg.PlaybackBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		errs = append(errs, errors.New("PLAYBACK_BASE_URL is required: an http(s) URL without a path, for example http://localhost:8088"))
	}
	cfg.PlaybackTokenTTL = time.Duration(intIn("PLAYBACK_TOKEN_TTL_HOURS", "4", 2, 6)) * time.Hour

	// --- creator uploads
	const mib = 1 << 20
	int64In := func(key, def string, lo, hi int64) int64 {
		n, err := strconv.ParseInt(get(key, def), 10, 64)
		if err != nil || n < lo || n > hi {
			errs = append(errs, fmt.Errorf("%s must be an integer from %d to %d", key, lo, hi))
			n, _ = strconv.ParseInt(def, 10, 64)
		}
		return n
	}
	cfg.UploadMaxBytes = int64In("UPLOAD_MAX_BYTES", "8589934592", 1*mib, 64<<30)
	cfg.UploadPartBytes = int64In("UPLOAD_PART_BYTES", "67108864", 5*mib, 512*mib) // S3 minimum part is 5 MiB
	if cfg.UploadPartBytes > 0 && (cfg.UploadMaxBytes+cfg.UploadPartBytes-1)/cfg.UploadPartBytes > 10000 {
		errs = append(errs, errors.New("UPLOAD_PART_BYTES is too small: S3 allows at most 10000 parts per upload, raise it or lower UPLOAD_MAX_BYTES"))
	}
	cfg.UploadURLTTL = time.Duration(intIn("UPLOAD_URL_TTL_MINUTES", "15", 1, 60)) * time.Minute
	cfg.UploadAbandonAfter = time.Duration(intIn("UPLOAD_ABANDON_HOURS", "24", 1, 168)) * time.Hour
	cfg.UploadSweepEvery = time.Duration(intIn("UPLOAD_SWEEP_MINUTES", "60", 1, 1440)) * time.Minute
	cfg.UploadMaxActive = intIn("UPLOAD_MAX_ACTIVE_PER_CREATOR", "3", 1, 50)
	cfg.UploadDailyBytes = int64In("UPLOAD_DAILY_BYTES_PER_CREATOR", "34359738368", 1*mib, 1<<50)
	cfg.UploadMaxDurationSecs = intIn("UPLOAD_MAX_DURATION_MINUTES", "240", 1, 1440) * 60
	cfg.UploadMaxWidth = intIn("UPLOAD_MAX_WIDTH", "3840", 16, 16384)
	cfg.UploadMaxHeight = intIn("UPLOAD_MAX_HEIGHT", "2160", 16, 16384)

	// --- API documentation page
	cfg.APIDocsEnabled = cfg.Env == "local"
	if v := strings.TrimSpace(getenv("API_DOCS_ENABLED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, errors.New("API_DOCS_ENABLED must be true or false"))
		} else {
			cfg.APIDocsEnabled = b
		}
	}
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
