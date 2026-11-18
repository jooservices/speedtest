package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/settings"
)

var digestEventChannels = []string{"telegram", "email"}

const maxDigestChunkTextUnits = 3400

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
	if err != nil {
		return err
	}
	if !publish || !slices.Contains(channels, "telegram") {
		return recordEventTx(ctx, tx, event, "", data, now, channels, publish)
	}
	return s.recordDigestTelegramParts(ctx, tx, event, payload, channels, now)
}

func (s *Store) recordDigestTelegramParts(ctx context.Context, tx pgx.Tx, event EventView, payload map[string]string, channels []string, now time.Time) error {
	chunks := splitDigestTelegramSummary(payload["summary"])
	total := fmt.Sprint(len(chunks))
	for index, chunk := range chunks {
		partPayload := mapsClone(payload)
		partPayload["telegram_summary"] = chunk
		partPayload["telegram_part"] = fmt.Sprint(index + 1)
		partPayload["telegram_parts"] = total
		partChannels := channels
		if index > 0 || !slices.Contains(channels, "email") {
			partPayload["summary"] = chunk
		}
		if index > 0 {
			partChannels = []string{"telegram"}
		}
		data, err := buildEventPayload(ctx, tx, event, "", partPayload, now)
		if err != nil {
			return err
		}
		if err := recordEventTx(ctx, tx, event, "", data, now, partChannels, true); err != nil {
			return err
		}
	}
	return nil
}

func splitDigestTelegramSummary(summary string) []string {
	if summary == "" {
		return []string{""}
	}
	var chunks []string
	var current strings.Builder
	units := 0
	for _, line := range strings.SplitAfter(summary, "\n") {
		lineUnits := telegramEscapedTextUnits(line)
		if lineUnits <= maxDigestChunkTextUnits {
			if units+lineUnits > maxDigestChunkTextUnits && current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
				units = 0
			}
			current.WriteString(line)
			units += lineUnits
			continue
		}
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			units = 0
		}
		for _, character := range line {
			characterUnits := telegramEscapedRuneUnits(character)
			if units+characterUnits > maxDigestChunkTextUnits && current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
				units = 0
			}
			current.WriteRune(character)
			units += characterUnits
		}
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func telegramEscapedRuneUnits(character rune) int {
	switch character {
	case '&':
		return 5
	case '<', '>':
		return 4
	case '"':
		return 6
	default:
		if character > 0xFFFF {
			return 2
		}
		return 1
	}
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
