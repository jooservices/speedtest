package agentapi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const validResult = `{"id":"00000000-0000-4000-8000-000000000001","ts":"2026-11-03T00:00:00Z","status":"ok","down":123.4,"up":56.7,"ping":3,"jitter":null,"dLat":null,"uLat":null,"loss":null,"serverId":1,"ip":"203.0.113.1","threads":4,"bytesDown":1024,"bytesUp":512,"duration":2.3,"error":null,"manual":false,"server":{"id":1,"name":"test","city":"","sponsor":"","endpoint":"https://speed.example.test/backend"}}`

func change(data, key string, value any) []byte {
	var object map[string]any
	_ = json.Unmarshal([]byte(data), &object)
	object[key] = value
	output, _ := json.Marshal(object)
	return output
}
func TestResultValidation(t *testing.T) {
	for _, status := range []string{"ok", "degraded", "failed"} {
		data := change(validResult, "status", status)
		if status == "failed" {
			data = change(string(data), "error", "engine failed")
			for _, key := range []string{"down", "up", "ping", "serverId"} {
				data = change(string(data), key, nil)
			}
		}
		result, err := ParseResult(data)
		if err != nil || result.Jitter != nil || string(result.Raw) != "{}" || result.Warnings == nil {
			t.Fatalf("%s: %+v %v", status, result, err)
		}
	}
	for _, item := range []struct {
		status string
		class  string
		valid  bool
	}{
		{status: "ok", class: "other", valid: true},
		{status: "degraded", class: "other", valid: true},
		{status: "failed", class: "rate_limited", valid: true},
		{status: "ok", class: "rate_limited"},
		{status: "degraded", class: "timeout"},
	} {
		data := change(validResult, "status", item.status)
		data = change(string(data), "failureClass", item.class)
		if item.status == "failed" {
			data = change(string(data), "error", "Too many requests")
			for _, key := range []string{"down", "up", "ping", "serverId"} {
				data = change(string(data), key, nil)
			}
		}
		_, err := ParseResult(data)
		if (err == nil) != item.valid {
			t.Fatalf("status=%s class=%s: valid=%t err=%v", item.status, item.class, item.valid, err)
		}
	}
	full := validResult[:len(validResult)-1] + `,"raw":{"nested":[1,true]},"loadedDown":{"low":null,"iqm":4,"high":10},"loadedUp":{"low":1,"iqm":2,"high":3},"server":{"id":1,"name":"a","city":"b","sponsor":"c","host":"test.example"},"engineISP":"isp","engineResultId":"engine-1","engineResultUrl":"https://example.test/result/1","engineIp":"2001:db8::1","warnings":["IP mismatch"]}`
	if _, err := ParseResult([]byte(full)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{"id": "invalid", "ts": "2026-11-03T00:00:00+00:00", "status": "unknown", "ip": "example.test", "threads": 65, "bytesDown": -1, "bytesUp": -1, "duration": -1, "error": "not null", "raw": []int{}, "loadedDown": map[string]any{"low": -1, "iqm": 0, "high": 0}, "loadedUp": map[string]any{"low": nil}, "server": map[string]any{"id": 0, "name": "", "city": "", "sponsor": ""}, "engineIp": "1.2.3.4/24", "engineResultUrl": "relative/path", "warnings": make([]string, 11), "other": 1}
	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			if _, err := ParseResult(change(validResult, key, value)); !errors.Is(err, ErrInvalid) {
				t.Fatal("accepted invalid item", key)
			}
		})
	}
	for _, key := range []string{"down", "up", "ping", "jitter", "dLat", "uLat", "loss", "serverId"} {
		if _, err := ParseResult(change(validResult, key, -1)); err == nil {
			t.Fatal("accepted negative", key)
		}
	}
	for _, key := range []string{"id", "ts", "status", "down", "up", "ping", "jitter", "dLat", "uLat", "loss", "serverId", "ip", "threads", "bytesDown", "bytesUp", "duration", "error", "manual"} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(validResult), &fields)
		delete(fields, key)
		data, _ := json.Marshal(fields)
		if _, err := ParseResult(data); err == nil {
			t.Fatal("accepted missing", key)
		}
	}
	invalids := [][]byte{[]byte(`null`), []byte(`[]`), []byte(`{`), []byte(validResult + `{}`), change(validResult, "loss", 101), change(validResult, "threads", 0), change(validResult, "raw", nil), change(validResult, "raw", map[string]string{"x": strings.Repeat("x", 64<<10)}), change(validResult, "warnings", []string{strings.Repeat("x", 2001)}), change(validResult, "error", strings.Repeat("x", 2001)), change(validResult, "down", nil), change(validResult, "status", "failed"), change(full, "server", map[string]any{"id": 1, "name": "", "city": "", "sponsor": "", "host": strings.Repeat("x", 256)}), change(full, "loadedUp", map[string]any{"low": 0, "iqm": -1, "high": 0}), change(validResult, "engineResultUrl", "http://%")}
	for _, data := range invalids {
		if _, err := ParseResult(data); err == nil {
			t.Fatal("accepted invalid", string(data[:min(80, len(data))]))
		}
	}
	failed := change(string(change(validResult, "status", "failed")), "error", " ")
	if _, err := ParseResult(failed); err == nil {
		t.Fatal("accepted blank failure")
	}
	if UUID("00000000000040008000000000000001") || UUID("00000000-0000-4000-8000-00000000000g") {
		t.Fatal("invalid UUID")
	}
	if !UUID("ABCDEF00-0000-4000-8000-000000000001") {
		t.Fatal("valid uppercase UUID rejected")
	}
	var value any
	if Decode([]byte(`{} {}`), &value) == nil {
		t.Fatal("trailing JSON")
	}
}

