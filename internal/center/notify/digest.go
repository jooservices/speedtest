package notify

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/settings"
)

var digestEventChannels = []string{"telegram", "email"}

// PublishDigestEventTx builds and records an eligible daily or weekly digest in
// the caller's transaction. Disabled or undeliverable periods are completed
// without creating an alert event or outbox row.
func (s *Store) PublishDigestEventTx(ctx context.Context, tx pgx.Tx, key string, build func(settings.Settings) (map[string]string, bool, error)) error {
	if key != "digest.daily" && key != "digest.weekly" {
		return ErrInvalidEvent
	}
	event, err := (&EventStore{DB: tx}).GetEvent(ctx, key)
	if err != nil || !event.Enabled {
		return err
	}
	view, channels, err := configuredDigestChannels(ctx, s, tx, event.Channels)
	if err != nil || len(channels) == 0 {
		return err
	}
	event.Channels = channels
	if build == nil {
		return ErrInvalidEvent
	}
	payload, publish, err := build(view)
	if err != nil || !publish {
		return err
	}
	now := s.now()
	data, err := buildEventPayload(ctx, tx, event, "", payload, now)
	if err != nil {
		return err
	}
	channels, publish, err = s.eventDelivery(ctx, tx, event, key, "", data, now, "")
	if err != nil || !publish {
		return err
	}
	return recordEventTx(ctx, tx, event, "", data, now, channels, true)
}

func configuredDigestChannels(ctx context.Context, store *Store, tx pgx.Tx, channels []string) (settings.Settings, []string, error) {
	selected := make([]string, 0, len(channels))
	var view settings.Settings
	for _, channel := range channels {
		if !slices.Contains(digestEventChannels, channel) {
			continue
		}
		current, _, _, err := store.configurationTx(ctx, tx, channel)
		if err != nil {
			if errors.Is(err, ErrUnconfigured) {
				continue
			}
			return settings.Settings{}, nil, err
		}
		if len(selected) == 0 {
			view = current
		}
		selected = append(selected, channel)
	}
	return view, selected, nil
}

// configuredChannels filters the event's configured delivery channels.
func configuredChannels(ctx context.Context, store *Store, tx pgx.Tx, channels, allowed []string) ([]string, error) {
	selected := make([]string, 0, len(channels))
	for _, channel := range channels {
		if !slices.Contains(allowed, channel) {
			continue
		}
		if _, _, _, err := store.configurationTx(ctx, tx, channel); err != nil {
			if errors.Is(err, ErrUnconfigured) {
				continue
			}
			return nil, err
		}
		selected = append(selected, channel)
	}
	return selected, nil
}
