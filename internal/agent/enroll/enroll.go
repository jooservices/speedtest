// Package enroll exchanges a one-time enrollment token for a long-lived node
// credential at the command center, then stores the credential in a mode-0600
// file. The token and the credential are secrets and must never be logged.
package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultCredentialPath is where the node stores its credential when
// SPEEDTEST_AGENT_CREDENTIAL is not set.
const DefaultCredentialPath = "/etc/speedtest-agent/credential"

// Request carries the values sent to POST /api/agent/enroll.
type Request struct {
	// Token is the single-use enrollment token (a secret, never logged).
	Token string
	// Name is the node name; it must match the name the token was issued for.
	Name string
	// Host is an optional description of the machine (usually the hostname).
	Host string
	// Agent identifies the agent software, for example "speedtest-agent/1.2.3".
	Agent string
}

// Result is what a successful enrollment returns.
type Result struct {
	NodeID     string
	Credential string
}

// Client performs enrollment against a command center.
type Client struct {
	// BaseURL is the command-center origin, for example "https://center.example".
	BaseURL *url.URL
	// HTTP is the client used for the request; when nil, http.DefaultClient is
	// used.
	HTTP *http.Client
}

// enrollRequest is the JSON body of EnrollRequest in docs/openapi.yaml. host is
// optional and omitted when empty.
type enrollRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Host  string `json:"host,omitempty"`
	Agent string `json:"agent"`
}

// successEnvelope is the shared Success wrapper around enrollResponse.data.
type successEnvelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    enrolledData    `json:"data"`
	Meta    json.RawMessage `json:"meta"`
	Errors  json.RawMessage `json:"errors"`
}

type enrolledData struct {
	NodeID     string `json:"nodeId"`
	Credential string `json:"credential"`
}

// problem is an RFC 9457 error body.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// Enroll exchanges the one-time token for the node credential and returns it.
// The returned credential is a secret; callers must store it without logging
// it. Enroll itself never logs the token or the credential.
func (c Client) Enroll(ctx context.Context, req Request) (Result, error) {
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	if c.BaseURL == nil {
		return Result{}, errors.New("center URL is required")
	}
	if c.BaseURL.Scheme != "http" && c.BaseURL.Scheme != "https" {
		return Result{}, fmt.Errorf("center URL must be http or https, got %q", c.BaseURL.String())
	}

	body, err := json.Marshal(enrollRequest(req))
	if err != nil {
		return Result{}, fmt.Errorf("encode enroll request: %w", err)
	}

	endpoint := c.BaseURL.JoinPath("api", "agent", "enroll").String()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build enroll request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("enroll request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read enroll response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, decodeProblem(resp.StatusCode, respBody)
	}

	var env successEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return Result{}, fmt.Errorf("decode enroll response: %w", err)
	}
	if env.Data.Credential == "" {
		return Result{}, errors.New("enroll response has no credential")
	}
	return Result{NodeID: env.Data.NodeID, Credential: env.Data.Credential}, nil
}

func validateRequest(req Request) error {
	if req.Token == "" {
		return errors.New("token must not be blank")
	}
	if req.Name == "" {
		return errors.New("name must not be blank")
	}
	if req.Agent == "" {
		return errors.New("agent must not be blank")
	}
	return nil
}

// decodeProblem turns a non-2xx body into an error without echoing secrets. The
// problem detail comes from the server, which never includes tokens or
// credentials.
func decodeProblem(status int, body []byte) error {
	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("enroll failed with status %d", status)
	}
	detail := p.Detail
	if detail == "" {
		detail = p.Title
	}
	if detail == "" {
		return fmt.Errorf("enroll failed with status %d", status)
	}
	return fmt.Errorf("enroll failed: %s", detail)
}

// StoreCredential atomically replaces path with the credential in mode 0600.
// The parent directory is created with mode 0700. Existing file contents are
// never overwritten before the restrictive permissions are in place.
func StoreCredential(path, credential string) error {
	return storeCredential(path, credential, operatingSystemCredentialStore())
}

type credentialTempFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type credentialDirectory interface {
	Sync() error
	Close() error
}

type credentialStoreOps struct {
	mkdirAll   func(string, os.FileMode) error
	createTemp func(string, string) (credentialTempFile, error)
	rename     func(string, string) error
	remove     func(string) error
	openDir    func(string) (credentialDirectory, error)
}

func operatingSystemCredentialStore() credentialStoreOps {
	return credentialStoreOps{
		mkdirAll: os.MkdirAll,
		createTemp: func(dir, pattern string) (credentialTempFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		rename: os.Rename,
		remove: os.Remove,
		openDir: func(path string) (credentialDirectory, error) {
			return os.Open(path)
		},
	}
}

func storeCredential(path, credential string, ops credentialStoreOps) (resultErr error) {
	dir := filepath.Dir(path)
	if err := ops.mkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}

	temp, err := ops.createTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create credential temp file: %w", err)
	}
	tempPath := temp.Name()
	open := true
	renamed := false
	defer func() {
		if open {
			if err := temp.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close credential temp file: %w", err))
			}
		}
		if !renamed {
			if err := ops.remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove credential temp file: %w", err))
			}
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set credential temp permissions: %w", err)
	}
	written, err := temp.Write([]byte(credential))
	if err != nil {
		return fmt.Errorf("write credential temp file: %w", err)
	}
	if written != len(credential) {
		return fmt.Errorf("write credential temp file: %w", io.ErrShortWrite)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync credential temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		open = false
		return fmt.Errorf("close credential temp file: %w", err)
	}
	open = false

	if err := ops.rename(tempPath, path); err != nil {
		return fmt.Errorf("replace credential: %w", err)
	}
	renamed = true

	return syncCredentialDirectory(dir, ops)
}

// LoadCredential reads the credential file and refuses to start the node if the
// file is readable by group or others, or if it is empty. The returned value is
// a secret and must never be logged.
func LoadCredential(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read credential %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("credential %s has permissions %04o; it must be readable only by the owner (0600)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credential %s: %w", path, err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" {
		return "", fmt.Errorf("credential %s is empty", path)
	}
	return credential, nil
}

func syncCredentialDirectory(dir string, ops credentialStoreOps) error {
	dirFile, err := ops.openDir(dir)
	if err != nil {
		return fmt.Errorf("open credential directory: %w", err)
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return fmt.Errorf("sync credential directory: %w", err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("close credential directory: %w", err)
	}
	return nil
}
