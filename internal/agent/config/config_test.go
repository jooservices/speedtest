package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/agent/engine"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadDefaultsNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{
		Engine:               "ookla",
		Name:                 "node",
		ExpectedDownloadMbps: 0,
		ExpectedUploadMbps:   0,
		Interval:             60 * time.Minute,
	}
	if cfg != want {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "config.yaml", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Name != "node" || cfg.Interval != 60*time.Minute {
		t.Fatalf("Load() = %+v, want defaults", cfg)
	}
}

func TestLoadFromFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "config.yaml", ""+
		"name: viettel-fast-50\n"+
		"engine: librespeed\n"+
		"parallel_connections: 8\n"+
		"expected_download_mbps: 300\n"+
		"expected_upload_mbps: 150\n"+
		"interval: 45m\n"+
		"network_interface: enp3s0\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	want := Config{
		Engine:               "librespeed",
		Name:                 "viettel-fast-50",
		ExpectedDownloadMbps: 300,
		ExpectedUploadMbps:   150,
		Interval:             45 * time.Minute,
		ParallelConnections:  8,
		NetworkInterface:     "enp3s0",
	}
	if cfg != want {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	path := writeFile(t, t.TempDir(), "config.yaml", "name: only-name\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Name != "only-name" {
		t.Fatalf("Name = %q, want only-name", cfg.Name)
	}
	if cfg.Engine != "ookla" || cfg.ExpectedDownloadMbps != 0 || cfg.ExpectedUploadMbps != 0 || cfg.Interval != 60*time.Minute {
		t.Fatalf("Load() = %+v, want file name with default speeds/interval", cfg)
	}
}

func TestLoadExplicitFiveAndThirtyMinuteIntervals(t *testing.T) {
	for _, interval := range []time.Duration{5 * time.Minute, 30 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "config.yaml", "interval: "+interval.String()+"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if cfg.Interval != interval {
				t.Fatalf("interval = %s, want explicit %s", cfg.Interval, interval)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{
			name:    "malformed yaml",
			file:    "name: [unclosed",
			wantErr: "parse config",
		},
		{
			name:    "unknown key",
			file:    "nam: typo\n",
			wantErr: "parse config",
		},
		{
			name:    "unknown engine",
			file:    "engine: speedtest-cli\n",
			wantErr: "engine must be ookla, librespeed or speedtestcli",
		},
		{
			name:    "download not an integer",
			file:    "expected_download_mbps: fast\n",
			wantErr: "parse config",
		},
		{
			name:    "interval not a duration",
			file:    "interval: often\n",
			wantErr: "interval must be a duration",
		},
		{
			name:    "file download negative",
			file:    "expected_download_mbps: -1\n",
			wantErr: "expected_download_mbps must be >= 0",
		},
		{
			name:    "file name blank",
			file:    "name: '   '\n",
			wantErr: "name must not be blank",
		},
		{
			name:    "file interval zero",
			file:    "interval: 0s\n",
			wantErr: "interval must be greater than 0",
		},
		{
			name:    "both network selectors",
			file:    "network_interface: eth0\nsource_ip: 192.0.2.10\n",
			wantErr: "set only one of network_interface or source_ip",
		},
		{
			name:    "invalid source IP",
			file:    "source_ip: not-an-ip\n",
			wantErr: "source_ip \"not-an-ip\" must be an IPv4 or IPv6 literal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.yaml")
			if tt.file != "" {
				path = writeFile(t, t.TempDir(), "config.yaml", tt.file)
			}

			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load() error = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReadError(t *testing.T) {
	dir := t.TempDir()

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() error = nil, want a read error for a directory path")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load() error = %v, want it to contain %q", err, "read config")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "ok", cfg: Config{Name: "n", Engine: "ookla", Interval: time.Minute}},
		{name: "blank name", cfg: Config{Engine: "ookla", Interval: time.Minute}, wantErr: "name must not be blank"},
		{name: "negative download", cfg: Config{Name: "n", Engine: "ookla", Interval: time.Minute, ExpectedDownloadMbps: -1}, wantErr: "expected_download_mbps must be >= 0"},
		{name: "negative upload", cfg: Config{Name: "n", Engine: "ookla", Interval: time.Minute, ExpectedUploadMbps: -1}, wantErr: "expected_upload_mbps must be >= 0"},
		{name: "zero interval", cfg: Config{Name: "n", Engine: "ookla"}, wantErr: "interval must be greater than 0"},
		{name: "negative interval", cfg: Config{Name: "n", Engine: "ookla", Interval: -time.Minute}, wantErr: "interval must be greater than 0"},
		{name: "negative parallel connections", cfg: Config{Name: "n", Engine: "ookla", Interval: time.Minute, ParallelConnections: -1}, wantErr: "parallel_connections must be greater than 0 when set"},
		{name: "both network selectors", cfg: Config{Name: "n", Engine: "ookla", Interval: time.Minute, NetworkInterface: "eth0", SourceIP: "192.0.2.10"}, wantErr: "set only one of network_interface or source_ip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewEngineUsesNodeSelection(t *testing.T) {
	for _, key := range []string{
		"SPEEDTEST_OOKLA_BINARY", "SPEEDTEST_OOKLA_ARGS", "SPEEDTEST_OOKLA_TIMEOUT",
		"SPEEDTEST_LIBRESPEED_BINARY", "SPEEDTEST_LIBRESPEED_SERVER_JSON",
		"SPEEDTEST_LIBRESPEED_CONCURRENT", "SPEEDTEST_LIBRESPEED_TIMEOUT",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("SPEEDTEST_LIBRESPEED_CONCURRENT", "3")

	ookla, err := (Config{Name: "n", Engine: "ookla", Interval: time.Minute}).NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ookla.(*engine.Ookla); !ok {
		t.Fatalf("NewEngine() = %T, want *engine.Ookla", ookla)
	}
	librespeed, err := (Config{Name: "n", Engine: "librespeed", Interval: time.Minute, ParallelConnections: 6}).NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := librespeed.(*engine.LibreSpeed); !ok {
		t.Fatalf("NewEngine() = %T, want *engine.LibreSpeed", librespeed)
	}
	if _, err := (Config{Name: "n", Engine: "unknown", Interval: time.Minute}).NewEngine(); err == nil {
		t.Fatal("NewEngine accepted an unsupported engine")
	}
}
