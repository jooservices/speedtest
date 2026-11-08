package report

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jooservices/speedtest/internal/shared/failureclass"
)

// Status values a result can report to the command center.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusFailed   = "failed"
)

// DefaultThreads is used when no thread count is supplied. It matches the
// default the command center advertises for a new node.
const DefaultThreads = 4

// LoadedLatency preserves engine-reported loaded latency in milliseconds; a
// nil member means the value is unavailable.
type LoadedLatency struct {
	Low  *float64 `json:"low"`
	IQM  *float64 `json:"iqm"`
	High *float64 `json:"high"`
}

// Server retains the measurement server identity in the wire shape. City is the
// engine's location string and Sponsor is empty because the engine does not
// expose a distinct sponsor value.
type Server struct {
	ID      int64  `json:"id,omitempty"`
	Name    string `json:"name"`
	City    string `json:"city"`
	Sponsor string `json:"sponsor"`
	Host    string `json:"host,omitempty"`
}

// Result is one reportable result, mirroring the ResultInput wire schema in
// docs/openapi.yaml. ID is the UUID allocated before spooling; it is what makes
// a delivery idempotent at the center. Measurements are nullable; unavailable
// values stay null, never zero. Required keys (down, up, ping, jitter, dLat,
// uLat, loss, serverId, error) are always present, null when unavailable.
type Result struct {
	Engine string `json:"engine,omitempty"`
	ID     string `json:"id"`
	TS     string `json:"ts"`
	Status string `json:"status"`

	Down   *float64 `json:"down"`
	Up     *float64 `json:"up"`
	Ping   *float64 `json:"ping"`
	Jitter *float64 `json:"jitter"`
	DLat   *float64 `json:"dLat"`
	ULat   *float64 `json:"uLat"`
	Loss   *float64 `json:"loss"`

	ServerID     *int64  `json:"serverId"`
	IP           string  `json:"ip"`
	Threads      int     `json:"threads"`
	BytesDown    int64   `json:"bytesDown"`
	BytesUp      int64   `json:"bytesUp"`
	Duration     float64 `json:"duration"`
	Error        *string `json:"error"`
	FailureClass string  `json:"failureClass,omitempty"`
	Manual       bool    `json:"manual"`

	Raw        json.RawMessage `json:"raw,omitempty"`
	LoadedDown *LoadedLatency  `json:"loadedDown,omitempty"`
	LoadedUp   *LoadedLatency  `json:"loadedUp,omitempty"`
	Server     *Server         `json:"server,omitempty"`

	EngineISP       string   `json:"engineISP,omitempty"`
	EngineResultID  string   `json:"engineResultId,omitempty"`
	EngineResultURL string   `json:"engineResultUrl,omitempty"`
	EngineIP        string   `json:"engineIp,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// Validate checks that r satisfies the wire schema constraints, so an invalid
// result is rejected here rather than quarantined by the center after transport.
func (r Result) Validate() error {
	if r.Engine != "" && r.Engine != "ookla" && r.Engine != "librespeed" {
		return errors.New("unsupported engine")
	}
	if !isUUID(r.ID) {
		return errors.New("result id must be a UUID")
	}
	if _, err := time.Parse(time.RFC3339, r.TS); err != nil || !strings.HasSuffix(r.TS, "Z") {
		return fmt.Errorf("result ts must be RFC 3339 UTC, got %q", r.TS)
	}
	if _, err := netip.ParseAddr(r.IP); err != nil && (r.IP != "" || r.Status != StatusFailed) {
		return fmt.Errorf("result ip must be an IP literal, got %q", r.IP)
	}
	if r.Threads < 1 || r.Threads > 64 {
		return fmt.Errorf("result threads must be between 1 and 64, got %d", r.Threads)
	}
	if r.BytesDown < 0 || r.BytesUp < 0 {
		return fmt.Errorf("result byte counts must be >= 0")
	}
	if !finite(r.Duration) || r.Duration < 0 {
		return fmt.Errorf("result duration must be >= 0, got %f", r.Duration)
	}
	for _, m := range []struct {
		name string
		v    *float64
	}{{"down", r.Down}, {"up", r.Up}, {"ping", r.Ping}, {"jitter", r.Jitter}, {"dLat", r.DLat}, {"uLat", r.ULat}} {
		if m.v != nil && (!finite(*m.v) || *m.v < 0) {
			return fmt.Errorf("result %s must be >= 0", m.name)
		}
	}
	if r.Loss != nil && (!finite(*r.Loss) || *r.Loss < 0 || *r.Loss > 100) {
		return fmt.Errorf("result loss must be between 0 and 100, got %f", *r.Loss)
	}

	if err := r.validateDetails(); err != nil {
		return err
	}
	switch r.Status {
	case StatusFailed:
		if r.Error == nil || *r.Error == "" {
			return errors.New("a failed result requires a non-empty error")
		}
		class := r.FailureClass
		if class == "" {
			class = failureclass.FromText(*r.Error)
		}
		if !failureclass.Valid(class) {
			return fmt.Errorf("unsupported failureClass %q", r.FailureClass)
		}
	case StatusOK, StatusDegraded:
		if r.Error != nil {
			return errors.New("an ok or degraded result must have a null error")
		}
		if r.Down == nil || r.Up == nil || r.Ping == nil {
			return errors.New("an ok or degraded result requires down, up and ping")
		}
		if r.Server == nil {
			return errors.New("an ok or degraded result requires a server identity")
		}
		if r.FailureClass != "" && r.FailureClass != failureclass.Other {
			return errors.New("an ok or degraded result must use failureClass other")
		}
	default:
		return fmt.Errorf("result status must be ok, degraded or failed, got %q", r.Status)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func isUUID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(decoded) == 16
}

// MarshalJSON emits an unknown failed-test IP as null, never an invented address.
func (r Result) MarshalJSON() ([]byte, error) {
	type wire Result
	var ip *string
	if r.IP != "" {
		ip = &r.IP
	}
	return json.Marshal(struct {
		wire
		IP *string `json:"ip"`
	}{wire(r), ip})
}

func (r Result) validateDetails() error {
	if r.Error != nil && len([]rune(*r.Error)) > 2000 {
		return errors.New("result error exceeds 2000 characters")
	}
	if len(r.Raw) > 0 {
		var obj map[string]json.RawMessage
		if len(r.Raw) > 65536 || json.Unmarshal(r.Raw, &obj) != nil || obj == nil {
			return errors.New("raw must be a JSON object of at most 64 KiB")
		}
	}
	if len(r.Warnings) > 10 {
		return errors.New("too many result warnings")
	}
	for _, w := range r.Warnings {
		if len([]rune(w)) > 2000 {
			return errors.New("result warning too long")
		}
	}
	for _, l := range []*LoadedLatency{r.LoadedDown, r.LoadedUp} {
		if l != nil {
			for _, v := range []*float64{l.Low, l.IQM, l.High} {
				if v != nil && (!finite(*v) || *v < 0) {
					return errors.New("invalid loaded latency")
				}
			}
		}
	}
	if r.ServerID != nil && *r.ServerID <= 0 {
		return errors.New("serverId must be positive")
	}
	if r.Server != nil && (r.Server.ID < 0 || (strings.TrimSpace(r.Server.Name) == "" && strings.TrimSpace(r.Server.Host) == "") || len([]rune(r.Server.Host)) > 255) {
		return errors.New("invalid server identity")
	}
	if r.EngineIP != "" {
		if _, err := netip.ParseAddr(r.EngineIP); err != nil {
			return errors.New("engineIp must be an IP literal")
		}
	}
	if r.EngineResultURL != "" {
		u, err := url.Parse(r.EngineResultURL)
		if err != nil || u.Scheme == "" {
			return errors.New("engineResultUrl must be an absolute URI")
		}
	}
	return nil
}
