package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeSSHOptions struct {
	password string
	sudo     bool
	mode     string
	public   ssh.PublicKey
	onStart  func()
}

type fakeSSHServer struct {
	listener     net.Listener
	config       *ssh.ServerConfig
	options      fakeSSHOptions
	address      string
	fingerprint  string
	connections  []net.Conn
	mu           sync.Mutex
	commands     []string
	stdin        [][]byte
	tokens       chan string
	tokenReady   chan struct{}
	tokenOnce    sync.Once
	authAttempts atomic.Int32
}

func newFakeSSHServer(t *testing.T, options fakeSSHOptions) *fakeSSHServer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSSHServer{options: options, tokens: make(chan string, 1), tokenReady: make(chan struct{})}
	config := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			server.authAttempts.Add(1)
			if string(password) == options.password && options.password != "" {
				return nil, nil
			}
			return nil, errors.New("authentication rejected")
		},
	}
	if options.public != nil {
		config.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), options.public.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("public key rejected")
		}
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.listener = listener
	server.config = config
	server.address = listener.Addr().String()
	server.fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
	go server.acceptLoop()
	t.Cleanup(server.close)
	return server
}

func (s *fakeSSHServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connections = append(s.connections, conn)
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

func (s *fakeSSHServer) serveConn(conn net.Conn) {
	serverConn, channels, requests, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	for channelRequest := range channels {
		if channelRequest.ChannelType() != "session" {
			_ = channelRequest.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		channel, channelRequests, err := channelRequest.Accept()
		if err != nil {
			continue
		}
		go s.serveSession(channel, channelRequests)
	}
	_ = serverConn.Close()
}

func (s *fakeSSHServer) serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		s.recordCommand(payload.Command)
		data, _ := io.ReadAll(channel)
		s.recordStdin(data)
		s.handleExec(channel, payload.Command, data)
		return
	}
}

func (s *fakeSSHServer) handleExec(channel ssh.Channel, command string, input []byte) {
	if strings.Contains(command, "'true'") {
		if !s.options.sudo {
			s.exit(channel, 1)
			return
		}
		s.exit(channel, 0)
		return
	}
	if strings.Contains(command, "token.pipe") {
		lines := strings.Split(strings.TrimSuffix(string(input), "\n"), "\n")
		if len(lines) >= 2 {
			s.tokens <- strings.Join(lines[1:], "\n")
			s.tokenOnce.Do(func() { close(s.tokenReady) })
		}
		s.exit(channel, 0)
		return
	}
	if strings.Contains(command, "cat > /var/tmp/speedtest-installer-") {
		s.exit(channel, 0)
		return
	}
	if strings.Contains(string(input), "STP|detect|5") {
		switch s.options.mode {
		case "unsupported":
			_, _ = io.WriteString(channel, "STP|detect|5\nSTP|unsupported|0\n")
			s.exit(channel, 20)
		case "disconnect":
			_, _ = io.WriteString(channel, "STP|detect|5\nSTP|download|20\n")
			s.disconnect()
		default:
			for _, line := range []string{"STP|detect|5\n", "STP|download|20\n", "STP|verify|35\n", "STP|user|50\n", "STP|enroll|70\n", "STP|start|85\n"} {
				if _, err := io.WriteString(channel, line); err != nil {
					return
				}
				if strings.HasPrefix(line, "STP|enroll|") {
					select {
					case <-s.tokenReady:
					case <-time.After(3 * time.Second):
						s.exit(channel, 1)
						return
					}
				}
				if strings.HasPrefix(line, "STP|start|") && s.options.onStart != nil {
					s.options.onStart()
				}
			}
			_, _ = io.WriteString(channel, "STP|prepared|90\n")
			s.exit(channel, 0)
		}
		return
	}
	s.exit(channel, 0)
}

func (s *fakeSSHServer) exit(channel ssh.Channel, status uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = channel.Close()
}

func (s *fakeSSHServer) recordCommand(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
}

func (s *fakeSSHServer) recordStdin(value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stdin = append(s.stdin, append([]byte(nil), value...))
}

func (s *fakeSSHServer) close() {
	_ = s.listener.Close()
	s.disconnect()
}

func (s *fakeSSHServer) disconnect() {
	s.mu.Lock()
	connections := append([]net.Conn(nil), s.connections...)
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *fakeSSHServer) input() SSHInput {
	host, portText, _ := net.SplitHostPort(s.address)
	port, _ := strconv.Atoi(portText)
	return SSHInput{Host: host, Port: port, User: "admin", Auth: "password", Secret: s.options.password}
}

