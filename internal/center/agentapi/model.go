// Package agentapi implements the authenticated node reporting protocol.
package agentapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

var (
	ErrInvalid  = errors.New("invalid input")
	ErrConflict = errors.New("conflicting state")
	ErrNotFound = errors.New("resource not found")
)

// Result preserves nullable measurements and original engine details.
type Result struct {
	Engine               string   `json:"engine,omitempty"`
	ID                   string   `json:"id"`
	TS                   string   `json:"ts"`
	Status               string   `json:"status"`
	Down                 *float64 `json:"down"`
	Up                   *float64 `json:"up"`
	Ping                 *float64 `json:"ping"`
	Jitter               *float64 `json:"jitter"`
	DLat                 *float64 `json:"dLat"`
	ULat                 *float64 `json:"uLat"`
	Loss                 *float64 `json:"loss"`
	ServerID             *int     `json:"serverId"`
	IP                   *string  `json:"ip"`
	Threads              int      `json:"threads"`
	BytesDown            int64    `json:"bytesDown"`
	BytesUp              int64    `json:"bytesUp"`
	Duration             float64  `json:"duration"`
	Error                *string  `json:"error"`
	FailureClass         string   `json:"failureClass,omitempty"`
	classOmitted         bool
	Manual               bool            `json:"manual"`
	Raw                  json.RawMessage `json:"raw"`
	LoadedDown           *LoadedLatency  `json:"loadedDown"`
	LoadedUp             *LoadedLatency  `json:"loadedUp"`
	Server               *Server         `json:"server"`
	EngineISP            *string         `json:"engineISP"`
	EngineResultID       *string         `json:"engineResultId"`
	EngineResultURL      *string         `json:"engineResultUrl"`
	EngineIP             *string         `json:"engineIp"`
	Warnings             []string        `json:"warnings"`
	ServerSelectionKnown bool            `json:"serverSelectionKnown"`
	PinnedServerID       *int64          `json:"pinnedServerId,omitempty"`
}

type LoadedLatency struct {
	Low  *float64 `json:"low"`
	IQM  *float64 `json:"iqm"`
	High *float64 `json:"high"`
}

type Server struct {
	ID      *int   `json:"id,omitempty"`
	Name    string `json:"name"`
	City    string `json:"city"`
	Sponsor string `json:"sponsor"`
	Host    string `json:"host,omitempty"`
}

// ServerOption is one discovered target that an owner can pin for an engine.
type ServerOption struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	City       string   `json:"city"`
	ISP        string   `json:"isp"`
	Host       string   `json:"host"`
	DistanceKM *float64 `json:"distanceKm,omitempty"`
}

// Decode rejects unknown fields, trailing JSON, missing required keys and null
// for nonnullable properties. Nullable values remain distinguishable from zero.
func Decode(data []byte, target any, required ...string) error {
	_, err := decodeFields(data, target, required...)
	return err
}

func decodeFields(data []byte, target any, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, ErrInvalid
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, ErrInvalid
		}
	}
	targetType := reflect.TypeOf(target).Elem()
	if targetType.Kind() == reflect.Struct {
		if strictShapeFields(fields, targetType) != nil {
			return nil, ErrInvalid
		}
	} else if strictShape(data, targetType) != nil {
		return nil, ErrInvalid
	}
	if json.Unmarshal(data, target) != nil {
		return nil, ErrInvalid
	}
	return fields, nil
}

// encoding/json accepts null into scalar fields and matches struct keys without
// case sensitivity. The contract permits neither behavior for those properties.
func strictShape(data []byte, kind reflect.Type) error {
	if kind == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if kind.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		return strictShape(data, kind.Elem())
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrInvalid
	}
	switch kind.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return ErrInvalid
		}
		return strictShapeFields(fields, kind)
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return ErrInvalid
		}
		for _, item := range items {
			if strictShape(item, kind.Elem()) != nil {
				return ErrInvalid
			}
		}
	}
	return nil
}

