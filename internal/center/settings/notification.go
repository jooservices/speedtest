package settings

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// NotificationSecret decrypts a single channel credential for the delivery worker.
// It is deliberately absent from the public Store interface and HTTP read model.
func (s *PostgresStore) NotificationSecret(ctx context.Context, channel string) (string, error) {
	switch channel {
	case "email", "telegram", "slack", "webhook":
	default:
		return "", errors.New("unknown notification channel")
	}
	var encrypted []byte
	err := s.pool.QueryRow(ctx, `SELECT secret_ciphertext FROM settings WHERE key=$1`, "notify."+channel+".secret").Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("cannot read notification credential")
	}
	plain, err := s.cipher.Decrypt(encrypted)
	if err != nil {
		return "", errors.New("cannot decrypt notification credential")
	}
	return string(plain), nil
}

// NotificationConfig reads public preferences and a sender credential using the
// claimed delivery transaction. Reusing its connection avoids pool starvation
// when concurrent workers hold all available transaction connections.
func (s *PostgresStore) NotificationConfig(ctx context.Context, tx pgx.Tx, channel string) (Settings, string, error) {
	reader := *s
	if tx != nil {
		reader.pool = tx
	}
	view, _, err := reader.Get(ctx)
	if err != nil {
		return Settings{}, "", err
	}
	secret, err := reader.NotificationSecret(ctx, channel)
	return view, secret, err
}
