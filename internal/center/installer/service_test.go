package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/ssh"
)

type unavailableDB struct{}

var errOffline = errors.New("database unavailable")

func (unavailableDB) Begin(context.Context) (pgx.Tx, error) { return nil, errOffline }
func (unavailableDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errOffline
}
func (unavailableDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errOffline }
func (unavailableDB) QueryRow(context.Context, string, ...any) pgx.Row        { return unavailableRow{} }

type unavailableRow struct{}

func (unavailableRow) Scan(...any) error { return errOffline }

type unavailableEnrollment struct{}

func (unavailableEnrollment) IssueInstallToken(context.Context, string) (string, string, error) {
	return "", "", errOffline
}
func (unavailableEnrollment) RevokeInstallToken(context.Context, string) error { return errOffline }

func TestServiceValidationAndUnavailableDatabase(t *testing.T) {
	ctx := context.Background()
	options := ServiceOptions{Database: unavailableDB{}, Enrollment: unavailableEnrollment{}, AppKey: make([]byte, 32), CenterURL: "https://center.test"}
	for _, address := range []string{"", ":bad", "ftp://center.test", "https://user:password@center.test", "https://center.test/?secret=value"} {
		invalid := options
		invalid.CenterURL = address
		if _, err := NewService(ctx, invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid origin accepted: %v", err)
		}
	}
	if _, err := NewService(ctx, ServiceOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := NewService(ctx, options); !errors.Is(err, errOffline) {
		t.Fatal(err)
	}
	s := &Service{store: NewPostgresStoreWithKey(unavailableDB{}, options.AppKey)}
	for _, owner := range []string{"", "owner"} {
		if _, err := s.Probe(ctx, owner, "", ProbeInput{}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := s.Probe(ctx, "owner", "key", ProbeInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, "owner", "key", "invalid", InstallInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	const id = "00000000-0000-4000-8000-000000000043"
	input := InstallInput{ProbeID: id, Fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	if _, err := s.Start(ctx, "owner", "key", id, input); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	input.SSH = SSHInput{Host: "node.test", Port: 22, User: "root", Auth: "password", Secret: "test"}
	if _, err := s.Start(ctx, "owner", "key", id, input); !errors.Is(err, errOffline) {
		t.Fatal(err)
	}
	if err := s.waitHeartbeat(ctx, id, nil); !errors.Is(err, errOffline) {
		t.Fatal(err)
	}
	s.failDetached(id, "Safe failure")
	for _, stage := range []string{"user", "verify", "enroll", "start", "prepared", "other"} {
		if stageFailureMessage(stage) == "" {
			t.Fatal("missing message")
		}
	}
	for _, input := range []SSHInput{{Auth: "password", Secret: "test"}, {Auth: "key"}, {SudoPassword: "other"}} {
		_ = sudoPassword(input)
	}
	if secretInputError(InstallInput{}) == nil {
		t.Fatal("missing secret accepted")
	}
	if _, err := hashJSON(make(chan int)); err == nil {
		t.Fatal("unsupported JSON accepted")
	}
	if _, err := keyedHashJSON(make([]byte, 32), make(chan int)); err == nil {
		t.Fatal("unsupported JSON accepted")
	}
}

func TestSSHKeyAuthenticationAndConnectionFailures(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server := newFakeSSHServer(t, fakeSSHOptions{public: signer.PublicKey(), sudo: true})
	input := server.input()
	input.Auth = "key"
	input.Secret = string(pem.EncodeToMemory(block))
	client, closeClient, err := connectSSH(context.Background(), input, server.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	closeClient()
	if err := commitRemote(client, "id", ""); !errors.Is(err, ErrDisconnect) {
		t.Fatal(err)
	}
	if err := rollbackRemote(client, "id", ""); !errors.Is(err, ErrDisconnect) {
		t.Fatal(err)
	}
	if err := writeTokenFIFO(client, "id", "", "token"); !errors.Is(err, ErrDisconnect) {
		t.Fatal(err)
	}
	input.Secret = "not a key"
	if _, _, err := connectSSH(context.Background(), input, server.fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err := connectSSH(context.Background(), SSHInput{}, server.fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input = server.input()
	input.Auth = "password"
	input.Secret = "test"
	if _, _, err := connectSSH(ctx, input, server.fingerprint); !errors.Is(err, ErrDisconnect) {
		t.Fatal(err)
	}
	if _, err := probeHostKey(ctx, ProbeInput{Host: input.Host, Port: input.Port}); !errors.Is(err, ErrDisconnect) {
		t.Fatal(err)
	}
	if _, err := probeHostKey(ctx, ProbeInput{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// A live connection is also closed when its owning operation is canceled.
	input.Auth = "key"
	input.Secret = string(pem.EncodeToMemory(block))
	active, cancelActive := context.WithCancel(context.Background())
	client, closeClient, err = connectSSH(active, input, server.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	cancelActive()
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case err := <-done:
		_ = err
	case <-time.After(time.Second):
		t.Fatal("canceled SSH operation retained connection")
	}
}
