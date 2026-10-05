// Package config loads worker configuration from environment variables.
package config

import "strings"

// Config holds worker settings. Queue, database and storage settings arrive with the transcoding step.
type Config struct {
	Env        string
	HealthAddr string
	LogLevel   string
}

// Load reads configuration using the supplied getenv.
func Load(getenv func(string) string) Config {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	return Config{
		Env:        get("APP_ENV", "local"),
		HealthAddr: get("WORKER_HEALTH_ADDR", ":8081"),
		LogLevel:   get("WORKER_LOG_LEVEL", "info"),
	}
}
