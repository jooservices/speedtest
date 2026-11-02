package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/testutil"
)

func newEngine(t *testing.T, c Config) *Ookla {
	t.Helper()
	e, err := NewOokla(c)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wrapper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunnerDefaultArgumentsAndPin(t *testing.T) {
	fake := testutil.BuildFakeSpeedtest(t)
	t.Setenv("ENGINE_TEST_FAKE", fake)
	// The shared fake does not accept Ookla's license/format flags. The adapter
	// validates them before forwarding only the remaining server pin to the fake.
	adapter := script(t, `test "$1" = --accept-license && test "$2" = --accept-gdpr && test "$3" = -f && test "$4" = json || exit 3
shift 4
exec "$ENGINE_TEST_FAKE" "$@"
`)
	for _, id := range []int64{0, 42} {
		r, err := newEngine(t, Config{Binary: adapter}).Run(context.Background(), Options{ServerID: id})
		if err != nil || *r.Down != 300 || r.Server == nil {
			t.Fatalf("run: %+v %v", r, err)
		}
		if id > 0 && r.Server.ID != id {
			t.Fatalf("server=%d", r.Server.ID)
		}
	}
	e := newEngine(t, Config{})
	if e.config.Binary != "speedtest" || e.config.Timeout != 120*time.Second {
		t.Fatal(e.config)
	}
}

func TestRunnerPassesNetworkBindingFlags(t *testing.T) {
	fake := testutil.BuildFakeSpeedtest(t)
	capture := filepath.Join(t.TempDir(), "args")
	t.Setenv("ENGINE_TEST_FAKE", fake)
	t.Setenv("ENGINE_TEST_ARGS", capture)
	adapter := script(t, `printf '%s\n' "$@" > "$ENGINE_TEST_ARGS"
exec "$ENGINE_TEST_FAKE"
`)
	for _, tc := range []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "interface",
			opts: Options{ServerID: 42, InterfaceName: "enp3s0"},
			want: []string{"--accept-license", "--accept-gdpr", "-f", "json", "--server-id", "42", "--interface", "enp3s0"},
		},
		{
			name: "source IP",
			opts: Options{SourceIP: "192.0.2.10"},
			want: []string{"--accept-license", "--accept-gdpr", "-f", "json", "--ip", "192.0.2.10"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newEngine(t, Config{Binary: adapter}).Run(context.Background(), tc.opts)
			if err != nil || r.Down == nil {
				t.Fatalf("Run() = %+v, %v", r, err)
			}
			gotBytes, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Fields(string(gotBytes))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("arguments = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunnerCustomArgumentsAndConcurrency(t *testing.T) {
	args := []string{"--scenario", "loss"}
	e := newEngine(t, Config{Binary: testutil.BuildFakeSpeedtest(t), Args: args})
	args[1] = "crash"
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			r, err := e.Run(context.Background(), Options{})
			if err != nil || r.Loss == nil || *r.Loss != 12 {
				t.Errorf("concurrent: %+v %v", r, err)
			}
		})
	}
	wg.Wait()
	r, err := newEngine(t, Config{Binary: testutil.BuildFakeSpeedtest(t), Args: []string{}}).Run(context.Background(), Options{})
	if err != nil || *r.Down != 300 {
		t.Fatalf("empty arguments: %v", err)
	}
}

