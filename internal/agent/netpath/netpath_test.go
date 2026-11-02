package netpath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

func TestBindingValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding Binding
		wantErr string
	}{
		{name: "default route"},
		{name: "interface", binding: Binding{Interface: "eth0"}},
		{name: "source IPv4", binding: Binding{SourceIP: "192.0.2.10"}},
		{name: "source IPv6", binding: Binding{SourceIP: "2001:db8::10"}},
		{name: "trims whitespace", binding: Binding{Interface: " eth0 "}},
		{name: "both selectors", binding: Binding{Interface: "eth0", SourceIP: "192.0.2.10"}, wantErr: "set only one"},
		{name: "invalid source", binding: Binding{SourceIP: "router"}, wantErr: "must be an IPv4 or IPv6 literal"},
		{name: "long interface", binding: Binding{Interface: "interface-name-is-too-long"}, wantErr: "15-character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.binding.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	up := net.Interface{Name: "eth0", Flags: net.FlagUp}
	down := net.Interface{Name: "eth1"}
	addresses := map[string][]net.Addr{
		"eth0": {&net.IPNet{IP: net.ParseIP("192.0.2.10"), Mask: net.CIDRMask(24, 32)}},
		"eth1": {&net.IPNet{IP: net.ParseIP("192.0.2.11"), Mask: net.CIDRMask(24, 32)}},
		"eth2": {},
		"eth3": {stringAddr("192.0.2.12/24"), stringAddr("invalid")},
	}
	addressErr := map[string]error{"eth4": errors.New("address query failed")}
	catalog := fakeCatalog{
		allInterfaces: []net.Interface{{Name: "eth4", Flags: net.FlagUp}, up, down, {Name: "eth2", Flags: net.FlagUp}, {Name: "eth3", Flags: net.FlagUp}},
		byNameMap:     map[string]net.Interface{"eth0": up, "eth1": down, "eth2": {Name: "eth2", Flags: net.FlagUp}, "eth3": {Name: "eth3", Flags: net.FlagUp}, "eth4": {Name: "eth4", Flags: net.FlagUp}},
		addressMap:    addresses,
		addressErr:    addressErr,
	}
	for _, tc := range []struct {
		name      string
		binding   Binding
		wantPath  string
		wantError string
	}{
		{name: "default route", wantPath: "default route"},
		{name: "interface", binding: Binding{Interface: "eth0"}, wantPath: "interface eth0"},
		{name: "missing interface", binding: Binding{Interface: "missing0"}, wantError: "does not exist"},
		{name: "interface has no address", binding: Binding{Interface: "eth2"}, wantError: "has no IP address"},
		{name: "interface is down", binding: Binding{Interface: "eth1"}, wantError: "is down"},
		{name: "interface address query error", binding: Binding{Interface: "eth4"}, wantError: "read addresses"},
		{name: "assigned source IP", binding: Binding{SourceIP: "192.0.2.10"}, wantPath: "source IP 192.0.2.10"},
		{name: "CIDR source address", binding: Binding{SourceIP: "192.0.2.12"}, wantPath: "source IP 192.0.2.12"},
		{name: "unassigned source IP", binding: Binding{SourceIP: "192.0.2.99"}, wantError: "not assigned to an up local interface"},
		{name: "source IP belongs to down interface", binding: Binding{SourceIP: "192.0.2.11"}, wantError: "assigned to down interface"},
		{name: "invalid selector combination", binding: Binding{Interface: "eth0", SourceIP: "192.0.2.10"}, wantError: "set only one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := resolve(tc.binding, catalog)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("resolve() = %+v, %v; want error containing %q", path, err, tc.wantError)
				}
				return
			}
			if err != nil || path.Description() != tc.wantPath {
				t.Fatalf("resolve() = %+v, %v; want path %q", path, err, tc.wantPath)
			}
		})
	}
	if _, err := resolve(Binding{SourceIP: "192.0.2.10"}, fakeCatalog{allErr: errors.New("enumeration failed")}); err == nil || !strings.Contains(err.Error(), "list network interfaces") {
		t.Fatalf("resolve() error = %v, want interface enumeration error", err)
	}
}

func TestAddressIPHandlesNilAndAddressRepresentations(t *testing.T) {
	want := net.ParseIP("192.0.2.10")
	if addressIP(nil) != nil {
		t.Fatal("addressIP(nil) should be nil")
	}
	if got := addressIP(&net.IPAddr{IP: want}); !got.Equal(want) {
		t.Fatalf("addressIP(IPAddr) = %v, want %v", got, want)
	}
	if got := addressIP(stringAddr("192.0.2.10/24")); !got.Equal(want) {
		t.Fatalf("addressIP(CIDR string) = %v, want %v", got, want)
	}
	if got := addressIP(stringAddr("invalid")); got != nil {
		t.Fatalf("addressIP(invalid string) = %v, want nil", got)
	}
}

