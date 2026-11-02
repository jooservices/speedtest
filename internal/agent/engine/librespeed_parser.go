package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"time"
)

type libreSpeedReport struct {
	Timestamp *time.Time `json:"timestamp"`
	Server    struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"server"`
	Client struct {
		IP           string `json:"ip"`
		ISP          string `json:"isp"`
		Organization string `json:"organization"`
		Org          string `json:"org"`
	} `json:"client"`
	BytesSent     *uint64  `json:"bytes_sent"`
	BytesReceived *uint64  `json:"bytes_received"`
	Ping          *float64 `json:"ping"`
	Jitter        *float64 `json:"jitter"`
	Upload        *float64 `json:"upload"`
	Download      *float64 `json:"download"`
	Share         string   `json:"share"`
}

// ParseLibreSpeed parses one librespeed-cli --json report. The CLI has emitted
// a single object in older releases and a one-element array in newer releases.
// Fields absent from its report remain unavailable in Result.
func ParseLibreSpeed(raw []byte) (Result, error) {
	r := Result{}
	if len(raw) > maxOutput || !json.Valid(raw) {
		return failed(r, InvalidResult, nil)
	}
	r.Raw = append(json.RawMessage(nil), raw...)

	var reports []libreSpeedReport
	if bytes.TrimSpace(raw)[0] == '[' {
		if err := json.Unmarshal(raw, &reports); err != nil || len(reports) != 1 {
			return failed(r, InvalidResult, err)
		}
	} else {
		var report libreSpeedReport
		if err := json.Unmarshal(raw, &report); err != nil {
			return failed(r, InvalidResult, err)
		}
		reports = []libreSpeedReport{report}
	}
	v := reports[0]
	if v.Timestamp == nil || v.Timestamp.IsZero() || v.Ping == nil || v.Jitter == nil || v.Download == nil || v.Upload == nil || v.BytesSent == nil || v.BytesReceived == nil {
		return failed(r, InvalidResult, nil)
	}
	for _, value := range []*float64{v.Ping, v.Jitter, v.Download, v.Upload} {
		if *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return failed(r, InvalidResult, nil)
		}
	}
	serverURL, err := url.ParseRequestURI(v.Server.URL)
	if err != nil || (serverURL.Scheme != "http" && serverURL.Scheme != "https") || serverURL.Host == "" {
		return failed(r, InvalidResult, nil)
	}
	const maxInt64 = uint64(1<<63 - 1)
	if *v.BytesSent > maxInt64 || *v.BytesReceived > maxInt64 {
		return failed(r, InvalidResult, nil)
	}

	r.Timestamp = v.Timestamp.UTC()
	r.Down, r.Up, r.Ping, r.Jitter = v.Download, v.Upload, v.Ping, v.Jitter
	r.BytesDown, r.BytesUp = int64(*v.BytesReceived), int64(*v.BytesSent)
	r.Server = &Server{Name: v.Server.Name, Host: serverURL.Host}
	r.ISP = v.Client.ISP
	if r.ISP == "" {
		r.ISP = v.Client.Organization
	}
	if r.ISP == "" {
		r.ISP = v.Client.Org
	}
	r.ExternalIP = v.Client.IP
	r.ResultURL = v.Share
	return r, nil
}
