package alerts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jooservices/speedtest/internal/center/jobs"
)

// DB is the subset of pgxpool.Pool used by the alert engine and API.
type DB interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Store owns alert rule/event reads and writes and the incremental evaluator.
type Store struct {
	// ObserveRun reports pass success without changing worker behavior.
	ObserveRun func(bool)
	DB         DB
	AppKey     []byte
	Now        func() time.Time
	Random     io.Reader
	Logger     *slog.Logger
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) random() io.Reader {
	if s.Random != nil {
		return s.Random
	}
	return rand.Reader
}

const ruleColumns = `id::text,name,metric,op,value,for_tests,node_ids IS NULL,
	COALESCE(node_ids::text[],ARRAY[]::text[]),severity,channels,enabled,revision,COALESCE(engine,'')`

type rowScanner interface{ Scan(...any) error }

func scanRule(row rowScanner) (Rule, error) {
	var r Rule
	var all bool
	err := row.Scan(&r.ID, &r.Name, &r.Metric, &r.Op, &r.Value, &r.ForTests, &all, &r.NodeIDs, &r.Severity, &r.Channels, &r.Enabled, &r.Revision, &r.Engine)
	if err != nil {
		return Rule{}, err
	}
	r.AllNodes = all
	r = ruleView(r)
	return r, nil
}

const eventColumns = `id::text,rule_id::text,node_id::text,isp_id::text,severity,ts,resolved_at,title,detail,acked`

func qualifyEventColumns(alias string) string {
	return alias + `.id::text,` + alias + `.rule_id::text,` + alias + `.node_id::text,` + alias + `.isp_id::text,` +
		alias + `.severity,` + alias + `.ts,` + alias + `.resolved_at,` + alias + `.title,` + alias + `.detail,` + alias + `.acked`
}

func scanEvent(row rowScanner) (Event, error) {
	var e Event
	var ruleID *string
	var nodeID *string
	var ts time.Time
	var resolved *time.Time
	if err := row.Scan(&e.ID, &ruleID, &nodeID, &e.Severity, &ts, &resolved, &e.Title, &e.Detail, &e.Acked); err != nil {
		return Event{}, err
	}
	e.RuleID = ruleID
	e.NodeID = nodeID
	e.TS = ts.UTC().Format(time.RFC3339Nano)
	if resolved != nil {
		value := resolved.UTC().Format(time.RFC3339Nano)
		e.ResolvedAt = &value
	}
	return e, nil
}

func ruleETag(id string, revision int64) string {
	return fmt.Sprintf(`"alert-rule-%s-%d"`, id, revision)
}

func eventETag(e Event) string {
	state := e.ID + "\x00" + e.TS + "\x00" + fmt.Sprint(e.Acked)
	if e.ResolvedAt != nil {
		state += "\x00" + *e.ResolvedAt
	}
	hash := sha256.Sum256([]byte(state))
	return `"alert-event-` + e.ID + `-` + hex.EncodeToString(hash[:6]) + `"`
}

// ListRules returns a stable page of rules and the total number of rules.
func (s *Store) ListRules(ctx context.Context, page, size int) ([]Rule, int, error) {
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM alert_rules`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+ruleColumns+` FROM alert_rules ORDER BY created_at,id LIMIT $1 OFFSET $2`, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	rules := make([]Rule, 0)
	for rows.Next() {
		r, scanErr := scanRule(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		rules = append(rules, r)
	}
	return rules, total, rows.Err()
}

// GetRule reads one rule and its strong revision ETag.
func (s *Store) GetRule(ctx context.Context, id string) (Rule, string, error) {
	if !validUUID(id) {
		return Rule{}, "", ErrNotFound
	}
	r, err := scanRule(s.DB.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, "", ErrNotFound
	}
	if err != nil {
		return Rule{}, "", err
	}
	return r, ruleETag(r.ID, r.Revision), nil
}

// ReplaceRule atomically replaces a rule only if its ETag still matches.
func (s *Store) ReplaceRule(ctx context.Context, id string, in RuleInput, ifMatch string) (Rule, string, error) {
	if !validUUID(id) {
		return Rule{}, "", ErrNotFound
	}
	clean, err := NormalizeRule(in)
	if err != nil {
		return Rule{}, "", err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Rule{}, "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM alert_rules WHERE id=$1 FOR UPDATE`, id).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, "", ErrNotFound
	}
	if err != nil {
		return Rule{}, "", err
	}
	if ruleETag(id, revision) != ifMatch {
		return Rule{}, "", ErrStale
	}
	if err = checkNodeIDs(ctx, tx, clean); err != nil {
		return Rule{}, "", err
	}
	if err = closeRuleStreaks(ctx, tx, id, s.now()); err != nil {
		return Rule{}, "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE alert_rules SET name=$2,metric=$3,op=$4,value=$5,for_tests=$6,node_ids=$7,
		severity=$8,channels=$9,enabled=$10,engine=NULLIF($11,''),revision=revision+1,updated_at=now() WHERE id=$1`,
		id, clean.Name, clean.Metric, clean.Op, clean.Value, clean.ForTests, ruleNodeIDs(clean), clean.Severity, clean.Channels, clean.Enabled, clean.Engine); err != nil {
		return Rule{}, "", err
	}
	r, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id=$1`, id))
	if err != nil {
		return Rule{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return Rule{}, "", err
	}
	return r, ruleETag(r.ID, r.Revision), nil
}

