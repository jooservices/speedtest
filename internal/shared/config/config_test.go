package config

import (
	"strings"
	"testing"
	"time"
)

var allEnvVars = []string{
	"SPEEDTEST_NODE_NAME",
	"SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS",
	"SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS",
	"SPEEDTEST_NODE_INTERVAL",
	"SPEEDTEST_CENTER_URL",
	"SPEEDTEST_HTTP_ADDR",
	"SPEEDTEST_UI_DIR",
	"SPEEDTEST_DB_HOST",
	"SPEEDTEST_DB_PORT",
	"SPEEDTEST_DB_NAME",
	"SPEEDTEST_DB_USER",
	"SPEEDTEST_DB_PASSWORD",
	"SPEEDTEST_DB_SSLMODE",
}

// resetEnv clears every known variable so the documented defaults apply.
func resetEnv(t *testing.T) {
	t.Helper()
	for _, key := range allEnvVars {
		t.Setenv(key, "")
	}
}

func TestLoadAgentDefaults(t *testing.T) {
	resetEnv(t)

	cfg, err := LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent() error = %v, want nil", err)
	}

	want := AgentConfig{
		Name:                 "node",
		ExpectedDownloadMbps: 0,
		ExpectedUploadMbps:   0,
		Interval:             30 * time.Minute,
		CenterURL:            "http://127.0.0.1:8080",
	}
	if cfg != want {
		t.Fatalf("LoadAgent() = %+v, want %+v", cfg, want)
	}
}

func TestLoadAgentOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv("SPEEDTEST_NODE_NAME", "  viettel-fast-50  ")
	t.Setenv("SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS", "300")
	t.Setenv("SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS", "150")
	t.Setenv("SPEEDTEST_NODE_INTERVAL", "45m")
	t.Setenv("SPEEDTEST_CENTER_URL", "https://center.example.com")

	cfg, err := LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent() error = %v, want nil", err)
	}

	want := AgentConfig{
		Name:                 "viettel-fast-50",
		ExpectedDownloadMbps: 300,
		ExpectedUploadMbps:   150,
		Interval:             45 * time.Minute,
		CenterURL:            "https://center.example.com",
	}
	if cfg != want {
		t.Fatalf("LoadAgent() = %+v, want %+v", cfg, want)
	}
}

