package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxServerOptions    = 1000
	maxServerListOutput = 2 << 20
)

type ooklaServers struct {
	Servers []json.RawMessage `json:"servers"`
}

type ooklaServer struct {
	ID       json.RawMessage `json:"id"`
	Name     string          `json:"name"`
	Location string          `json:"location"`
	City     string          `json:"city"`
	Host     string          `json:"host"`
	Sponsor  string          `json:"sponsor"`
	ISP      string          `json:"isp"`
	Distance json.RawMessage `json:"distance"`
}

// ParseOoklaServers validates the JSON emitted by `speedtest --servers -f json`.
func ParseOoklaServers(data []byte) ([]ServerOption, error) {
	var list ooklaServers
	if len(data) == 0 || len(data) > maxServerListOutput || json.Unmarshal(data, &list) != nil || list.Servers == nil || len(list.Servers) > maxServerOptions {
		return nil, errors.New("invalid Ookla server list")
	}
	servers := make([]ServerOption, 0, len(list.Servers))
	seen := make(map[int64]bool, len(list.Servers))
	for _, raw := range list.Servers {
		server, err := parseOoklaServer(raw)
		if err != nil || seen[server.ID] {
			return nil, errors.New("invalid Ookla server list")
		}
		seen[server.ID] = true
		servers = append(servers, server)
	}
	return servers, nil
}

func parseOoklaServer(raw []byte) (ServerOption, error) {
	var in ooklaServer
	if json.Unmarshal(raw, &in) != nil {
		return ServerOption{}, errors.New("invalid server")
	}
	id, err := parseServerID(in.ID)
	if err != nil {
		return ServerOption{}, err
	}
	name := strings.TrimSpace(in.Name)
	host := strings.TrimSpace(in.Host)
	if id < 1 || name == "" && host == "" {
		return ServerOption{}, errors.New("invalid server")
	}
	city := firstText(in.City, in.Location)
	isp := firstText(in.ISP, in.Sponsor)
	option := ServerOption{ID: id, Name: firstText(name, host), City: city, ISP: isp, Host: host, DistanceKM: parseDistance(in.Distance)}
	if !validServerOptionText(option) {
		return ServerOption{}, errors.New("invalid server")
	}
	return option, nil
}

func parseServerID(raw json.RawMessage) (int64, error) {
	value := strings.Trim(string(raw), `" `)
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 || id > math.MaxInt32 {
		return 0, errors.New("invalid server ID")
	}
	return id, nil
}

func parseDistance(raw json.RawMessage) *float64 {
	value, err := strconv.ParseFloat(strings.Trim(string(raw), `" `), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil
	}
	return &value
}

func firstText(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type libreServer struct {
	ID       json.RawMessage `json:"id"`
	Name     string          `json:"name"`
	Server   string          `json:"server"`
	City     string          `json:"city"`
	Location string          `json:"location"`
	Sponsor  string          `json:"sponsorName"`
	ISP      string          `json:"isp"`
}

// ParseLibreSpeedServers accepts the server JSON format documented by the
// LibreSpeed CLI; that format has no distance or coordinates.
func ParseLibreSpeedServers(data []byte) ([]ServerOption, error) {
	var list []json.RawMessage
	if len(data) == 0 || len(data) > maxServerListOutput || json.Unmarshal(data, &list) != nil || list == nil || len(list) > maxServerOptions {
		return nil, errors.New("invalid LibreSpeed server list")
	}
	servers := make([]ServerOption, 0, len(list))
	seen := make(map[int64]bool, len(list))
	for _, raw := range list {
		server, err := parseLibreServer(raw)
		if err != nil || seen[server.ID] {
			return nil, errors.New("invalid LibreSpeed server list")
		}
		seen[server.ID] = true
		servers = append(servers, server)
	}
	sort.Slice(servers, func(i, j int) bool { return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name) })
	return servers, nil
}

func parseLibreServer(raw []byte) (ServerOption, error) {
	var in libreServer
	if json.Unmarshal(raw, &in) != nil {
		return ServerOption{}, errors.New("invalid server")
	}
	id, err := parseServerID(in.ID)
	if err != nil {
		return ServerOption{}, err
	}
	u, err := parseServerEndpoint(in.Server)
	if err != nil {
		return ServerOption{}, errors.New("invalid server host")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ServerOption{}, errors.New("invalid server name")
	}
	option := ServerOption{ID: id, Name: name, City: firstText(in.City, in.Location), ISP: firstText(in.ISP, in.Sponsor), Host: u.Host, Endpoint: serverEndpointIdentity(u)}
	if !validServerOptionText(option) {
		return ServerOption{}, errors.New("invalid server")
	}
	return option, nil
}

func validServerOptionText(server ServerOption) bool {
	return server.ID <= math.MaxInt32 && validServerText(server.Name, 200) && validServerText(server.City, 120) &&
		validServerText(server.ISP, 200) && validServerText(server.Host, 255)
}

