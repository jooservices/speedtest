// Package ipapi serves the owner-facing IP and location views: change history, per-node addresses, lookups
// through the geolocation chain and the map of current locations.
package ipapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/geo"
	"github.com/jooservices/speedtest/internal/center/ownerapi"
)

// ErrInvalid reports input that is not an IPv4 or IPv6 literal.
var ErrInvalid = errors.New("not an IP address literal")

// DB is the subset of pgxpool.Pool the store uses.
type DB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Resolver is the geolocation chain: cache, offline database, then the configured fallback providers.
type Resolver interface {
	Lookup(ctx context.Context, address string) (geo.Location, error)
}

// Store reads IP data for the owner.
type Store struct {
	DB       DB
	Resolver Resolver
}

func geoOf(ip string, source *string, raw []byte) any {
	if source == nil {
		return nil
	}
	if g := ownerapi.GeoObject(ip, *source, raw); g != nil {
		return g
	}
	return nil
}

func nodeArg(node string) any {
	if node == "" {
		return nil
	}
	return node
}

// History lists address changes, newest first. The first address a node ever reports is not a change and is not listed.
func (s *Store) History(ctx context.Context, node string, page, size int) ([]map[string]any, int, error) {
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM node_ip_history WHERE from_ip IS NOT NULL AND ($1::uuid IS NULL OR node_id = $1::uuid)`, nodeArg(node)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT h.id::text, h.node_id::text, h.ts, host(h.from_ip), host(h.to_ip), fi.source, fi.geo, ti.source, ti.geo,
		fo.id::text, fo.asn, fo.organisation, fo.name, ti_isp.id::text, ti_isp.asn, ti_isp.organisation, ti_isp.name
		FROM node_ip_history h
		JOIN ip_addresses ti ON ti.ip = h.to_ip
		JOIN ip_addresses fi ON fi.ip = h.from_ip
		LEFT JOIN isps fo ON fo.id = COALESCE(h.from_isp_id, fi.isp_id)
		LEFT JOIN isps ti_isp ON ti_isp.id = COALESCE(h.to_isp_id, ti.isp_id)
		WHERE h.from_ip IS NOT NULL AND ($1::uuid IS NULL OR h.node_id = $1::uuid)
		ORDER BY h.ts DESC, h.id DESC LIMIT $2 OFFSET $3`, nodeArg(node), size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, nodeID, from, to string
		var ts time.Time
		var fromSource, toSource *string
		var fromGeo, toGeo []byte
		var fromISPID, fromOrg, fromName, toISPID, toOrg, toName *string
		var fromASN, toASN *uint
		if err := rows.Scan(&id, &nodeID, &ts, &from, &to, &fromSource, &fromGeo, &toSource, &toGeo,
			&fromISPID, &fromASN, &fromOrg, &fromName, &toISPID, &toASN, &toOrg, &toName); err != nil {
			return nil, 0, err
		}
		items = append(items, map[string]any{"id": id, "nodeId": nodeID, "ts": ts.UTC().Format(time.RFC3339Nano), "from": from, "to": to,
			"fromGeo": geoOf(from, fromSource, fromGeo), "toGeo": geoOf(to, toSource, toGeo),
			"fromIsp": ispView(fromISPID, fromASN, fromOrg, fromName), "toIsp": ispView(toISPID, toASN, toOrg, toName)})
	}
	return items, total, rows.Err()
}

func ispView(id *string, asn *uint, org, name *string) any {
	if id == nil || asn == nil || org == nil || name == nil {
		return nil
	}
	return map[string]any{"id": *id, "asn": *asn, "organisation": *org, "name": *name}
}

// Addresses lists the distinct addresses each node has used, most recently seen first.
func (s *Store) Addresses(ctx context.Context, node string, page, size int) ([]map[string]any, int, error) {
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM node_ip_addresses WHERE $1::uuid IS NULL OR node_id = $1::uuid`, nodeArg(node)).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT a.node_id::text, host(a.ip), a.first_seen, a.last_seen, a.tests,
		CASE WHEN a.speed_tests > 0 THEN (a.down_sum / a.speed_tests)::float8 END,
		CASE WHEN a.speed_tests > 0 THEN (a.up_sum / a.speed_tests)::float8 END, i.source, i.geo
		FROM node_ip_addresses a JOIN ip_addresses i ON i.ip = a.ip
		WHERE $1::uuid IS NULL OR a.node_id = $1::uuid
		ORDER BY a.last_seen DESC, a.node_id, a.ip LIMIT $2 OFFSET $3`, nodeArg(node), size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var nodeID, ip string
		var first, last time.Time
		var tests int64
		var down, up *float64
		var source *string
		var raw []byte
		if err := rows.Scan(&nodeID, &ip, &first, &last, &tests, &down, &up, &source, &raw); err != nil {
			return nil, 0, err
		}
		item := map[string]any{"nodeId": nodeID, "ip": ip, "first": first.UTC().Format(time.RFC3339Nano), "last": last.UTC().Format(time.RFC3339Nano),
			"tests": tests, "avgDown": nil, "avgUp": nil, "geo": geoOf(ip, source, raw)}
		if down != nil {
			item["avgDown"] = *down
		}
		if up != nil {
			item["avgUp"] = *up
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// Map returns the most recently observed address of each node with its location; nodes that never reported an address are left out.
func (s *Store) Map(ctx context.Context, node string) ([]map[string]any, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT ON (a.node_id) a.node_id::text, host(a.ip), i.source, i.geo
		FROM node_ip_addresses a JOIN ip_addresses i ON i.ip = a.ip
		WHERE $1::uuid IS NULL OR a.node_id = $1::uuid
		ORDER BY a.node_id, a.last_seen DESC, a.ip`, nodeArg(node))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var nodeID, ip string
		var source *string
		var raw []byte
		if err := rows.Scan(&nodeID, &ip, &source, &raw); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"nodeId": nodeID, "ip": ip, "geo": geoOf(ip, source, raw)})
	}
	return items, rows.Err()
}

// ParseLiteral accepts only IPv4 and IPv6 literals. Host names, zones, ports and prefixes are refused, so a lookup can never
// trigger name resolution.
func ParseLiteral(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 45 || strings.ContainsAny(text, "%/[]") {
		return "", ErrInvalid
	}
	ip := net.ParseIP(text)
	if ip == nil {
		return "", ErrInvalid
	}
	return ip.String(), nil
}

// Lookup returns the Geo object for an address, or nil when no source knows it. The chain caches what it finds.
func (s *Store) Lookup(ctx context.Context, text string) (map[string]any, error) {
	ip, err := ParseLiteral(text)
	if err != nil {
		return nil, err
	}
	if s.Resolver == nil {
		return nil, nil
	}
	location, err := s.Resolver.Lookup(ctx, ip)
	if errors.Is(err, geo.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var source string
	if err := s.DB.QueryRow(ctx, `SELECT source FROM ip_addresses WHERE ip = $1::inet`, ip).Scan(&source); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	raw, err := json.Marshal(location)
	if err != nil {
		return nil, err
	}
	return ownerapi.GeoObject(ip, source, raw), nil
}
