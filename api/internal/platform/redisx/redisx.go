// Package redisx builds the shared Redis client and its readiness check.
package redisx

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// New parses a redis:// URL and returns a client. It does not connect eagerly,
// so a down Redis shows up in /readyz instead of crash-looping the API.
func New(url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		// Do not wrap err: it may echo parts of the URL, including the password.
		return nil, errors.New("invalid REDIS_URL")
	}
	opt.DialTimeout = 3 * time.Second
	opt.ReadTimeout = 2 * time.Second
	opt.WriteTimeout = 2 * time.Second
	opt.PoolSize = 10
	return redis.NewClient(opt), nil
}

// Check returns a readiness check that runs PING through the client.
func Check(c *redis.Client) func(context.Context) error {
	return func(ctx context.Context) error { return c.Ping(ctx).Err() }
}
