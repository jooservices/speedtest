package installer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Database is the narrow PostgreSQL surface used by installer state.
type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PostgresStore persists safe probes, install states, and replayable progress.
// SSH passwords, private keys, and enrollment tokens are never accepted here.
// The application key protects idempotency comparisons for install requests
// without leaving an offline-guessable digest of credentials in PostgreSQL.
type PostgresStore struct {
	db     Database
	appKey []byte
}

func NewPostgresStore(db Database) *PostgresStore { return &PostgresStore{db: db} }

func NewPostgresStoreWithKey(db Database, appKey []byte) *PostgresStore {
	return &PostgresStore{db: db, appKey: append([]byte(nil), appKey...)}
}

func (s *PostgresStore) saveProbe(ctx context.Context, principal, key string, input ProbeInput, probe SSHProbe) (SSHProbe, error) {
	requestHash, err := hashJSON(input)
	if err != nil {
		return SSHProbe{}, err
	}
	keyHash := digest(key)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return SSHProbe{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockIdempotency(ctx, tx, principal, keyHash); err != nil {
		return SSHProbe{}, err
	}
	var storedHash, storedID string
	err = tx.QueryRow(ctx, "SELECT request_hash,probe_id::text FROM installer_probe_idempotency WHERE principal=$1 AND key_hash=$2 AND expires_at>now() FOR UPDATE", principal, keyHash).Scan(&storedHash, &storedID)
	if err == nil {
		if storedHash != requestHash {
			return SSHProbe{}, ErrConflict
		}
		replayed, readErr := scanProbe(tx.QueryRow(ctx, "SELECT id::text,fingerprint,expires_at FROM installer_probes WHERE id=$1", storedID))
		if readErr != nil || !replayed.ExpiresAt.After(time.Now().UTC()) {
			return SSHProbe{}, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return SSHProbe{}, err
		}
		return replayed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SSHProbe{}, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM installer_probe_idempotency WHERE expires_at<=now()"); err != nil {
		return SSHProbe{}, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM installer_probes WHERE expires_at<=now()"); err != nil {
		return SSHProbe{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO installer_probes(id,host,port,fingerprint,expires_at) VALUES($1,$2,$3,$4,$5)",
		probe.ID, input.Host, input.Port, probe.Fingerprint, probe.ExpiresAt); err != nil {
		return SSHProbe{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO installer_probe_idempotency(principal,key_hash,request_hash,probe_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '24 hours')", principal, keyHash, requestHash, probe.ID); err != nil {
		return SSHProbe{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SSHProbe{}, err
	}
	return probe, nil
}

func (s *PostgresStore) createInstallation(ctx context.Context, principal, key, id, nodeID string, input InstallInput) (Installation, bool, error) {
	requestHash, err := keyedHashJSON(s.appKey, struct {
		NodeID string
		Input  InstallInput
	}{nodeID, input})
	if err != nil {
		return Installation{}, false, err
	}
	keyHash := digest(key)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Installation{}, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockIdempotency(ctx, tx, principal, keyHash); err != nil {
		return Installation{}, false, err
	}
	var storedHash, storedID string
	err = tx.QueryRow(ctx, "SELECT request_hash,installation_id::text FROM installer_idempotency WHERE principal=$1 AND key_hash=$2 AND expires_at>now() FOR UPDATE", principal, keyHash).Scan(&storedHash, &storedID)
	if err == nil {
		if !hmac.Equal([]byte(storedHash), []byte(requestHash)) {
			return Installation{}, false, ErrConflict
		}
		replayed, readErr := scanInstallation(tx.QueryRow(ctx, "SELECT id::text,node_id::text,status,step,error FROM installer_jobs WHERE id=$1", storedID))
		if readErr != nil {
			return Installation{}, false, readErr
		}
		if err := tx.Commit(ctx); err != nil {
			return Installation{}, false, err
		}
		return replayed, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, false, err
	}

	if err := validateInstallProbe(ctx, tx, input); err != nil {
		return Installation{}, false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM nodes WHERE id=$1)", nodeID).Scan(&exists); err != nil {
		return Installation{}, false, err
	}
	if !exists {
		return Installation{}, false, ErrNotFound
	}
	if _, err := tx.Exec(ctx, "DELETE FROM installer_jobs WHERE status IN ('succeeded','failed') AND updated_at < now()-interval '30 days'"); err != nil {
		return Installation{}, false, err
	}
	job := Installation{ID: id, NodeID: nodeID, Status: "pending", Step: "queued"}
	if _, err := tx.Exec(ctx, "INSERT INTO installer_jobs(id,node_id,principal,status,step) VALUES($1,$2,$3,'pending','queued')", job.ID, job.NodeID, principal); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "installer_jobs_one_active_per_node_idx" {
			return Installation{}, false, ErrBusy
		}
		return Installation{}, false, err
	}
	return finishInstallation(ctx, tx, job, input.ProbeID, principal, keyHash, requestHash)
}

func (s *PostgresStore) installation(ctx context.Context, id string) (Installation, error) {
	if !validID(id) {
		return Installation{}, ErrNotFound
	}
	return scanInstallation(s.db.QueryRow(ctx, "SELECT id::text,node_id::text,status,step,error FROM installer_jobs WHERE id=$1", id))
}

func (s *PostgresStore) appendEvent(ctx context.Context, installationID, step, message string, pct float64, status string, safeError *string) error {
	if pct < 0 || pct > 100 || message == "" || step == "" {
		return ErrInvalid
	}
	switch status {
	case "running", "succeeded", "failed":
	default:
		return ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var current string
	err = tx.QueryRow(ctx, "SELECT status FROM installer_jobs WHERE id=$1 FOR UPDATE", installationID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if current == "succeeded" || current == "failed" {
		return nil
	}
	var high float64
	if err := tx.QueryRow(ctx, "SELECT COALESCE(max(pct),0) FROM installer_events WHERE installation_id=$1", installationID).Scan(&high); err != nil {
		return err
	}
	if pct < high {
		pct = high
	}
	eventID, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE installer_jobs SET status=$2,step=$3,error=$4,updated_at=now() WHERE id=$1", installationID, status, step, safeError); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO installer_events(id,installation_id,step,message,pct,status) VALUES($1,$2,$3,$4,$5,$6)", eventID, installationID, step, message, pct, status); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) events(ctx context.Context, installationID, cursor string) ([]Event, Installation, error) {
	job, err := s.installation(ctx, installationID)
	if err != nil {
		return nil, Installation{}, err
	}
	var after int64
	if cursor != "" {
		if !validID(cursor) {
			return nil, Installation{}, ErrInvalid
		}
		err := s.db.QueryRow(ctx, "SELECT seq FROM installer_events WHERE id=$1 AND installation_id=$2", cursor, installationID).Scan(&after)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, Installation{}, ErrCursorExpired
		}
		if err != nil {
			return nil, Installation{}, err
		}
	}
	rows, err := s.db.Query(ctx, "SELECT id::text,ts,step,message,pct,status FROM installer_events WHERE installation_id=$1 AND seq>$2 ORDER BY seq LIMIT 100", installationID, after)
	if err != nil {
		return nil, Installation{}, err
	}
	defer rows.Close()
	events := make([]Event, 0, 8)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.TS, &event.Step, &event.Message, &event.Pct, &event.Status); err != nil {
			return nil, Installation{}, err
		}
		event.TS = event.TS.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, Installation{}, err
	}
	return events, job, nil
}

