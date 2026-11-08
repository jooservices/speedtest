package isp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ErrInvalid reports ASN or organisation input that cannot form an ISP.
var ErrInvalid = errors.New("invalid ISP")

// DB is the subset of pgx pool or transaction used to ensure and link ISPs.
type DB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Entity is a persisted ISP row.
type Entity struct {
	ID           string
	ASN          uint
	Organisation string
	Name         string
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Ensure upserts an ISP for the ASN and returns it. ASN 0 or empty organisation is skipped.
func Ensure(ctx context.Context, db DB, asn uint, organisation string) (*Entity, error) {
	organisation = strings.TrimSpace(organisation)
	if asn == 0 || organisation == "" {
		return nil, nil
	}
	name := NormaliseName(organisation)
	if name == "" {
		name = fmt.Sprintf("AS%d", asn)
	}
	var e Entity
	err := db.QueryRow(ctx, `SELECT id::text, asn, organisation, name FROM isps WHERE asn=$1`, asn).
		Scan(&e.ID, &e.ASN, &e.Organisation, &e.Name)
	if err == nil {
		if e.Organisation != organisation || e.Name != name {
			if _, err = db.Exec(ctx, `UPDATE isps SET organisation=$2, name=$3, updated_at=now() WHERE id=$1::uuid`, e.ID, organisation, name); err != nil {
				return nil, err
			}
			e.Organisation, e.Name = organisation, name
		}
		return &e, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(ctx, `INSERT INTO isps(id,asn,organisation,name) VALUES ($1::uuid,$2,$3,$4)
		ON CONFLICT (asn) DO UPDATE SET organisation=EXCLUDED.organisation, name=EXCLUDED.name, updated_at=now()`,
		id, asn, organisation, name); err != nil {
		return nil, err
	}
	err = db.QueryRow(ctx, `SELECT id::text, asn, organisation, name FROM isps WHERE asn=$1`, asn).
		Scan(&e.ID, &e.ASN, &e.Organisation, &e.Name)
	return &e, err
}

// LinkAddress sets ip_addresses.isp_id for the address when an ISP can be derived.
func LinkAddress(ctx context.Context, db DB, ip string, asn uint, organisation string) (*Entity, error) {
	e, err := Ensure(ctx, db, asn, organisation)
	if err != nil || e == nil {
		return e, err
	}
	_, err = db.Exec(ctx, `UPDATE ip_addresses SET isp_id=$2::uuid WHERE ip=$1::inet AND isp_id IS DISTINCT FROM $2::uuid`, ip, e.ID)
	return e, err
}

// LinkResult copies the address ISP onto the result row.
func LinkResult(ctx context.Context, db DB, resultID, nodeID, ip string) error {
	if ip == "" {
		return nil
	}
	_, err := db.Exec(ctx, `UPDATE results r SET isp_id=a.isp_id
		FROM ip_addresses a WHERE r.id=$1::uuid AND r.node_id=$2::uuid AND a.ip=$3::inet AND a.isp_id IS NOT NULL
		AND r.isp_id IS DISTINCT FROM a.isp_id`, resultID, nodeID, ip)
	return err
}

// View returns the public ISP object for an entity, or nil.
func View(e *Entity) map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{"id": e.ID, "asn": e.ASN, "organisation": e.Organisation, "name": e.Name}
}

// ValidID reports whether value is a UUID.
func ValidID(value string) bool { return uuidPattern.MatchString(value) }
