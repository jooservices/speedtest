package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/report"
)

func newDigestScheduler(database report.DigestDB, dashboardURL string, notifications *notify.Store, logger *slog.Logger) *report.DigestScheduler {
	if notifications == nil {
		return nil
	}
	return &report.DigestScheduler{
		DB: database, Log: logger,
		Enqueue: func(ctx context.Context, tx pgx.Tx, period report.Period) error {
			summary, err := (&report.Store{DB: tx}).Summarize(ctx, period)
			if err != nil {
				return err
			}
			message, err := report.FormatDigest(summary, dashboardURL)
			if err != nil {
				return err
			}
			eventKey, err := digestEventKey(summary.Kind)
			if err != nil {
				return err
			}
			return notifications.PublishDigestEventTx(ctx, tx, eventKey, map[string]string{
				"period": message.Period, "summary": message.Summary, "link": message.Link,
			})
		},
	}
}

func digestEventKey(kind report.PeriodKind) (string, error) {
	switch kind {
	case report.DailyPeriod:
		return "digest.daily", nil
	case report.WeeklyPeriod:
		return "digest.weekly", nil
	default:
		return "", fmt.Errorf("unsupported digest period kind %q", kind)
	}
}
