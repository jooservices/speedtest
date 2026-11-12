package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const loginFailureWindowMinutes = 10

// RecordSignIn publishes once when an owner signs in from a previously unseen IP.
func (s *Store) RecordSignIn(ctx context.Context, userKey, ip string) error {
	if userKey == "" || ip == "" {
		return nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `INSERT INTO notification_signin_locations(user_key,ip) VALUES($1,$2)
		ON CONFLICT(user_key,ip) DO NOTHING`, userKey, ip)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		if err := s.PublishEventTx(ctx, tx, "security.signin_new", "", map[string]string{"ip": ip}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RecordLoginFailure counts invalid credentials by source IP and publishes at the configured threshold.
func (s *Store) RecordLoginFailure(ctx context.Context, ip string) error {
	if ip == "" {
		return nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	ts := s.now()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ip); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM notification_login_failures WHERE attempted_at < $1::timestamptz - interval '1 day'`, ts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO notification_login_failures(ip,attempted_at) VALUES($1,$2)`, ip, ts); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM notification_login_failures
		WHERE ip=$1 AND attempted_at >= $2::timestamptz - $3::double precision * interval '1 minute'`, ip, ts, loginFailureWindowMinutes).Scan(&count); err != nil {
		return err
	}
	if err := s.publishLoginBurst(ctx, tx, ip, ts, count); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) publishLoginBurst(ctx context.Context, tx pgx.Tx, ip string, ts time.Time, count int) error {
	event, err := (&EventStore{DB: tx}).GetEvent(ctx, "security.login_burst")
	if err != nil {
		return err
	}
	threshold := int(event.Thresholds["attempts"])
	if threshold < 1 {
		threshold = 5
	}
	if count != threshold {
		return nil
	}
	return s.PublishEventTx(ctx, tx, "security.login_burst", "", map[string]string{"count": fmt.Sprint(count), "ip": ip})
}
