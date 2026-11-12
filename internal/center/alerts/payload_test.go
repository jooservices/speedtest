package alerts

import (
	"encoding/json"
	"testing"

	"github.com/jooservices/speedtest/internal/center/agentapi"
)

func TestBuildPayloadQualityAndLimits(t *testing.T) {
	down, up, ping, jitter, loss := 180.0, 90.0, 12.0, 3.0, 0.2
	ip := "203.0.113.9"
	isp := "Viettel"
	result := agentapi.Result{
		TS: "2026-11-08T01:00:00Z", Status: "ok", Down: &down, Up: &up, Ping: &ping, Jitter: &jitter, Loss: &loss,
		IP: &ip, EngineISP: &isp, Server: &agentapi.Server{Name: "HCMC", City: "HCMC"},
	}
	data := BuildPayload("quality.download_low", "Fiber", "https://center.example/", "warn", result, 300, 100, DefaultQualityLimits(), map[string]string{
		"value": "60", "threshold": "65", "old_ip": "198.51.100.1",
	})
	if data["node"] != "Fiber" || data["download_ok"] != "no" || data["upload_ok"] != "yes" {
		t.Fatalf("payload %#v", data)
	}
	if data["ping"] != "12" || data["jitter"] != "3" || data["loss"] != "0.2" {
		t.Fatalf("quality fields %#v", data)
	}
	if data["link"] != "https://center.example/#/alerts" || data["severity_icon"] == "" {
		t.Fatalf("link/icon %#v", data)
	}
	raw := PayloadJSON(data)
	var decoded map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded["event"] == "" {
		t.Fatalf("json %s %v", raw, err)
	}
	for _, metric := range []string{"down_pct", "up_pct", "ping", "jitter", "loss", "failed"} {
		rule := Rule{Metric: metric, Value: 65, ForTests: 2}
		v, _, _ := metricValue(rule, result, 300, 100)
		if metric != "failed" && v == "" {
			t.Fatalf("metricValue %s empty", metric)
		}
	}
	if DefaultQualityLimits().DownloadPct != 65 || okFlag(false) != "no" || mbps(nil) != "" || optNum(nil) != "" {
		t.Fatal("helpers")
	}
	empty := BuildPayload("missing.event", "N", "", "", agentapi.Result{}, 0, 0, DefaultQualityLimits(), nil)
	if empty["event"] != "missing.event" || empty["severity_icon"] == "" {
		t.Fatalf("empty event %#v", empty)
	}
	if string(PayloadJSON(map[string]string{"a": "b"})) == "" {
		t.Fatal("payload json")
	}
	fractional := DefaultQualityLimits()
	fractional.DownloadPct = 65.5
	data = BuildPayload("quality.download_low", "Fiber", "", "warn", result, 300, 100, fractional, nil)
	if data["download_limit"] != "65.5% of expected" || metricThreshold(Rule{Metric: "down_pct", Value: 65.25}) != "65.25% of expected" {
		t.Fatalf("fractional configured threshold was rounded: limit=%q trigger=%q", data["download_limit"], metricThreshold(Rule{Metric: "down_pct", Value: 65.25}))
	}
	value, expected, pct := metricValue(Rule{Metric: "down_pct", Value: 65.25}, result, 300, 100)
	if value != "180 Mbps" || expected != "300 Mbps" || pct != "60" {
		t.Fatalf("quality trigger context = %q/%q/%q", value, expected, pct)
	}
	if failureReason(agentapi.Result{Status: "failed", FailureClass: "timeout"}) != "Timeout" {
		t.Fatal("classified failure reason was not normalized for event templates")
	}
}
