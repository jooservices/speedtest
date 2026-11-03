// Package agentapi implements the authenticated node reporting protocol.
package agentapi

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalid  = errors.New("invalid input")
	ErrConflict = errors.New("conflicting state")
	ErrNotFound = errors.New("resource not found")
)

// Result preserves nullable measurements and original engine details.
type Result struct {
	ID              string          `json:"id"`
	TS              string          `json:"ts"`
	Status          string          `json:"status"`
	Down            *float64        `json:"down"`
	Up              *float64        `json:"up"`
	Ping            *float64        `json:"ping"`
	Jitter          *float64        `json:"jitter"`
	DLat            *float64        `json:"dLat"`
	ULat            *float64        `json:"uLat"`
	Loss            *float64        `json:"loss"`
	ServerID        *int            `json:"serverId"`
	IP              string          `json:"ip"`
	Threads         int             `json:"threads"`
	BytesDown       int64           `json:"bytesDown"`
	BytesUp         int64           `json:"bytesUp"`
	Duration        float64         `json:"duration"`
	Error           *string         `json:"error"`
	Manual          bool            `json:"manual"`
	Raw             json.RawMessage `json:"raw"`
	LoadedDown      *LoadedLatency  `json:"loadedDown"`
	LoadedUp        *LoadedLatency  `json:"loadedUp"`
	Server          *Server         `json:"server"`
	EngineISP       *string         `json:"engineISP"`
	EngineResultID  *string         `json:"engineResultId"`
	EngineResultURL *string         `json:"engineResultUrl"`
	EngineIP        *string         `json:"engineIp"`
	Warnings        []string        `json:"warnings"`
}

type LoadedLatency struct {
	Low  *float64 `json:"low"`
	IQM  *float64 `json:"iqm"`
	High *float64 `json:"high"`
}

type Server struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	City    string `json:"city"`
	Sponsor string `json:"sponsor"`
	Host    string `json:"host,omitempty"`
}

// Decode rejects unknown fields, trailing JSON, missing required keys and null
// for nonnullable properties. Nullable values remain distinguishable from zero.
func Decode(data []byte, target any, required ...string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return ErrInvalid
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return ErrInvalid
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || strictShape(data, reflect.TypeOf(target).Elem()) != nil {
		return ErrInvalid
	}
	return nil
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

func nonNull(data []byte, keys ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
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
	if Decode(data, &r, "id", "ts", "status", "down", "up", "ping", "jitter", "dLat", "uLat", "loss", "serverId", "ip", "threads", "bytesDown", "bytesUp", "duration", "error", "manual") != nil ||
		!nonNull(data, "id", "ts", "status", "ip", "threads", "bytesDown", "bytesUp", "duration", "manual", "raw", "warnings") {
		return r, ErrInvalid
	}
	if !UUID(r.ID) || !timestamp(r.TS) || !address(r.IP) || r.Threads < 1 || r.Threads > 64 || r.BytesDown < 0 || r.BytesUp < 0 || !number(&r.Duration) || !text(r.Error, 2000) {
		return r, ErrInvalid
	}
	for _, metric := range []*float64{r.Down, r.Up, r.Ping, r.Jitter, r.DLat, r.ULat, r.Loss} {
		if !number(metric) {
			return r, ErrInvalid
		}
	}
	if r.Loss != nil && *r.Loss > 100 || r.ServerID != nil && *r.ServerID < 1 {
		return r, ErrInvalid
	}
	switch r.Status {
	case "ok", "degraded":
		if r.Down == nil || r.Up == nil || r.Ping == nil || r.ServerID == nil || r.Error != nil {
			return r, ErrInvalid
		}
	case "failed":
		if r.Error == nil || strings.TrimSpace(*r.Error) == "" {
			return r, ErrInvalid
		}
	default:
		return r, ErrInvalid
	}
	if r.Raw == nil {
		r.Raw = json.RawMessage(`{}`)
	}
	var raw map[string]json.RawMessage
	if len(r.Raw) > 64<<10 || json.Unmarshal(r.Raw, &raw) != nil || raw == nil {
		return r, ErrInvalid
	}
	for key, loaded := range map[string]*LoadedLatency{"loadedDown": r.LoadedDown, "loadedUp": r.LoadedUp} {
		if loaded != nil {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(data, &fields)
			if Decode(fields[key], new(LoadedLatency), "low", "iqm", "high") != nil || !number(loaded.Low) || !number(loaded.IQM) || !number(loaded.High) {
				return r, ErrInvalid
			}
		}
	}
	if r.Server != nil {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		if Decode(fields["server"], new(Server), "id", "name", "city", "sponsor") != nil || !nonNull(fields["server"], "id", "name", "city", "sponsor", "host") || r.Server.ID < 1 || utf8.RuneCountInString(r.Server.Host) > 255 {
			return r, ErrInvalid
		}
	}
	if r.EngineIP != nil && !address(*r.EngineIP) {
		return r, ErrInvalid
	}
	if r.EngineResultURL != nil {
		u, err := url.Parse(*r.EngineResultURL)
		if err != nil || u.Scheme == "" {
			return r, ErrInvalid
		}
	}
	if len(r.Warnings) > 10 {
		return r, ErrInvalid
	}
	for _, warning := range r.Warnings {
		if utf8.RuneCountInString(warning) > 2000 {
			return r, ErrInvalid
		}
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	r.ID = strings.ToLower(r.ID)
	return r, nil
}

type UpdateResult struct {
	Version string  `json:"version"`
	Status  string  `json:"status"`
	TS      string  `json:"ts"`
	Error   *string `json:"error"`
}

type Heartbeat struct {
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
	Status   string   `json:"status"`
	Phase    *string  `json:"phase,omitempty"`
	Pct      *float64 `json:"pct,omitempty"`
	ResultID *string  `json:"resultId,omitempty"`
	Error    *string  `json:"error,omitempty"`
}

func ParseAck(data []byte) (Ack, error) {
	var a Ack
	if Decode(data, &a, "status") != nil || !nonNull(data, "status", "phase", "pct") || !text(a.Phase, 200) || !text(a.Error, 2000) || !number(a.Pct) || a.Pct != nil && *a.Pct > 100 || a.ResultID != nil && !UUID(*a.ResultID) {
		return a, ErrInvalid
	}
	if a.Status != "running" && a.Status != "succeeded" && a.Status != "failed" || a.Status == "failed" && (a.Error == nil || strings.TrimSpace(*a.Error) == "") {
		return a, ErrInvalid
	}
	return a, nil
}

type Command struct {
	ID        string     `json:"id"`
	NodeID    string     `json:"nodeId"`
	Type      string     `json:"type"`
	Version   *string    `json:"version"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	AckedAt   *time.Time `json:"ackedAt"`
	Phase     string     `json:"phase"`
	Pct       float64    `json:"pct"`
	ResultID  *string    `json:"resultId"`
	Error     *string    `json:"error"`
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
	terminal := c.Status == "succeeded" || c.Status == "failed"
	if next.Pct < c.Pct || terminal && !sameAck(c, next) {
		return c, ErrConflict
	}
	return next, nil
}
func sameAck(a, b Command) bool {
	return a.Status == b.Status && a.Phase == b.Phase && a.Pct == b.Pct && equalString(a.ResultID, b.ResultID) && equalString(a.Error, b.Error)
}
func equalString(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