func (s *PostgresStore) lastHeartbeat(ctx context.Context, nodeID string) (*time.Time, error) {
	var stamp pgtype.Timestamptz
	err := s.db.QueryRow(ctx, "SELECT last_seen_at FROM nodes WHERE id=$1", nodeID).Scan(&stamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !stamp.Valid {
		return nil, nil
	}
	value := stamp.Time.UTC()
	return &value, nil
}

func (s *PostgresStore) recoverInterrupted(ctx context.Context) error {
	rows, err := s.db.Query(ctx, "SELECT id::text FROM installer_jobs WHERE status IN ('pending','running')")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		message := "The command center restarted before installation finished. Retry the installation."
		if err := s.appendEvent(ctx, id, "failed", message, 100, "failed", stringPtr(message)); err != nil {
			return err
		}
	}
	return nil
}

func scanProbe(row pgx.Row) (SSHProbe, error) {
	var probe SSHProbe
	err := row.Scan(&probe.ID, &probe.Fingerprint, &probe.ExpiresAt)
	probe.ExpiresAt = probe.ExpiresAt.UTC()
	return probe, err
}

func scanInstallation(row pgx.Row) (Installation, error) {
	var job Installation
	var message pgtype.Text
	err := row.Scan(&job.ID, &job.NodeID, &job.Status, &job.Step, &message)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Installation{}, ErrNotFound
		}
		return Installation{}, err
	}
	if message.Valid {
		job.Error = stringPtr(message.String)
	}
	return job, nil
}

func lockIdempotency(ctx context.Context, tx pgx.Tx, principal, keyHash string) error {
	sum := sha256.Sum256([]byte(principal + "\x00" + keyHash))
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8])))
	return err
}

func hashJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digest(string(raw)), nil
}

func keyedHashJSON(key []byte, value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stringPtr(value string) *string { return &value }

func validID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	raw := hex.EncodeToString(value[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", raw[:8], raw[8:12], raw[12:16], raw[16:20], raw[20:]), nil
}

func validateInstallProbe(ctx context.Context, tx pgx.Tx, input InstallInput) error {
	var probeHost, probeFingerprint string
	var probePort int
	var expiresAt time.Time
	var consumedAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, "SELECT host,port,fingerprint,expires_at,consumed_at FROM installer_probes WHERE id=$1 FOR UPDATE", input.ProbeID).Scan(&probeHost, &probePort, &probeFingerprint, &expiresAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !expiresAt.After(time.Now().UTC()) || consumedAt.Valid ||
		!strings.EqualFold(probeHost, input.SSH.Host) || probePort != input.SSH.Port ||
		probeFingerprint != input.Fingerprint {
		return ErrConflict
	}
	return nil
}

func finishInstallation(ctx context.Context, tx pgx.Tx, job Installation, probeID, principal, keyHash, requestHash string) (Installation, bool, error) {
	eventID, err := newID()
	if err != nil {
		return Installation{}, false, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO installer_events(id,installation_id,step,message,pct,status) VALUES($1,$2,'queued','The installation was queued.',0,'running')", eventID, job.ID); err != nil {
		return Installation{}, false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE installer_probes SET consumed_at=now() WHERE id=$1", probeID); err != nil {
		return Installation{}, false, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM installer_idempotency WHERE expires_at<=now()"); err != nil {
		return Installation{}, false, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO installer_idempotency(principal,key_hash,request_hash,installation_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '24 hours')", principal, keyHash, requestHash, job.ID); err != nil {
		return Installation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Installation{}, false, err
	}
	return job, false, nil
}
