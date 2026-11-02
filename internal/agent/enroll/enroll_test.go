package enroll

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseURL(t *testing.T, s *httptest.Server) *url.URL {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return u
}

func enrollSuccessJSON(nodeID, credential string) []byte {
	body, _ := json.Marshal(map[string]any{
		"success": true,
		"code":    201,
		"message": "enrolled",
		"data": map[string]any{
			"nodeId":     nodeID,
			"credential": credential,
			"config":     map[string]any{},
		},
		"meta":   map[string]any{},
		"errors": nil,
	})
	return body
}

func TestEnrollSuccess(t *testing.T) {
	req := Request{
		Token: "one-time-token",
		Name:  "viettel-fast-50",
		Host:  "edge-1",
		Agent: "speedtest-agent/1.2.3",
	}

	var gotMethod, gotPath, gotContentType string
	var gotBody map[string]any

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(enrollSuccessJSON("node-123", "super-secret-credential"))
	}))
	defer s.Close()

	got, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), req)
	if err != nil {
		t.Fatalf("Enroll() error = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/agent/enroll" {
		t.Errorf("path = %q, want /api/agent/enroll", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if gotBody["token"] != "one-time-token" {
		t.Errorf("body token = %v", gotBody["token"])
	}
	if gotBody["name"] != "viettel-fast-50" {
		t.Errorf("body name = %v", gotBody["name"])
	}
	if gotBody["host"] != "edge-1" {
		t.Errorf("body host = %v", gotBody["host"])
	}
	if gotBody["agent"] != "speedtest-agent/1.2.3" {
		t.Errorf("body agent = %v", gotBody["agent"])
	}

	if got.NodeID != "node-123" {
		t.Errorf("NodeID = %q, want node-123", got.NodeID)
	}
	if got.Credential != "super-secret-credential" {
		t.Errorf("Credential = %q, want super-secret-credential", got.Credential)
	}
}

func TestEnrollOmitsEmptyHost(t *testing.T) {
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(enrollSuccessJSON("n", "c"))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "t", Name: "n", Agent: "a",
	})
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	if _, present := gotBody["host"]; present {
		t.Errorf("body host = %v, want it omitted", gotBody["host"])
	}
}

func TestEnrollServerProblem(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,"detail":"invalid or expired token"}`))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "bad", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid or expired token") {
		t.Fatalf("Enroll() error = %v, want it to surface the problem detail", err)
	}
}

func TestEnrollProblemTitleFallback(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401}`))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "bad", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("Enroll() error = %v, want it to surface the title", err)
	}
}

func TestEnrollProblemWithoutDetailOrTitle(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"about:blank","status":403}`))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "t", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("Enroll() error = %v, want a status 403 error", err)
	}
}

func TestEnrollNonJSONErrorBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "t", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Enroll() error = %v, want a status 500 error", err)
	}
}

func TestEnrollMissingCredential(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"code":201,"message":"x","data":{"nodeId":"n"},"meta":{},"errors":null}`))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "t", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("Enroll() error = %v, want a missing-credential error", err)
	}
}

func TestEnrollMalformedSuccessBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{not json"))
	}))
	defer s.Close()

	_, err := Client{BaseURL: baseURL(t, s)}.Enroll(context.Background(), Request{
		Token: "t", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "decode enroll response") {
		t.Fatalf("Enroll() error = %v, want a decode error", err)
	}
}

func TestEnrollNetworkError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u := baseURL(t, s)
	s.Close()

	_, err := Client{BaseURL: u}.Enroll(context.Background(), Request{
		Token: "t", Name: "n", Agent: "a",
	})
	if err == nil || !strings.Contains(err.Error(), "enroll request") {
		t.Fatalf("Enroll() error = %v, want a request error", err)
	}
}

