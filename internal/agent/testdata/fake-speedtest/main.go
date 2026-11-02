// Command fake-speedtest is a test double for the Ookla speedtest CLI. It
// prints the same JSON shape the real CLI emits with `--format=json`, but the
// values and failure behaviour are configurable so agent tests never depend on
// a real internet speed test.
//
// Scenarios:
//
//	success  a healthy measurement with a fast download/upload
//	slow     a valid but slow measurement
//	loss     a valid measurement with packet loss
//	timeout  hangs until killed, emulating a speed test that never returns
//	crash    exits non-zero with no valid JSON, emulating a crashed engine
//
// Individual values can be overridden with flags. See docs/testing-harness.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// speedtestResult mirrors the JSON produced by the Ookla CLI for
// `--format=json`. Bandwidth is bytes per second; latency and jitter are
// milliseconds; packetLoss is a percent.
type speedtestResult struct {
	Type       string    `json:"type"`
	Timestamp  string    `json:"timestamp"`
	Ping       ping      `json:"ping"`
	Download   transfer  `json:"download"`
	Upload     transfer  `json:"upload"`
	PacketLoss *float64  `json:"packetLoss,omitempty"`
	ISP        string    `json:"isp"`
	Interface  iface     `json:"interface"`
	Server     server    `json:"server"`
	Result     resultRef `json:"result"`
}

type ping struct {
	Jitter  float64 `json:"jitter"`
	Latency float64 `json:"latency"`
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
}

type transfer struct {
	Bandwidth int64   `json:"bandwidth"`
	Bytes     int64   `json:"bytes"`
	Elapsed   int64   `json:"elapsed"`
	Latency   latency `json:"latency"`
}

type latency struct {
	Iqm    float64 `json:"iqm"`
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
	Jitter float64 `json:"jitter"`
}

type iface struct {
	InternalIP string `json:"internalIp"`
	Name       string `json:"name"`
	MACAddr    string `json:"macAddr"`
	IsVPN      bool   `json:"isVpn"`
	ExternalIP string `json:"externalIp"`
}

type server struct {
	ID       int    `json:"id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
	IP       string `json:"ip"`
}

type resultRef struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Persisted bool   `json:"persisted"`
}

// config is the resolved measurement the fake reports.
type config struct {
	scenario      string
	down          float64 // Mbps
	up            float64 // Mbps
	ping          float64 // ms
	jitter        float64 // ms
	loss          float64 // percent, 0-100
	lossAvailable bool
	serverID      int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses args, emulates the requested scenario and returns the process exit
// code: 0 for a successful measurement, 1 when the timeout scenario is
// cancelled, 2 for usage or engine errors.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "speedtest: %v\n", err)
		return 2
	}

	switch cfg.scenario {
	case "timeout":
		<-ctx.Done()
		return 1
	case "crash":
		fmt.Fprintln(stderr, "speedtest: fatal: simulated engine crash")
		return 2
	default:
		if err := json.NewEncoder(stdout).Encode(buildResult(cfg)); err != nil {
			fmt.Fprintf(stderr, "speedtest: write result: %v\n", err)
			return 2
		}
		return 0
	}
}

// parseFlags parses the command line into a resolved config. Explicit flags
// override the scenario defaults.
func parseFlags(args []string, errw io.Writer) (config, error) {
	fs := flag.NewFlagSet("fake-speedtest", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() {
		fmt.Fprintln(errw, "usage: fake-speedtest [flags]")
		fmt.Fprintln(errw, "emulates the Ookla speedtest CLI --format=json output.")
		fs.PrintDefaults()
	}

	scenario := fs.String("scenario", "success", "one of success, slow, loss, timeout, crash")
	down := fs.Float64("down", 0, "download bandwidth in Mbps (overrides scenario default)")
	up := fs.Float64("up", 0, "upload bandwidth in Mbps (overrides scenario default)")
	ping := fs.Float64("ping", 0, "latency in ms (overrides scenario default)")
	jitter := fs.Float64("jitter", 0, "jitter in ms (overrides scenario default)")
	loss := fs.Float64("loss", 0, "packet loss percent 0-100 (overrides scenario default)")
	serverID := fs.Int("server-id", 0, "server id (overrides scenario default)")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg, ok := scenarioDefaults(*scenario)
	if !ok {
		return config{}, fmt.Errorf("unknown scenario %q: must be one of success, slow, loss, timeout, crash", *scenario)
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "down":
			cfg.down = *down
		case "up":
			cfg.up = *up
		case "ping":
			cfg.ping = *ping
		case "jitter":
			cfg.jitter = *jitter
		case "loss":
			cfg.loss = *loss
			cfg.lossAvailable = true
		case "server-id":
			cfg.serverID = *serverID
		}
	})

	return cfg, nil
}

// scenarioDefaults returns the preset measurement for a scenario.
func scenarioDefaults(name string) (config, bool) {
	switch name {
	case "success":
		return config{scenario: "success", down: 300, up: 150, ping: 8, jitter: 1, loss: 0, serverID: 12345}, true
	case "slow":
		return config{scenario: "slow", down: 8, up: 3, ping: 250, jitter: 60, loss: 0, serverID: 12345}, true
	case "loss":
		return config{scenario: "loss", lossAvailable: true, down: 120, up: 100, ping: 45, jitter: 22, loss: 12, serverID: 12345}, true
	case "timeout":
		return config{scenario: "timeout", serverID: 12345}, true
	case "crash":
		return config{scenario: "crash", serverID: 12345}, true
	default:
		return config{}, false
	}
}

// buildResult assembles an Ookla-shaped result from a config.
func buildResult(cfg config) speedtestResult {
	lat := cfg.ping
	down := mbpsToBandwidth(cfg.down)
	up := mbpsToBandwidth(cfg.up)

	var packetLoss *float64
	if cfg.lossAvailable {
		packetLoss = &cfg.loss
	}
	return speedtestResult{
		Type:      "result",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Ping: ping{
			Jitter:  cfg.jitter,
			Latency: lat,
			Low:     lat * 0.9,
			High:    lat * 1.1,
		},
		Download: transfer{
			Bandwidth: down,
			Bytes:     down * 8,
			Elapsed:   8000,
			Latency: latency{
				Iqm:    lat,
				Low:    lat * 0.9,
				High:   lat * 1.1,
				Jitter: cfg.jitter,
			},
		},
		Upload: transfer{
			Bandwidth: up,
			Bytes:     up * 10,
			Elapsed:   10000,
			Latency: latency{
				Iqm:    lat,
				Low:    lat * 0.9,
				High:   lat * 1.1,
				Jitter: cfg.jitter,
			},
		},
		PacketLoss: packetLoss,
		ISP:        "Viettel",
		Interface: iface{
			InternalIP: "192.168.1.100",
			Name:       "eth0",
			MACAddr:    "aa:bb:cc:dd:ee:ff",
			IsVPN:      false,
			ExternalIP: "203.0.113.10",
		},
		Server: server{
			ID:       cfg.serverID,
			Host:     "speedtest.hanoi.example",
			Port:     8080,
			Name:     "Hanoi",
			Location: "Hanoi",
			Country:  "Vietnam",
			IP:       "203.0.113.20",
		},
		Result: resultRef{
			ID:        "00000000-0000-4000-8000-000000000000",
			URL:       "https://www.speedtest.net/result/c/00000000-0000-4000-8000-000000000000",
			Persisted: true,
		},
	}
}

// mbpsToBandwidth converts megabits per second to Ookla bytes per second.
func mbpsToBandwidth(mbps float64) int64 {
	return int64(mbps * 1_000_000 / 8)
}