func TestProbeHostKeyUsesNoCredentials(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "ssh-password", sudo: true})
	host, portText, _ := net.SplitHostPort(server.address)
	port, _ := strconv.Atoi(portText)
	fingerprint, err := probeHostKey(context.Background(), ProbeInput{Host: host, Port: port})
	if err != nil {
		t.Fatalf("probeHostKey() error = %v", err)
	}
	if fingerprint != server.fingerprint {
		t.Fatalf("fingerprint = %q, want %q", fingerprint, server.fingerprint)
	}
	if attempts := server.authAttempts.Load(); attempts != 0 {
		t.Fatalf("probe attempted authentication %d times", attempts)
	}
}

func TestConnectSSHRejectsWrongPassword(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "correct-password", sudo: true})
	input := server.input()
	input.Secret = "wrong-password"
	_, _, err := connectSSH(context.Background(), input, server.fingerprint)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("connectSSH() error = %v, want ErrAuth", err)
	}
}

func TestSudoPreflightRejectsAccountWithoutSudo(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "ssh-password", sudo: false})
	client, closeClient, err := connectSSH(context.Background(), server.input(), server.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	if err := sudoPreflight(client, "sudo-password"); !errors.Is(err, ErrSudo) {
		t.Fatalf("sudoPreflight() error = %v, want ErrSudo", err)
	}
}

func TestConnectSSHRejectsChangedHostKey(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "ssh-password", sudo: true})
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherSigner, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = connectSSH(context.Background(), server.input(), ssh.FingerprintSHA256(otherSigner.PublicKey()))
	if !errors.Is(err, ErrHostKeyChange) {
		t.Fatalf("connectSSH() error = %v, want ErrHostKeyChange", err)
	}
}

func TestInstallScriptTransfersEnrollmentTokenOverFIFO(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "ssh-password", sudo: true})
	client, closeClient, err := connectSSH(context.Background(), server.input(), server.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	if err := sudoPreflight(client, "sudo-password"); err != nil {
		t.Fatal(err)
	}
	const id = "00000000-0000-4000-8000-000000000043"
	if err := uploadConfig(client, id, "https://center.example.test", "node-a", "sudo-password"); err != nil {
		t.Fatal(err)
	}
	const enrollmentToken = "enr_test-only-token"
	var stages []string
	stage, err := runInstallScript(context.Background(), client, id, "sudo-password", enrollmentToken, func(stage string, _ float64) {
		stages = append(stages, stage)
	})
	if err != nil {
		t.Fatalf("runInstallScript() error = %v", err)
	}
	if stage != "prepared" {
		t.Fatalf("final stage = %q, want prepared", stage)
	}
	wantStages := []string{"detect", "download", "verify", "user", "enroll", "start", "prepared"}
	if strings.Join(stages, ",") != strings.Join(wantStages, ",") {
		t.Fatalf("stages = %v, want %v", stages, wantStages)
	}
	select {
	case token := <-server.tokens:
		if token != enrollmentToken {
			t.Fatalf("FIFO token = %q, want supplied token", token)
		}
	case <-time.After(time.Second):
		t.Fatal("installer did not write the token FIFO")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, command := range server.commands {
		if strings.Contains(command, "ssh-password") || strings.Contains(command, "sudo-password") || strings.Contains(command, enrollmentToken) {
			t.Fatalf("secret appeared in remote command %q", command)
		}
	}
}

func TestInstallScriptRejectsUnsupportedArchitecture(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "ssh-password", sudo: true, mode: "unsupported"})
	client, closeClient, err := connectSSH(context.Background(), server.input(), server.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	stage, err := runInstallScript(context.Background(), client, "00000000-0000-4000-8000-000000000043", "sudo-password", "token", nil)
	if stage != "unsupported" || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("runInstallScript() = (%q, %v), want unsupported architecture", stage, err)
	}
}

func TestInstallScriptReportsMidInstallDisconnect(t *testing.T) {
	server := newFakeSSHServer(t, fakeSSHOptions{password: "ssh-password", sudo: true, mode: "disconnect"})
	client, closeClient, err := connectSSH(context.Background(), server.input(), server.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	_, err = runInstallScript(context.Background(), client, "00000000-0000-4000-8000-000000000043", "sudo-password", "token", nil)
	if !errors.Is(err, ErrDisconnect) {
		t.Fatalf("runInstallScript() error = %v, want ErrDisconnect", err)
	}
}