func TestLoadAgentInvalid(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "download not an integer",
			env:     map[string]string{"SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS": "fast"},
			wantErr: "SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS",
		},
		{
			name:    "download negative",
			env:     map[string]string{"SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS": "-1"},
			wantErr: "SPEEDTEST_NODE_EXPECTED_DOWNLOAD_MBPS must be >= 0",
		},
		{
			name:    "upload not an integer",
			env:     map[string]string{"SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS": "-1.5"},
			wantErr: "SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS",
		},
		{
			name:    "upload negative",
			env:     map[string]string{"SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS": "-1"},
			wantErr: "SPEEDTEST_NODE_EXPECTED_UPLOAD_MBPS must be >= 0",
		},
		{
			name:    "interval not a duration",
			env:     map[string]string{"SPEEDTEST_NODE_INTERVAL": "often"},
			wantErr: "SPEEDTEST_NODE_INTERVAL",
		},
		{
			name:    "interval zero",
			env:     map[string]string{"SPEEDTEST_NODE_INTERVAL": "0s"},
			wantErr: "SPEEDTEST_NODE_INTERVAL must be greater than 0",
		},
		{
			name:    "interval negative",
			env:     map[string]string{"SPEEDTEST_NODE_INTERVAL": "-5m"},
			wantErr: "SPEEDTEST_NODE_INTERVAL must be greater than 0",
		},
		{
			name:    "name blank",
			env:     map[string]string{"SPEEDTEST_NODE_NAME": "   "},
			wantErr: "SPEEDTEST_NODE_NAME must not be blank",
		},
		{
			name:    "center url without scheme",
			env:     map[string]string{"SPEEDTEST_CENTER_URL": "center.example.com"},
			wantErr: "SPEEDTEST_CENTER_URL must be an absolute http or https URL",
		},
		{
			name:    "center url relative path",
			env:     map[string]string{"SPEEDTEST_CENTER_URL": "/results"},
			wantErr: "SPEEDTEST_CENTER_URL must be an absolute http or https URL",
		},
		{
			name:    "center url wrong scheme",
			env:     map[string]string{"SPEEDTEST_CENTER_URL": "ftp://center.example.com"},
			wantErr: "SPEEDTEST_CENTER_URL must be an absolute http or https URL",
		},
		{
			name:    "center url missing host",
			env:     map[string]string{"SPEEDTEST_CENTER_URL": "http://"},
			wantErr: "SPEEDTEST_CENTER_URL must be an absolute http or https URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := LoadAgent()
			if err == nil {
				t.Fatalf("LoadAgent() error = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadAgent() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadCenterDefaults(t *testing.T) {
	resetEnv(t)

	cfg, err := LoadCenter()
	if err != nil {
		t.Fatalf("LoadCenter() error = %v, want nil", err)
	}

	want := CenterConfig{
		HTTPAddr: "127.0.0.1:8080",
		UIDir:    "",
		Database: DatabaseConfig{
			Host:     "127.0.0.1",
			Port:     5432,
			Name:     "speedtest",
			User:     "speedtest",
			Password: "",
			SSLMode:  "disable",
		},
	}
	if cfg != want {
		t.Fatalf("LoadCenter() = %+v, want %+v", cfg, want)
	}
}

func TestLoadCenterOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv("SPEEDTEST_HTTP_ADDR", "0.0.0.0:9000")
	t.Setenv("SPEEDTEST_UI_DIR", "./ui")
	t.Setenv("SPEEDTEST_DB_HOST", "db.internal")
	t.Setenv("SPEEDTEST_DB_PORT", "6543")
	t.Setenv("SPEEDTEST_DB_NAME", "speedtest_test")
	t.Setenv("SPEEDTEST_DB_USER", "speedtest_app")
	t.Setenv("SPEEDTEST_DB_PASSWORD", "s3cret")
	t.Setenv("SPEEDTEST_DB_SSLMODE", "require")

	cfg, err := LoadCenter()
	if err != nil {
		t.Fatalf("LoadCenter() error = %v, want nil", err)
	}

	want := CenterConfig{
		HTTPAddr: "0.0.0.0:9000",
		UIDir:    "./ui",
		Database: DatabaseConfig{
			Host:     "db.internal",
			Port:     6543,
			Name:     "speedtest_test",
			User:     "speedtest_app",
			Password: "s3cret",
			SSLMode:  "require",
		},
	}
	if cfg != want {
		t.Fatalf("LoadCenter() = %+v, want %+v", cfg, want)
	}
}

func TestLoadCenterInvalid(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "port not an integer",
			env:     map[string]string{"SPEEDTEST_DB_PORT": "not-a-port"},
			wantErr: "SPEEDTEST_DB_PORT",
		},
		{
			name:    "port zero",
			env:     map[string]string{"SPEEDTEST_DB_PORT": "0"},
			wantErr: "SPEEDTEST_DB_PORT must be between 1 and 65535",
		},
		{
			name:    "port too large",
			env:     map[string]string{"SPEEDTEST_DB_PORT": "65536"},
			wantErr: "SPEEDTEST_DB_PORT must be between 1 and 65535",
		},
		{
			name:    "port negative",
			env:     map[string]string{"SPEEDTEST_DB_PORT": "-1"},
			wantErr: "SPEEDTEST_DB_PORT must be between 1 and 65535",
		},
		{
			name:    "sslmode unknown",
			env:     map[string]string{"SPEEDTEST_DB_SSLMODE": "enabled"},
			wantErr: "SPEEDTEST_DB_SSLMODE must be one of disable, allow, prefer, require, verify-ca, verify-full",
		},
		{
			name:    "http addr missing port",
			env:     map[string]string{"SPEEDTEST_HTTP_ADDR": "localhost"},
			wantErr: "SPEEDTEST_HTTP_ADDR must be a valid host:port address",
		},
		{
			name:    "http addr port not numeric",
			env:     map[string]string{"SPEEDTEST_HTTP_ADDR": ":notaport"},
			wantErr: "SPEEDTEST_HTTP_ADDR must have a numeric port between 1 and 65535",
		},
		{
			name:    "http addr port zero",
			env:     map[string]string{"SPEEDTEST_HTTP_ADDR": ":0"},
			wantErr: "SPEEDTEST_HTTP_ADDR must have a numeric port between 1 and 65535",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := LoadCenter()
			if err == nil {
				t.Fatalf("LoadCenter() error = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadCenter() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadCenterValidSSLModes(t *testing.T) {
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			resetEnv(t)
			t.Setenv("SPEEDTEST_DB_SSLMODE", mode)

			cfg, err := LoadCenter()
			if err != nil {
				t.Fatalf("LoadCenter() error = %v, want nil", err)
			}
			if cfg.Database.SSLMode != mode {
				t.Fatalf("LoadCenter() SSLMode = %q, want %q", cfg.Database.SSLMode, mode)
			}
		})
	}
}

func TestLoadCenterValidHTTPAddrs(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:9000", "127.0.0.1:8080", "[::1]:8080"} {
		t.Run(addr, func(t *testing.T) {
			resetEnv(t)
			t.Setenv("SPEEDTEST_HTTP_ADDR", addr)

			cfg, err := LoadCenter()
			if err != nil {
				t.Fatalf("LoadCenter() error = %v, want nil", err)
			}
			if cfg.HTTPAddr != addr {
				t.Fatalf("LoadCenter() HTTPAddr = %q, want %q", cfg.HTTPAddr, addr)
			}
		})
	}
}

func TestDatabaseConfigDSN(t *testing.T) {
	db := DatabaseConfig{
		Host:     "127.0.0.1",
		Port:     5432,
		Name:     "speedtest",
		User:     "speedtest",
		Password: "p@ss:word",
		SSLMode:  "disable",
	}

	got := db.DSN()
	want := "postgres://speedtest:p%40ss%3Aword@127.0.0.1:5432/speedtest?sslmode=disable"
	if got != want {
		t.Fatalf("DSN() = %q, want %q", got, want)
	}
}

func TestDatabaseConfigDSNEmptyPassword(t *testing.T) {
	db := DatabaseConfig{
		Host:    "127.0.0.1",
		Port:    5432,
		Name:    "speedtest",
		User:    "speedtest",
		SSLMode: "disable",
	}

	got := db.DSN()
	want := "postgres://speedtest:@127.0.0.1:5432/speedtest?sslmode=disable"
	if got != want {
		t.Fatalf("DSN() = %q, want %q", got, want)
	}
}
