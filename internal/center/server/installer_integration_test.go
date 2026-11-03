//go:build integration

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/speedtest/internal/center/agentapi"
	"github.com/jooservices/speedtest/internal/center/db/dbtest"
	"github.com/jooservices/speedtest/internal/center/installer"
	"github.com/jooservices/speedtest/internal/center/nodes"
	"golang.org/x/crypto/ssh"
)

// TestInstallerHTTPSSHEnrollmentEndToEndIntegration covers the backend flow
// behind the JOOST-41 wizard: confirmed probe -> install -> enrollment ->
// authenticated heartbeat -> replayable SSE -> status. The separate process
// tests execute the actual remote shell scripts on a disposable filesystem.
func TestInstallerHTTPSSHEnrollmentEndToEndIntegration(t *testing.T) {
	pool, ctx, key := openServerIntegrationPool(t)
	dbtest.Reset(t, pool)
	t.Cleanup(func() { dbtest.Reset(t, pool) })
	dbtest.MigrateUp(t, pool)
	if _, err := pool.Exec(ctx, "INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps) VALUES($1,'installer-e2e',100,50)", installerTestNodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schedules(node_id) VALUES($1)", installerTestNodeID); err != nil {
		t.Fatal(err)
	}
	nodeService := nodes.NewService(pool, key)
	service, err := installer.NewService(ctx, installer.ServiceOptions{Database: pool, Enrollment: nodeService, CenterURL: "https://center.test", AppKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	// Reuse owner authentication steps; every subsequent operation crosses HTTP.
	authHandler := newInstallerHTTPTestHandler(t, service)
	// The owner handler's session store is used unchanged; dispatch the public
	// agent protocol through the real node service and PostgreSQL store.
	agentHandler := NewHandler(Options{NodeCredentials: nodeService, AgentAPI: &agentapi.Postgres{Pool: pool}})
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/agent/") {
			agentHandler.ServeHTTP(w, r)
			return
		}
		authHandler.ServeHTTP(w, r)
	})
	center := httptest.NewServer(mux)
	defer center.Close()
	cookie, csrf := loginInstallerHTTPTestOwner(t, authHandler)
	client := &http.Client{Timeout: 10 * time.Second}
	request := func(method, path, body, bearer string, owner bool) (int, []byte) {
		req, err := http.NewRequestWithContext(ctx, method, center.URL+path, strings.NewReader(body))
		if err != nil {
			t.Error(err)
			return 0, nil
		}
		req.Header.Set("Content-Type", "application/json")
		if owner {
			req.AddCookie(cookie)
			req.Header.Set("Origin", center.URL)
			req.Header.Set("X-CSRF-Token", csrf)
			req.Header.Set("Idempotency-Key", path)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Errorf("HTTP request failed: %v", err)
			return 0, nil
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Error(err)
		}
		return response.StatusCode, data
	}
	enrolled := make(chan string, 1)
	target := installerE2ESSH(t, func(token string) {
		payload, _ := json.Marshal(map[string]string{"token": token, "name": "installer-e2e", "host": "fixture", "agent": "e2e"})
		status, data := request("POST", "/api/agent/enroll", string(payload), "", false)
		if status != 201 {
			t.Errorf("enrollment status %d", status)
			enrolled <- ""
			return
		}
		var response struct {
			Data struct {
				Credential string `json:"credential"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			t.Error(err)
		}
		enrolled <- response.Data.Credential
	}, func() {
		var credential string
		select {
		case credential = <-enrolled:
		case <-ctx.Done():
			return
		}
		// The install worker records its baseline upon consuming the start marker.
		time.Sleep(100 * time.Millisecond)
		status, _ := request("POST", "/api/agent/heartbeat", `{"agent":"installer-e2e","uptime":1,"lastTest":null}`, credential, false)
		if status != 200 {
			t.Errorf("heartbeat status %d", status)
		}
	})
	host, portString, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portString)
	probeBody, _ := json.Marshal(installer.ProbeInput{Host: host, Port: port})
	status, data := request("POST", "/api/installer/probes", string(probeBody), "", true)
	if status != 201 {
		t.Fatalf("probe status %d: %s", status, data)
	}
	var probe struct {
		Data installer.SSHProbe `json:"data"`
		Meta map[string]any     `json:"meta"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Meta == nil {
		t.Fatalf("invalid probe envelope: %v", err)
	}
	input := installer.InstallInput{ProbeID: probe.Data.ID, Fingerprint: probe.Data.Fingerprint, SSH: installer.SSHInput{Host: host, Port: port, User: "admin", Auth: "password", Secret: "e2e-test-password"}}
	body, _ := json.Marshal(input)
	status, data = request("POST", "/api/nodes/"+installerTestNodeID+"/installations", string(body), "", true)
	if status != 202 {
		t.Fatalf("install status %d: %s", status, data)
	}
	var job struct {
		Data installer.Installation `json:"data"`
		Meta map[string]any         `json:"meta"`
	}
	if err := json.Unmarshal(data, &job); err != nil || job.Meta == nil {
		t.Fatalf("invalid install envelope: %v", err)
	}
	status, events := request("GET", "/api/installations/"+job.Data.ID+"/events", "", "", true)
	if status != 200 || !strings.Contains(string(events), `"status":"succeeded"`) {
		t.Fatalf("SSE did not finish: %d %s", status, events)
	}
	status, data = request("GET", "/api/installations/"+job.Data.ID, "", "", true)
	if status != 200 {
		t.Fatalf("status GET: %d", status)
	}
	if err := json.Unmarshal(data, &job); err != nil || job.Data.Status != "succeeded" || job.Meta == nil {
		t.Fatalf("unexpected status: %v %+v", err, job)
	}
	if strings.Contains(string(events)+string(data), input.SSH.Secret) {
		t.Fatal("credential in owner output")
	}
	t.Log("PASS: HTTP probe/confirmation/install -> SSH token FIFO -> HTTP agent enrollment/heartbeat -> SSE success -> status; meta objects present")
}

func installerE2ESSH(t *testing.T, onEnroll func(string), onStart func()) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if string(password) != "e2e-test-password" {
			return nil, fmt.Errorf("authentication rejected")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					_ = conn.Close()
					return
				}
				defer func() { _ = sshConn.Close() }()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					channel, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer func() { _ = channel.Close() }()
						for req := range requests {
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
								return
							}
							_ = req.Reply(true, nil)
							input, _ := io.ReadAll(channel)
							switch {
							case strings.Contains(payload.Command, "token.pipe"):
								_, token, _ := strings.Cut(string(input), "\n")
								onEnroll(strings.TrimSpace(token))
							case strings.Contains(string(input), "STP|detect|5"):
								_, _ = io.WriteString(channel, "STP|detect|5\nSTP|download|20\nSTP|verify|35\nSTP|user|50\nSTP|enroll|70\n")
								// The FIFO handler runs in another SSH session.
								time.Sleep(100 * time.Millisecond)
								_, _ = io.WriteString(channel, "STP|start|85\n")
								onStart()
								_, _ = io.WriteString(channel, "STP|prepared|90\n")
							}
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	return listener.Addr().String()
}
