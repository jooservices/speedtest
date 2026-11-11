package ownerapi

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jooservices/speedtest/internal/center/nodes"
)

func TestGeoObject(t *testing.T) {
	raw := []byte(`{"city":"Hanoi","region":"Hanoi","country":"vn","latitude":21.0285,"longitude":105.8542,"time_zone":"Asia/Ho_Chi_Minh","accuracy_radius":20,"asn":7552,"organisation":"Viettel Group"}`)
	g := GeoObject("203.0.113.5", "GeoLite2", raw)
	if g["ip"] != "203.0.113.5" || g["version"] != 4 || g["source"] != "GeoLite2" || g["asn"] != uint(7552) || g["asnOrg"] != "Viettel Group" ||
		g["isp"] != "Viettel Group" || g["city"] != "Hanoi" || g["region"] != "Hanoi" || g["country"] != "vn" || g["countryCode"] != "VN" ||
		g["lat"] != 21.0285 || g["lon"] != 105.8542 || g["timezone"] != "Asia/Ho_Chi_Minh" || g["radiusKm"] != float64(20) {
		t.Fatalf("geo = %v", g)
	}
	v6 := GeoObject("2001:db8::1", "", []byte(`{"city":"X","country":"Vietnam"}`))
	if v6["version"] != 6 || v6["source"] != "unknown" || v6["countryCode"] != nil || v6["lat"] != nil {
		t.Fatalf("v6 geo = %v", v6)
	}
	for name, tc := range map[string]struct {
		ip  string
		raw []byte
	}{"bad ip": {"nope", raw}, "empty": {"203.0.113.5", nil}, "empty object": {"203.0.113.5", []byte(`{}`)}, "invalid json": {"203.0.113.5", []byte(`{`)}} {
		if g := GeoObject(tc.ip, "x", tc.raw); g != nil {
			t.Errorf("%s: geo = %v, want nil", name, g)
		}
	}
}

func TestStatusDerivation(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fresh, stale := now.Add(-30*time.Second), now.Add(-HeartbeatTimeout-time.Second)
	for name, tc := range map[string]struct {
		n      nodeRow
		latest map[string]any
		want   string
	}{
		"disabled":      {nodeRow{Enabled: false, ScheduleOn: true, LastSeen: &fresh}, nil, "paused"},
		"schedule off":  {nodeRow{Enabled: true, ScheduleOn: false, LastSeen: &fresh}, nil, "paused"},
		"never seen":    {nodeRow{Enabled: true, ScheduleOn: true}, nil, "offline"},
		"stale":         {nodeRow{Enabled: true, ScheduleOn: true, LastSeen: &stale}, nil, "offline"},
		"failed":        {nodeRow{Enabled: true, ScheduleOn: true, LastSeen: &fresh}, map[string]any{"status": "failed"}, "failed"},
		"degraded":      {nodeRow{Enabled: true, ScheduleOn: true, LastSeen: &fresh}, map[string]any{"status": "degraded"}, "degraded"},
		"ok":            {nodeRow{Enabled: true, ScheduleOn: true, LastSeen: &fresh}, map[string]any{"status": "ok"}, "online"},
		"no result yet": {nodeRow{Enabled: true, ScheduleOn: true, LastSeen: &fresh}, nil, "online"},
		"paused wins":   {nodeRow{Enabled: false, ScheduleOn: true}, map[string]any{"status": "failed"}, "paused"},
	} {
		if got := tc.n.status(now, tc.latest); got != tc.want {
			t.Errorf("%s: status = %q, want %q", name, got, tc.want)
		}
	}
}

