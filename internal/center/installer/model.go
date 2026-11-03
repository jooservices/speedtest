// Package installer installs the Speedtest agent over a confirmed SSH host key.
package installer

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid       = errors.New("invalid installer input")
	ErrNotFound      = errors.New("installer resource not found")
	ErrConflict      = errors.New("installer resource conflict")
	ErrBusy          = errors.New("an installation is already running for this node")
	ErrHostKeyChange = errors.New("SSH host key changed")
	ErrAuth          = errors.New("SSH authentication failed")
	ErrSudo          = errors.New("sudo is unavailable")
	ErrUnsupported   = errors.New("unsupported Linux architecture")
	ErrDisconnect    = errors.New("SSH connection was lost")
	ErrCursorExpired = errors.New("installer event cursor expired")
)

const (
	probeLifetime     = 10 * time.Minute
	jobRetention      = 30 * 24 * time.Hour
	installTimeout    = 12 * time.Minute
	heartbeatTimeout  = 2 * time.Minute
	heartbeatInterval = 500 * time.Millisecond
)

var (
	hostLabelPattern = regexp.MustCompile("^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$")
	userPattern      = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_.-]{0,99}$")
)

// ProbeInput is the unauthenticated SSH handshake target.
type ProbeInput struct {
	Host string "json:\"host\""
	Port int    "json:\"port\""
}

// SSHInput is accepted only by the confirmed installation request. Secret and
// SudoPassword are used in memory and are never included in progress objects.
type SSHInput struct {
	Host         string "json:\"host\""
	Port         int    "json:\"port\""
	User         string "json:\"user\""
	Auth         string "json:\"auth\""
	Secret       string "json:\"secret\""
	SudoPassword string "json:\"sudoPassword,omitempty\""
}

// InstallInput binds one installation to a previously observed host key.
type InstallInput struct {
	ProbeID     string   "json:\"probeId\""
	Fingerprint string   "json:\"fingerprint\""
	SSH         SSHInput "json:\"ssh\""
}

// SSHProbe is safe to return to the owner for explicit fingerprint approval.
type SSHProbe struct {
	ID          string    "json:\"id\""
	Fingerprint string    "json:\"fingerprint\""
	ExpiresAt   time.Time "json:\"expiresAt\""
}

// Installation is the public, secret-free state of a remote install.
type Installation struct {
	ID     string  "json:\"id\""
	NodeID string  "json:\"nodeId\""
	Status string  "json:\"status\""
	Step   string  "json:\"step\""
	Error  *string "json:\"error\""
}

// Event is one persisted SSE progress payload.
type Event struct {
	ID      string    "json:\"id\""
	TS      time.Time "json:\"ts\""
	Step    string    "json:\"step\""
	Message string    "json:\"message\""
	Pct     float64   "json:\"pct\""
	Status  string    "json:\"status\""
}

func (in ProbeInput) normalize() (ProbeInput, error) {
	in.Host = strings.TrimSpace(in.Host)
	if !validHost(in.Host) || in.Port < 1 || in.Port > 65535 {
		return ProbeInput{}, ErrInvalid
	}
	return in, nil
}

func (in SSHInput) normalize() (SSHInput, error) {
	probe, err := (ProbeInput{Host: in.Host, Port: in.Port}).normalize()
	if err != nil || !userPattern.MatchString(in.User) || len(in.Secret) == 0 || len(in.Secret) > 32<<10 ||
		len(in.SudoPassword) > 4096 || strings.ContainsAny(in.SudoPassword, "\r\n\x00") {
		return SSHInput{}, ErrInvalid
	}
	in.Host = probe.Host
	switch in.Auth {
	case "password":
		if strings.ContainsRune(in.Secret, 0) {
			return SSHInput{}, ErrInvalid
		}
	case "key":
		if strings.ContainsRune(in.Secret, 0) {
			return SSHInput{}, ErrInvalid
		}
	default:
		return SSHInput{}, ErrInvalid
	}
	return in, nil
}

func validHost(host string) bool {
	if len(host) == 0 || len(host) > 255 || strings.ContainsAny(host, " \t\r\n/@\\[]%") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validFingerprint(value string) bool {
	if !strings.HasPrefix(value, "SHA256:") {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, "SHA256:"))
	return err == nil && len(decoded) == 32
}

func generatedMessage(stage string) string {
	switch stage {
	case "connect":
		return "Connecting securely to the node."
	case "detect":
		return "Checking the node operating system and architecture."
	case "download":
		return "Downloading the agent and service files."
	case "verify":
		return "Verifying the agent checksum."
	case "user":
		return "Preparing the dedicated agent account."
	case "enroll":
		return "Enrolling the agent with the command center."
	case "start":
		return "Starting the agent service."
	case "heartbeat":
		return "Waiting for the first heartbeat."
	case "cleanup":
		return "Removing temporary installer files."
	default:
		return fmt.Sprintf("Installation is working on step %s.", stage)
	}
}
