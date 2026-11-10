package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // Pure Go keeps the agent independent of CGO.
)

const (
	DefaultMaxResults       = 10000
	DefaultMaxBytes   int64 = 64 << 20
	DefaultMaxAge           = 168 * time.Hour
)

// SpoolConfig bounds pending payload bytes, count and measurement age.
// Nonpositive limits select defaults. Dir must be private to one agent.
type SpoolConfig struct {
	Dir        string
	MaxResults int
	MaxBytes   int64
	MaxAge     time.Duration
}

// Spool is a transactional SQLite FIFO. FULL synchronous commits make a result
// durable before transport. Only committed acknowledgements remove results.
// The mutex serializes database operations; SQLite also locks across processes.
type Spool struct {
	mu     sync.Mutex
	db     *sql.DB
	cfg    SpoolConfig
	logger *slog.Logger
	now    func() time.Time
}

// Open opens or creates a private spool and enforces retention before replay.
func Open(cfg SpoolConfig, logger *slog.Logger) (*Spool, error) {
	if cfg.Dir == "" {
		return nil, errors.New("spool directory must not be empty")
	}
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = DefaultMaxResults
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("spool directory must have private permissions (0700)")
	}
	path := filepath.Join(cfg.Dir, "spool.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL; PRAGMA journal_mode=DELETE;
 CREATE TABLE IF NOT EXISTS command_state(id TEXT PRIMARY KEY, payload BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS rate_limit_state(engine TEXT PRIMARY KEY, deadline INTEGER NOT NULL, attempt INTEGER NOT NULL, recorded INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS pending(seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, ts INTEGER NOT NULL, payload BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS quarantine(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,ts INTEGER NOT NULL,payload BLOB NOT NULL);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Spool{db: db, cfg: cfg, logger: logger, now: time.Now}
	if err := s.Prune(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the SQLite connection. Reopening resumes pending deliveries.
func (s *Spool) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.db.Close() }

// Append commits a validated result before any sender can see it. Reusing an
// ID with identical content is harmless; conflicting content is rejected.
func (s *Spool) Append(r Result) error { return s.AppendWithState(r, "", nil) }

// AppendWithState commits a result and its composing command state together.
// State is private local metadata, never part of the wire result.
func (s *Spool) AppendWithState(r Result, key string, state json.RawMessage) error {
	return s.AppendWithMetadata(r, key, state, nil)
}

// AppendWithMetadata commits a result, its command state, and one rate-limit state update together.
func (s *Spool) AppendWithMetadata(r Result, key string, state json.RawMessage, update *RateLimitStateUpdate) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > maxBatchBytes-14 {
		return errors.New("result exceeds batch payload limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = appendPendingResult(tx, r, data); err != nil {
		return err
	}
	if err = appendCommandState(tx, key, state); err != nil {
		return err
	}
	if err = appendRateLimitState(tx, update); err != nil {
		return err
	}
	dropped, err := s.commitAppend(tx)
	if err != nil {
		return err
	}
	s.warn(dropped)
	return nil
}

func appendPendingResult(tx *sql.Tx, result Result, data []byte) error {
	var existing []byte
	err := tx.QueryRow(`SELECT payload FROM pending WHERE id=?`, result.ID).Scan(&existing)
	if err == nil {
		if string(existing) != string(data) {
			return errors.New("spool UUID content conflict")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ts, _ := time.Parse(time.RFC3339, result.TS)
	_, err = tx.Exec(`INSERT INTO pending(id,ts,payload) VALUES(?,?,?)`, result.ID, ts.Unix(), data)
	return err
}

func appendCommandState(tx *sql.Tx, key string, state json.RawMessage) error {
	if key == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO command_state(id,payload) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, key, []byte(state))
	return err
}

func appendRateLimitState(tx *sql.Tx, update *RateLimitStateUpdate) error {
	if update == nil {
		return nil
	}
	if update.Engine == "" {
		return errors.New("rate-limit engine must not be empty")
	}
	if update.State == nil {
		_, err := tx.Exec(`DELETE FROM rate_limit_state WHERE engine=?`, engineKey(update.Engine))
		return err
	}
	if update.State.Attempt < 1 || !update.State.Recorded || update.State.Deadline.IsZero() {
		return errors.New("invalid rate-limit state")
	}
	_, err := tx.Exec(`INSERT INTO rate_limit_state(engine,deadline,attempt,recorded) VALUES(?,?,?,1)
		ON CONFLICT(engine) DO UPDATE SET deadline=excluded.deadline,attempt=excluded.attempt,recorded=excluded.recorded`,
		engineKey(update.Engine), update.State.Deadline.UnixNano(), update.State.Attempt)
	return err
}

func (s *Spool) commitAppend(tx *sql.Tx) (int, error) {
	dropped, err := s.prune(tx, "pending")
	if err != nil {
		return 0, err
	}
	if _, err = s.prune(tx, "quarantine"); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return dropped, nil
}

// RateLimitStates reads all durable engine windows before the agent resumes work.
func (s *Spool) RateLimitStates() (map[string]RateLimitState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT engine,deadline,attempt,recorded FROM rate_limit_state ORDER BY engine`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	states := make(map[string]RateLimitState)
	for rows.Next() {
		var engine string
		var deadline int64
		var state RateLimitState
		var recorded int
		if err := rows.Scan(&engine, &deadline, &state.Attempt, &recorded); err != nil {
			return nil, err
		}
		if (engine != "ookla" && engine != "librespeed") || deadline <= 0 || state.Attempt < 1 || recorded != 1 {
			return nil, errors.New("invalid durable rate-limit state")
		}
		state.Deadline = time.Unix(0, deadline).UTC()
		state.Recorded = true
		states[engine] = state
	}
	return states, rows.Err()
}

// Prune drops oldest results until all limits hold, including during outages.
func (s *Spool) Prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	dropped, err := s.prune(tx, "pending")
	if err != nil {
		return err
	}
	if _, err = s.prune(tx, "quarantine"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.warn(dropped)
	return nil
}

func (s *Spool) prune(tx *sql.Tx, table string) (int, error) {
	var count int
	var total int64
	if err := tx.QueryRow(`SELECT count(*),coalesce(sum(length(payload)),0) FROM `+table).Scan(&count, &total); err != nil {
		return 0, err
	}
	dropped := 0
	for count > 0 {
		var seq, ts, size int64
		if err := tx.QueryRow(`SELECT seq,(SELECT min(ts) FROM `+table+`),length(payload) FROM `+table+` ORDER BY seq LIMIT 1`).Scan(&seq, &ts, &size); err != nil {
			return 0, err
		}
		if count <= s.cfg.MaxResults && total <= s.cfg.MaxBytes && s.now().Sub(time.Unix(ts, 0)) <= s.cfg.MaxAge {
			break
		}
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE seq=?`, seq); err != nil {
			return 0, err
		}
		count--
		total -= size
		dropped++
	}
	return dropped, nil
}

func (s *Spool) warn(n int) {
	if n > 0 {
		s.logger.Warn("spool cap reached; dropped oldest unsent result", "dropped", n)
	}
}

// Peek returns independent decoded copies of the oldest pending results.
func (s *Spool) Peek(n int) ([]Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		return nil, errors.New("batch size must be positive")
	}
	rows, err := s.db.Query(`SELECT payload FROM pending ORDER BY seq LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Result
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var r Result
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("decode spool: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Ack removes only the oldest result. A crash before commit replays its UUID.
func (s *Spool) Ack(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`DELETE FROM pending WHERE seq=(SELECT min(seq) FROM pending) AND id=?`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM pending WHERE id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("result is not the oldest pending result")
		}
		// Retention may drop an in-flight result while a concurrent producer appends.
	}
	return nil
}

// Quarantine records a safe rejection diagnostic and removes the rejected
// result. Payloads and server-provided strings are never written to logs.
func (s *Spool) Quarantine(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO quarantine(id,ts,payload) SELECT id,ts,payload FROM pending WHERE id=? AND seq=(SELECT min(seq) FROM pending)`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM pending WHERE id=? AND seq=(SELECT min(seq) FROM pending)`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM pending WHERE id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return errors.New("quarantine result is not the oldest pending result")
		}
	}
	if _, err = s.prune(tx, "quarantine"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.logger.Warn("center rejected result; quarantined", "id", id, "reason", reason)
	return nil
}

// SaveCommand durably records a bounded lifecycle command journal.
func (s *Spool) SaveCommand(id string, state json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO command_state(id,payload) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, id, []byte(state))
	return err
}

// DeleteCommand removes a terminal command evicted by the bounded history.
func (s *Spool) DeleteCommand(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM command_state WHERE id=?`, id)
	return err
}

// Commands returns independent lifecycle journal entries at startup.
func (s *Spool) Commands() (map[string]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id,payload FROM command_state`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]json.RawMessage)
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		out[id] = json.RawMessage(data)
	}
	return out, rows.Err()
}