func TestNodeViewShape(t *testing.T) {
	now := time.Now().UTC()
	seen := now.Add(-time.Second)
	sid := int32(7)
	ip := "192.0.2.9"
	n := nodeRow{ID: "id", Name: "VM1", Down: 300, Up: 100, Threads: 4, ServerID: &sid, ColorSlot: 2, Enabled: true, ScheduleOn: true,
		Interval: 30, Offset: 10, SourceIP: &ip, CreatedAt: now, LastSeen: &seen, Revision: 5}
	v := n.view(now, map[string]any{"status": "ok"}, map[string]any{"id": "r"}, "203.0.113.1", map[string]any{"ip": "203.0.113.1"}, map[string]any{"name": "Viettel"})
	if v["server"] != int64(7) || v["status"] != "online" || v["currentIp"] != "203.0.113.1" || v["latestGood"] == nil || v["geo"] == nil ||
		v["isp"].(map[string]any)["name"] != "Viettel" ||
		v["binding"].(map[string]any)["sourceIp"] != "192.0.2.9" || v["interval"] != int64(30) || v["lastSeen"] == nil || v["updateVersion"] != nil {
		t.Fatalf("view = %v", v)
	}
	bare := nodeRow{Enabled: true, ScheduleOn: true}.view(now, nil, nil, "", nil, nil)
	if bare["server"] != "auto" || bare["latest"] != nil || bare["currentIp"] != nil || bare["lastSeen"] != nil || bare["status"] != "offline" {
		t.Fatalf("bare view = %v", bare)
	}
	if ETag(5) != `"node-5"` {
		t.Fatal("ETag format")
	}
	input := n.input()
	if input.Name != "VM1" || *input.ServerID != 7 || *input.Binding.SourceIP != "192.0.2.9" || !input.Enabled {
		t.Fatalf("input = %+v", input)
	}
	if (nodeRow{}).input().ServerID != nil {
		t.Fatal("automatic server must stay nil")
	}
}

func raw(m map[string]string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		out[k] = json.RawMessage(v)
	}
	return out
}

