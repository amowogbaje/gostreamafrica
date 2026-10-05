// Package logging builds the structured logger used by every service.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON slog logger. Unknown levels fall back to info.
func New(w io.Writer, level, env string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", "api", "env", env)
}