func TestRunnerUsesSameSourceForEngineAndCenterEcho(t *testing.T) {
	var seenPath, seenAuth, seenRemoteIP string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenAuth = r.URL.Path, r.Header.Get("Authorization")
		seenRemoteIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		writeEcho(w, http.StatusOK, "198.51.100.25")
	}))
	t.Cleanup(server.Close)

	measurement := &recordingEngine{result: engine.Result{ExternalIP: "203.0.113.10"}}
	runner := newTestRunner(t, measurement, server.URL, Binding{SourceIP: "127.0.0.1"})
	result, err := runner.Run(context.Background(), engine.Options{ServerID: 17, InterfaceName: "eth0", SourceIP: "192.0.2.50"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if seenPath != "/api/agent/ip" || seenAuth != "Bearer node-test-token" {
		t.Fatalf("center request path/auth = %q/%q", seenPath, seenAuth)
	}
	if seenRemoteIP != "127.0.0.1" {
		t.Fatalf("center saw source %q, want 127.0.0.1", seenRemoteIP)
	}
	if measurement.options.InterfaceName != "" || measurement.options.SourceIP != "127.0.0.1" || measurement.options.ServerID != 17 {
		t.Fatalf("engine options = %+v", measurement.options)
	}
	if result.PublicIP != "198.51.100.25" || result.Measurement.ExternalIP != "203.0.113.10" {
		t.Fatalf("result = %+v; want center IP and preserved engine IP", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "external IP mismatch") {
		t.Fatalf("warnings = %q, want mismatch warning", result.Warnings)
	}
	encoded, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"warnings":[`) {
		t.Fatalf("RunResult JSON = %s, %v; want a warnings field", encoded, err)
	}
}

func TestRunnerInterfaceBindingAndMatchingIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			t.Errorf("SplitHostPort(%q): %v", r.RemoteAddr, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeEcho(w, http.StatusOK, remoteIP)
	}))
	t.Cleanup(server.Close)
	measurement := &recordingEngine{result: engine.Result{ExternalIP: "127.0.0.1"}}
	runner := newTestRunner(t, measurement, server.URL, Binding{Interface: "lo"})
	result, err := runner.Run(context.Background(), engine.Options{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if measurement.options.InterfaceName != "lo" || measurement.options.SourceIP != "" {
		t.Fatalf("engine options = %+v, want interface lo", measurement.options)
	}
	if result.PublicIP != "127.0.0.1" || len(result.Warnings) != 0 {
		t.Fatalf("result = %+v, want matching IP without warnings", result)
	}
}

func TestRunnerRejectsMissingNetworkPathBeforeEngine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	measurement := &recordingEngine{}
	runner := newTestRunner(t, measurement, server.URL, Binding{Interface: "missing0"})
	_, err := runner.Run(context.Background(), engine.Options{})
	if err == nil || !strings.Contains(err.Error(), `network_interface "missing0" does not exist`) {
		t.Fatalf("Run() error = %v, want clear missing interface error", err)
	}
	if measurement.called {
		t.Fatal("engine ran with a missing interface")
	}
}

func TestRunnerEchoFailuresPreserveMeasurementAsWarnings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{name: "status", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, want: "public IP detection failed"},
		{name: "redirect", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, httptest.NewRequest(http.MethodGet, "/", nil), "/elsewhere", http.StatusFound)
		}, want: "public IP detection failed"},
		{name: "malformed JSON", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }, want: "public IP detection failed"},
		{name: "bad envelope", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"success":false,"code":200,"data":{"ip":"198.51.100.25"}}`)
		}, want: "public IP detection failed"},
		{name: "invalid IP", handler: func(w http.ResponseWriter, _ *http.Request) { writeEcho(w, http.StatusOK, "center") }, want: "public IP detection failed"},
		{name: "oversized body", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", echoMaxBody+1)))
		}, want: "public IP detection failed"},
		{name: "trailing data", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, successBody("198.51.100.25")+" extra")
		}, want: "public IP detection failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			t.Cleanup(server.Close)
			measurement := &recordingEngine{result: engine.Result{ExternalIP: "203.0.113.10"}}
			result, err := newTestRunner(t, measurement, server.URL, Binding{}).Run(context.Background(), engine.Options{})
			if err != nil || result.Measurement.ExternalIP != "203.0.113.10" {
				t.Fatalf("Run() = %+v, %v; want measurement preserved", result, err)
			}
			if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], tc.want) {
				t.Fatalf("warnings = %q, want %q", result.Warnings, tc.want)
			}
		})
	}
}

