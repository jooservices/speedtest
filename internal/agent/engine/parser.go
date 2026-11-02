package engine

import (
	"encoding/json"
	"math"
	"time"
)

type transfer struct {
	Bandwidth *float64       `json:"bandwidth"`
	Bytes     int64          `json:"bytes"`
	Elapsed   float64        `json:"elapsed"`
	Latency   *LoadedLatency `json:"latency"`
}

type ooklaJSON struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Ping      struct {
		Latency *float64 `json:"latency"`
		Jitter  *float64 `json:"jitter"`
	} `json:"ping"`
	Download, Upload transfer
	PacketLoss       *float64 `json:"packetLoss"`
	ISP              string   `json:"isp"`
	Interface        struct {
		ExternalIP string `json:"externalIp"`
	} `json:"interface"`
	Server *Server `json:"server"`
	Result struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"result"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// ParseOokla parses one final JSON object, retaining unknown fields verbatim.
// Duration is the sum of reported download/upload elapsed milliseconds, in seconds;
// it excludes server selection and other wall-clock setup time.
func ParseOokla(raw []byte) (Result, error) {
	r := Result{}
	if len(raw) > maxOutput || !json.Valid(raw) {
		return failed(r, InvalidResult, nil)
	}
	r.Raw = append(json.RawMessage(nil), raw...)
	var v ooklaJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return failed(r, InvalidResult, err)
	}
	if v.Type != "result" {
		if v.Message != "" || v.Error != "" {
			return failed(r, classify(v.Message+" "+v.Error), nil)
		}
		return failed(r, InvalidResult, nil)
	}
	if v.Timestamp.IsZero() || v.Server == nil || v.Server.ID <= 0 || v.Download.Bandwidth == nil || v.Upload.Bandwidth == nil || v.Ping.Latency == nil {
		return failed(r, InvalidResult, nil)
	}
	values := []*float64{v.Download.Bandwidth, v.Upload.Bandwidth, v.Ping.Latency, v.Ping.Jitter, v.PacketLoss}
	for _, loaded := range []*LoadedLatency{v.Download.Latency, v.Upload.Latency} {
		if loaded != nil {
			values = append(values, loaded.Low, loaded.IQM, loaded.High)
		}
	}
	for _, x := range values {
		if x != nil && (*x < 0 || math.IsNaN(*x) || math.IsInf(*x, 0)) {
			return failed(r, InvalidResult, nil)
		}
	}
	if (v.PacketLoss != nil && *v.PacketLoss > 100) || v.Download.Bytes < 0 || v.Upload.Bytes < 0 || v.Download.Elapsed < 0 || v.Upload.Elapsed < 0 {
		return failed(r, InvalidResult, nil)
	}
	down, up := *v.Download.Bandwidth*8/1e6, *v.Upload.Bandwidth*8/1e6
	duration := (v.Download.Elapsed + v.Upload.Elapsed) / 1000
	if math.IsInf(down, 0) || math.IsInf(up, 0) || math.IsInf(duration, 0) {
		return failed(r, InvalidResult, nil)
	}
	r.Timestamp = v.Timestamp.UTC()
	r.Down, r.Up, r.Ping, r.Jitter, r.Loss = &down, &up, v.Ping.Latency, v.Ping.Jitter, v.PacketLoss
	r.LoadedDown, r.LoadedUp, r.Server = v.Download.Latency, v.Upload.Latency, v.Server
	r.ISP, r.ExternalIP, r.ResultID, r.ResultURL = v.ISP, v.Interface.ExternalIP, v.Result.ID, v.Result.URL
	r.BytesDown, r.BytesUp, r.Duration = v.Download.Bytes, v.Upload.Bytes, duration
	return r, nil
}
