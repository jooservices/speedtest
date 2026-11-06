package agentapi

import (
	"strings"
	"testing"
)

func TestHeartbeatEngineCapabilities(t *testing.T) {
	for _, engines := range []string{`[]`, `["ookla"]`, `["ookla","librespeed"]`} {
		h, err := ParseHeartbeat([]byte(`{"agent":"v2","uptime":1,"lastTest":null,"engines":` + engines + `}`))
		if err != nil || h.Engines == nil {
			t.Fatal(h, err)
		}
	}
	for _, engines := range []string{`null`, `["bad"]`, `["ookla","ookla"]`, `[null]`} {
		if _, err := ParseHeartbeat([]byte(`{"agent":"v2","uptime":1,"lastTest":null,"engines":` + engines + `}`)); err == nil {
			t.Fatal("accepted", engines)
		}
	}
}

func TestResultEngineIdentityDefaultsAndValidation(t *testing.T) {
	// A failed measurement needs no invented server or address.
	base := `{"id":"00000000-0000-4000-8000-000000000071","ts":"2026-11-06T00:00:00Z","status":"failed","down":null,"up":null,"ping":null,"jitter":null,"dLat":null,"uLat":null,"loss":null,"serverId":null,"ip":null,"threads":4,"bytesDown":0,"bytesUp":0,"duration":0,"error":"offline","manual":false}`
	for _, name := range []string{"ookla", "librespeed"} {
		result, err := ParseResult([]byte(strings.Replace(base, `"status":`, `"engine":"`+name+`","status":`, 1)))
		if err != nil || result.Engine != name {
			t.Fatal(result, err)
		}
	}
	result, err := ParseResult([]byte(base))
	if err != nil || result.Engine != "ookla" {
		t.Fatal(result, err)
	}
	if _, err := ParseResult([]byte(strings.Replace(base, `"status":`, `"engine":"bad","status":`, 1))); err == nil {
		t.Fatal("unsupported engine accepted")
	}
}
