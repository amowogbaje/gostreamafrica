// Package app wires the modules together. main uses it to build the server, and the OpenAPI
// contract tests use it to build the very same server, so the tests cannot drift from production.
package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"streamafrica/api/internal/apidocs"
	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/catalog"
	"streamafrica/api/internal/creators"
	"streamafrica/api/internal/platform/config"
	"streamafrica/api/internal/platform/database"
	"streamafrica/api/internal/platform/health"
	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
	"streamafrica/api/internal/platform/redisx"
	"streamafrica/api/internal/platform/s3"
	"streamafrica/api/internal/platform/server"
	"streamafrica/api/internal/playback"
)

// Options are the collaborators the app needs. Pool and Redis may be nil only when no request is
// served (the route-listing test).
type Options struct {
	Config         config.Config
	Pool           *pgxpool.Pool
	Redis          *redis.Client
	Logger         *slog.Logger
	Mailer         auth.Mailer           // default: SMTP from Config
	Now            func() time.Time      // default: time.Now
	SyncEmail      bool                  // send email inline (tests)
	CacheNamespace string                // Redis key prefix for the catalog cache (default "catalog")
	Objects        creators.ObjectStore  // default: the S3 client built from Config
	Scanner        creators.Scanner      // default: no scanning (logged at start-up)
	Entitlements   playback.Entitlements // default: published titles only (Phase 4 replaces it)
	OnRoute        func(path string, methods []string)
}

// App is the assembled application.
type App struct {
	Handler  http.Handler
	Auth     *auth.Service
	Catalog  *catalog.Service
	Creators *creators.Service
	// Sweeper cleans up abandoned uploads. Start it with `go app.Sweeper.Run(ctx)`; it is not
	// started here so tests control when it runs.
	Sweeper *creators.Sweeper
}

// New builds the application.
func New(o Options) (*App, error) {
	cfg := o.Config
	now := o.Now
	if now == nil {
		now = time.Now
	}

	hasher, err := auth.NewHasher(auth.Params{
		MemoryKiB: cfg.Argon2MemoryKiB, Time: cfg.Argon2Time, Parallelism: cfg.Argon2Parallelism,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	tokens, err := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTTL, now)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	mailer := o.Mailer
	if mailer == nil {
		mailer, err = auth.NewSMTPMailer(auth.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, From: cfg.SMTPFrom,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, RequireTLS: cfg.SMTPRequireTLS,
		})
		if err != nil {
			return nil, fmt.Errorf("mailer: %w", err)
		}
	}
	limiter := ratelimit.NewRedisStore(o.Redis)

	authSvc, err := auth.NewService(auth.Deps{
		Store:   auth.NewPGStore(o.Pool),
		Hasher:  hasher,
		Tokens:  tokens,
		Tracker: auth.NewRedisTracker(o.Redis, auth.DefaultPairPolicy(), auth.DefaultAccountPolicy()),
		Mailer:  mailer,
		Logger:  o.Logger,
		Now:     now,
		Config: auth.ServiceConfig{
			RefreshTTL:         cfg.RefreshTTL,
			RefreshAbsoluteTTL: cfg.RefreshAbsoluteTTL,
			WebBaseURL:         cfg.WebBaseURL,
			SyncEmail:          o.SyncEmail,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	authHandler := auth.NewHandler(authSvc, tokens, limiter, o.Logger, auth.HandlerConfig{
		CookieSecure:     cfg.CookieSecure,
		CookiePath:       cfg.RefreshCookiePath,
		TrustedProxyHops: cfg.TrustedProxyHops,
		AllowedOrigins:   cfg.AllowedOrigins,
	})

	catalogSvc := catalog.NewService(catalog.NewPGStore(o.Pool), catalog.NewRedisCache(o.Redis, o.CacheNamespace), o.Logger, now,
		catalog.Config{HomeTTL: cfg.CatalogHomeTTL, GenresTTL: cfg.CatalogGenresTTL})
	catalogHandler := catalog.NewHandler(catalogSvc, catalog.AuthMiddleware{
		Required: authHandler.Authenticate(),
		Optional: authHandler.OptionalAuthenticate(),
	}, limiter, o.Logger)

	objects := o.Objects
	if objects == nil {
		client, err := s3.New(s3.Config{Endpoint: cfg.S3Endpoint, PublicEndpoint: cfg.S3PublicEndpoint, Region: cfg.S3Region,
			AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey})
		if err != nil {
			return nil, fmt.Errorf("object storage: %w", err)
		}
		objects = client
	}
	scanner := o.Scanner
	if scanner == nil {
		scanner = creators.NoopScanner{}
		o.Logger.Warn("uploads are NOT virus-scanned: no scanner is configured (fine for local development only)")
	}
	creatorStore := creators.NewPGStore(o.Pool)
	creatorSvc := creators.NewService(creatorStore, objects, scanner, o.Logger, now, creators.Config{
		RawBucket: cfg.S3BucketRaw, HLSBucket: cfg.S3BucketHLS, PublicBucket: cfg.S3BucketPublic, MaxBytes: cfg.UploadMaxBytes, PartBytes: cfg.UploadPartBytes, URLTTL: cfg.UploadURLTTL,
		AbandonAfter: cfg.UploadAbandonAfter, MaxActive: cfg.UploadMaxActive, DailyBytes: cfg.UploadDailyBytes,
		Policy: creators.DefaultPolicy(cfg.UploadMaxDurationSecs, cfg.UploadMaxWidth, cfg.UploadMaxHeight),
	})
	creatorHandler := creators.NewHandler(creatorSvc, authHandler.Authenticate(), limiter, o.Logger)
	sweeper := creators.NewSweeper(creatorStore, objects, o.Logger, now, creators.SweeperConfig{
		Bucket: cfg.S3BucketRaw, AbandonAfter: cfg.UploadAbandonAfter, Every: cfg.UploadSweepEvery})

	ent := o.Entitlements
	if ent == nil {
		ent = playback.PublishedOnly{}
	}
	playbackSvc := playback.NewService(playback.NewPGStore(o.Pool), ent, playback.NewSigner(cfg.PlaybackBaseURL, cfg.PlaybackSigningKey),
		cfg.PlaybackTokenTTL, cfg.PlaybackBaseURL+"/public", o.Logger, now)
	playbackHandler := playback.NewHandler(playbackSvc, authHandler.Authenticate(), limiter, o.Logger)

	mounts := []func(httpx.Mux){authHandler.Mount, catalogHandler.Mount, creatorHandler.Mount, playbackHandler.Mount}
	if cfg.APIDocsEnabled {
		docs, err := apidocs.New()
		if err != nil {
			return nil, fmt.Errorf("apidocs: %w", err)
		}
		mounts = append(mounts, docs.Mount)
	}

	return &App{
		Auth:     authSvc,
		Catalog:  catalogSvc,
		Creators: creatorSvc,
		Sweeper:  sweeper,
		Handler: server.NewHandler(server.Deps{
			Logger: o.Logger,
			Dependencies: []health.Dependency{
				{Name: "postgres", Check: database.Check(o.Pool)},
				{Name: "redis", Check: redisx.Check(o.Redis)},
				{Name: "s3", Check: health.TCP(cfg.S3Addr)},
			},
			Mount:   mounts,
			OnRoute: o.OnRoute,
		}),
	}, nil
}

// Wait blocks until background work (emails) has finished. Call it during shutdown.
func (a *App) Wait() { a.Auth.Wait() }
