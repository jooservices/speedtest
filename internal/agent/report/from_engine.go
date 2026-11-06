package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

// Options controls how an engine result becomes a reportable Result.
type Options struct {
	// Manual marks the result as requested by an operator rather than the
	// schedule.
	Manual bool
	// Threads is the connection count; zero selects DefaultThreads.
	Threads int
	// IP is the center-observed address and overrides the engine value when
	// available. The engine address is preserved separately for cross-checking.
	IP string
	// ID is a pre-allocated result UUID; empty allocates a fresh one.
	ID string
}

// FromEngine converts an engine measurement into a reportable Result and
// allocates its UUID. The UUID is allocated here, before spooling, so the center
// can deduplicate identical retries under the ingestion contract. A failed measurement becomes a failed
// result with null measurements; a successful one keeps every reported value.
func FromEngine(e engine.Result, o Options) (Result, error) {
	r := Result{
		Manual:  o.Manual,
		Threads: o.Threads,
	}
	if r.Threads == 0 {
		r.Threads = DefaultThreads
	}

	if e.Error != "" {
		r.Status = StatusFailed
		err := e.Error
		r.Error = &err
	} else {
		r.Status = StatusOK
	}

	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	r.TS = ts.UTC().Format(time.RFC3339)

	r.copyMeasurement(e, o)

	if len(e.Raw) > 0 {
		r.Raw = append(json.RawMessage(nil), e.Raw...)
		if bytes.HasPrefix(bytes.TrimSpace(r.Raw), []byte("[")) {
			wrapped, err := json.Marshal(map[string]json.RawMessage{"reports": r.Raw})
			if err != nil {
				return Result{}, err
			}
			r.Raw = wrapped
		}
	}

	if o.ID != "" {
		r.ID = o.ID
	} else {
		id, err := newID()
		if err != nil {
			return Result{}, err
		}
		r.ID = id
	}

	if err := r.Validate(); err != nil {
		return Result{}, fmt.Errorf("invalid result: %w", err)
	}
	return r, nil
}

// loaded copies an engine loaded-latency triple into the wire shape.
func loaded(l *engine.LoadedLatency) *LoadedLatency {
	if l == nil {
		return nil
	}
	return &LoadedLatency{Low: l.Low, IQM: l.IQM, High: l.High}
}

func (r *Result) copyMeasurement(e engine.Result, o Options) {
	r.Down, r.Up, r.Ping, r.Jitter, r.Loss = e.Down, e.Up, e.Ping, e.Jitter, e.Loss
	if e.LoadedDown != nil {
		r.LoadedDown = loaded(e.LoadedDown)
		r.DLat = e.LoadedDown.IQM
	}
	if e.LoadedUp != nil {
		r.LoadedUp = loaded(e.LoadedUp)
		r.ULat = e.LoadedUp.IQM
	}
	if e.Server != nil {
		id := e.Server.ID
		if id > 0 {
			r.ServerID = &id
		}
		r.Server = &Server{
			ID:      e.Server.ID,
			Name:    e.Server.Name,
			City:    e.Server.Location,
			Sponsor: "",
			Host:    e.Server.Host,
		}
	}

	r.IP = e.ExternalIP
	if o.IP != "" {
		r.IP = o.IP
	}
	r.BytesDown, r.BytesUp, r.Duration = e.BytesDown, e.BytesUp, e.Duration
	r.EngineISP, r.EngineResultID, r.EngineResultURL, r.EngineIP =
		e.ISP, e.ResultID, e.ResultURL, e.ExternalIP
}
