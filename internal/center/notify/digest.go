package notify

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

var digestEventChannels = []string{"telegram", "email"}

// PublishDigestEventTx records a daily or weekly digest and queues its enabled
// Telegram/e-mail destinations in the caller's transaction.
func (s *Store) PublishDigestEventTx(ctx context.Context, tx pgx.Tx, key string, payload map[string]string) error {
	if key != "digest.daily" && key != "digest.weekly" {
		return ErrInvalidEvent
	}
	_, err := s.publishEventTxWithChannelFilter(ctx, tx, key, "", payload, s.now(), "", digestEventChannels)
	return err
}

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