func TestRunnerDoesNotEchoAfterEngineFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("center IP endpoint called after failed measurement")
	}))
	t.Cleanup(server.Close)
	measurement := &recordingEngine{err: errors.New("engine failed")}
	result, err := newTestRunner(t, measurement, server.URL, Binding{}).Run(context.Background(), engine.Options{})
	if err == nil || result.Measurement.Error != "" {
		t.Fatalf("Run() = %+v, %v; want engine error", result, err)
	}
}

func TestRunnerSkipsMismatchWhenEngineIPIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEcho(w, http.StatusOK, "198.51.100.25")
	}))
	t.Cleanup(server.Close)
	measurement := &recordingEngine{}
	result, err := newTestRunner(t, measurement, server.URL, Binding{}).Run(context.Background(), engine.Options{InterfaceName: "lo", SourceIP: "127.0.0.1"})
	if err != nil || result.PublicIP != "198.51.100.25" || len(result.Warnings) != 0 {
		t.Fatalf("Run() = %+v, %v; want center IP without mismatch warning", result, err)
	}
	if measurement.options.InterfaceName != "" || measurement.options.SourceIP != "" {
		t.Fatalf("default route options = %+v, want no binding", measurement.options)
	}
}

func TestRunnerWarnsWhenEngineIPIsInvalid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEcho(w, http.StatusOK, "198.51.100.25")
	}))
	t.Cleanup(server.Close)
	measurement := &recordingEngine{result: engine.Result{ExternalIP: "not-an-ip"}}
	result, err := newTestRunner(t, measurement, server.URL, Binding{}).Run(context.Background(), engine.Options{})
	if err != nil || result.PublicIP != "198.51.100.25" {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "engine reported an invalid external IP") {
		t.Fatalf("warnings = %q, want invalid engine IP warning", result.Warnings)
	}
}

func TestCenterEchoRequestAndBodyErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		url   string
		serve http.HandlerFunc
	}{
		{name: "connection refused", url: "http://127.0.0.1:1"},
		{name: "invalid endpoint URL", url: "http://127.0.0.1/path with spaces"},
		{name: "read failure", serve: func(w http.ResponseWriter, r *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, buffer, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_, _ = fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Length: 40\r\nConnection: close\r\n\r\n{}")
			_ = buffer.Flush()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			centerURL := tc.url
			if tc.serve != nil {
				server := httptest.NewServer(tc.serve)
				t.Cleanup(server.Close)
				centerURL = server.URL
			}
			runner, err := NewRunner(&recordingEngine{}, centerURL, "token", Binding{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runner.centerPublicIP(context.Background(), Path{})
			if err == nil {
				t.Fatal("centerPublicIP() returned nil error")
			}
		})
	}
}

func TestCenterEchoRejectsInvalidRequestURL(t *testing.T) {
	runner := &Runner{centerURL: url.URL{Scheme: "http invalid", Host: "center.example"}, credential: "token"}
	if _, err := runner.centerPublicIP(context.Background(), Path{}); err == nil || !strings.Contains(err.Error(), "create center IP request") {
		t.Fatalf("centerPublicIP() error = %v, want request construction error", err)
	}
}

func TestBoundTransportRejectsUnknownInterface(t *testing.T) {
	transport := newBoundTransport(Path{Interface: "missing0"})
	defer transport.CloseIdleConnections()
	_, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "bind socket to network_interface") {
		t.Fatalf("DialContext() error = %v, want interface bind error", err)
	}
}

func TestLocalAddressChoosesSocketType(t *testing.T) {
	ip := net.ParseIP("127.0.0.1")
	if _, ok := localAddress("tcp", ip).(*net.TCPAddr); !ok {
		t.Fatal("localAddress(tcp) did not return TCP address")
	}
	if _, ok := localAddress("udp", ip).(*net.UDPAddr); !ok {
		t.Fatal("localAddress(udp) did not return UDP address")
	}
}