func TestRunnerErrorsAndDeadlines(t *testing.T) {
	fake := testutil.BuildFakeSpeedtest(t)
	for _, tc := range []struct {
		name   string
		config Config
		opts   Options
		class  ErrorClass
	}{
		{"missing", Config{Binary: filepath.Join(t.TempDir(), "absent")}, Options{}, BinaryMissing},
		{"missing PATH", Config{Binary: "speedtest-nonexistent-joost15"}, Options{}, BinaryMissing},
		{"crash", Config{Binary: fake, Args: []string{"--scenario", "crash"}}, Options{}, Execution},
		{"timeout", Config{Binary: fake, Args: []string{"--scenario", "timeout"}, Timeout: 20 * time.Millisecond}, Options{}, Timeout},
		{"override timeout", Config{Binary: fake, Args: []string{"--scenario", "timeout"}}, Options{Timeout: 20 * time.Millisecond}, Timeout},
		{"negative pin", Config{}, Options{ServerID: -1}, InvalidOptions},
		{"negative timeout", Config{}, Options{Timeout: -1}, InvalidOptions},
		{"both network selectors", Config{}, Options{InterfaceName: "eth0", SourceIP: "192.0.2.10"}, InvalidOptions},
		{"invalid source IP", Config{}, Options{SourceIP: "not-an-ip"}, InvalidOptions},
		{"long interface name", Config{}, Options{InterfaceName: "interface-name-too-long"}, InvalidOptions},
		{"invalid output", Config{Binary: script(t, "printf invalid")}, Options{}, InvalidResult},
		{"oversize output", Config{Binary: script(t, "head -c 70000 /dev/zero")}, Options{}, InvalidResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			r, err := newEngine(t, tc.config).Run(context.Background(), tc.opts)
			assertClass(t, r, err, tc.class)
			if time.Since(started) > 3*time.Second {
				t.Fatal("run did not terminate promptly")
			}
			if tc.class == Timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		class := Canceled
		cause := context.Canceled
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			class, cause = Timeout, context.DeadlineExceeded
		} else {
			cancel()
		}
		r, err := newEngine(t, Config{Binary: fake}).Run(ctx, Options{})
		cancel()
		assertClass(t, r, err, class)
		if !errors.Is(err, cause) {
			t.Fatal(err)
		}
	}
	if _, err := NewOokla(Config{Timeout: -1}); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

func TestRunnerFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		message string
		class   ErrorClass
	}{
		{"license acceptance required", License}, {"gdpr consent required", License}, {"licence required", License},
		{"No route to host", NoRoute}, {"Network is unreachable", NoRoute}, {"Network unreachable", NoRoute},
		{"Cannot resolve host", NoRoute}, {"Could not resolve hostname", NoRoute},
		{"Unable to connect to server", ServerUnavailable}, {"Connection refused", ServerUnavailable},
		{"Timeout", Timeout}, {"Operation timed out", Timeout}, {"unexpected exit", Execution},
	} {
		t.Run(tc.message, func(t *testing.T) {
			t.Setenv("ENGINE_TEST_MESSAGE", tc.message+" private-marker")
			bin := script(t, `printf '%s' "$ENGINE_TEST_MESSAGE" >&2
exit 1
`)
			r, err := newEngine(t, Config{Binary: bin}).Run(context.Background(), Options{})
			assertClass(t, r, err, tc.class)
			if strings.Contains(r.Error, "private-marker") || r.Raw != nil {
				t.Fatal("diagnostic leaked")
			}
		})
	}
	raw := `{"type":"log","level":"error","message":"Server unavailable"}`
	t.Setenv("ENGINE_TEST_OUTPUT", raw)
	for _, exit := range []string{"0", "1"} {
		bin := script(t, `printf '%s' "$ENGINE_TEST_OUTPUT"
exit `+exit+"\n")
		r, err := newEngine(t, Config{Binary: bin}).Run(context.Background(), Options{})
		assertClass(t, r, err, ServerUnavailable)
		if string(r.Raw) != raw {
			t.Fatal("failure JSON lost")
		}
	}
}

func TestCaptureBounded(t *testing.T) {
	var b capture
	input := []byte(strings.Repeat("a", maxOutput+10))
	n, err := b.Write(input)
	if n != len(input) || err != nil || len(b.data) != maxOutput || !b.overflow {
		t.Fatal("capture limit")
	}
	n, err = b.Write([]byte("more"))
	if n != 4 || err != nil || len(b.data) != maxOutput {
		t.Fatal("capture should drain")
	}
}

func TestConfigFromEnv(t *testing.T) {
	for _, key := range []string{"SPEEDTEST_OOKLA_BINARY", "SPEEDTEST_OOKLA_ARGS", "SPEEDTEST_OOKLA_TIMEOUT"} {
		t.Setenv(key, "")
	}
	c, err := ConfigFromEnv()
	if err != nil || c.Timeout != DefaultTimeout || c.Args != nil {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	t.Setenv("SPEEDTEST_OOKLA_BINARY", "/path with spaces/test")
	t.Setenv("SPEEDTEST_OOKLA_ARGS", `["--custom", "two words", "$(ignored)"]`)
	t.Setenv("SPEEDTEST_OOKLA_TIMEOUT", "3s")
	c, err = ConfigFromEnv()
	if err != nil || c.Binary != "/path with spaces/test" || c.Timeout != 3*time.Second || !reflect.DeepEqual(c.Args, []string{"--custom", "two words", "$(ignored)"}) {
		t.Fatalf("custom: %+v %v", c, err)
	}
	for _, raw := range []string{"bad", "null", `{"a":1}`, `[1]`, `[null]`} {
		t.Setenv("SPEEDTEST_OOKLA_ARGS", raw)
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatal("accepted invalid arguments")
		}
	}
	t.Setenv("SPEEDTEST_OOKLA_ARGS", "[]")
	for _, raw := range []string{"bad", "0s", "-1s"} {
		t.Setenv("SPEEDTEST_OOKLA_TIMEOUT", raw)
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatal("accepted invalid timeout")
		}
	}
}
