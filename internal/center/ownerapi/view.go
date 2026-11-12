// Package ownerapi implements the dashboard (session-protected) read and write
// operations on nodes and results, producing the JSON objects of docs/openapi.yaml.
package ownerapi

import (
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/geo"
)

// ResultColumns lists the columns scanned by ScanResult, qualified with the alias r.
const ResultColumns = `r.id::text, r.node_id::text, r.ts, r.status, r.down_mbps, r.up_mbps, r.ping_ms, r.jitter_ms,
	r.down_latency_ms, r.up_latency_ms, r.loss_percent, r.server_id, r.server, host(r.ip), r.threads, r.bytes_down,
	r.bytes_up, r.duration_seconds, r.error, r.manual, r.raw, r.loaded_down, r.loaded_up, r.engine_isp,
	r.engine_result_id, r.engine_result_url, host(r.engine_ip), r.warnings, r.engine, r.failure_class,
	r.server_selection_known, r.pinned_server_id_at_test, r.isp_id::text`

// ResultView is a result in the shape of the contract plus the IP used to attach its location.
type ResultView struct {
	Object map[string]any
	IP     string
}

// ScanResult reads one row selected with ResultColumns followed by the columns for extra. The geo member is nil
// until the caller attaches it.
func ScanResult(row pgx.Row, extra ...any) (ResultView, error) {
	var id, nodeID, status, engine, failureClass string
	var ts time.Time
	var down, up, ping, jitter, dLat, uLat, loss, duration *float64
	var bytesDown, bytesUp *int64
	var threads int32
	var server, raw, loadedDown, loadedUp, warnings []byte
	var ip, errText, engineISP, engineResultID, engineResultURL, engineIP, ispID *string
	var manual bool
	var sid *int32
	var selectionKnown bool
	var pinnedSID *int64
	dest := []any{&id, &nodeID, &ts, &status, &down, &up, &ping, &jitter, &dLat, &uLat, &loss, &sid, &server, &ip,
		&threads, &bytesDown, &bytesUp, &duration, &errText, &manual, &raw, &loadedDown, &loadedUp, &engineISP,
		&engineResultID, &engineResultURL, &engineIP, &warnings, &engine, &failureClass, &selectionKnown, &pinnedSID, &ispID}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return ResultView{}, err
	}
	var serverIDValue any
	if sid != nil {
		serverIDValue = int64(*sid)
	}
	obj := map[string]any{
		"engine": engine, "id": id, "nodeId": nodeID, "ts": ts.UTC().Format(time.RFC3339Nano), "status": status,
		"down": nullable(down), "up": nullable(up), "ping": nullable(ping), "jitter": nullable(jitter),
		"dLat": nullable(dLat), "uLat": nullable(uLat), "loss": nullable(loss), "serverId": serverIDValue,
		"ip": nullable(ip), "threads": int64(threads), "bytesDown": nullable(bytesDown), "bytesUp": nullable(bytesUp),
		"duration": nullable(duration), "error": nullable(errText), "failureClass": failureClass, "manual": manual,
		"raw": jsonObject(raw), "server": jsonOrNil(server), "geo": nil,
		"loadedDown": jsonOrNil(loadedDown), "loadedUp": jsonOrNil(loadedUp),
		"engineISP": nullable(engineISP), "engineResultId": nullable(engineResultID),
		"engineResultUrl": nullable(engineResultURL), "engineIp": nullable(engineIP), "warnings": jsonArray(warnings), "pinnedServerId": nullable(pinnedSID),
		"serverSelectionKnown": selectionKnown, "ispId": nullable(ispID),
	}
	view := ResultView{Object: obj}
	if ip != nil {
		view.IP = *ip
	}
	return view, nil
}

