package config

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
)

var allEnvVars = []string{
	"APP_ENV",
	"SPEEDTEST_HTTP_ADDR",
	"SPEEDTEST_UI_DIR",
	"SPEEDTEST_SESSION_TTL",
	"SPEEDTEST_LOGIN_MAX_ATTEMPTS",
	"SPEEDTEST_LOGIN_WINDOW",
	"SPEEDTEST_TRUSTED_PROXY_CIDRS",
	"SPEEDTEST_DB_HOST",
	"SPEEDTEST_DB_PORT",
	"SPEEDTEST_DB_NAME",
	"SPEEDTEST_DB_USER",
	"SPEEDTEST_DB_PASSWORD",
	"SPEEDTEST_DB_SSLMODE",
	"APP_KEY",
	"SPEEDTEST_AGENT_REQUESTS_PER_MINUTE",
	"SPEEDTEST_PUBLIC_URL",
	"SPEEDTEST_RELEASE_DIR",
}

// resetEnv clears every known variable so the documented defaults apply.
func resetEnv(t *testing.T) {
	t.Helper()
	for _, key := range allEnvVars {
		t.Setenv(key, "")
	}
}

func TestLoadCenterDefaults(t *testing.T) {
	resetEnv(t)

	cfg, err := LoadCenter()
	if err != nil {
		t.Fatalf("LoadCenter() error = %v, want nil", err)
	}

	want := CenterConfig{
		HTTPAddr:          "127.0.0.1:8080",
		AppEnv:            "local",
		UIDir:             "",
		SessionTTL:        12 * time.Hour,
		LoginMaxAttempts:  5,
		LoginWindow:       15 * time.Minute,
		TrustedProxyCIDRs: "",
		Database: DatabaseConfig{
			Host:     "127.0.0.1",
			Port:     5432,
			Name:     "speedtest_test",
			User:     "speedtest",
			Password: "",
			SSLMode:  "disable",
		},
		AgentRequestsPerMinute: 120,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("LoadCenter() = %+v, want %+v", cfg, want)
	}
}

func TestLoadCenterOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv("SPEEDTEST_HTTP_ADDR", "0.0.0.0:9000")
	t.Setenv("APP_ENV", "test")
	t.Setenv("SPEEDTEST_UI_DIR", "./ui")
	t.Setenv("SPEEDTEST_SESSION_TTL", "6h")
	t.Setenv("SPEEDTEST_LOGIN_MAX_ATTEMPTS", "8")
	t.Setenv("SPEEDTEST_LOGIN_WINDOW", "10m")
	t.Setenv("SPEEDTEST_TRUSTED_PROXY_CIDRS", "127.0.0.1/32,10.0.0.0/8")
	t.Setenv("SPEEDTEST_DB_HOST", "db.internal")
	t.Setenv("SPEEDTEST_DB_PORT", "6543")
	t.Setenv("SPEEDTEST_DB_NAME", "speedtest_test")
	t.Setenv("SPEEDTEST_DB_USER", "speedtest_app")
	t.Setenv("SPEEDTEST_DB_PASSWORD", "s3cret")
	t.Setenv("SPEEDTEST_DB_SSLMODE", "require")
	t.Setenv("SPEEDTEST_AGENT_REQUESTS_PER_MINUTE", "240")
	encodedKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	t.Setenv("APP_KEY", "base64:"+encodedKey)

	cfg, err := LoadCenter()
	if err != nil {
		t.Fatalf("LoadCenter() error = %v, want nil", err)
	}

	want := CenterConfig{
		HTTPAddr:          "0.0.0.0:9000",
		AppEnv:            "test",
		UIDir:             "./ui",
		SessionTTL:        6 * time.Hour,
		LoginMaxAttempts:  8,
		LoginWindow:       10 * time.Minute,
		TrustedProxyCIDRs: "127.0.0.1/32,10.0.0.0/8",
		Database: DatabaseConfig{
			Host:     "db.internal",
			Port:     6543,
			Name:     "speedtest_test",
			User:     "speedtest_app",
			Password: "s3cret",
			SSLMode:  "require",
		},
		AppKey:                 []byte(strings.Repeat("k", 32)),
		AgentRequestsPerMinute: 240,
	}
	if !reflect.DeepEqual(cfg, want) {
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
			name:    "public URL without release directory",
			env:     map[string]string{"SPEEDTEST_PUBLIC_URL": "https://speedtest.example.com"},
			wantErr: "SPEEDTEST_PUBLIC_URL and SPEEDTEST_RELEASE_DIR must be set together",
		},
		{
			name:    "release directory without public URL",
			env:     map[string]string{"SPEEDTEST_RELEASE_DIR": "/opt/speedtest/dist"},
			wantErr: "SPEEDTEST_PUBLIC_URL and SPEEDTEST_RELEASE_DIR must be set together",
		},
		{
			name:    "agent rate limit invalid",
			env:     map[string]string{"SPEEDTEST_AGENT_REQUESTS_PER_MINUTE": "0"},
			wantErr: "SPEEDTEST_AGENT_REQUESTS_PER_MINUTE must be between",
		},
		{
			name:    "app key invalid",
			env:     map[string]string{"APP_KEY": "not-base64"},
			wantErr: "APP_KEY must be base64 encoding of exactly 32 bytes",
		},
		{
			name:    "unknown app environment",
			env:     map[string]string{"APP_ENV": "preview"},
			wantErr: "APP_ENV must be local, test, staging or production",
		},
		{
			name:    "invalid session duration",
			env:     map[string]string{"SPEEDTEST_SESSION_TTL": "tomorrow"},
			wantErr: "SPEEDTEST_SESSION_TTL must be a Go duration",
		},
		{
			name:    "session duration too long",
			env:     map[string]string{"SPEEDTEST_SESSION_TTL": "744h"},
			wantErr: "SPEEDTEST_SESSION_TTL must be positive and at most 720h",
		},
		{
			name:    "login limit too small",
			env:     map[string]string{"SPEEDTEST_LOGIN_MAX_ATTEMPTS": "0"},
			wantErr: "SPEEDTEST_LOGIN_MAX_ATTEMPTS must be between 1 and 100",
		},
		{
			name:    "invalid login window",
			env:     map[string]string{"SPEEDTEST_LOGIN_WINDOW": "0s"},
			wantErr: "SPEEDTEST_LOGIN_WINDOW must be positive and at most 24h",
		},
		{
			name:    "invalid trusted proxy CIDR",
			env:     map[string]string{"SPEEDTEST_TRUSTED_PROXY_CIDRS": "not-an-ip"},
			wantErr: "SPEEDTEST_TRUSTED_PROXY_CIDRS must be a comma-separated list of CIDRs",
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

func TestLoadCenterInstallerSettings(t *testing.T) {
	resetEnv(t)
	t.Setenv("SPEEDTEST_PUBLIC_URL", " https://speedtest.example.com ")
	t.Setenv("SPEEDTEST_RELEASE_DIR", " /opt/speedtest/dist ")
	cfg, err := LoadCenter()
	if err != nil {
		t.Fatalf("LoadCenter() error = %v", err)
	}
	if cfg.PublicURL != "https://speedtest.example.com" || cfg.ReleaseDir != "/opt/speedtest/dist" {
		t.Fatalf("installer settings = %q, %q", cfg.PublicURL, cfg.ReleaseDir)
	}
}
