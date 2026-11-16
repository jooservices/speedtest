package settings

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store reads and writes the command center's settings and enrolment key.
// Implementations must never expose stored secrets.
type Store interface {
	Get(ctx context.Context) (Settings, string, error)
	Replace(ctx context.Context, ifMatch string, input SettingsInput) (Settings, string, error)
	RotateEnrollmentKey(ctx context.Context) (IssuedKey, error)
}

// Store-level errors mapped to HTTP responses by the server.
var (
	// ErrStale reports an If-Match value that does not match the current revision.
	ErrStale = errors.New("settings changed; reload and retry")
	// ErrPreconditionRequired reports a missing If-Match header on a replace.
	ErrPreconditionRequired = errors.New("If-Match header required")
)

// db is the subset of pgx behavior used by the store, satisfied by both
// *pgxpool.Pool and pgx.Tx.
type db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// pool is the part of *pgxpool.Pool the store uses; tests wrap it to inject failures.
type pool interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// PostgresStore persists settings in the settings table. Public preferences
// live under key "display"; recoverable secrets are AES-GCM ciphertext under
// their own keys; the enrolment key is stored as a SHA-256 hash.
type PostgresStore struct {
	pool     pool
	cipher   *Cipher
	defaults Settings
}

// NewPostgresStore wraps a connection pool. defaults supplies the values used
// before the owner has saved anything.
func NewPostgresStore(connections *pgxpool.Pool, cipher *Cipher, defaults Settings) *PostgresStore {
	return &PostgresStore{pool: connections, cipher: cipher, defaults: defaults}
}

// Get returns the current public settings (secrets masked) and a strong ETag
// derived from the revision column.
func (s *PostgresStore) Get(ctx context.Context) (Settings, string, error) {
	view := s.defaults
	// Before anything is saved the revision is 0, so the first save always moves the ETag.
	revision := int64(0)

	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value, revision FROM settings WHERE key = 'display'`).Scan(&raw, &revision)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No saved preferences yet; defaults apply.
	case err != nil:
		return Settings{}, "", err
	default:
		var stored Settings
		if err := json.Unmarshal(raw, &stored); err != nil {
			return Settings{}, "", errors.New("stored settings are malformed")
		}
		view = stored
		// Rows saved before the language setting existed read as the default language.
		if view.Language == "" {
			view.Language = DefaultLanguage
		}
	}

	if err := overlayShared(ctx, s.pool, &view); err != nil {
		return Settings{}, "", err
	}

	view.GeoKeyConfigured, err = secretPresent(ctx, s.pool, keyGeoSecret)
	if err != nil {
		return Settings{}, "", err
	}
	if view.Notify.Email.Configured, err = secretPresent(ctx, s.pool, keyEmailSecret); err != nil {
		return Settings{}, "", err
	}
	if view.Notify.Telegram.Configured, err = secretPresent(ctx, s.pool, keyTelegramSecret); err != nil {
		return Settings{}, "", err
	}
	if view.Notify.Slack.Configured, err = secretPresent(ctx, s.pool, keySlackSecret); err != nil {
		return Settings{}, "", err
	}
	if view.Notify.Webhook.Configured, err = secretPresent(ctx, s.pool, keyWebhookSecret); err != nil {
		return Settings{}, "", err
	}
	if view.EnrollKeyConfigured, err = enrolmentPresent(ctx, s.pool); err != nil {
		return Settings{}, "", err
	}

	return view, quoteETag(revision), nil
}

// Replace atomically validates the If-Match precondition, stores the new
// preferences, and writes any supplied secrets. Omitted secrets are preserved.
func (s *PostgresStore) Replace(ctx context.Context, ifMatch string, input SettingsInput) (Settings, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Settings{}, "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	current, err := displayRevision(ctx, tx)
	if err != nil {
		return Settings{}, "", err
	}
	if ifMatch == "" {
		return Settings{}, "", ErrPreconditionRequired
	}
	if ifMatch != quoteETag(current) {
		return Settings{}, "", ErrStale
	}

	view, err := s.replaceSecrets(ctx, tx, input)
	if err != nil {
		return Settings{}, "", err
	}

	data, err := json.Marshal(view)
	if err != nil {
		return Settings{}, "", errors.New("cannot encode settings")
	}
	if err := writeShared(ctx, tx, view); err != nil {
		return Settings{}, "", err
	}
	var newRevision int64
	err = tx.QueryRow(ctx, `
		INSERT INTO settings (key, value, revision, updated_at)
		VALUES ('display', $1, 1, now())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, revision = settings.revision + 1, updated_at = now()
		RETURNING revision`, data).Scan(&newRevision)
	if err != nil {
		return Settings{}, "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return Settings{}, "", err
	}
	return view, quoteETag(newRevision), nil
}

// RotateEnrollmentKey generates a fresh key, stores only its SHA-256 hash, and
// returns the plaintext exactly once.
func (s *PostgresStore) RotateEnrollmentKey(ctx context.Context) (IssuedKey, error) {
	token, err := generateEnrollmentKey()
	if err != nil {
		return IssuedKey{}, err
	}
	createdAt := time.Now().UTC()

	payload, err := json.Marshal(enrolmentRecord{Hash: hashToken(token), CreatedAt: createdAt.Format(time.RFC3339)})
	if err != nil {
		return IssuedKey{}, errors.New("cannot encode enrolment key")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssuedKey{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	defaults, err := json.Marshal(s.defaults)
	if err != nil {
		return IssuedKey{}, errors.New("cannot encode default settings")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO settings (key, value, revision, updated_at) VALUES ('display', $1, 1, now()) ON CONFLICT (key) DO NOTHING`, defaults); err != nil {
		return IssuedKey{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO settings (key, value, revision, updated_at)
		VALUES ('enrolment', $1, 1, now())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, revision = settings.revision + 1, updated_at = now()`, payload); err != nil {
		return IssuedKey{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE settings SET revision = revision + 1, updated_at = now() WHERE key = 'display'`); err != nil {
		return IssuedKey{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedKey{}, err
	}

	return IssuedKey{EnrollToken: token, CreatedAt: createdAt.Format(time.RFC3339)}, nil
}

type enrolmentRecord struct {
	Hash      string `json:"hash"`
	CreatedAt string `json:"createdAt"`
}

// writeSecret encrypts and stores a supplied secret, or reports whether a
// secret is already stored when the write value is nil.
func (s *PostgresStore) writeSecret(ctx context.Context, tx pgx.Tx, key string, secret *string) (bool, error) {
	if secret == nil {
		return secretPresent(ctx, tx, key)
	}
	data, err := s.cipher.Encrypt([]byte(*secret))
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO settings (key, secret_ciphertext, revision, updated_at)
		VALUES ($1, $2, 1, now())
		ON CONFLICT (key) DO UPDATE
		SET secret_ciphertext = EXCLUDED.secret_ciphertext, value = NULL,
		    revision = settings.revision + 1, updated_at = now()`, key, data)
	if err != nil {
		return false, err
	}
	return true, nil
}

func secretPresent(ctx context.Context, d db, key string) (bool, error) {
	var exists bool
	err := d.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key = $1 AND secret_ciphertext IS NOT NULL)`, key).Scan(&exists)
	return exists, err
}

