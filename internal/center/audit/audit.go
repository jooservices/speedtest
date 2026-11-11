// Package audit records who changed what and when. Entries describe the change in words and ids; request bodies,
// passwords, tokens and other secrets are never stored.
package audit

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Entry is one recorded change.
type Entry struct {
	ID           string    `json:"id"`
	TS           time.Time `json:"ts"`
	Actor        string    `json:"actor"`
	AuthMethod   string    `json:"authMethod"`
	CredentialID string    `json:"credentialId,omitempty"`
	Action       string    `json:"action"`
	TargetType   string    `json:"targetType"`
	TargetID     string    `json:"targetId"`
	Summary      string    `json:"summary"`
	Remote       string    `json:"remote"`
}

const (
	AuthMethodSession = "session"
	AuthMethodBearer  = "bearer"
)

// Store keeps the audit log in PostgreSQL.
type Store struct {
	DB  Pool
	Now func() time.Time
}

// Pool is the pgx subset used (satisfied by *pgxpool.Pool).
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Record stores one entry. The summary is cut to 300 characters.
func (s *Store) Record(ctx context.Context, e Entry) error {
	return record(ctx, s.DB, e, s.Now)
}

// RecordTx writes an audit entry inside the caller's transaction.
func RecordTx(ctx context.Context, tx pgx.Tx, e Entry) error {
	return record(ctx, tx, e, nil)
}

type entryWriter interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func record(ctx context.Context, writer entryWriter, e Entry, nowFn func() time.Time) error {
	if e.Actor == "" || e.Action == "" {
		return fmt.Errorf("audit entry needs an actor and an action")
	}
	if e.AuthMethod == "" {
		e.AuthMethod = AuthMethodSession
	}
	if e.AuthMethod != AuthMethodSession && e.AuthMethod != AuthMethodBearer {
		return fmt.Errorf("audit entry has an unsupported authentication method")
	}
	if len(e.Summary) > 300 {
		e.Summary = e.Summary[:300]
	}
	now := time.Now()
	if nowFn != nil {
		now = nowFn()
	}
	_, err := writer.Exec(ctx, `INSERT INTO audit_log(id,ts,actor,action,target_type,target_id,summary,remote,auth_method,credential_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid)`,
		newID(), now.UTC(), e.Actor, e.Action, e.TargetType, e.TargetID, e.Summary, e.Remote, e.AuthMethod, e.CredentialID)
	return err
}

// Filter narrows a listing: Action matches by prefix ("node." for every node change).
type Filter struct{ Action string }

