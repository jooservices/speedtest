package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jooservices/speedtest/internal/agent/testutil"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	raw, err := testutil.RunFakeSpeedtest(t, context.Background(), "success")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertClass(t *testing.T, r Result, err error, class ErrorClass) {
	t.Helper()
	var failure *RunError
	if !errors.As(err, &failure) || failure.Class != class || r.Error != err.Error() {
		t.Fatalf("result error=%q, err=%v, want %s", r.Error, err, class)
	}
	if r.Down != nil || r.Up != nil || r.Ping != nil {
		t.Fatal("failure manufactured metrics")
	}
}

func TestParseOoklaFixtures(t *testing.T) {
	for _, scenario := range []string{"success", "slow", "loss"} {
		t.Run(scenario, func(t *testing.T) {
			raw, err := testutil.RunFakeSpeedtest(t, context.Background(), scenario)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ParseOokla(raw)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Timestamp        string
				Download, Upload struct {
					Bandwidth float64
					Bytes     int64
					Elapsed   float64
					Latency   LoadedLatency
				}
				Ping       struct{ Latency, Jitter float64 }
				PacketLoss *float64
				ISP        string
				Interface  struct{ ExternalIP string }
				Server     Server
				Result     struct{ ID, URL string }
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if *r.Down != wire.Download.Bandwidth*8/1e6 || *r.Up != wire.Upload.Bandwidth*8/1e6 || *r.Ping != wire.Ping.Latency || *r.Jitter != wire.Ping.Jitter || !reflect.DeepEqual(r.Loss, wire.PacketLoss) {
				t.Fatalf("metrics: %+v", r)
			}
			if !reflect.DeepEqual(r.LoadedDown, &wire.Download.Latency) || !reflect.DeepEqual(r.LoadedUp, &wire.Upload.Latency) || !reflect.DeepEqual(r.Server, &wire.Server) {
				t.Fatalf("details: %+v", r)
			}
			if r.ISP != wire.ISP || r.ExternalIP != wire.Interface.ExternalIP || r.ResultID != wire.Result.ID || r.ResultURL != wire.Result.URL || r.Timestamp.Format("2006-01-02T15:04:05Z07:00") != wire.Timestamp {
				t.Fatalf("metadata: %+v", r)
			}
			if r.BytesDown != wire.Download.Bytes || r.BytesUp != wire.Upload.Bytes || r.Duration != (wire.Download.Elapsed+wire.Upload.Elapsed)/1000 || string(r.Raw) != string(raw) || r.Error != "" {
				t.Fatalf("counts/raw/error: %+v", r)
			}
			raw[0] = '!'
			if r.Raw[0] != '{' {
				t.Fatal("raw aliases caller memory")
			}
		})
	}
}

func TestParseLegacyAndZero(t *testing.T) {
	raw, err := os.ReadFile("testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseOokla(raw)
	if err != nil {
		t.Fatal(err)
	}
	if *r.Down != 9.206656 || *r.Up != 0.928184 || r.Duration != 23.823 || r.Loss != nil || r.LoadedDown != nil || r.LoadedUp != nil || r.Server.Location != "London" {
		t.Fatalf("legacy: %+v", r)
	}
	raw, err = testutil.RunFakeSpeedtest(t, context.Background(), "success", "--down", "0", "--up", "0", "--ping", "0", "--jitter", "0", "--loss", "0")
	if err != nil {
		t.Fatal(err)
	}
	r, err = ParseOokla(raw)
	if err != nil || r.Loss == nil || *r.Loss != 0 || *r.Down != 0 || *r.Up != 0 || *r.Ping != 0 || *r.Jitter != 0 {
		t.Fatalf("zero: %+v %v", r, err)
	}
}

func TestParseInvalidAndFailure(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		class ErrorClass
	}{
		{"", InvalidResult}, {"{", InvalidResult}, {"null", InvalidResult}, {"[]", InvalidResult}, {"{}", InvalidResult},
		{`{"type":"result","timestamp":"bad"}`, InvalidResult},
		{`{"type":"log","message":"No route to host"}`, NoRoute},
		{`{"type":"error","error":"Server unavailable"}`, ServerUnavailable},
		{`{"type":"log","message":"License must be accepted"}`, License},
		{`{"type":"log","message":"operation timed out"}`, Timeout},
		{`{"type":"error","message":"unexpected failure"}`, Execution},
	} {
		t.Run(tc.raw, func(t *testing.T) { r, err := ParseOokla([]byte(tc.raw)); assertClass(t, r, err, tc.class) })
	}
	raw := fixture(t)
	mutations := []func(map[string]any){
		func(v map[string]any) { delete(v, "timestamp") },
		func(v map[string]any) { delete(v, "server") },
		func(v map[string]any) { v["server"].(map[string]any)["id"] = 0 },
		func(v map[string]any) { delete(v["download"].(map[string]any), "bandwidth") },
		func(v map[string]any) { v["upload"].(map[string]any)["bandwidth"] = nil },
		func(v map[string]any) { v["ping"].(map[string]any)["latency"] = nil },
		func(v map[string]any) { v["packetLoss"] = -1 },
		func(v map[string]any) { v["packetLoss"] = 101 },
		func(v map[string]any) { v["download"].(map[string]any)["bandwidth"] = -1 },
		func(v map[string]any) { v["download"].(map[string]any)["bytes"] = -1 },
		func(v map[string]any) { v["upload"].(map[string]any)["bytes"] = -1 },
		func(v map[string]any) { v["download"].(map[string]any)["elapsed"] = -1 },
		func(v map[string]any) { v["upload"].(map[string]any)["elapsed"] = -1 },
		func(v map[string]any) { v["upload"].(map[string]any)["latency"].(map[string]any)["high"] = -1 },
		func(v map[string]any) { v["download"].(map[string]any)["bandwidth"] = 1e308 },
	}
	for _, mutate := range mutations {
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		mutate(v)
		changed, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseOokla(changed)
		assertClass(t, r, err, InvalidResult)
	}
	r, err := ParseOokla([]byte(strings.Repeat(" ", maxOutput+1)))
	assertClass(t, r, err, InvalidResult)
}