func enrolmentPresent(ctx context.Context, d db) (bool, error) {
	var exists bool
	err := d.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key = 'enrolment')`).Scan(&exists)
	return exists, err
}

func displayRevision(ctx context.Context, d db) (int64, error) {
	var revision int64
	err := d.QueryRow(ctx, `SELECT revision FROM settings WHERE key = 'display'`).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func quoteETag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

func generateEnrollmentKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("cannot generate enrolment key")
	}
	return enrollmentPrefix + hex.EncodeToString(raw), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sharedKeys are the settings other components read as their own rows: the retention job, the agent configuration
// and the schedule rules. They are the single source of truth for those values; the "display" row keeps a copy only
// so that older rows still decode.
var sharedKeys = []string{"retentionDays", "engine", "quietStart", "quietEnd", "stagger", "maxConcurrent", "notifyQuietStart", "notifyQuietEnd", "notifyQuietTimezone", "notifyQuietCritical"}

// overlayShared replaces the stored copy of the shared values with the rows the other components use.
func overlayShared(ctx context.Context, d interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, view *Settings) error {
	rows, err := d.Query(ctx, `SELECT key, value FROM settings WHERE key = ANY($1) AND value IS NOT NULL`, sharedKeys)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		switch key {
		case "retentionDays":
			_ = json.Unmarshal(raw, &view.RetentionDays)
		case "engine":
			_ = json.Unmarshal(raw, &view.Engine)
		case "quietStart":
			_ = json.Unmarshal(raw, &view.QuietStart)
		case "quietEnd":
			_ = json.Unmarshal(raw, &view.QuietEnd)
		case "notifyQuietStart":
			_ = json.Unmarshal(raw, &view.NotifyQuietStart)
		case "notifyQuietEnd":
			_ = json.Unmarshal(raw, &view.NotifyQuietEnd)
		case "notifyQuietTimezone":
			_ = json.Unmarshal(raw, &view.NotifyQuietZone)
		case "notifyQuietCritical":
			_ = json.Unmarshal(raw, &view.NotifyQuietCritical)
		case "stagger":
			_ = json.Unmarshal(raw, &view.Stagger)
		case "maxConcurrent":
			_ = json.Unmarshal(raw, &view.MaxConcurrent)
		}
	}
	return rows.Err()
}

// writeShared stores the shared values under their own keys. When quiet hours, the stagger flag or the concurrency
// limit change, every schedule revision moves so that agents fetch their new configuration.
func writeShared(ctx context.Context, tx pgx.Tx, view Settings) error {
	before := Settings{}
	if err := overlayShared(ctx, tx, &before); err != nil {
		return err
	}
	values := map[string]any{"retentionDays": view.RetentionDays, "engine": view.Engine, "quietStart": view.QuietStart,
		"quietEnd": view.QuietEnd, "stagger": view.Stagger, "maxConcurrent": view.MaxConcurrent,
		"notifyQuietStart": view.NotifyQuietStart, "notifyQuietEnd": view.NotifyQuietEnd, "notifyQuietTimezone": view.NotifyQuietZone, "notifyQuietCritical": view.NotifyQuietCritical}
	for _, key := range sharedKeys {
		raw, err := json.Marshal(values[key])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2::jsonb)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, secret_ciphertext = NULL,
			revision = settings.revision + 1, updated_at = now() WHERE settings.value IS DISTINCT FROM EXCLUDED.value`, key, string(raw)); err != nil {
			return err
		}
	}
	zoneChanged := false
	// Agents read the quiet-hours zone from the flat timezone row, which must be an IANA name. A display zone of
	// "browser" has to be resolved by the client first, so it never overwrites a usable zone.
	if zone := view.Timezone; zone != "" && zone != "browser" {
		if _, err := time.LoadLocation(zone); err == nil {
			raw, _ := json.Marshal(zone)
			res, err := tx.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('timezone', $1::jsonb)
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, secret_ciphertext = NULL,
				revision = settings.revision + 1, updated_at = now() WHERE settings.value IS DISTINCT FROM EXCLUDED.value`, string(raw))
			if err != nil {
				return err
			}
			if res.RowsAffected() > 0 {
				zoneChanged = true
			}
		}
	}
	if zoneChanged || before.QuietStart != view.QuietStart || before.QuietEnd != view.QuietEnd || before.Stagger != view.Stagger || before.MaxConcurrent != view.MaxConcurrent || before.Engine != view.Engine {
		if _, err := tx.Exec(ctx, `UPDATE engine_schedules SET revision=revision+1`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE schedules SET revision = revision + 1, updated_at = now()`); err != nil {
			return err
		}
	}
	return nil
}

