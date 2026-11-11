// Package nodes implements node enrollment and node credential lifecycle.
package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const enrollmentLifetime = 24 * time.Hour

var (
	ErrNotFound       = errors.New("node not found")
	ErrInvalidToken   = errors.New("invalid or expired enrollment token")
	ErrInvalidAuth    = errors.New("invalid credential")
	ErrUnauthorized   = errors.New("owner session required")
	ErrForbidden      = errors.New("request forbidden")
	ErrConflict       = errors.New("resource conflict")
	ErrInvalidInput   = errors.New("invalid input")
	ErrCryptoKeyUnset = errors.New("idempotency encryption key is not configured")
)

// Database is the subset of pgxpool.Pool used by Service.
type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Service owns credential issuance and validation backed by PostgreSQL.
type Service struct {
	db     Database
	appKey []byte
	random io.Reader
	now    func() time.Time
}

// NewService creates a node service. appKey must be a 32-byte key decoded from APP_KEY.
func NewService(db Database, appKey []byte) *Service {
	return &Service{db: db, appKey: append([]byte(nil), appKey...), random: rand.Reader, now: time.Now}
}

func (s *Service) uuid() (string, error)                { return newUUIDFrom(s.random) }
func (s *Service) secret(prefix string) (string, error) { return newSecretFrom(s.random, prefix) }

// Binding is the node's optional local interface/source address binding.
type Binding struct {
	Iface    string  `json:"iface"`
	SourceIP *string `json:"sourceIp"`
}

// Input contains the fields accepted when creating an independent node.
type Input struct {
	Name         string  `json:"name"`
	Label        string  `json:"label"`
	Provider     string  `json:"provider"`
	Plan         string  `json:"plan"`
	ISPPlanID    *string `json:"ispPlanId"`
	ExpectedDown float64 `json:"expectedDown"`
	ExpectedUp   float64 `json:"expectedUp"`
	Threads      int     `json:"threads"`
	ServerID     *int    `json:"-"`
	Note         string  `json:"note"`
	Enabled      bool    `json:"enabled"`
	Binding      Binding `json:"binding"`
	AutoUpdate   bool    `json:"autoUpdate"`
}

// ServerSelection accepts either "auto" or a positive server ID.
type ServerSelection struct{ ID *int }

func (s *ServerSelection) UnmarshalJSON(raw []byte) error {
	if string(raw) == `"auto"` {
		s.ID = nil
		return nil
	}
	var id int
	if err := json.Unmarshal(raw, &id); err != nil || id < 1 {
		return ErrInvalidInput
	}
	s.ID = &id
	return nil
}

func (s ServerSelection) MarshalJSON() ([]byte, error) {
	if s.ID == nil {
		return []byte(`"auto"`), nil
	}
	return json.Marshal(*s.ID)
}

// Normalize applies API defaults and validates the complete request.
func (i Input) Normalize() (Input, error) {
	i.Name = strings.TrimSpace(i.Name)
	if !validText(i.Name, 1, 100) || !validText(i.Label, 0, 200) ||
		!validText(i.Provider, 0, 200) || !validText(i.Plan, 0, 200) ||
		!validText(i.Note, 0, 4000) || !validText(i.Binding.Iface, 0, 64) ||
		!validPlanSpeeds(i) {
		return Input{}, ErrInvalidInput
	}
	if i.ISPPlanID != nil && !validUUID(*i.ISPPlanID) {
		return Input{}, ErrInvalidInput
	}
	if i.Threads == 0 {
		i.Threads = 4
	}
	if i.Threads < 1 || i.Threads > 64 || (i.ServerID != nil && *i.ServerID < 1) {
		return Input{}, ErrInvalidInput
	}
	if i.Binding.SourceIP != nil {
		if net.ParseIP(*i.Binding.SourceIP) == nil {
			return Input{}, ErrInvalidInput
		}
		normalized := net.ParseIP(*i.Binding.SourceIP).String()
		i.Binding.SourceIP = &normalized
	}
	return i, nil
}

