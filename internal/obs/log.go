package obs

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON slog logger writing to stdout. Vector tails pod
// stdout in the cluster, so structured JSON is the only output format.
func NewLogger(level, env, version string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With(
		slog.String("service", "agent-gateway"),
		slog.String("env", env),
		slog.String("version", version),
	)
}
