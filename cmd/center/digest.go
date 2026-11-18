package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/notify"
	"github.com/jooservices/speedtest/internal/center/report"
	"github.com/jooservices/speedtest/internal/center/settings"
)

func newDigestScheduler(database report.DigestDB, notifications *notify.Store, logger *slog.Logger) *report.DigestScheduler {
	if notifications == nil {
		return nil
	}
	return &report.DigestScheduler{
		DB: database, Log: logger,
		Enqueue: func(ctx context.Context, tx pgx.Tx, period report.Period) error {
			eventKey, err := digestEventKey(period.Kind())
			if err != nil {
				return err
			}
			return notifications.PublishDigestEventTx(ctx, tx, eventKey, func(view settings.Settings) (map[string]string, bool, error) {
				summary, err := (&report.Store{DB: tx}).Summarize(ctx, period)
				if err != nil {
					return nil, false, err
				}
				message, err := report.FormatDigest(summary, view.CenterURL)
				if errors.Is(err, report.ErrInvalidDashboardURL) {
					return nil, false, nil
				}
				if err != nil {
					return nil, false, err
				}
				return map[string]string{
					"period": message.Period, "summary": message.Summary,
					"telegram_summary": message.TelegramSummary, "link": message.Link,
				}, true, nil
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
