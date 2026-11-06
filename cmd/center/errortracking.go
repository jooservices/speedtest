package main

import (
	"log/slog"

	"github.com/jooservices/speedtest/internal/shared/errtrack"
)

// startErrorTracking turns on Sentry when SPEEDTEST_SENTRY_DSN is set. A bad setting is logged and the center
// carries on without it: error tracking must never stop the product from starting.
func startErrorTracking(component string, logger *slog.Logger) func() {
	options, err := errtrack.FromEnv(component, version)
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
		logger.Info("error tracking is on", "component", component, "environment", options.Environment, "release", options.Release)
	}
	return flush
}
