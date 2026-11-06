package main

import (
	"log/slog"

	"github.com/jooservices/speedtest/internal/shared/errtrack"
)

// startErrorTracking turns on Sentry when SPEEDTEST_SENTRY_DSN is set in the agent's environment. A bad setting is
// logged and the agent carries on without it.
func startErrorTracking(logger *slog.Logger) func() {
	options, err := errtrack.FromEnv("agent", version)
	if err != nil {
		logger.Warn("error tracking is off", "reason", err.Error())
		return func() {}
	}
	flush, err := errtrack.Init(options)
	if err != nil {
		logger.Warn("error tracking is off", "reason", err.Error())
		return func() {}
	}
	if errtrack.Enabled() {
		logger.Info("error tracking is on", "component", "agent", "environment", options.Environment, "release", options.Release)
	}
	return flush
}