// List returns one page, newest first, and the total.
func (s *Store) List(ctx context.Context, f Filter, page, size int) ([]Entry, int, error) {
	where, args := "", []any{}
	if f.Action != "" {
		where, args = " WHERE action LIKE $1", append(args, escapeLike(f.Action)+"%")
	}
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_log`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, size, (page-1)*size)
	rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT id::text,ts,actor,auth_method,coalesce(credential_id::text,''),action,target_type,target_id,summary,remote FROM audit_log%s ORDER BY ts DESC, id LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.ID, &e.TS, &e.Actor, &e.AuthMethod, &e.CredentialID, &e.Action, &e.TargetType, &e.TargetID, &e.Summary, &e.Remote)
		e.TS = e.TS.UTC()
		return e, err
	})
	if items == nil {
		items = []Entry{}
	}
	return items, total, err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

var (
	uuidSegment = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

type rule struct {
	method, pattern, action, targetType, summary string
}

// rules map an owner write to a readable action. Patterns use {id} for one path segment.
var rules = []rule{
	{"POST", "/api/nodes", "node.create", "node", "Added a node"},
	{"POST", "/api/access-tokens", "access_token.create", "access_token", "Created an API access token"},
	{"DELETE", "/api/access-tokens/{id}", "access_token.revoke", "access_token", "Revoked an API access token"},
	{"PATCH", "/api/nodes/{id}", "node.update", "node", "Changed node settings"},
	{"DELETE", "/api/nodes/{id}", "node.delete", "node", "Removed a node"},
	{"POST", "/api/nodes/{id}/run", "node.run", "node", "Started a speed test"},
	{"POST", "/api/nodes/{id}/commands", "node.command", "node", "Sent a command to a node"},
	{"POST", "/api/nodes/{id}/credential", "node.credential", "node", "Rotated a node credential"},
	{"DELETE", "/api/nodes/{id}/credential", "node.credential", "node", "Revoked a node credential"},
	{"POST", "/api/nodes/{id}/installations", "node.install", "node", "Installed the agent over SSH"},
	{"POST", "/api/nodes/{id}/uninstallations", "node.uninstall", "node", "Removed the agent over SSH"},
	{"PUT", "/api/schedules/{id}", "schedule.update", "schedule", "Changed a node schedule"},
	{"PUT", "/api/schedules/rules", "schedule.rules", "schedule", "Changed the schedule rules"},
	{"POST", "/api/schedules/stagger", "schedule.stagger", "schedule", "Spread out start times"},
	{"POST", "/api/alerts/rules", "alert.rule.create", "alert_rule", "Created an alert rule"},
	{"PUT", "/api/alerts/rules/{id}", "alert.rule.update", "alert_rule", "Changed an alert rule"},
	{"DELETE", "/api/alerts/rules/{id}", "alert.rule.delete", "alert_rule", "Deleted an alert rule"},
	{"PUT", "/api/alerts/events/{id}/ack", "alert.ack", "alert_event", "Acknowledged or reopened an alert"},
	{"POST", "/api/alerts/acknowledgements", "alert.ack_all", "alert_event", "Acknowledged all alerts"},
	{"PUT", "/api/settings", "settings.update", "settings", "Changed settings"},
	{"POST", "/api/settings/enrolment-key", "settings.enrolment_key", "settings", "Generated a new enrolment key"},
	{"POST", "/api/notifications/test/{id}", "notification.test", "notification", "Sent a test notification"},
	{"PATCH", "/api/notifications/events/{id}", "notification.event.update", "notification_event", "Changed a notification event"},
	{"POST", "/api/notifications/events/{id}/tests/{id}", "notification.event.test", "notification_event", "Sent a sample notification event"},
	{"PUT", "/api/notifications/events/{id}/nodes/{id}", "notification.event.node.update", "notification_event", "Changed a node notification event"},
	{"PUT", "/api/notifications/templates/{id}/{id}", "notification.template.update", "notification_template", "Changed a notification template"},
	{"POST", "/api/notifications/templates/{id}/{id}/resets", "notification.template.reset", "notification_template", "Reset a notification template"},
	{"POST", "/api/auth/password", "auth.password", "user", "Changed the password"},
	{"POST", "/api/auth/2fa/enable", "auth.two_factor_enable", "user", "Turned two-factor sign-in on"},
	{"POST", "/api/auth/2fa/disable", "auth.two_factor_disable", "user", "Turned two-factor sign-in off"},
	{"POST", "/api/ip/refreshes", "ip.refresh", "ip", "Refreshed locations"},
}

func match(pattern, path string) (id string, ok bool) {
	a, b := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(a) != len(b) {
		return "", false
	}
	var ids []string
	for i := range a {
		if a[i] == "{id}" {
			ids = append(ids, b[i])
			continue
		}
		if a[i] != b[i] {
			return "", false
		}
	}
	return strings.Join(ids, "/"), true
}

// Classify turns an owner write into an entry (without actor, time or origin). ok is false for requests that are
// not changes worth recording (reads, agent traffic, sign-in).
func Classify(method, path string) (Entry, bool) {
	path = strings.TrimRight(path, "/")
	if !strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api/agent/") {
		return Entry{}, false
	}
	// A literal pattern beats one with a parameter ("/api/schedules/rules" before "/api/schedules/{id}").
	var best *rule
	var bestID string
	for i := range rules {
		r := &rules[i]
		if r.method != method {
			continue
		}
		id, ok := match(r.pattern, path)
		if !ok {
			continue
		}
		if best == nil || strings.Count(r.pattern, "{id}") < strings.Count(best.pattern, "{id}") {
			best, bestID = r, id
		}
	}
	if best == nil {
		return Entry{}, false
	}
	e := Entry{Action: best.action, TargetType: best.targetType, Summary: best.summary}
	if bestID != "" && (uuidSegment.MatchString(bestID) || len(bestID) <= 40) {
		e.TargetID = bestID
	}
	return e, true
}