func TestApplyPatch(t *testing.T) {
	base := func() nodes.Input {
		ip := "192.0.2.1"
		return nodes.Input{Name: "VM1", Label: "L", ExpectedDown: 300, ExpectedUp: 100, Threads: 4, Enabled: true, Binding: nodes.Binding{Iface: "eth0", SourceIP: &ip}}
	}
	in := base()
	err := applyPatch(&in, raw(map[string]string{"name": `"VM2"`, "label": `"x"`, "provider": `"p"`, "plan": `"q"`, "note": `"n"`, "expectedDown": `500`,
		"expectedUp": `250`, "enabled": `false`, "autoUpdate": `true`, "threads": `8`, "server": `12`, "binding": `{"iface":"","sourceIp":null}`}))
	if err != nil || in.Name != "VM2" || in.Label != "x" || in.Provider != "p" || in.Plan != "q" || in.Note != "n" || in.ExpectedDown != 500 || in.ExpectedUp != 250 ||
		in.Enabled || !in.AutoUpdate || in.Threads != 8 || *in.ServerID != 12 || in.Binding.Iface != "" || in.Binding.SourceIP != nil {
		t.Fatalf("patched = %+v err=%v", in, err)
	}
	in = base()
	if err := applyPatch(&in, raw(map[string]string{"server": `"auto"`, "binding": `{"sourceIp":"198.51.100.4"}`})); err != nil || in.ServerID != nil || *in.Binding.SourceIP != "198.51.100.4" || in.Binding.Iface != "eth0" {
		t.Fatalf("partial binding patch = %+v err=%v", in, err)
	}
	for name, patch := range map[string]map[string]string{
		"null required":   {"name": `null`},
		"name type":       {"name": `5`},
		"number type":     {"expectedDown": `"fast"`},
		"bool type":       {"enabled": `"yes"`},
		"threads type":    {"threads": `"4"`},
		"threads zero":    {"threads": `0`},
		"server invalid":  {"server": `0`},
		"server text":     {"server": `"manual"`},
		"binding type":    {"binding": `5`},
		"binding ip":      {"binding": `{"sourceIp":"nope"}`},
		"binding ip type": {"binding": `{"sourceIp":5}`},
	} {
		in := base()
		if err := applyPatch(&in, raw(patch)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestApplyOnePlanDefault(t *testing.T) {
	for _, tc := range []struct {
		name        string
		patch       map[string]string
		wasDefault  bool
		initial     float64
		wantValue   float64
		wantDefault bool
	}{
		{name: "updates a default", wasDefault: true, initial: 300, wantValue: 700, wantDefault: true},
		{name: "keeps an override", wasDefault: false, initial: 500, wantValue: 500},
		{name: "explicit equal value becomes override", patch: map[string]string{"expectedDown": `500`}, wasDefault: true, initial: 500, wantValue: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.initial
			gotDefault := applyOnePlanDefault(raw(tc.patch), "expectedDown", tc.wasDefault, &value, 700)
			if value != tc.wantValue || gotDefault != tc.wantDefault {
				t.Fatalf("value/default=%v/%v want %v/%v", value, gotDefault, tc.wantValue, tc.wantDefault)
			}
		})
	}
}

func TestSameNullable(t *testing.T) {
	value, other := "plan-a", "plan-b"
	for _, tc := range []struct {
		name string
		a, b *string
		want bool
	}{
		{name: "both nil", want: true},
		{name: "one nil", b: &value},
		{name: "equal values", a: &value, b: &value, want: true},
		{name: "different values", a: &value, b: &other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameNullable(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameNullable()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestQueueCommandRejectsInvalidRequestsBeforeTouchingTheDatabase(t *testing.T) {
	s := &Store{}
	for name, tc := range map[string]struct{ typ, version string }{
		"unknown type":            {"reboot", ""},
		"update_agent no version": {"update_agent", ""},
		"blank version":           {"update_agent", "  "},
		"version too long":        {"update_agent", string(make([]byte, 65))},
		"version on other type":   {"run_now", "1.0"},
	} {
		if _, err := s.QueueCommand(context.Background(), "id", tc.typ, tc.version); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestJSONHelpersAndUUID(t *testing.T) {
	if jsonOrNil(nil) != nil || jsonOrNil([]byte("null")) != nil || jsonOrNil([]byte("{")) != nil {
		t.Fatal("jsonOrNil must return nil for empty, null and invalid input")
	}
	if m, ok := jsonObject([]byte(`[1]`)).(map[string]any); !ok || len(m) != 0 {
		t.Fatal("jsonObject must fall back to an empty object")
	}
	if a, ok := jsonArray([]byte(`{}`)).([]any); !ok || len(a) != 0 {
		t.Fatal("jsonArray must fall back to an empty array")
	}
	if a := jsonArray([]byte(`["w"]`)).([]any); len(a) != 1 {
		t.Fatal("jsonArray lost its content")
	}
	id, err := newUUID()
	if err != nil || len(id) != 36 || id[14] != '4' {
		t.Fatalf("uuid = %q err=%v", id, err)
	}
	other, _ := newUUID()
	if id == other {
		t.Fatal("uuids must differ")
	}
	var nilStore *Store
	_ = nilStore
	if (&Store{}).now().IsZero() {
		t.Fatal("default clock")
	}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !(&Store{Now: func() time.Time { return fixed }}).now().Equal(fixed) {
		t.Fatal("injected clock")
	}
}

type ownerScanRow struct{ err error }

func (r ownerScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	serverID, ip := int32(7), "203.0.113.4"
	resultID, engineIP := "result-ref", "203.0.113.5"
	values := []any{"result-id", "node-id", now, "ok", ptrFloat(100), ptrFloat(50), ptrFloat(5), ptrFloat(1), ptrFloat(20), ptrFloat(25), ptrFloat(0), &serverID,
		[]byte(`{"name":"London"}`), &ip, int32(4), ptrInt64(1000), ptrInt64(500), ptrFloat(10), ptrString(""), true,
		[]byte(`{"download":true}`), []byte(`{"p95":20}`), []byte(`{"p95":25}`), ptrString("Example ISP"), &resultID, ptrString("https://example.test/result"), &engineIP,
		[]byte(`["warning"]`), "ookla", "other", "extra value"}
	for i, target := range dest {
		value := reflect.ValueOf(target).Elem()
		if values[i] == nil {
			value.SetZero()
			continue
		}
		value.Set(reflect.ValueOf(values[i]))
	}
	return nil
}

func ptrFloat(value float64) *float64 { return &value }
func ptrInt64(value int64) *int64     { return &value }
func ptrString(value string) *string  { return &value }

func TestScanResultMapsColumnsAndErrors(t *testing.T) {
	var extra string
	view, err := ScanResult(ownerScanRow{}, &extra)
	if err != nil {
		t.Fatal(err)
	}
	if view.IP != "203.0.113.4" || view.Object["id"] != "result-id" || view.Object["nodeId"] != "node-id" || view.Object["status"] != "ok" ||
		view.Object["engine"] != "ookla" || view.Object["failureClass"] != "other" || view.Object["manual"] != true || view.Object["geo"] != nil || view.Object["server"].(map[string]any)["name"] != "London" || extra != "extra value" {
		t.Fatalf("result view = %+v extra=%q", view, extra)
	}
	if _, err := ScanResult(ownerScanRow{err: pgx.ErrNoRows}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("scan error = %v", err)
	}
}