func TestResultSelectionSnapshotDecode(t *testing.T) {
	legacy, err := ParseResult([]byte(validResult))
	if err != nil || legacy.ServerSelectionKnown || legacy.PinnedServerID != nil {
		t.Fatalf("legacy result snapshot=%+v err=%v", legacy, err)
	}
	known := change(validResult, "serverSelectionKnown", true)
	known = change(string(known), "pinnedServerId", 54321)
	result, err := ParseResult(known)
	if err != nil || !result.ServerSelectionKnown || result.PinnedServerID == nil || *result.PinnedServerID != 54321 {
		t.Fatalf("known result snapshot=%+v err=%v", result, err)
	}
	unknown := change(validResult, "pinnedServerId", 54321)
	if _, err := ParseResult(unknown); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted pin without a known measurement selection: %v", err)
	}
}

func TestParseResultPreservesFailureClassPresence(t *testing.T) {
	data := change(validResult, "status", "failed")
	data = change(string(data), "error", "speedtest: timeout")
	for _, key := range []string{"down", "up", "ping", "jitter", "dLat", "uLat", "loss", "serverId", "ip", "server"} {
		data = change(string(data), key, nil)
	}
	legacy, err := ParseResult(data)
	if err != nil || !legacy.classOmitted || legacy.FailureClass != "timeout" {
		t.Fatalf("omitted class = %+v, %v", legacy, err)
	}
	explicit, err := ParseResult(change(string(data), "failureClass", "timeout"))
	if err != nil || explicit.classOmitted || explicit.FailureClass != "timeout" {
		t.Fatalf("explicit class = %+v, %v", explicit, err)
	}
	for _, value := range []any{"", nil} {
		if _, err := ParseResult(change(string(data), "failureClass", value)); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted explicit empty class %v: %v", value, err)
		}
	}
}