func TestEnrollValidation(t *testing.T) {
	tests := []struct {
		name    string
		req     Request
		wantErr string
	}{
		{name: "empty token", req: Request{Name: "n", Agent: "a"}, wantErr: "token must not be blank"},
		{name: "empty name", req: Request{Token: "t", Agent: "a"}, wantErr: "name must not be blank"},
		{name: "empty agent", req: Request{Token: "t", Name: "n"}, wantErr: "agent must not be blank"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Client{BaseURL: &url.URL{Scheme: "http", Host: "x"}}.Enroll(context.Background(), tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Enroll() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestEnrollNilBaseURL(t *testing.T) {
	_, err := Client{}.Enroll(context.Background(), Request{Token: "t", Name: "n", Agent: "a"})
	if err == nil || !strings.Contains(err.Error(), "center URL is required") {
		t.Fatalf("Enroll() error = %v, want a missing-URL error", err)
	}
}

func TestEnrollBadScheme(t *testing.T) {
	_, err := Client{BaseURL: &url.URL{Scheme: "ftp", Host: "x"}}.Enroll(context.Background(), Request{Token: "t", Name: "n", Agent: "a"})
	if err == nil || !strings.Contains(err.Error(), "center URL must be http or https") {
		t.Fatalf("Enroll() error = %v, want a scheme error", err)
	}
}

func TestStoreAndLoadCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "credential")

	if err := StoreCredential(path, "my-credential"); err != nil {
		t.Fatalf("StoreCredential() error = %v", err)
	}

	got, err := LoadCredential(path)
	if err != nil {
		t.Fatalf("LoadCredential() error = %v", err)
	}
	if got != "my-credential" {
		t.Fatalf("LoadCredential() = %q, want my-credential", got)
	}
}

func TestStoreCredentialMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := StoreCredential(path, "secret"); err != nil {
		t.Fatalf("StoreCredential() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential mode = %04o, want 0600", mode)
	}
}

func TestStoreCredentialTightensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := StoreCredential(path, "new-secret"); err != nil {
		t.Fatalf("StoreCredential() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("credential mode = %04o, want 0600", mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "new-secret" {
		t.Fatalf("credential = %q, want new secret", data)
	}
}

func TestLoadCredentialWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadCredential(path)
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("LoadCredential() error = %v, want a permission error mentioning 0600", err)
	}
}

func TestLoadCredentialMissing(t *testing.T) {
	_, err := LoadCredential(filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "read credential") {
		t.Fatalf("LoadCredential() error = %v, want a read error", err)
	}
}

func TestStoreCredentialMkdirError(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := StoreCredential(filepath.Join(parentFile, "credential"), "secret")
	if err == nil || !strings.Contains(err.Error(), "create credential directory") {
		t.Fatalf("StoreCredential() error = %v, want a directory error", err)
	}
	assertNoCredentialTempFiles(t, filepath.Dir(parentFile), "credential")
}

func TestStoreCredentialRenameErrorCleansTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	err := StoreCredential(path, "secret")
	if err == nil || !strings.Contains(err.Error(), "replace credential") {
		t.Fatalf("StoreCredential() error = %v, want a replace error", err)
	}
	assertNoCredentialTempFiles(t, dir, "credential")
}