func validPlanSpeeds(i Input) bool {
	if i.ISPPlanID == nil {
		return positiveFinite(i.ExpectedDown) && positiveFinite(i.ExpectedUp)
	}
	return (i.ExpectedDown == 0 || positiveFinite(i.ExpectedDown)) && (i.ExpectedUp == 0 || positiveFinite(i.ExpectedUp))
}

func validText(value string, min, max int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= min && length <= max
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// CreateNode creates the node, its default schedule, and a hashed 24-hour token.
func (s *Service) CreateNode(ctx context.Context, input Input, principal, idempotencyKey string) (json.RawMessage, bool, error) {
	input, err := input.Normalize()
	if err != nil {
		return nil, false, err
	}
	request := map[string]any{"node": input, "server": serverValue(input.ServerID)}
	return s.idempotent(ctx, principal, "create-node", idempotencyKey, request, func(tx pgx.Tx) (any, error) {
		return s.createNodeTx(ctx, tx, input)
	})
}

func (s *Service) createNodeTx(ctx context.Context, tx pgx.Tx, input Input) (any, error) {
	if input.ISPPlanID != nil {
		var down, up float64
		if err := tx.QueryRow(ctx, `SELECT advertised_down_mbps,advertised_up_mbps FROM isp_plans WHERE id=$1::uuid`, *input.ISPPlanID).Scan(&down, &up); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInvalidInput
			}
			return nil, err
		}
		if input.ExpectedDown == 0 {
			input.ExpectedDown = down
		}
		if input.ExpectedUp == 0 {
			input.ExpectedUp = up
		}
	}
	nodeID, err := s.uuid()
	if err != nil {
		return nil, err
	}
	token, err := s.secret("enr_")
	if err != nil {
		return nil, err
	}
	colorSlot, err := nextColorSlot(ctx, tx)
	if err != nil {
		return nil, err
	}
	var createdAt, expiresAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO nodes (id,name,label,provider,plan,isp_plan_id,expected_down_mbps,expected_up_mbps,threads,server_id,color_slot,enabled,auto_update,binding_interface,binding_source_ip,note)
		VALUES ($1,$2,$3,$4,$5,$6::uuid,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING created_at`, nodeID, input.Name, input.Label, input.Provider, input.Plan, input.ISPPlanID,
		input.ExpectedDown, input.ExpectedUp, input.Threads, input.ServerID, colorSlot,
		input.Enabled, input.AutoUpdate, input.Binding.Iface, input.Binding.SourceIP, input.Note).Scan(&createdAt)
	if err != nil {
		return nil, translateDBError(err)
	}
	var interval, offset int
	err = tx.QueryRow(ctx, `
		WITH legacy AS (
			INSERT INTO schedules (node_id) VALUES ($1)
			RETURNING node_id, interval_minutes, offset_minutes, enabled, revision
		), engine AS (
			INSERT INTO engine_schedules(node_id,engine,interval_minutes,offset_minutes,enabled,revision)
			SELECT node_id,'ookla',interval_minutes,offset_minutes,enabled,revision FROM legacy
			RETURNING node_id
		)
		SELECT legacy.interval_minutes, legacy.offset_minutes
		FROM legacy JOIN engine USING (node_id)`, nodeID).Scan(&interval, &offset)
	if err != nil {
		return nil, translateDBError(err)
	}
	credentialID, err := s.uuid()
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO node_credentials (id,node_id,kind,token_hash,expires_at)
		VALUES ($1,$2,'enrollment',$3,now()+$4*interval '1 second')
		RETURNING expires_at`, credentialID, nodeID, hashSecret(token), enrollmentLifetime.Seconds()).Scan(&expiresAt)
	if err != nil {
		return nil, translateDBError(err)
	}
	return map[string]any{
		"node":      nodeResponse(nodeID, input, colorSlot, createdAt, interval, offset, "", ""),
		"token":     token,
		"expiresAt": expiresAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

// IssueInstallToken creates a new one-time token for an existing node. The
// installer keeps the plaintext only in its worker and transfers it over SSH.
func (s *Service) IssueInstallToken(ctx context.Context, nodeID string) (string, string, error) {
	if !validUUID(nodeID) {
		return "", "", ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var nodeName string
	err = tx.QueryRow(ctx, "SELECT name FROM nodes WHERE id=$1 FOR UPDATE", nodeID).Scan(&nodeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	token, err := s.secret("enr_")
	if err != nil {
		return "", "", err
	}
	credentialID, err := s.uuid()
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE node_credentials SET revoked_at=now() WHERE node_id=$1 AND kind='enrollment' AND consumed_at IS NULL AND revoked_at IS NULL", nodeID); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO node_credentials(id,node_id,kind,token_hash,expires_at) VALUES($1,$2,'enrollment',$3,now()+$4*interval '1 second')", credentialID, nodeID, hashSecret(token), enrollmentLifetime.Seconds()); err != nil {
		return "", "", translateDBError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return token, nodeName, nil
}

// RevokeInstallToken makes an unused installer-issued token unusable. A
// consumed token is left as a historical record and cannot authenticate again.
func (s *Service) RevokeInstallToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, "UPDATE node_credentials SET revoked_at=now() WHERE token_hash=$1 AND kind='enrollment' AND consumed_at IS NULL AND revoked_at IS NULL", hashSecret(token))
	return err
}

// Enroll atomically consumes a valid one-time token and replaces any existing bearer credential.
func (s *Service) Enroll(ctx context.Context, token, name, host, agent string) (map[string]any, error) {
	if token == "" || !validText(strings.TrimSpace(name), 1, 100) || !validText(host, 0, 255) || !validText(agent, 1, 64) {
		return nil, ErrInvalidInput
	}
	name = strings.TrimSpace(name)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var credentialID, nodeID, storedHash string
	err = tx.QueryRow(ctx, `SELECT id::text,node_id::text,token_hash FROM node_credentials WHERE token_hash=$1 AND kind='enrollment'`, hashSecret(token)).Scan(&credentialID, &nodeID, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if !constantHashEqual(storedHash, hashSecret(token)) {
		return nil, ErrInvalidToken
	}
	node, err := lockNode(ctx, tx, nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if err := s.validateEnrollment(ctx, tx, credentialID, node.Name, name); err != nil {
		return nil, err
	}
	bearer, err := s.secret("sp_")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_credentials SET consumed_at=now() WHERE id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, credentialID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_credentials SET revoked_at=now() WHERE node_id=$1 AND kind='bearer' AND revoked_at IS NULL`, nodeID); err != nil {
		return nil, err
	}
	bearerID, err := s.uuid()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO node_credentials (id,node_id,kind,token_hash) VALUES ($1,$2,'bearer',$3)`, bearerID, nodeID, hashSecret(bearer)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE nodes SET host=$2,agent=$3,updated_at=now() WHERE id=$1`, nodeID, host, agent); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"nodeId": node.ID, "credential": bearer, "config": node.agentConfig()}, nil
}

// RotateCredential revokes every existing bearer before inserting a replacement.
func (s *Service) RotateCredential(ctx context.Context, nodeID, principal, idempotencyKey string) (json.RawMessage, bool, error) {
	if !validUUID(nodeID) {
		return nil, false, ErrNotFound
	}
	return s.idempotent(ctx, principal, "rotate-credential:"+nodeID, idempotencyKey, map[string]string{"nodeId": nodeID}, func(tx pgx.Tx) (any, error) {
		node, err := lockNode(ctx, tx, nodeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		credential, err := s.secret("sp_")
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE node_credentials SET revoked_at=now() WHERE node_id=$1 AND kind='bearer' AND revoked_at IS NULL`, nodeID); err != nil {
			return nil, err
		}
		credentialID, err := s.uuid()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO node_credentials (id,node_id,kind,token_hash) VALUES ($1,$2,'bearer',$3)`, credentialID, nodeID, hashSecret(credential)); err != nil {
			return nil, err
		}
		return map[string]any{"nodeId": node.ID, "credential": credential, "config": node.agentConfig()}, nil
	})
}

// RevokeCredential revokes all current bearer credentials; repeated calls are safe.
func (s *Service) RevokeCredential(ctx context.Context, nodeID string) error {
	if !validUUID(nodeID) {
		return ErrNotFound
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := lockNode(ctx, tx, nodeID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_credentials SET revoked_at=now() WHERE node_id=$1 AND kind='bearer' AND revoked_at IS NULL`, nodeID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AuthenticateAgent verifies a bearer on every request and returns its node ID.
func (s *Service) AuthenticateAgent(ctx context.Context, bearer string) (string, error) {
	if bearer == "" {
		return "", ErrInvalidAuth
	}
	hash := hashSecret(bearer)
	var nodeID, storedHash string
	var revokedAt pgtype.Timestamptz
	err := s.db.QueryRow(ctx, `SELECT node_id::text,token_hash,revoked_at FROM node_credentials WHERE token_hash=$1 AND kind='bearer'`, hash).Scan(&nodeID, &storedHash, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!constantHashEqual(storedHash, hash) || revokedAt.Valid)) {
		return "", ErrInvalidAuth
	}
	if err != nil {
		return "", err
	}
	return nodeID, nil
}

// ValidateEnrollmentKey compares the one-time-disclosed Settings key in constant time.
func (s *Service) ValidateEnrollmentKey(ctx context.Context, presented string) (string, error) {
	if presented == "" {
		return "", ErrInvalidAuth
	}
	var storedHash string
	err := s.db.QueryRow(ctx, `SELECT COALESCE(encode(secret_ciphertext,'hex'),'') FROM settings WHERE key='enrollment_key' AND secret_ciphertext IS NOT NULL`).Scan(&storedHash)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !constantHashEqual(storedHash, hashSecret(presented))) {
		return "", ErrInvalidAuth
	}
	if err != nil {
		return "", err
	}
	return "enrollment:" + storedHash, nil
}

// RotateEnrollmentKey replaces the Settings bootstrap key and returns it only once.
func (s *Service) RotateEnrollmentKey(ctx context.Context, principal, idempotencyKey string) (json.RawMessage, bool, error) {
	return s.idempotent(ctx, principal, "rotate-enrollment-key", idempotencyKey, struct{}{}, func(tx pgx.Tx) (any, error) {
		key, err := s.secret("sek_")
		if err != nil {
			return nil, err
		}
		createdAt := s.now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO settings (key,secret_ciphertext) VALUES ('enrollment_key',decode($1::text,'hex')) ON CONFLICT (key) DO UPDATE SET value=NULL,secret_ciphertext=EXCLUDED.secret_ciphertext,revision=settings.revision+1,updated_at=now()`, hashSecret(key)); err != nil {
			return nil, err
		}
		return map[string]any{"enrollToken": key, "createdAt": createdAt.Format(time.RFC3339Nano)}, nil
	})
}

type nodeRecord struct {
	ID           string
	Name         string
	Label        string
	Provider     string
	Plan         string
	ExpectedDown float64
	ExpectedUp   float64
	Threads      int
	ServerID     pgtype.Int4
	ColorSlot    int
	Enabled      bool
	AutoUpdate   bool
	BindingIface string
	BindingIP    pgtype.Text
	Note         string
	Revision     int64
	Interval     int
	Offset       int
	ScheduleOn   bool
	Host         string
	Agent        string
	CreatedAt    time.Time
}

func lockNode(ctx context.Context, tx pgx.Tx, id string) (nodeRecord, error) {
	var n nodeRecord
	err := tx.QueryRow(ctx, `
		SELECT n.id::text,n.name,n.label,n.provider,n.plan,n.expected_down_mbps,n.expected_up_mbps,n.threads,n.server_id,n.color_slot,n.enabled,n.auto_update,n.binding_interface,n.binding_source_ip::text,n.note,n.config_revision,s.interval_minutes,s.offset_minutes,s.enabled,n.host,n.agent,n.created_at
		FROM nodes n JOIN schedules s ON s.node_id=n.id WHERE n.id=$1 FOR UPDATE OF n`, id).Scan(
		&n.ID, &n.Name, &n.Label, &n.Provider, &n.Plan, &n.ExpectedDown, &n.ExpectedUp,
		&n.Threads, &n.ServerID, &n.ColorSlot, &n.Enabled, &n.AutoUpdate, &n.BindingIface,
		&n.BindingIP, &n.Note, &n.Revision, &n.Interval, &n.Offset, &n.ScheduleOn,
		&n.Host, &n.Agent, &n.CreatedAt)
	return n, err
}

func (n nodeRecord) input() Input {
	var serverID *int
	if n.ServerID.Valid {
		id := int(n.ServerID.Int32)
		serverID = &id
	}
	var sourceIP *string
	if n.BindingIP.Valid {
		ip := n.BindingIP.String
		sourceIP = &ip
	}
	return Input{Name: n.Name, Label: n.Label, Provider: n.Provider, Plan: n.Plan,
		ExpectedDown: n.ExpectedDown, ExpectedUp: n.ExpectedUp, Threads: n.Threads,
		ServerID: serverID, Note: n.Note, Enabled: n.Enabled,
		Binding: Binding{Iface: n.BindingIface, SourceIP: sourceIP}, AutoUpdate: n.AutoUpdate}
}

func (n nodeRecord) agentConfig() map[string]any {
	input := n.input()
	return map[string]any{
		"revision": fmt.Sprint(n.Revision), "name": n.Name,
		"expectedDown": n.ExpectedDown, "expectedUp": n.ExpectedUp,
		"schedule": map[string]any{"interval": n.Interval, "offset": n.Offset, "enabled": n.ScheduleOn},
		"quiet":    map[string]any{"quietStart": "", "quietEnd": "", "timezone": "UTC"},
		"engine":   "ookla", "threads": n.Threads, "server": serverValue(input.ServerID),
		"binding":    map[string]any{"iface": n.BindingIface, "sourceIp": input.Binding.SourceIP},
		"autoUpdate": n.AutoUpdate, "release": nil,
	}
}

func nodeResponse(id string, i Input, colorSlot int, createdAt time.Time, interval, offset int, host, agent string) map[string]any {
	return map[string]any{
		"name": i.Name, "label": i.Label, "provider": i.Provider, "plan": i.Plan, "ispPlanId": i.ISPPlanID,
		"expectedDown": i.ExpectedDown, "expectedUp": i.ExpectedUp, "threads": i.Threads,
		"server": serverValue(i.ServerID), "note": i.Note, "enabled": i.Enabled,
		"binding": map[string]any{"iface": i.Binding.Iface, "sourceIp": i.Binding.SourceIP},
		"id":      id, "colorSlot": colorSlot, "interval": interval, "offset": offset,
		"host": host, "agent": agent, "createdAt": createdAt.UTC().Format(time.RFC3339Nano),
		"lastSeen": nil, "status": "offline", "latest": nil, "latestGood": nil,
		"currentIp": nil, "geo": nil, "isp": nil, "updateVersion": nil, "autoUpdate": i.AutoUpdate,
	}
}

func serverValue(id *int) any {
	if id == nil {
		return "auto"
	}
	return *id
}

func nextColorSlot(ctx context.Context, tx pgx.Tx) (int, error) {
	var slot int
	err := tx.QueryRow(ctx, `SELECT COALESCE((MAX(color_slot)%8)+1,1) FROM nodes`).Scan(&slot)
	return slot, err
}

func newUUID() (string, error) {
	return newUUIDFrom(rand.Reader)
}

func newUUIDFrom(random io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("generate identifier: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func newSecret(prefix string) (string, error) {
	return newSecretFrom(rand.Reader, prefix)
}

func newSecretFrom(random io.Reader, prefix string) (string, error) {
	var secret [32]byte
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return "", fmt.Errorf("generate credential: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

func hashSecret(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:])
}

func constantHashEqual(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	if len(compact) != 32 {
		return false
	}
	_, err := hex.DecodeString(compact)
	return err == nil
}

func translateDBError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

func (s *Service) validateEnrollment(ctx context.Context, tx pgx.Tx, credentialID, storedName, name string) error {
	var expiresAt time.Time
	var consumedAt, revokedAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT expires_at,consumed_at,revoked_at FROM node_credentials WHERE id=$1 FOR UPDATE`, credentialID).Scan(&expiresAt, &consumedAt, &revokedAt)
	if err != nil {
		return err
	}
	if storedName != name || !expiresAt.After(s.now()) || consumedAt.Valid || revokedAt.Valid {
		return ErrInvalidToken
	}
	return nil
}