func TestBoundResolverUsesBoundDialer(t *testing.T) {
	for _, path := range []Path{{}, {SourceIP: net.ParseIP("127.0.0.1")}, {Interface: "lo"}} {
		dialer := configuredDialer(path, &net.Dialer{Timeout: 100 * time.Millisecond})
		resolver := boundResolver(path, dialer)
		conn, err := resolver.Dial(context.Background(), "udp", "127.0.0.1:1")
		if err != nil {
			t.Fatalf("Resolver.Dial(%+v) error = %v", path, err)
		}
		if path.SourceIP != nil {
			localIP, _, splitErr := net.SplitHostPort(conn.LocalAddr().String())
			if splitErr != nil || localIP != path.SourceIP.String() {
				t.Fatalf("Resolver.Dial(%+v) local address = %q, %v", path, conn.LocalAddr(), splitErr)
			}
		}
		_ = conn.Close()
	}
}

func TestBindToDeviceReturnsRawConnAndSocketErrors(t *testing.T) {
	controlErr := errors.New("raw control failed")
	if err := bindToDevice(fakeRawConn{controlErr: controlErr}, "lo"); !errors.Is(err, controlErr) {
		t.Fatalf("bindToDevice() error = %v, want raw control error", err)
	}
	if err := bindToDevice(fakeRawConn{invoke: true}, "missing0"); err == nil || !strings.Contains(err.Error(), "bind socket to network_interface") {
		t.Fatalf("bindToDevice() error = %v, want setsockopt error", err)
	}
}

func TestNewRunnerValidation(t *testing.T) {
	engineStub := &recordingEngine{}
	for _, tc := range []struct {
		name, center, credential string
		measurement              engine.Engine
		binding                  Binding
		want                     string
	}{
		{name: "valid", center: "https://center.example", credential: "token", measurement: engineStub},
		{name: "nil engine", center: "https://center.example", credential: "token", want: "engine is required"},
		{name: "bad center URL", center: "file:///center", credential: "token", measurement: engineStub, want: "absolute http or https"},
		{name: "missing center host", center: "https:", credential: "token", measurement: engineStub, want: "absolute http or https"},
		{name: "center user info", center: "https://user:pass@center.example", credential: "token", measurement: engineStub, want: "without user info"},
		{name: "missing credential", center: "https://center.example", measurement: engineStub, want: "credential is required"},
		{name: "invalid binding", center: "https://center.example", credential: "token", measurement: engineStub, binding: Binding{Interface: "eth0", SourceIP: "192.0.2.10"}, want: "network binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRunner(tc.measurement, tc.center, tc.credential, tc.binding)
			if tc.want == "" && err != nil {
				t.Fatalf("NewRunner() error = %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("NewRunner() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func newTestRunner(t *testing.T, measurement engine.Engine, center string, binding Binding) *Runner {
	t.Helper()
	runner, err := NewRunner(measurement, center, "node-test-token", binding)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func writeEcho(w http.ResponseWriter, status int, ip string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, successBody(ip))
}

func successBody(ip string) string {
	return fmt.Sprintf(`{"success":true,"code":200,"message":"OK","data":{"ip":%q},"meta":{},"errors":null}`, ip)
}

type recordingEngine struct {
	result  engine.Result
	err     error
	options engine.Options
	called  bool
}

func (e *recordingEngine) Run(_ context.Context, options engine.Options) (engine.Result, error) {
	e.called = true
	e.options = options
	return e.result, e.err
}

type fakeCatalog struct {
	allInterfaces []net.Interface
	byNameMap     map[string]net.Interface
	addressMap    map[string][]net.Addr
	addressErr    map[string]error
	allErr        error
}

func (f fakeCatalog) byName(name string) (net.Interface, error) {
	iface, ok := f.byNameMap[name]
	if !ok {
		return net.Interface{}, errors.New("not found")
	}
	return iface, nil
}

func (f fakeCatalog) all() ([]net.Interface, error) {
	return f.allInterfaces, f.allErr
}

func (f fakeCatalog) addresses(iface net.Interface) ([]net.Addr, error) {
	return f.addressMap[iface.Name], f.addressErr[iface.Name]
}

type stringAddr string

func (a stringAddr) Network() string { return "test" }
func (a stringAddr) String() string  { return string(a) }

type fakeRawConn struct {
	controlErr error
	invoke     bool
}

func (f fakeRawConn) Control(fn func(uintptr)) error {
	if f.controlErr != nil {
		return f.controlErr
	}
	if f.invoke {
		fn(0)
	}
	return nil
}

func (fakeRawConn) Read(func(uintptr) bool) error  { return nil }
func (fakeRawConn) Write(func(uintptr) bool) error { return nil }
func (fakeRawConn) Close() error                   { return nil }
