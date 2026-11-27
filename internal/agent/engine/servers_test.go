package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseOoklaServers(t *testing.T) {
	servers, err := ParseOoklaServers([]byte(`{"servers":[{"id":42,"name":"Fiber ISP","location":"Hanoi","host":"hanoi.example","sponsor":"Fiber","distance":12.5},{"id":"7","name":"Backup","distance":"0"}]}`))
	if err != nil || len(servers) != 2 {
		t.Fatalf("ParseOoklaServers() = %#v, %v", servers, err)
	}
	if servers[0].City != "Hanoi" || servers[0].ISP != "Fiber" || servers[0].DistanceKM == nil || *servers[0].DistanceKM != 12.5 {
		t.Fatalf("Ookla fields = %#v", servers[0])
	}
	if servers[1].DistanceKM == nil || *servers[1].DistanceKM != 0 {
		t.Fatalf("zero distance must remain present: %#v", servers[1])
	}
	for _, raw := range []string{"", "{", `{"servers":null}`, `{"servers":[{}]}`, `{"servers":[{"id":2147483648,"name":"too large"}]}`, `{"servers":[{"id":1,"name":"a"},{"id":1,"name":"b"}]}`} {
		if _, err := ParseOoklaServers([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := ParseOoklaServers([]byte(`{"servers":[{"id":1,"name":"a","distance":"NaN"}]}`)); err != nil {
		t.Fatal("invalid optional distance should be omitted", err)
	}
	if _, err := ParseOoklaServers([]byte(`{"servers":[{"id":1,"name":"a","city":"` + strings.Repeat("c", 121) + `"}]}`)); err == nil {
		t.Fatal("oversized city accepted")
	}
}

func TestParseLibreSpeedServers(t *testing.T) {
	servers, err := ParseLibreSpeedServers([]byte(`[{"id":2,"name":"Zurich, CH","server":"https://shared.example/backend-b","sponsorName":"ISP Z"},{"id":1,"name":"Singapore","server":"https://shared.example/backend-a?region=apac","sponsorName":"ISP S"}]`))
	if err != nil || len(servers) != 2 {
		t.Fatalf("ParseLibreSpeedServers() = %#v, %v", servers, err)
	}
	if servers[0].Name != "Singapore" || servers[0].Host != "shared.example" || servers[0].Endpoint != "https://shared.example/backend-a" || servers[0].ISP != "ISP S" || servers[0].DistanceKM != nil {
		t.Fatalf("sorted server = %#v", servers[0])
	}
	if servers[1].Host != "shared.example" || servers[1].Endpoint != "https://shared.example/backend-b" || servers[1].DistanceKM != nil {
		t.Fatalf("relative scheme server = %#v", servers[1])
	}
	if empty, err := ParseLibreSpeedServers([]byte("[]")); err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, %v", empty, err)
	}
	for _, raw := range []string{"", "{", "null", `[{"id":1,"name":"No host"}]`, `[{"id":1,"name":"bad","server":"https://u:p@host"}]`, `[{"id":1,"name":"a","server":"https://a"},{"id":1,"name":"b","server":"https://b"}]`} {
		if _, err := ParseLibreSpeedServers([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{`[{"id":1,"name":" ","server":"https://a"}]`, `[{"id":1,"name":"bad","server":"https://bad host"}]`, `[{"id":1,"name":"` + strings.Repeat("n", 201) + `","server":"https://a"}]`} {
		if _, err := ParseLibreSpeedServers([]byte(raw)); err == nil {
			t.Errorf("accepted invalid server %q", raw)
		}
	}
}

// TestParseServerID pins the rules of the single gate for server IDs used by
// parseOoklaServer and parseLibreServer: the cutset trim of '"' and ' ', base 10
// only, and the range 1..math.MaxInt32. Rows marked "current behaviour" document
// what the code does today; they are not a promise.
func TestParseServerID(t *testing.T) {
	const invalid = "invalid server ID"
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
		nilRaw  bool
	}{
		// AC1: accepted values.
		{name: "AC1 bare number 1", raw: `1`, want: 1},
		{name: "AC1 bare number 42", raw: `42`, want: 42},
		{name: "AC1 quoted number", raw: `"42"`, want: 42},
		{name: "AC1 space-padded quoted number", raw: `" 42 "`, want: 42},
		{name: "AC1 upper bound", raw: `2147483647`, want: 2147483647},
		{name: "AC1 quoted upper bound", raw: `"2147483647"`, want: 2147483647},
		// Current behaviour, not a promise: ParseInt and the cutset accept these.
		{name: "current behaviour leading zeros", raw: `007`, want: 7},
		{name: "current behaviour leading plus", raw: `+7`, want: 7},
		{name: "current behaviour quoted leading plus", raw: `"+7"`, want: 7},
		{name: "current behaviour doubled quotes", raw: `""42""`, want: 42},
		{name: "current behaviour unquoted edge spaces", raw: ` 42 `, want: 42},
		{name: "current behaviour unbalanced quote", raw: `"42`, want: 42},
		{name: "current behaviour space then quote", raw: ` "42" `, want: 42},
		// AC2: out of range, overflow, empty and null.
		{name: "AC2 zero", raw: `0`, wantErr: true},
		{name: "AC2 negative", raw: `-1`, wantErr: true},
		{name: "AC2 one above upper bound", raw: `2147483648`, wantErr: true},
		{name: "AC2 quoted one above upper bound", raw: `"2147483648"`, wantErr: true},
		{name: "AC2 int64 overflow", raw: `99999999999999999999`, wantErr: true},
		{name: "AC2 empty input", raw: ``, wantErr: true},
		{name: "AC2 missing id field (nil RawMessage)", nilRaw: true, wantErr: true},
		{name: "AC2 null", raw: `null`, wantErr: true},
		{name: "AC2 empty quoted string", raw: `""`, wantErr: true},
		{name: "AC2 only spaces", raw: `   `, wantErr: true},
		// AC3: non-integer text.
		{name: "AC3 decimal", raw: `1.5`, wantErr: true},
		{name: "AC3 decimal with zero fraction", raw: `1.0`, wantErr: true},
		{name: "AC3 decimal 7.5", raw: `7.5`, wantErr: true},
		{name: "AC3 exponent", raw: `1e3`, wantErr: true},
		{name: "AC3 hex", raw: `0x10`, wantErr: true},
		{name: "AC3 letters", raw: `abc`, wantErr: true},
		{name: "AC3 quoted letters", raw: `"abc"`, wantErr: true},
		{name: "AC3 boolean", raw: `true`, wantErr: true},
		{name: "AC3 inner space", raw: `"4 2"`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.raw)
			if tc.nilRaw {
				raw = nil
			}
			got, err := parseServerID(raw)
			if tc.wantErr {
				if err == nil || err.Error() != invalid || got != 0 {
					t.Fatalf("parseServerID(%s) = %d, %v; want 0, %q", tc.raw, got, err, invalid)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseServerID(%s) = %d, %v; want %d, nil", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestParseLibreSpeedListRows(t *testing.T) {
	jsonServers, err := ParseLibreSpeedList([]byte(`[{"id":9,"name":"JSON list","server":"https://json.example"}]`))
	if err != nil || len(jsonServers) != 1 || jsonServers[0].Name != "JSON list" {
		t.Fatalf("JSON list = %#v, %v", jsonServers, err)
	}
	got, err := ParseLibreSpeedList([]byte("ID  Name\n1  Singapore, SG\n2  Hanoi, VN\n"))
	if err != nil || len(got) != 2 || got[0].Name != "Hanoi, VN" {
		t.Fatalf("rows = %#v, %v", got, err)
	}
	got, err = ParseLibreSpeedList([]byte("| ID | Name |\n| 3 | Tokyo, JP |\n"))
	if err != nil || len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("table rows = %#v, %v", got, err)
	}
	fixture, err := os.ReadFile("../testdata/fake-speedtest/librespeed-servers-v1.0.14.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err = ParseLibreSpeedList(fixture)
	if err != nil || len(got) != 2 || got[0].ID != 51 || got[0].Name != "Hanoi, Vietnam (Hanoi Speedtest)" || got[0].Host != "hn.example.test" || got[0].Endpoint != "https://hn.example.test/backend" || got[0].ISP != "Hanoi ISP" || got[0].DistanceKM != nil || got[1].ID != 52 || got[1].Host != "hn.example.test" || got[1].Endpoint != "https://hn.example.test/down" || got[1].ISP != "Singapore ISP" {
		t.Fatalf("LibreSpeed CLI rows = %#v, %v", got, err)
	}
	if _, err := ParseLibreSpeedList([]byte("no servers")); err == nil {
		t.Fatal("accepted output without server rows")
	}
	if _, err := ParseLibreSpeedList([]byte("2147483648  Too large\n")); err == nil {
		t.Fatal("oversized server ID accepted")
	}
	if _, err := ParseLibreSpeedList([]byte("4  Duplicate\n4  Duplicate\n")); err == nil {
		t.Fatal("duplicate server ID accepted")
	}
}

func TestParseLibreSpeedCLIEntryRejectsMalformedRows(t *testing.T) {
	for _, line := range []string{
		"bad-id: Server (https://server.example)",
		"1: Server",
		"1: Server (unterminated URL",
		"1: Server (https://[invalid)",
		"1: Server (https://user:password@server.example)",
		"1: (https://server.example)",
		"1: Server (https://server.example) [Sponsor: " + strings.Repeat("x", 201) + "]",
	} {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			t.Fatal("empty malformed fixture")
		}
		if server, ok := parseLibreSpeedCLIEntry(line, fields[0]); ok {
			t.Errorf("accepted malformed row %#v from %q", server, line)
		}
	}
}

func TestEngineListCommands(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "servers.json")
	argsPath := filepath.Join(dir, "args.txt")
	if err := os.WriteFile(output, []byte(`{"servers":[{"id":12,"name":"Ookla","host":"ookla.example"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVER_LIST_OUTPUT", output)
	t.Setenv("SERVER_LIST_ARGS", argsPath)
	bin := script(t, `printf '%s\n' "$@" > "$SERVER_LIST_ARGS"
cat "$SERVER_LIST_OUTPUT"
`)
	ookla, err := NewOokla(Config{Binary: bin, Args: []string{"--accept-license", "-f", "csv"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ookla.ListServers(context.Background(), Options{InterfaceName: "eth0"})
	if err != nil || len(got) != 1 || got[0].ID != 12 {
		t.Fatalf("Ookla list = %#v, %v", got, err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil || string(args) != "--servers\n--accept-license\n-f\njson\n--interface\neth0\n" {
		t.Fatalf("Ookla arguments = %q, %v", args, err)
	}
	if err := os.WriteFile(output, []byte("1  Singapore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	libre, err := NewLibreSpeed(LibreSpeedConfig{Binary: bin, ServerJSON: "https://servers.example/list.json", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err = libre.ListServers(context.Background(), Options{SourceIP: "192.0.2.10"})
	if err != nil || len(got) != 1 || got[0].Name != "Singapore" || got[0].DistanceKM != nil {
		t.Fatalf("LibreSpeed list = %#v, %v", got, err)
	}
	args, err = os.ReadFile(argsPath)
	if err != nil || string(args) != "--list\n--server-json\nhttps://servers.example/list.json\n--source\n192.0.2.10\n" {
		t.Fatalf("LibreSpeed arguments = %q, %v", args, err)
	}
	defaultLibre, err := NewLibreSpeed(LibreSpeedConfig{Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	got, err = defaultLibre.ListServers(context.Background(), Options{})
	if err != nil || len(got) != 1 {
		t.Fatalf("LibreSpeed default list = %#v, %v", got, err)
	}
	args, err = os.ReadFile(argsPath)
	if err != nil || string(args) != "--list\n" {
		t.Fatalf("LibreSpeed default arguments = %q, %v", args, err)
	}
	if _, err := defaultLibre.ListServers(context.Background(), Options{InterfaceName: "eth0"}); err == nil {
		t.Fatal("LibreSpeed server discovery accepted unsupported interface binding")
	}
	missing, err := NewLibreSpeed(LibreSpeedConfig{Binary: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.ListServers(context.Background(), Options{}); err == nil {
		t.Fatal("LibreSpeed server discovery accepted a missing executable")
	}
}

func TestServerListCommandFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  string
		want ErrorClass
	}{
		{name: "missing", bin: filepath.Join(t.TempDir(), "missing"), want: BinaryMissing},
		{name: "unreachable", bin: script(t, "echo 'specified server unavailable' >&2\nexit 1\n"), want: ServerUnavailable},
		{name: "invalid options", bin: "unused", want: InvalidOptions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewOokla(Config{Binary: tc.bin, Timeout: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{}
			if tc.want == InvalidOptions {
				opts.ServerID = -1
			}
			_, err = e.ListServers(context.Background(), opts)
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Class != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "unavailable") && strings.Contains(err.Error(), "specified") {
				t.Fatal("engine diagnostic leaked")
			}
		})
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		bin  string
		want ErrorClass
	}{
		{name: "timeout", ctx: context.Background(), bin: script(t, "sleep 1\n"), want: Timeout},
		{name: "canceled", ctx: canceledServerListContext(), bin: script(t, "sleep 1\n"), want: Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runServerListCommand(tc.ctx, tc.bin, nil, 20*time.Millisecond)
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Class != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
	if _, err := runServerListCommand(context.Background(), script(t, "head -c 2097153 /dev/zero\n"), nil, time.Second); err == nil || err.Error() != serverListFailure().Error() {
		t.Fatalf("oversized list output error = %v", err)
	}
	if !reflect.DeepEqual(ooklaServerArgs(nil, Options{}), []string{"--servers", "-f", "json"}) {
		t.Fatal("default Ookla server list flags changed")
	}
}

func TestServerListCaptureFailsClosed(t *testing.T) {
	var capture capture
	capture.limit = 1
	_, _ = capture.Write([]byte("too long"))
	if !capture.overflow {
		t.Fatal("server output capture did not enforce its configured bound")
	}
	if serverListFailure().Error() != "speedtest: server list unavailable" {
		t.Fatal("server list failure message changed")
	}
}

func canceledServerListContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
