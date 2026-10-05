// Package database owns the Postgres connection pool.
package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New builds a pool. It does not connect eagerly, so a down database
// shows up in /readyz instead of crash-looping the API.
func New(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// Do not wrap err: parse errors can echo connection details.
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Check returns a readiness check that runs SELECT 1 through the pool.
func Check(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return err
		}
		if one != 1 {
			return errors.New("unexpected result")
		}
		return nil
	}
}