func replacementView(input SettingsInput, geoConfigured, enrolConfigured, emailConfigured, telegramConfigured, slackConfigured, webhookConfigured bool) Settings {
	view := Settings{
		Theme:               input.Theme,
		Language:            input.Language,
		Range:               input.Range,
		Units:               input.Units,
		Timezone:            input.Timezone,
		Hour12:              input.Hour12,
		Decimals:            input.Decimals,
		RetentionDays:       input.RetentionDays,
		GeoProvider:         input.GeoProvider,
		GeoFallback:         input.GeoFallback,
		GeoAutoUpdate:       input.GeoAutoUpdate,
		Engine:              input.Engine,
		DefaultThreads:      input.DefaultThreads,
		ServerMode:          input.ServerMode,
		MaxConcurrent:       input.MaxConcurrent,
		Stagger:             input.Stagger,
		QuietStart:          input.QuietStart,
		QuietEnd:            input.QuietEnd,
		NotifyQuietStart:    input.NotifyQuietStart,
		NotifyQuietEnd:      input.NotifyQuietEnd,
		NotifyQuietZone:     input.NotifyQuietZone,
		NotifyQuietCritical: input.NotifyQuietCritical,
		CenterURL:           input.CenterURL,
		GeoKeyConfigured:    geoConfigured,
		EnrollKeyConfigured: enrolConfigured,
		Notify: NotificationSettings{
			Email:    ChannelConfig{On: input.Notify.Email.On, To: input.Notify.Email.To, Configured: emailConfigured},
			Telegram: ChannelConfig{On: input.Notify.Telegram.On, To: input.Notify.Telegram.To, Configured: telegramConfigured},
			Slack:    ChannelConfig{On: input.Notify.Slack.On, To: input.Notify.Slack.To, Configured: slackConfigured},
			Webhook:  ChannelConfig{On: input.Notify.Webhook.On, To: input.Notify.Webhook.To, Configured: webhookConfigured},
		},
	}
	return view
}

func (s *PostgresStore) replaceSecrets(ctx context.Context, tx pgx.Tx, input SettingsInput) (Settings, error) {
	geoConfigured, err := s.writeSecret(ctx, tx, keyGeoSecret, input.GeoKey)
	if err != nil {
		return Settings{}, err
	}
	emailConfigured, err := s.writeSecret(ctx, tx, keyEmailSecret, input.Notify.Email.Secret)
	if err != nil {
		return Settings{}, err
	}
	telegramConfigured, err := s.writeSecret(ctx, tx, keyTelegramSecret, input.Notify.Telegram.Secret)
	if err != nil {
		return Settings{}, err
	}
	slackConfigured, err := s.writeSecret(ctx, tx, keySlackSecret, input.Notify.Slack.Secret)
	if err != nil {
		return Settings{}, err
	}
	webhookConfigured, err := s.writeSecret(ctx, tx, keyWebhookSecret, input.Notify.Webhook.Secret)
	if err != nil {
		return Settings{}, err
	}
	enrolConfigured, err := enrolmentPresent(ctx, tx)
	if err != nil {
		return Settings{}, err
	}

	return replacementView(input, geoConfigured, enrolConfigured, emailConfigured, telegramConfigured, slackConfigured, webhookConfigured), nil
}