func strictShapeFields(fields map[string]json.RawMessage, kind reflect.Type) error {
	known := make(map[string]reflect.Type, kind.NumField())
	for i := range kind.NumField() {
		f := kind.Field(i)
		known[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
	}
	for key, value := range fields {
		fieldType, ok := known[key]
		if !ok || strictShape(value, fieldType) != nil {
			return ErrInvalid
		}
	}
	return nil
}

func nonNull(data []byte, keys ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	return nonNullFields(fields, keys...)
}

func nonNullFields(fields map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if value, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func UUID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil
}

func timestamp(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && strings.HasSuffix(value, "Z")
}

func number(value *float64) bool {
	return value == nil || (*value >= 0 && !math.IsInf(*value, 0) && !math.IsNaN(*value))
}
func address(value string) bool {
	ip, err := netip.ParseAddr(value)
	return err == nil && ip.Zone() == ""
}
func text(value *string, max int) bool { return value == nil || utf8.RuneCountInString(*value) <= max }

// ParseResult validates one item independently of its siblings.
func ParseResult(data []byte) (Result, error) {
	var r Result
	fields, err := decodeFields(data, &r, "id", "ts", "status", "down", "up", "ping", "jitter", "dLat", "uLat", "loss", "serverId", "ip", "threads", "bytesDown", "bytesUp", "duration", "error", "manual")
	if err != nil || !nonNullFields(fields, "id", "ts", "status", "threads", "bytesDown", "bytesUp", "duration", "manual", "raw", "warnings") {
		return r, ErrInvalid
	}
	_, present := fields["failureClass"]
	r.classOmitted = !present
	if present && (!nonNullFields(fields, "failureClass") || r.FailureClass == "") {
		return r, ErrInvalid
	}
	if r.Engine == "" {
		r.Engine = "ookla"
	}
	if r.Engine != "ookla" && r.Engine != "librespeed" {
		return r, ErrInvalid
	}
	if !UUID(r.ID) || !timestamp(r.TS) || (r.IP != nil && !address(*r.IP)) || r.Threads < 1 || r.Threads > 64 || r.BytesDown < 0 || r.BytesUp < 0 || !number(&r.Duration) || !text(r.Error, 2000) {
		return r, ErrInvalid
	}
	for _, metric := range []*float64{r.Down, r.Up, r.Ping, r.Jitter, r.DLat, r.ULat, r.Loss} {
		if !number(metric) {
			return r, ErrInvalid
		}
	}
	if r.Loss != nil && *r.Loss > 100 || r.ServerID != nil && *r.ServerID < 1 || r.PinnedServerID != nil && (*r.PinnedServerID < 1 || *r.PinnedServerID > math.MaxInt32 || !r.ServerSelectionKnown) {
		return r, ErrInvalid
	}
	if err := r.validateStatus(); err != nil {
		return r, err
	}
	if r.Raw == nil {
		r.Raw = json.RawMessage(`{}`)
	}
	var raw map[string]json.RawMessage
	if len(r.Raw) > 64<<10 || json.Unmarshal(r.Raw, &raw) != nil || raw == nil {
		return r, ErrInvalid
	}
	if err := r.validateDetails(fields); err != nil {
		return r, err
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	r.ID = strings.ToLower(r.ID)
	return r, nil
}

// validateStatus applies the rules that depend on the result status and fills the default failure class.
func (r *Result) validateStatus() error {
	switch r.Status {
	case "ok", "degraded":
		if r.Down == nil || r.Up == nil || r.Ping == nil || r.Server == nil || r.IP == nil || r.Error != nil {
			return ErrInvalid
		}
		if r.FailureClass == "" {
			r.FailureClass = failureclass.Other
		}
		if r.FailureClass != failureclass.Other {
			return ErrInvalid
		}
	case "failed":
		if r.Error == nil || strings.TrimSpace(*r.Error) == "" {
			return ErrInvalid
		}
		if r.FailureClass == "" {
			r.FailureClass = failureclass.FromText(*r.Error)
		}
		if !failureclass.Valid(r.FailureClass) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type UpdateResult struct {
	Version string  `json:"version"`
	Status  string  `json:"status"`
	TS      string  `json:"ts"`
	Error   *string `json:"error"`
}

type Heartbeat struct {
	Engines      []string      `json:"engines,omitempty"`
	Agent        string        `json:"agent"`
	Uptime       int64         `json:"uptime"`
	LastTest     *string       `json:"lastTest"`
	UpdateResult *UpdateResult `json:"updateResult"`
}

func ParseHeartbeat(data []byte) (Heartbeat, error) {
	var h Heartbeat
	if Decode(data, &h, "agent", "uptime", "lastTest") != nil || !nonNull(data, "agent", "uptime") || utf8.RuneCountInString(h.Agent) > 64 || h.Uptime < 0 || h.LastTest != nil && !timestamp(*h.LastTest) {
		return h, ErrInvalid
	}
	seenEngines := map[string]bool{}
	for _, name := range h.Engines {
		if (name != "ookla" && name != "librespeed") || seenEngines[name] {
			return h, ErrInvalid
		}
		seenEngines[name] = true
	}
	if h.UpdateResult != nil {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		u := h.UpdateResult
		if Decode(fields["updateResult"], new(UpdateResult), "version", "status", "ts", "error") != nil || !nonNull(fields["updateResult"], "version", "status", "ts") || !timestamp(u.TS) || (u.Status != "succeeded" && u.Status != "failed" && u.Status != "rolled_back") {
			return h, ErrInvalid
		}
	}
	return h, nil
}

type Ack struct {
	Status   string          `json:"status"`
	Phase    *string         `json:"phase,omitempty"`
	Pct      *float64        `json:"pct,omitempty"`
	ResultID *string         `json:"resultId,omitempty"`
	Error    *string         `json:"error,omitempty"`
	Servers  *[]ServerOption `json:"servers,omitempty"`
}

func ParseAck(data []byte) (Ack, error) {
	var a Ack
	if Decode(data, &a, "status") != nil || !nonNull(data, "status", "phase", "pct", "servers") || !text(a.Phase, 200) || !text(a.Error, 2000) || !number(a.Pct) || a.Pct != nil && *a.Pct > 100 || a.ResultID != nil && !UUID(*a.ResultID) || !validServerOptions(a.Servers) {
		return a, ErrInvalid
	}
	if a.Status != "running" && a.Status != "succeeded" && a.Status != "failed" || a.Status == "failed" && (a.Error == nil || strings.TrimSpace(*a.Error) == "") {
		return a, ErrInvalid
	}
	if a.Servers != nil && a.Status != "succeeded" {
		return a, ErrInvalid
	}
	return a, nil
}

func validServerOptions(servers *[]ServerOption) bool {
	if servers == nil {
		return true
	}
	if len(*servers) > 1000 {
		return false
	}
	seen := make(map[int64]bool, len(*servers))
	for _, server := range *servers {
		if server.ID < 1 || server.ID > math.MaxInt32 || seen[server.ID] || strings.TrimSpace(server.Name) == "" || !textValue(server.Name, 200) || !textValue(server.City, 120) || !textValue(server.ISP, 200) || !textValue(server.Host, 255) || server.DistanceKM != nil && !number(server.DistanceKM) {
			return false
		}
		seen[server.ID] = true
	}
	return true
}

func textValue(value string, max int) bool { return utf8.RuneCountInString(value) <= max }

type Command struct {
	ID        string     `json:"id"`
	NodeID    string     `json:"nodeId"`
	Type      string     `json:"type"`
	Engine    string     `json:"engine,omitempty"`
	Version   *string    `json:"version"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	AckedAt   *time.Time `json:"ackedAt"`
	Phase     string     `json:"phase"`
	Pct       float64    `json:"pct"`
	ResultID  *string    `json:"resultId"`
	Error     *string    `json:"error"`
	AckDigest []byte     `json:"-"`
}

// ApplyAck enforces monotonic progress and immutable terminal acknowledgements.
func ApplyAck(c Command, a Ack) (Command, error) {
	next := c
	next.Status = a.Status
	if a.Phase != nil {
		next.Phase = *a.Phase
	}
	if a.Pct != nil {
		next.Pct = *a.Pct
	}
	if a.ResultID != nil {
		id := strings.ToLower(*a.ResultID)
		next.ResultID = &id
	}
	next.Error = a.Error
	if a.Servers != nil {
		digest, err := serverOptionsDigest(*a.Servers)
		if err != nil {
			return c, ErrInvalid
		}
		next.AckDigest = digest
	}
	terminal := c.Status == "succeeded" || c.Status == "failed"
	if next.Pct < c.Pct || terminal && !sameAck(c, next) {
		return c, ErrConflict
	}
	return next, nil
}
func sameAck(a, b Command) bool {
	return a.Status == b.Status && a.Phase == b.Phase && a.Pct == b.Pct && equalString(a.ResultID, b.ResultID) && equalString(a.Error, b.Error) && bytes.Equal(a.AckDigest, b.AckDigest)
}
func equalString(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func serverOptionsDigest(servers []ServerOption) ([]byte, error) {
	ordered := append([]ServerOption(nil), servers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	payload, err := json.Marshal(ordered)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return digest[:], nil
}

func (r Result) validateDetails(fields map[string]json.RawMessage) error {
	for key, loaded := range map[string]*LoadedLatency{"loadedDown": r.LoadedDown, "loadedUp": r.LoadedUp} {
		if loaded != nil {
			if Decode(fields[key], new(LoadedLatency), "low", "iqm", "high") != nil || !number(loaded.Low) || !number(loaded.IQM) || !number(loaded.High) {
				return ErrInvalid
			}
		}
	}
	if r.Server != nil {
		serverFields, err := decodeFields(fields["server"], new(Server))
		if err != nil || !nonNullFields(serverFields, "id", "name", "city", "sponsor", "host") || (r.Server.ID != nil && *r.Server.ID < 1) || (strings.TrimSpace(r.Server.Name) == "" && strings.TrimSpace(r.Server.Host) == "") || utf8.RuneCountInString(r.Server.Host) > 255 {
			return ErrInvalid
		}
	}
	if r.EngineIP != nil && !address(*r.EngineIP) {
		return ErrInvalid
	}
	if r.EngineResultURL != nil {
		u, err := url.Parse(*r.EngineResultURL)
		if err != nil || u.Scheme == "" {
			return ErrInvalid
		}
	}
	if len(r.Warnings) > 10 {
		return ErrInvalid
	}
	for _, warning := range r.Warnings {
		if utf8.RuneCountInString(warning) > 2000 {
			return ErrInvalid
		}
	}
	return nil
}