func TestHeartbeatValidation(t *testing.T) {
	valid := `{"agent":"1.0","uptime":0,"lastTest":null}`
	for _, data := range []string{valid, `{"agent":"","uptime":12,"lastTest":"2026-11-03T00:00:00Z","updateResult":{"version":"v1","status":"succeeded","ts":"2026-11-03T00:00:00Z","error":null}}`} {
		if _, err := ParseHeartbeat([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]any{"agent": strings.Repeat("a", 65), "uptime": -1, "lastTest": "bad", "unknown": 1, "updateResult": map[string]any{"version": "v1", "status": "pending", "ts": "bad", "error": nil}} {
		if _, err := ParseHeartbeat(change(valid, key, value)); err == nil {
			t.Fatal("accepted", key)
		}
	}
	if _, err := ParseHeartbeat([]byte(`{"agent":null,"uptime":1,"lastTest":null}`)); err == nil {
		t.Fatal("null agent")
	}
}
func TestAckValidationAndTransitions(t *testing.T) {
	for _, data := range []string{`{"status":"running"}`, `{"status":"succeeded","pct":100,"phase":"done","resultId":"00000000-0000-4000-8000-000000000001"}`, `{"status":"failed","error":"engine failed"}`} {
		if _, err := ParseAck([]byte(data)); err != nil {
			t.Fatal(data, err)
		}
	}
	for key, value := range map[string]any{"status": "pending", "pct": 101, "phase": strings.Repeat("a", 201), "resultId": "invalid", "error": strings.Repeat("a", 2001), "unknown": true} {
		if _, err := ParseAck(change(`{"status":"running"}`, key, value)); err == nil {
			t.Fatal("accepted", key)
		}
	}
	for _, data := range []string{`{}`, `{"status":"failed"}`, `{"status":"failed","error":" "}`, `{"status":"running","pct":null}`} {
		if _, err := ParseAck([]byte(data)); err == nil {
			t.Fatal(data)
		}
	}
	c := Command{Status: "pending"}
	a, _ := ParseAck([]byte(`{"status":"running","phase":"download","pct":50}`))
	next, err := ApplyAck(c, a)
	if err != nil || next.Pct != 50 {
		t.Fatal(next, err)
	}
	lower := 49.0
	if _, err := ApplyAck(next, Ack{Status: "running", Pct: &lower}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	a, _ = ParseAck([]byte(`{"status":"succeeded","pct":100,"resultId":"00000000-0000-4000-8000-000000000001"}`))
	terminal, err := ApplyAck(next, a)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyAck(terminal, a)
	if err != nil || !sameAck(terminal, repeated) {
		t.Fatal(err)
	}
	if _, err := ApplyAck(terminal, Ack{Status: "failed"}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := ApplyAck(terminal, Ack{Status: "running"}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	x, y := "a", "b"
	if equalString(&x, &y) || equalString(nil, &x) || !equalString(&x, &x) {
		t.Fatal("pointer equality")
	}
}

func TestServerListAcknowledgementValidation(t *testing.T) {
	valid := `{"status":"succeeded","pct":100,"servers":[{"id":12,"name":"Singapore","city":"Singapore","isp":"Fiber","host":"sg.example","endpoint":"https://sg.example/backend","distanceKm":4.5}]}`
	ack, err := ParseAck([]byte(valid))
	if err != nil || ack.Servers == nil || len(*ack.Servers) != 1 || (*ack.Servers)[0].ID != 12 {
		t.Fatalf("server list ack=%+v err=%v", ack, err)
	}
	for _, data := range []string{
		`{"status":"succeeded","servers":null}`,
		`{"status":"succeeded","servers":[{"id":0,"name":"Bad"}]}`,
		`{"status":"succeeded","servers":[{"id":12,"name":""}]}`,
		`{"status":"succeeded","servers":[{"id":12,"name":"A"},{"id":12,"name":"B"}]}`,
		`{"status":"succeeded","servers":[{"id":12,"name":"A","distanceKm":-1}]}`,
		`{"status":"succeeded","servers":[{"id":2147483648,"name":"A"}]}`,
		`{"status":"succeeded","servers":[{"id":13,"name":"A","endpoint":"https://user:secret@sg.example/backend"}]}`,
		`{"status":"running","servers":[]}`,
	} {
		if _, err := ParseAck([]byte(data)); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
}

func TestTerminalCatalogAckDigestAllowsIdenticalRetryAndRejectsChanges(t *testing.T) {
	pct := float64(100)
	first := []ServerOption{{ID: 12, Name: "Hanoi"}, {ID: 13, Name: "Hue"}}
	terminal, err := ApplyAck(Command{Status: "pending"}, Ack{Status: "succeeded", Pct: &pct, Servers: &first})
	if err != nil {
		t.Fatal(err)
	}
	reordered := []ServerOption{first[1], first[0]}
	replay, err := ApplyAck(terminal, Ack{Status: "succeeded", Pct: &pct, Servers: &reordered})
	if err != nil || !sameAck(terminal, replay) {
		t.Fatalf("identical catalog retry=%+v err=%v", replay, err)
	}
	changed := []ServerOption{{ID: 12, Name: "Different Hanoi"}, {ID: 13, Name: "Hue"}}
	if _, err := ApplyAck(terminal, Ack{Status: "succeeded", Pct: &pct, Servers: &changed}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed terminal catalog error=%v, want conflict", err)
	}
}

func TestDecodeBoundaryFailures(t *testing.T) {
	var value any
	if Decode([]byte(`{} garbage`), &value) == nil {
		t.Fatal("invalid trailing value")
	}
	if nonNull([]byte(`broken`), "id") {
		t.Fatal("invalid JSON")
	}
	// Exercise the second decoder read independently of the initial object check.
	var object struct {
		X int `json:"x"`
	}
	if Decode([]byte(`{"x":null}`), &object) == nil {
		t.Fatal("null scalar accepted")
	}
}

func TestStrictWireShapes(t *testing.T) {
	for _, data := range [][]byte{change(validResult, "warnings", []any{nil}), change(validResult, "ID", "00000000-0000-4000-8000-000000000001"), change(validResult, "engineISP", 23), change(validResult, "loadedDown", map[string]any{"Low": 1, "iqm": 2, "high": 3}), change(validResult, "server", map[string]any{"id": 1, "name": nil, "city": "", "sponsor": ""})} {
		if _, err := ParseResult(data); err == nil {
			t.Fatal("invalid wire shape accepted")
		}
	}
	var target struct {
		Pointer *string  `json:"pointer"`
		Items   []string `json:"items"`
	}
	for _, data := range []string{`{"pointer":null,"items":[]}`, `{"pointer":"ok","items":["ok"]}`} {
		if Decode([]byte(data), &target) != nil {
			t.Fatal(data)
		}
	}
	// Direct shape checks cover malformed nested input without a database.
	for _, data := range []string{`[]`, `{`, `null`} {
		if strictShape([]byte(data), reflect.TypeOf(target)) == nil {
			t.Fatal(data)
		}
	}
	if strictShape([]byte(`{`), reflect.TypeOf([]string{})) == nil {
		t.Fatal("malformed slice")
	}
}
