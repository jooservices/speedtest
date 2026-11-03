package report

import (
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
	// IP is the node's public IP used when the engine reports none (for
	// example a failed test before the external IP was learned). Empty keeps
	// the engine-reported IP.
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
		r.ServerID = &id
		r.Server = &Server{
			ID:      e.Server.ID,
			Name:    e.Server.Name,
			City:    e.Server.Location,
			Sponsor: "",
			Host:    e.Server.Host,
		}
	}

	if e.ExternalIP != "" {
		r.IP = e.ExternalIP
	} else {
		r.IP = o.IP
	}
	r.BytesDown, r.BytesUp, r.Duration = e.BytesDown, e.BytesUp, e.Duration
	r.EngineISP, r.EngineResultID, r.EngineResultURL, r.EngineIP =
		e.ISP, e.ResultID, e.ResultURL, e.ExternalIP

	if len(e.Raw) > 0 {
		r.Raw = append(json.RawMessage(nil), e.Raw...)
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
