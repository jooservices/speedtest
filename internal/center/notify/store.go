package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jooservices/speedtest/internal/center/catalog"
	"github.com/jooservices/speedtest/internal/center/schedules"
	"github.com/jooservices/speedtest/internal/center/settings"
	"github.com/jooservices/speedtest/internal/shared/errtrack"
)

var (
	ErrUnconfigured = errors.New("notification channel is not configured")
	ErrNotFound     = errors.New("delivery not found")
)

// Preferences deliberately keeps credential access separate from public APIs.
type Preferences interface {
	NotificationConfig(context.Context, pgx.Tx, string) (settings.Settings, string, error)
}

// DeliveryFilter narrows the delivery list. Empty fields match every delivery.
type DeliveryFilter struct {
	Event   string
	Status  string
	Channel string
}

var deliveryStatuses = []string{"pending", "sent", "failed"}

// ValidDeliveryStatus reports whether status is a supported delivery outcome.
func ValidDeliveryStatus(status string) bool {
	return slices.Contains(deliveryStatuses, status)
}

// Delivery is safe metadata. Payloads, destinations and credentials are absent.
type Delivery struct {
	ID          string     `json:"id"`
	EventID     *string    `json:"eventId"`
	Channel     string     `json:"channel"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastAttempt *time.Time `json:"lastAttempt"`
	Error       *string    `json:"error"`
}

const (
	columns             = `id::text,event_id::text,channel,status,attempts,created_at,last_attempt,error`
	deliveryFilterWhere = `($1='' OR event_id=NULLIF($1,'')::uuid) AND ($2='' OR status=$2) AND ($3='' OR channel=$3)`
	deliveryCountSQL    = `SELECT count(*) FROM notification_deliveries WHERE ` + deliveryFilterWhere
	deliveryPageSQL     = `SELECT ` + columns + ` FROM notification_deliveries WHERE ` + deliveryFilterWhere + ` ORDER BY created_at DESC,id DESC LIMIT $4 OFFSET $5`
	// outboxPendingSQL and duePendingSQL are the claim predicates. The probes and the
	// locking selects share them, so the two cannot drift apart.
	outboxPendingSQL = `consumed_at IS NULL`
	duePendingSQL    = `status='pending' AND next_attempt_at<=$1`
)

// Store owns durable delivery creation and dispatch. Now is injectable for retry tests.
type Store struct {
	// ObserveRun reports pass success without changing worker behavior.
	ObserveRun func(bool)
	DB         *pgxpool.Pool
	// EventsDB optionally replaces DB for catalog override helpers (unit tests).
	EventsDB eventDB
	Settings Preferences
	Sender   Sender
	Now      func() time.Time
	Logger   *slog.Logger
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func Channel(view settings.Settings, channel string) (settings.ChannelConfig, bool) {
	switch channel {
	case "email":
		return view.Notify.Email, true
	case "telegram":
		return view.Notify.Telegram, true
	case "slack":
		return view.Notify.Slack, true
	case "webhook":
		return view.Notify.Webhook, true
	}
	return settings.ChannelConfig{}, false
}
func (s *Store) configuration(ctx context.Context, channel string) (settings.Settings, settings.ChannelConfig, string, error) {
	return s.configurationTx(ctx, nil, channel)
}
func (s *Store) configurationTx(ctx context.Context, tx pgx.Tx, channel string) (settings.Settings, settings.ChannelConfig, string, error) {
	view, secret, err := s.Settings.NotificationConfig(ctx, tx, channel)
	if err != nil {
		return view, settings.ChannelConfig{}, "", err
	}
	ch, ok := Channel(view, channel)
	facts, known := catalog.LookupChannel(channel)
	if !ok || !known || !ch.On || (facts.Destination != "webhook" && ch.To == "") {
		return view, ch, "", ErrUnconfigured
	}
	if facts.SecretRequired && secret == "" {
		return view, ch, "", ErrUnconfigured
	}
	return view, ch, secret, nil
}

// Test queues a test through the same retry path as an alert.
func (s *Store) Test(ctx context.Context, channel string) (Delivery, error) {
	if _, _, _, err := s.configuration(ctx, channel); err != nil {
		return Delivery{}, err
	}
	id := uuid.NewString()
	now := s.now()
	_, err := s.DB.Exec(ctx, `INSERT INTO notification_deliveries(id,channel,created_at,next_attempt_at) VALUES($1,$2,$3,$3)`, id, channel, now)
	if err != nil {
		return Delivery{}, err
	}
	return s.Delivery(ctx, id)
}
func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.EventID, &d.Channel, &d.Status, &d.Attempts, &d.CreatedAt, &d.LastAttempt, &d.Error)
	// The API contract uses UTC timestamps ending in Z, whatever the server's local zone is.
	d.CreatedAt = d.CreatedAt.UTC()
	if d.LastAttempt != nil {
		t := d.LastAttempt.UTC()
		d.LastAttempt = &t
	}
	return d, err
}
func (s *Store) Delivery(ctx context.Context, id string) (Delivery, error) {
	d, err := scanDelivery(s.DB.QueryRow(ctx, `SELECT `+columns+` FROM notification_deliveries WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return d, err
}
func (s *Store) Deliveries(ctx context.Context, filter DeliveryFilter, page, size int) ([]Delivery, int, error) {
	// Keep callers outside the HTTP layer from issuing an unbounded query.
	if page < 1 || size < 1 || size > 100 || page > 1000000 {
		return nil, 0, errors.New("invalid pagination")
	}
	var total int
	if err := s.DB.QueryRow(ctx, deliveryCountSQL, filter.Event, filter.Status, filter.Channel).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, deliveryPageSQL, filter.Event, filter.Status, filter.Channel, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, d)
	}
	return items, total, rows.Err()
}