func nullable[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

func jsonOrNil(raw []byte) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func jsonObject(raw []byte) any {
	if v, ok := jsonOrNil(raw).(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func jsonArray(raw []byte) any {
	if v, ok := jsonOrNil(raw).([]any); ok {
		return v
	}
	return []any{}
}

// stored mirrors the JSON written to ip_addresses.geo by the geolocation package.
type stored struct {
	City           string            `json:"city"`
	Region         string            `json:"region"`
	Country        string            `json:"country"`
	Latitude       float64           `json:"latitude"`
	Longitude      float64           `json:"longitude"`
	TimeZone       string            `json:"time_zone"`
	AccuracyRadius float64           `json:"accuracy_radius"`
	ASN            uint              `json:"asn"`
	Organisation   string            `json:"organisation"`
	QualityScore   int               `json:"quality_score"`
	FieldSources   map[string]string `json:"field_sources"`
}

// GeoObject converts a stored geolocation record into the contract Geo object.
// It returns nil for an address that has no stored location.
func GeoObject(ip string, source string, raw []byte) map[string]any {
	return GeoObjectAt(ip, source, raw, 0)
}

// GeoObjectAt adds a live quality score and inferred field sources to a stored record.
func GeoObjectAt(ip string, source string, raw []byte, ageDays int) map[string]any {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil
	}
	var g stored
	if len(raw) == 0 || json.Unmarshal(raw, &g) != nil || (g.City == "" && g.Country == "" && g.ASN == 0 && g.Latitude == 0 && g.Longitude == 0) {
		return nil
	}
	version := 6
	if parsed.To4() != nil {
		version = 4
	}
	if source == "" {
		source = "unknown"
	}
	return geoObjectFromStored(parsed.String(), version, source, g, ageDays)
}

func geoObjectFromStored(ip string, version int, source string, g stored, ageDays int) map[string]any {
	obj := map[string]any{"ip": ip, "version": version, "source": source}
	location := geo.Location{City: g.City, Region: g.Region, Country: g.Country, Latitude: g.Latitude, Longitude: g.Longitude,
		TimeZone: g.TimeZone, AccuracyRadius: uint16(max(0, min(int(g.AccuracyRadius), 65535))), ASN: g.ASN, Organisation: g.Organisation,
		FieldSources: sourceFields(g.FieldSources)}
	if g.QualityScore == 0 || ageDays > 0 {
		g.QualityScore = geo.QualityScore(location, source, ageDays)
	}
	obj["qualityScore"] = g.QualityScore
	if len(g.FieldSources) == 0 {
		merged, _ := geo.MergeLocations(location, source, geo.Location{}, "")
		g.FieldSources = fieldSourcesMap(merged.FieldSources)
	}
	if len(g.FieldSources) > 0 {
		obj["fieldSources"] = g.FieldSources
	}
	if g.ASN > 0 {
		obj["asn"] = g.ASN
	}
	if g.Organisation != "" {
		obj["asnOrg"] = g.Organisation
		obj["isp"] = g.Organisation
	}
	if g.Country != "" {
		obj["country"] = g.Country
		if len(g.Country) == 2 {
			obj["countryCode"] = strings.ToUpper(g.Country)
		}
	}
	if g.Region != "" {
		obj["region"] = g.Region
	}
	if g.City != "" {
		obj["city"] = g.City
	}
	if g.Latitude != 0 || g.Longitude != 0 {
		obj["lat"], obj["lon"] = g.Latitude, g.Longitude
	}
	if g.TimeZone != "" {
		obj["timezone"] = g.TimeZone
	}
	if g.AccuracyRadius > 0 {
		obj["radiusKm"] = g.AccuracyRadius
	}
	return obj
}

func sourceFields(values map[string]string) geo.FieldSources {
	return geo.FieldSources{City: values["city"], Region: values["region"], Country: values["country"], TimeZone: values["time_zone"],
		Organisation: values["organisation"], Coordinates: values["coordinates"], ASN: values["asn"], AccuracyRadius: values["accuracy_radius"]}
}

func fieldSourcesMap(s geo.FieldSources) map[string]string {
	values := map[string]string{"city": s.City, "region": s.Region, "country": s.Country, "time_zone": s.TimeZone,
		"organisation": s.Organisation, "coordinates": s.Coordinates, "asn": s.ASN, "accuracy_radius": s.AccuracyRadius}
	for key, value := range values {
		if value == "" {
			delete(values, key)
		}
	}
	return values
}