func TestStoreCredentialFailurePaths(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*credentialStoreOps)
		wantErr    string
		wantStored bool
	}{
		{
			name: "create temp file",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = func(string, string) (credentialTempFile, error) {
					return nil, os.ErrPermission
				}
			},
			wantErr: "create credential temp file",
		},
		{
			name: "set temp permissions",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.chmodErr = os.ErrPermission
				})
			},
			wantErr: "set credential temp permissions",
		},
		{
			name: "write temp file",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.writeErr = os.ErrPermission
				})
			},
			wantErr: "write credential temp file",
		},
		{
			name: "short write",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.shortWrite = true
				})
			},
			wantErr: "short write",
		},
		{
			name: "sync temp file",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.syncErr = os.ErrPermission
				})
			},
			wantErr: "sync credential temp file",
		},
		{
			name: "close temp file",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.closeErr = os.ErrPermission
				})
			},
			wantErr: "close credential temp file",
		},
		{
			name: "cleanup close error",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.writeErr = os.ErrPermission
					f.closeErr = os.ErrPermission
				})
			},
			wantErr: "close credential temp file",
		},
		{
			name: "cleanup remove error",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.writeErr = os.ErrPermission
				})
				ops.remove = func(path string) error {
					_ = os.Remove(path)
					return os.ErrPermission
				}
			},
			wantErr: "remove credential temp file",
		},
		{
			name: "temp already removed during failure",
			setup: func(ops *credentialStoreOps) {
				ops.createTemp = wrapCredentialTempFile(func(f *faultyCredentialTempFile) {
					f.writeErr = os.ErrPermission
					f.removeOnWrite = true
				})
			},
			wantErr: "write credential temp file",
		},
		{
			name: "open credential directory",
			setup: func(ops *credentialStoreOps) {
				ops.openDir = func(string) (credentialDirectory, error) {
					return nil, os.ErrPermission
				}
			},
			wantErr:    "open credential directory",
			wantStored: true,
		},
		{
			name: "sync credential directory",
			setup: func(ops *credentialStoreOps) {
				ops.openDir = wrapCredentialDirectory(func(d *faultyCredentialDirectory) {
					d.syncErr = os.ErrPermission
				})
			},
			wantErr:    "sync credential directory",
			wantStored: true,
		},
		{
			name: "close credential directory",
			setup: func(ops *credentialStoreOps) {
				ops.openDir = wrapCredentialDirectory(func(d *faultyCredentialDirectory) {
					d.closeErr = os.ErrPermission
				})
			},
			wantErr:    "close credential directory",
			wantStored: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "credential")
			ops := operatingSystemCredentialStore()
			tt.setup(&ops)

			err := storeCredential(path, "secret", ops)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("storeCredential() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.wantStored {
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("ReadFile() error = %v, want installed credential", readErr)
				}
				if string(data) != "secret" {
					t.Fatalf("credential = %q, want secret", data)
				}
			} else if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("credential unexpectedly installed: %v", statErr)
			}
			assertNoCredentialTempFiles(t, dir, "credential")
		})
	}
}

type faultyCredentialTempFile struct {
	*os.File
	chmodErr      error
	writeErr      error
	syncErr       error
	closeErr      error
	shortWrite    bool
	removeOnWrite bool
}

func (f *faultyCredentialTempFile) Chmod(mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.File.Chmod(mode)
}

func (f *faultyCredentialTempFile) Write(data []byte) (int, error) {
	if f.removeOnWrite {
		_ = os.Remove(f.Name())
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.shortWrite {
		return len(data) - 1, nil
	}
	return f.File.Write(data)
}

func (f *faultyCredentialTempFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *faultyCredentialTempFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

type faultyCredentialDirectory struct {
	*os.File
	syncErr  error
	closeErr error
}

func (d *faultyCredentialDirectory) Sync() error {
	if d.syncErr != nil {
		return d.syncErr
	}
	return d.File.Sync()
}

func (d *faultyCredentialDirectory) Close() error {
	err := d.File.Close()
	if d.closeErr != nil {
		return d.closeErr
	}
	return err
}

func wrapCredentialTempFile(configure func(*faultyCredentialTempFile)) func(string, string) (credentialTempFile, error) {
	return func(dir, pattern string) (credentialTempFile, error) {
		file, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		wrapped := &faultyCredentialTempFile{File: file}
		configure(wrapped)
		return wrapped, nil
	}
}

func wrapCredentialDirectory(configure func(*faultyCredentialDirectory)) func(string) (credentialDirectory, error) {
	return func(path string) (credentialDirectory, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		wrapped := &faultyCredentialDirectory{File: file}
		configure(wrapped)
		return wrapped, nil
	}
}

func assertNoCredentialTempFiles(t *testing.T, dir, base string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+base+".tmp-") {
			t.Errorf("temporary credential file left behind: %s", entry.Name())
		}
	}
}

func TestLoadCredentialEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadCredential(path)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("LoadCredential() error = %v, want an empty error", err)
	}
}