// hasOutboxWork reports whether an unconsumed outbox event exists. It runs on the pool
// without a transaction, so an idle queue costs one query and no rollback.
func (s *Store) hasOutboxWork(ctx context.Context) (bool, error) {
	var work bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM alert_notification_outbox WHERE `+outboxPendingSQL+`)`).Scan(&work)
	return work, err
}

// hasDueDelivery reports whether a pending delivery is due at now, without a transaction.
func (s *Store) hasDueDelivery(ctx context.Context, now time.Time) (bool, error) {
	var work bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE `+duePendingSQL+`)`, now).Scan(&work)
	return work, err
}

// Consume atomically creates all channel rows and marks one outbox event handled.
// An idle queue returns before a transaction is opened.
func (s *Store) Consume(ctx context.Context) (bool, error) {
	work, err := s.hasOutboxWork(ctx)
	if err != nil {
		return false, err
	}
	if !work {
		return false, nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var event string
	var channels []string
	err = tx.QueryRow(ctx, `SELECT event_id::text,channels FROM alert_notification_outbox WHERE `+outboxPendingSQL+` ORDER BY created_at,event_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&event, &channels)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := s.now()
	for _, channel := range channels {
		if _, err = tx.Exec(ctx, `INSERT INTO notification_deliveries(id,event_id,channel,created_at,next_attempt_at) VALUES($1,$2,$3,$4,$4) ON CONFLICT(event_id,channel) DO NOTHING`, uuid.NewString(), event, channel, now); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE alert_notification_outbox SET consumed_at=$2 WHERE event_id=$1`, event, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Dispatch locks one due row while sending, preventing concurrent worker attempts.
// External providers cannot participate in the transaction: a crash after acceptance
// can cause redelivery. Logical event/channel creation is exactly once.
// An idle queue returns before a transaction is opened.
func (s *Store) Dispatch(ctx context.Context) (bool, error) {
	now := s.now()
	work, err := s.hasDueDelivery(ctx, now)
	if err != nil {
		return false, err
	}
	if !work {
		return false, nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	d, err := scanDelivery(tx.QueryRow(ctx, `SELECT `+columns+` FROM notification_deliveries WHERE `+duePendingSQL+` ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.settle(ctx, tx, d, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// settle decides what happens to one claimed delivery (hold, expire or attempt) and records
// the outcome in tx. The caller commits.
func (s *Store) settle(ctx context.Context, tx pgx.Tx, d Delivery, now time.Time) error {
	o, err := s.decide(ctx, tx, d, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE notification_deliveries SET status=$2,attempts=$3,last_attempt=$4,error=$5,next_attempt_at=$6 WHERE id=$1`, d.ID, o.status, o.attempts, o.last, o.safeError, o.next); err != nil {
		return err
	}
	if o.attemptErr != nil && o.attempts >= 3 && d.EventID != nil {
		return s.escalateDeliveryFailure(ctx, tx, d.ID, *d.EventID, d.Channel, o.attempts, now)
	}
	return nil
}

// settleOutcome is what settle records on a delivery row after deciding its fate.
type settleOutcome struct {
	status     string
	safeError  *string
	last       *time.Time
	next       time.Time
	attempts   int
	attemptErr error
}

// decide picks the outcome for one claimed delivery: hold for quiet hours, expire after 24 hours,
// or make an attempt.
func (s *Store) decide(ctx context.Context, tx pgx.Tx, d Delivery, now time.Time) (settleOutcome, error) {
	deadline := d.CreatedAt.Add(24 * time.Hour)
	if !now.Before(deadline) {
		text := "delivery expired after 24 hours"
		return settleOutcome{status: "failed", safeError: &text, last: d.LastAttempt, next: now, attempts: d.Attempts}, nil
	}
	if d.EventID != nil {
		held, err := heldByQuietHours(ctx, tx, *d.EventID, now)
		if err != nil {
			return settleOutcome{}, err
		}
		if held {
			// Quiet hours: the alert stays recorded and the delivery waits; look again in a minute.
			return settleOutcome{status: "pending", last: d.LastAttempt, next: now.Add(time.Minute), attempts: d.Attempts}, nil
		}
	}
	return s.attemptOutcome(ctx, tx, d, now, deadline), nil
}

// attemptOutcome sends the delivery and reports the row state that follows: sent, or pending
// again with a backoff that never passes the 24 hour deadline.
func (s *Store) attemptOutcome(ctx context.Context, tx pgx.Tx, d Delivery, now, deadline time.Time) settleOutcome {
	err := s.attempt(ctx, tx, d)
	o := settleOutcome{status: "sent", last: &now, next: now, attempts: d.Attempts + 1, attemptErr: err}
	if err != nil {
		text := "notification attempt failed"
		o.status = "pending"
		o.safeError = &text
		o.next = now.Add(Backoff(o.attempts))
		if o.next.After(deadline) {
			o.next = deadline
		}
	}
	return o
}

// heldByQuietHours reports whether an alert notification must wait because the owner's notification quiet hours
// are in force now. Critical alerts are exempt when the owner left that option on. Test messages never wait.
func heldByQuietHours(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, eventID string, now time.Time) (bool, error) {
	rows, err := q.Query(ctx, `SELECT key, value FROM settings WHERE key IN ('notifyQuietStart','notifyQuietEnd','notifyQuietTimezone','notifyQuietCritical') AND value IS NOT NULL`)
	if err != nil {
		return false, err
	}
	window := schedules.Quiet{Start: "23:00", End: "07:00", Timezone: "Asia/Bangkok"}
	critical := true
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return false, err
		}
		switch key {
		case "notifyQuietStart":
			_ = json.Unmarshal(raw, &window.Start)
		case "notifyQuietEnd":
			_ = json.Unmarshal(raw, &window.End)
		case "notifyQuietTimezone":
			_ = json.Unmarshal(raw, &window.Timezone)
		case "notifyQuietCritical":
			_ = json.Unmarshal(raw, &critical)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if !window.Contains(now) {
		return false, nil
	}
	var templateTest bool
	if err := q.QueryRow(ctx, `SELECT detail=$2 FROM alert_events WHERE id=$1`, eventID, templateTestDetail).Scan(&templateTest); err != nil {
		return false, err
	}
	if templateTest {
		return false, nil
	}
	if critical {
		var severity string
		if err := q.QueryRow(ctx, `SELECT severity FROM alert_events WHERE id=$1`, eventID).Scan(&severity); err != nil {
			return false, err
		}
		if severity == "crit" {
			return false, nil
		}
	}
	return true, nil
}

// Loop drains bounded batches and stops promptly with the center context.
func (s *Store) Loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ok := true
		for i := 0; i < 100 && ctx.Err() == nil; i++ {
			consumed, e1 := s.Consume(ctx)
			dispatched, e2 := s.Dispatch(ctx)
			if e1 != nil || e2 != nil {
				ok = false
				if ctx.Err() == nil {
					if s.Logger != nil {
						s.Logger.Error("notification worker iteration failed")
					}
					errtrack.CaptureError(errors.Join(e1, e2), map[string]string{"loop": "notifications"})
				}
				break
			}
			if !consumed && !dispatched {
				break
			}
		}
		if s.ObserveRun != nil {
			s.ObserveRun(ok && ctx.Err() == nil)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Store) attempt(ctx context.Context, tx pgx.Tx, d Delivery) error {
	attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	view, ch, secret, sendErr := s.configurationTx(attemptCtx, tx, d.Channel)
	if sendErr != nil {
		return sendErr
	}
	text, err := s.renderDelivery(attemptCtx, tx, d, view.CenterURL)
	if err != nil {
		return err
	}
	return s.Sender.Send(attemptCtx, d.Channel, ch.To, secret, text)
}