func validServerText(value string, max int) bool { return utf8.RuneCountInString(value) <= max }

// ParseLibreSpeedList accepts JSON output when available and the stable ID/name
// rows printed by the CLI's `--list` mode.
func ParseLibreSpeedList(data []byte) ([]ServerOption, error) {
	if servers, err := ParseLibreSpeedServers(data); err == nil {
		return servers, nil
	}
	return parseLibreSpeedRows(data)
}

func parseLibreSpeedRows(data []byte) ([]ServerOption, error) {
	if len(data) == 0 || len(data) > maxServerListOutput {
		return nil, errors.New("invalid LibreSpeed server list")
	}
	servers := []ServerOption{}
	seen := make(map[int64]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), maxServerListOutput)
	for scanner.Scan() {
		if row, ok := parseLibreSpeedRow(scanner.Text()); ok {
			if seen[row.ID] {
				return nil, errors.New("invalid LibreSpeed server list")
			}
			seen[row.ID] = true
			servers = append(servers, row)
			if len(servers) > maxServerOptions {
				return nil, errors.New("invalid LibreSpeed server list")
			}
		}
	}
	if scanner.Err() != nil || len(servers) == 0 {
		return nil, errors.New("invalid LibreSpeed server list")
	}
	sort.Slice(servers, func(i, j int) bool { return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name) })
	return servers, nil
}

func parseLibreSpeedRow(line string) (ServerOption, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "|") {
		parts := strings.Split(strings.Trim(line, "|"), "|")
		if len(parts) >= 2 {
			return serverRow(parts[0], parts[1])
		}
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ServerOption{}, false
	}
	if strings.HasSuffix(fields[0], ":") {
		return parseLibreSpeedCLIEntry(line, fields[0])
	}
	return serverRow(fields[0], strings.TrimSpace(strings.TrimPrefix(line, fields[0])))
}

func parseLibreSpeedCLIEntry(line, rawID string) (ServerOption, bool) {
	id, err := strconv.ParseInt(strings.TrimSuffix(rawID, ":"), 10, 64)
	if err != nil || id < 1 || id > math.MaxInt32 {
		return ServerOption{}, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, rawID))
	sponsor := ""
	if start := strings.LastIndex(rest, " [Sponsor: "); start >= 0 && strings.HasSuffix(rest, "]") {
		sponsor = strings.TrimSpace(rest[start+len(" [Sponsor: ") : len(rest)-1])
		rest = strings.TrimSpace(rest[:start])
	}
	for start := strings.LastIndex(rest, " ("); start >= 0; start = strings.LastIndex(rest[:start], " (") {
		serverText := rest[start+2:]
		if !strings.HasSuffix(serverText, ")") {
			continue
		}
		serverURL, err := parseServerEndpoint(serverText[:len(serverText)-1])
		if err != nil {
			continue
		}
		option := ServerOption{ID: id, Name: strings.TrimSpace(rest[:start]), ISP: sponsor, Host: serverURL.Host, Endpoint: serverEndpointIdentity(serverURL)}
		if option.Name == "" || !validServerOptionText(option) {
			return ServerOption{}, false
		}
		return option, true
	}
	return ServerOption{}, false
}

func parseServerEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("invalid server endpoint")
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u, nil
}

func serverEndpointIdentity(u *url.URL) string {
	if u == nil || u.User != nil {
		return ""
	}
	identity := *u
	identity.Scheme = strings.ToLower(identity.Scheme)
	identity.Host = strings.ToLower(identity.Host)
	identity.RawQuery, identity.ForceQuery = "", false
	identity.Fragment, identity.RawFragment = "", ""
	if identity.Path == "" {
		identity.Path = "/"
	}
	return identity.String()
}

func serverRow(rawID, name string) (ServerOption, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	name = strings.TrimSpace(name)
	if err != nil || id < 1 || id > math.MaxInt32 || name == "" || strings.Trim(name, "-+| ") == "" || !validServerText(name, 200) {
		return ServerOption{}, false
	}
	return ServerOption{ID: id, Name: name}, true
}

func validServerListOptions(opts Options) bool {
	return opts.ServerID == 0 && opts.Timeout >= 0 && validNetworkOptions(opts)
}

func serverListFailure() error { return fmt.Errorf("speedtest: server list unavailable") }

func runServerListCommand(ctx context.Context, binary string, args []string, timeout time.Duration) ([]byte, error) {
	if binary == "" {
		return nil, &RunError{Class: BinaryMissing}
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	var stdout, stderr capture
	stdout.limit, stderr.limit = maxServerListOutput, 4<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		class := Canceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			class = Timeout
		}
		return nil, &RunError{Class: class, cause: ctx.Err()}
	}
	if err != nil {
		class := classify(string(stdout.data) + "\n" + string(stderr.data))
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			class = BinaryMissing
		}
		return nil, &RunError{Class: class, cause: err}
	}
	if stdout.overflow {
		return nil, serverListFailure()
	}
	return stdout.data, nil
}
