package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

type speedtestCLIReport struct {
	Download      *float64   `json:"download"`
	Upload        *float64   `json:"upload"`
	Ping          *float64   `json:"ping"`
	Timestamp     *time.Time `json:"timestamp"`
	BytesSent     *uint64    `json:"bytes_sent"`
	BytesReceived *uint64    `json:"bytes_received"`
	Share         *string    `json:"share"`
	Server        struct {
		Name    string          `json:"name"`
		Country string          `json:"country"`
		Sponsor string          `json:"sponsor"`
		Host    string          `json:"host"`
		ID      json.RawMessage `json:"id"`
	} `json:"server"`
	Client struct {
		IP  string `json:"ip"`
		ISP string `json:"isp"`
	} `json:"client"`
}

// ParseSpeedtestCLI parses one `speedtest-cli --json` report. The client reports
// download and upload in bits per second; jitter, packet loss and latency under load
// are not reported and stay unavailable.
func ParseSpeedtestCLI(raw []byte) (Result, error) {
	r := Result{}
	if len(raw) > maxOutput || !json.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return failed(r, InvalidResult, nil)
	}
	r.Raw = append(json.RawMessage(nil), raw...)
	var v speedtestCLIReport
	if err := json.Unmarshal(raw, &v); err != nil {
		return failed(r, InvalidResult, err)
	}
	if v.Timestamp == nil || v.Timestamp.IsZero() || v.Download == nil || v.Upload == nil || v.Ping == nil || v.BytesSent == nil || v.BytesReceived == nil {
		return failed(r, InvalidResult, nil)
	}
	for _, value := range []*float64{v.Download, v.Upload, v.Ping} {
		if *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return failed(r, InvalidResult, nil)
		}
	}
	const maxInt64 = uint64(1<<63 - 1)
	if *v.BytesSent > maxInt64 || *v.BytesReceived > maxInt64 {
		return failed(r, InvalidResult, nil)
	}
	down, up := *v.Download/1e6, *v.Upload/1e6
	ping := *v.Ping
	r.Timestamp = v.Timestamp.UTC()
	r.Down, r.Up, r.Ping = &down, &up, &ping
	r.BytesDown, r.BytesUp = int64(*v.BytesReceived), int64(*v.BytesSent)
	server := &Server{Name: strings.TrimSpace(v.Server.Sponsor), Host: v.Server.Host}
	if server.Name == "" {
		server.Name = strings.TrimSpace(v.Server.Name)
	}
	location := strings.TrimSpace(v.Server.Name)
	if country := strings.TrimSpace(v.Server.Country); country != "" {
		if location != "" {
			location += ", "
		}
		location += country
	}
	server.Location = location
	if id, ok := serverID(v.Server.ID); ok {
		server.ID = id
	}
	if server.Name != "" || server.Host != "" {
		r.Server = server
	}
	r.ISP = v.Client.ISP
	if ip := net.ParseIP(v.Client.IP); ip != nil {
		r.ExternalIP = ip.String()
	}
	if v.Share != nil {
		r.ResultURL = *v.Share
	}
	return r, nil
}

// serverID accepts the identifier as a JSON string or number.
func serverID(raw json.RawMessage) (int64, bool) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}