// DeleteRule deletes the rule while keeping its historical events detached.
func (s *Store) DeleteRule(ctx context.Context, id, ifMatch string) error {
	if !validUUID(id) {
		return ErrNotFound
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM alert_rules WHERE id=$1 FOR UPDATE`, id).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ruleETag(id, revision) != ifMatch {
		return ErrStale
	}
	if err = closeRuleStreaks(ctx, tx, id, s.now()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM alert_rules WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func closeRuleStreaks(ctx context.Context, tx pgx.Tx, ruleID string, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE alert_events e SET resolved_at=GREATEST(e.ts,$2)
		FROM alert_state s WHERE s.rule_id=$1 AND s.active AND s.active_event_id=e.id AND e.resolved_at IS NULL`, ruleID, at); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM alert_state WHERE rule_id=$1`, ruleID)
	return err
}

func checkNodeIDs(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, in RuleInput) error {
	if in.AllNodes {
		return nil
	}
	var count int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE id=ANY($1::uuid[])`, in.NodeIDs).Scan(&count); err != nil {
		return err
	}
	if count != len(in.NodeIDs) {
		return ErrInvalid
	}
	return nil
}

func ruleNodeIDs(in RuleInput) []string {
	if in.AllNodes {
		return nil
	}
	return in.NodeIDs
}

// ListEvents returns a newest-first page matching the event activity filters.
func (s *Store) ListEvents(ctx context.Context, filter EventFilter, page, size int) ([]Event, int, error) {
	var node, isp any
	if filter.NodeID != "" {
		node = filter.NodeID
	}
	if filter.ISPID != "" {
		isp = filter.ISPID
	}
	args := []any{filter.State == "open", filter.Severity, node, isp}
	where := `($1::boolean=false OR e.acked=false) AND ($2::text='' OR e.severity=$2) AND ($3::uuid IS NULL OR e.node_id=$3)
		AND ($4::uuid IS NULL OR e.isp_id=$4::uuid)`
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM alert_events e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+qualifyEventColumns("e")+` FROM alert_events e WHERE `+where+` ORDER BY e.ts DESC,e.id DESC LIMIT $5 OFFSET $6`, append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		e, scanErr := scanEvent(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// GetEvent reads a single event and its acknowledgement ETag.
func (s *Store) GetEvent(ctx context.Context, id string) (Event, string, error) {
	if !validUUID(id) {
		return Event{}, "", ErrNotFound
	}
	e, err := scanEvent(s.DB.QueryRow(ctx, `SELECT `+eventColumns+` FROM alert_events WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, "", ErrNotFound
	}
	if err != nil {
		return Event{}, "", err
	}
	return e, eventETag(e), nil
}

// Acknowledge sets or clears the attention flag without changing the alert streak.
func (s *Store) Acknowledge(ctx context.Context, id string, acked bool, ifMatch string) (Event, string, error) {
	if !validUUID(id) {
		return Event{}, "", ErrNotFound
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Event{}, "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	e, err := scanEvent(tx.QueryRow(ctx, `SELECT `+eventColumns+` FROM alert_events WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, "", ErrNotFound
	}
	if err != nil {
		return Event{}, "", err
	}
	if eventETag(e) != ifMatch {
		return Event{}, "", ErrStale
	}
	if _, err = tx.Exec(ctx, `UPDATE alert_events SET acked=$2 WHERE id=$1`, id, acked); err != nil {
		return Event{}, "", err
	}
	e.Acked = acked
	if err = tx.Commit(ctx); err != nil {
		return Event{}, "", err
	}
	return e, eventETag(e), nil
}

func validUUID(id string) bool { return uuidPattern.MatchString(strings.ToLower(id)) }

// AcknowledgeAll acknowledges every event that is unacknowledged now and records the work as a job. Events that
// arrive afterwards are not touched. The update is one statement, so the job is already finished when it is returned.
func (s *Store) AcknowledgeAll(ctx context.Context) (string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	id, err := jobs.Start(ctx, tx, "alerts-acknowledge-all")
	if err != nil {
		return "", err
	}
	_, execErr := tx.Exec(ctx, `UPDATE alert_events SET acked = true WHERE acked = false AND ts <= $1`, s.now())
	if err := jobs.Finish(ctx, tx, id, execErr); err != nil {
		return "", err
	}
	if execErr != nil {
		return "", execErr
	}
	return id, tx.Commit(ctx)
}
