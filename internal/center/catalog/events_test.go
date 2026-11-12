package catalog

import (
	"slices"
	"testing"
)

func TestEventsCatalogContract(t *testing.T) {
	got := Events()
	if len(got) < 30 {
		t.Fatalf("events = %d, want a full catalog", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		if e.Key == "" || e.Category == "" || e.Title == "" || !severitiesOK(e.Severity) {
			t.Fatalf("invalid event %+v", e)
		}
		if seen[e.Key] {
			t.Fatalf("duplicate key %s", e.Key)
		}
		seen[e.Key] = true
		if len(e.Channels) == 0 || len(e.Templates) == 0 {
			t.Fatalf("%s missing channels or templates", e.Key)
		}
		for _, ch := range e.Channels {
			if _, ok := LookupChannel(ch); !ok {
				t.Fatalf("%s channel %s unknown", e.Key, ch)
			}
		}
		for ch, body := range e.Templates {
			if _, ok := LookupChannel(ch); !ok || body == "" {
				t.Fatalf("%s template %s invalid", e.Key, ch)
			}
		}
		if !slices.Contains(e.Placeholders, "node") || !slices.Contains(e.Placeholders, "link") {
			t.Fatalf("%s missing required placeholders", e.Key)
		}
	}
	got[0].Key = "mutated"
	got[0].Channels[0] = "mutated"
	got[0].Templates["email"] = "mutated"
	if Events()[0].Key == "mutated" || Events()[0].Channels[0] == "mutated" {
		t.Fatal("caller mutated shared catalog")
	}
}

func TestLookupAndMetricMapping(t *testing.T) {
	e, ok := LookupEvent("test.success")
	if !ok || !e.Enabled || !e.NodeScoped || e.Severity != "info" {
		t.Fatalf("test.success = %+v %v", e, ok)
	}
	if _, ok := LookupEvent("missing"); ok {
		t.Fatal("missing accepted")
	}
	for metric, key := range map[string]string{
		"offline": "node.offline", "failed": "test.failed", "ip_changed": "network.ip_changed",
		"down_pct": "quality.download_low", "up_pct": "quality.upload_low",
		"ping": "quality.ping_high", "jitter": "quality.jitter_high", "loss": "quality.loss_high",
	} {
		got, ok := EventForAlertMetric(metric)
		if !ok || got.Key != key || got.AlertMetric != metric {
			t.Fatalf("%s -> %+v %v", metric, got, ok)
		}
	}
	if _, ok := EventForAlertMetric("unknown"); ok {
		t.Fatal("unknown metric mapped")
	}
	if !ValidEventKey("node.online") || ValidEventKey("nope") {
		t.Fatal("ValidEventKey mismatch")
	}
	if !ValidEventPlaceholder("test.success", "download") || !ValidEventPlaceholder("test.success", "severity_icon") {
		t.Fatal("expected placeholders rejected")
	}
	if ValidEventPlaceholder("test.success", "secret") || ValidEventPlaceholder("missing", "node") {
		t.Fatal("bad placeholders accepted")
	}
	offline, _ := LookupEvent("node.offline")
	if offline.RecoveryKey != "node.online" {
		t.Fatalf("offline recovery = %q", offline.RecoveryKey)
	}
	drop, _ := LookupEvent("quality.drop_vs_median")
	if !drop.EngineScoped || drop.Enabled {
		t.Fatalf("engine-scoped default wrong: %+v", drop)
	}
}

func severitiesOK(s string) bool { return s == "info" || s == "warn" || s == "crit" }

func TestBasePlaceholdersDedupesExtras(t *testing.T) {
	got := basePlaceholders("node", "extra_token")
	if !slices.Contains(got, "extra_token") || !slices.Contains(got, "node") {
		t.Fatalf("got %v", got)
	}
	count := 0
	for _, p := range got {
		if p == "node" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("node counted %d", count)
	}
}
