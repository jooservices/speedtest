package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/testutil"
)

func TestFakeFromDifferentPackage(t *testing.T) {
	for _, scenario := range []string{"success", "slow", "loss"} {
		t.Run(scenario, func(t *testing.T) {
			out, err := testutil.RunFakeSpeedtest(t, context.Background(), scenario)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"type", "timestamp", "ping", "download", "upload", "isp", "interface", "server", "result"} {
				if _, ok := got[key]; !ok {
					t.Fatalf("missing %s", key)
				}
			}
			_, loss := got["packetLoss"]
			if loss != (scenario == "loss") {
				t.Fatalf("packetLoss presence: %s", out)
			}
			fields := map[string][]string{"ping": {"jitter", "latency", "low", "high"}, "download": {"bandwidth", "bytes", "elapsed", "latency"}, "upload": {"bandwidth", "bytes", "elapsed", "latency"}, "interface": {"internalIp", "name", "macAddr", "isVpn", "externalIp"}, "server": {"id", "host", "port", "name", "location", "country"}, "result": {"id", "url", "persisted"}}

			for _, key := range []string{"download", "upload"} {
				var transfer struct{ Latency map[string]json.RawMessage }
				if err := json.Unmarshal(got[key], &transfer); err != nil {
					t.Fatal(err)
				}
				for _, field := range []string{"iqm", "low", "high", "jitter"} {
					if _, ok := transfer.Latency[field]; !ok {
						t.Fatalf("missing %s.latency.%s", key, field)
					}
				}
			}
			for key, required := range fields {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(got[key], &obj); err != nil {
					t.Fatal(err)
				}
				for _, field := range required {
					if _, ok := obj[field]; !ok {
						t.Fatalf("missing %s.%s", key, field)
					}
				}
			}
		})
	}
	out, err := testutil.RunFakeSpeedtest(t, context.Background(), "success", "--down", "1", "--loss", "0")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Download   struct{ Bandwidth int64 }
		PacketLoss *float64
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Download.Bandwidth != 125000 || got.PacketLoss == nil || *got.PacketLoss != 0 {
		t.Fatalf("overrides: %s", out)
	}
	out, err = testutil.RunFakeSpeedtest(t, context.Background(), "crash")
	if err == nil || len(out) != 0 {
		t.Fatalf("crash: %s %v", out, err)
	}
	path := testutil.BuildFakeSpeedtest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err = exec.CommandContext(ctx, path, "--scenario", "timeout").Output()
	if ctx.Err() != context.DeadlineExceeded || err == nil || len(out) != 0 {
		t.Fatalf("timeout: %s %v", out, err)
	}
}

func TestBuildLifetimeAndConcurrency(t *testing.T) {
	var previous string
	t.Run("owner", func(t *testing.T) { previous = testutil.BuildFakeSpeedtest(t) })
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatalf("temporary binary remains: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			path := testutil.BuildFakeSpeedtest(t)
			if _, err := exec.Command(path).Output(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
