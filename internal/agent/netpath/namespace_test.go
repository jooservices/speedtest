//go:build integration

package netpath

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

func TestNamespaceEchoUsesSelectedSourceAddress(t *testing.T) {
	if os.Getenv("SPEEDTEST_NETPATH_NAMESPACE_TEST") != "1" {
		t.Skip("run scripts/test-netpath-namespace.sh to create isolated network namespaces")
	}
	centerURL := os.Getenv("SPEEDTEST_NETPATH_CENTER_URL")
	interfaceName := os.Getenv("SPEEDTEST_NETPATH_INTERFACE")
	wantIP := os.Getenv("SPEEDTEST_NETPATH_EXPECTED_IP")
	if centerURL == "" || interfaceName == "" || wantIP == "" {
		t.Fatal("namespace test requires center URL, interface, and expected IP")
	}
	measurement := &recordingEngine{result: engine.Result{ExternalIP: wantIP}}
	runner, err := NewRunner(measurement, centerURL, "namespace-test-token", Binding{Interface: interfaceName})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, engine.Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PublicIP != wantIP {
		t.Fatalf("center observed public IP %q, want selected source %q", result.PublicIP, wantIP)
	}
	if measurement.options.InterfaceName != interfaceName {
		t.Fatalf("engine interface = %q, want %q", measurement.options.InterfaceName, interfaceName)
	}
	if strings.Contains(strings.Join(result.Warnings, " "), "mismatch") {
		t.Fatalf("unexpected mismatch warning: %q", result.Warnings)
	}
}
