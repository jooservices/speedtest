package installer

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	workerCount = 4
	jobQueueCap = 16
)

var safeCenterURL = regexp.MustCompile(`^https?://[A-Za-z0-9._-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

// EnrollmentIssuer creates an in-memory one-time token for an existing node.
type EnrollmentIssuer interface {
	IssueInstallToken(context.Context, string) (token, nodeName string, err error)
	RevokeInstallToken(context.Context, string) error
}

// ServiceOptions supplies the persistent database, token issuer, and configured
// command-center origin. It must contain no request or credential data.
type ServiceOptions struct {
	Database   Database
	Enrollment EnrollmentIssuer
	CenterURL  string
	AppKey     []byte
	Logger     *slog.Logger
}

type jobRequest struct {
	installation Installation
	input        InstallInput
}

// Service coordinates short-lived host-key probes and asynchronous installs.
type Service struct {
	store      *PostgresStore
	enrollment EnrollmentIssuer
	centerURL  string
	logger     *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc
	jobs       chan jobRequest
	workers    sync.WaitGroup
	queueMu    sync.Mutex
	closed     bool
}

// NewService starts a bounded pool of installation workers and marks any
// in-flight jobs from a previous center process as failed.
func NewService(parent context.Context, options ServiceOptions) (*Service, error) {
	if options.Database == nil || options.Enrollment == nil || len(options.AppKey) != 32 {
		return nil, ErrInvalid
	}
	centerURL := strings.TrimRight(strings.TrimSpace(options.CenterURL), "/")
	parsed, err := url.Parse(centerURL)
	if err != nil || parsed.Host == "" || !safeCenterURL.MatchString(centerURL) || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(centerURL, " \t\r\n\"'") {
		return nil, ErrInvalid
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	store := NewPostgresStoreWithKey(options.Database, options.AppKey)
	if err := store.recoverInterrupted(parent); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	service := &Service{
		store: store, enrollment: options.Enrollment, centerURL: centerURL,
		logger: options.Logger, ctx: ctx, cancel: cancel, jobs: make(chan jobRequest, jobQueueCap),
	}
	for range workerCount {
		service.workers.Add(1)
		go service.worker()
	}
	return service, nil
}

// Close cancels active operations and waits for the worker pool to stop.
func (s *Service) Close(ctx context.Context) error {
	s.queueMu.Lock()
	s.closed = true
	s.cancel()
	s.queueMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Probe records a fingerprint from an SSH handshake that sent no credentials.
func (s *Service) Probe(ctx context.Context, principal, key string, input ProbeInput) (SSHProbe, error) {
	if principal == "" || key == "" || len(key) > 128 {
		return SSHProbe{}, ErrInvalid
	}
	input, err := input.normalize()
	if err != nil {
		return SSHProbe{}, err
	}
	fingerprint, err := probeHostKey(ctx, input)
	if err != nil {
		return SSHProbe{}, err
	}
	id, err := newID()
	if err != nil {
		return SSHProbe{}, err
	}
	probe := SSHProbe{ID: id, Fingerprint: fingerprint, ExpiresAt: time.Now().UTC().Add(probeLifetime)}
	return s.store.saveProbe(ctx, principal, key, input, probe)
}

// Start accepts an explicitly confirmed fingerprint and queues a secret-free
// persistent job; SSH credentials remain only in the bounded in-memory queue.
func (s *Service) Start(ctx context.Context, principal, key, nodeID string, input InstallInput) (Installation, error) {
	if principal == "" || key == "" || len(key) > 128 || !validID(nodeID) ||
		!validID(input.ProbeID) || !validFingerprint(input.Fingerprint) {
		return Installation{}, ErrInvalid
	}
	var err error
	input.SSH, err = input.SSH.normalize()
	if err != nil || secretInputError(input) != nil {
		return Installation{}, ErrInvalid
	}
	id, err := newID()
	if err != nil {
		return Installation{}, err
	}
	job, replay, err := s.store.createInstallation(ctx, principal, key, id, nodeID, input)
	if err != nil {
		return Installation{}, err
	}
	if replay {
		return job, nil
	}
	s.queueMu.Lock()
	if s.closed {
		s.queueMu.Unlock()
		s.failDetached(job.ID, "The command center is shutting down. Retry the installation.")
		return job, nil
	}
	select {
	case s.jobs <- jobRequest{installation: job, input: input}:
		s.queueMu.Unlock()
	case <-ctx.Done():
		s.queueMu.Unlock()
		s.failDetached(job.ID, "The installation request was canceled before it started. Retry the installation.")
	default:
		s.queueMu.Unlock()
		s.failDetached(job.ID, "The installer is busy. Retry the installation in a moment.")
	}
	return job, nil
}

// Get reads the public installation state without exposing request credentials.
func (s *Service) Get(ctx context.Context, id string) (Installation, error) {
	return s.store.installation(ctx, id)
}

// Events returns the events after cursor and the current job state.
func (s *Service) Events(ctx context.Context, id, cursor string) ([]Event, Installation, error) {
	return s.store.events(ctx, id, cursor)
}

func (s *Service) worker() {
	defer s.workers.Done()
	for {
		select {
		case <-s.ctx.Done():
			for {
				select {
				case job := <-s.jobs:
					s.failDetached(job.installation.ID, "The command center is shutting down. Retry the installation.")
				default:
					return
				}
			}
		case job := <-s.jobs:
			s.run(job)
		}
	}
}

func (s *Service) run(job jobRequest) {
	defer func() {
		job.input.SSH.Secret = ""
		job.input.SSH.SudoPassword = ""
	}()
	ctx, cancel := context.WithTimeout(s.ctx, installTimeout)
	defer cancel()
	input := job.input
	pass := sudoPassword(input.SSH)
	_ = s.progress(ctx, job.installation.ID, "connect", generatedMessage("connect"), 1)

	client, closeClient, err := connectSSH(ctx, input.SSH, input.Fingerprint)
	if err != nil {
		message := messageForFailure(err, "connect")
		s.fail(job.installation.ID, message)
		s.logger.Warn("SSH installation failed", "installation_id", job.installation.ID, "reason", message)
		return
	}
	defer closeClient()
	if err := sudoPreflight(client, pass); err != nil {
		message := messageForFailure(ErrSudo, "connect")
		s.fail(job.installation.ID, message)
		s.logger.Warn("SSH installation failed", "installation_id", job.installation.ID, "reason", message)
		return
	}

	token, nodeName, err := s.enrollment.IssueInstallToken(ctx, job.installation.NodeID)
	if err != nil {
		s.fail(job.installation.ID, "The command center could not prepare the node for enrollment.")
		return
	}
	defer func() {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer revokeCancel()
		_ = s.enrollment.RevokeInstallToken(revokeCtx, token)
	}()
	if err := uploadConfig(client, job.installation.ID, s.centerURL, nodeName, pass); err != nil {
		s.rollback(client, job.installation.ID, pass, input.SSH, input.Fingerprint)
		s.fail(job.installation.ID, messageForFailure(err, "download"))
		return
	}

	var heartbeatBaseline *time.Time
	var heartbeatBaselineErr error
	stage, err := runInstallScript(ctx, client, job.installation.ID, pass, token, func(stage string, pct float64) {
		if stage == "start" {
			heartbeatBaseline, heartbeatBaselineErr = s.store.lastHeartbeat(ctx, job.installation.NodeID)
		}
		message := generatedMessage(stage)
		if stage == "prepared" {
			message = "The service is running. Checking its first heartbeat."
		}
		_ = s.progress(ctx, job.installation.ID, stage, message, pct)
	})
	if err != nil {
		s.rollback(client, job.installation.ID, pass, input.SSH, input.Fingerprint)
		message := messageForFailure(err, stage)
		s.fail(job.installation.ID, message)
		s.logger.Warn("SSH installation failed", "installation_id", job.installation.ID, "reason", message)
		return
	}
	if heartbeatBaselineErr != nil {
		s.rollback(client, job.installation.ID, pass, input.SSH, input.Fingerprint)
		message := "The command center could not verify the node heartbeat before starting the agent."
		s.fail(job.installation.ID, message)
		s.logger.Warn("SSH installation failed", "installation_id", job.installation.ID, "reason", message)
		return
	}

	_ = s.progress(ctx, job.installation.ID, "heartbeat", generatedMessage("heartbeat"), 93)
	if err := s.waitHeartbeat(ctx, job.installation.NodeID, heartbeatBaseline); err != nil {
		s.rollback(client, job.installation.ID, pass, input.SSH, input.Fingerprint)
		message := "The agent service started but the command center did not receive a heartbeat."
		s.fail(job.installation.ID, message)
		s.logger.Warn("SSH installation failed", "installation_id", job.installation.ID, "reason", message)
		return
	}
	if err := commitRemote(client, job.installation.ID, pass); err != nil {
		s.rollback(client, job.installation.ID, pass, input.SSH, input.Fingerprint)
		message := "The agent is running, but temporary installer files could not be removed."
		s.fail(job.installation.ID, message)
		s.logger.Warn("SSH installation failed", "installation_id", job.installation.ID, "reason", message)
		return
	}
	_ = s.complete(job.installation.ID)
	s.logger.Info("SSH installation succeeded", "installation_id", job.installation.ID)
}

func (s *Service) waitHeartbeat(ctx context.Context, nodeID string, baseline *time.Time) error {
	deadline := time.NewTimer(heartbeatTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		seen, err := s.store.lastHeartbeat(ctx, nodeID)
		if err != nil {
			return err
		}
		if seen != nil && (baseline == nil || seen.After(*baseline)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("heartbeat timeout")
		case <-ticker.C:
		}
	}
}

func (s *Service) rollback(client *ssh.Client, id, pass string, input SSHInput, fingerprint string) {
	if err := rollbackRemote(client, id, pass); err == nil {
		return
	}
	reconnectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recovered, closeClient, err := connectSSH(reconnectCtx, input, fingerprint)
	if err != nil {
		return
	}
	defer closeClient()
	_ = rollbackRemote(recovered, id, pass)
}

func (s *Service) progress(ctx context.Context, id, stage, message string, pct float64) error {
	return s.store.appendEvent(ctx, id, stage, message, pct, "running", nil)
}

func (s *Service) fail(id, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.store.appendEvent(ctx, id, "failed", message, 100, "failed", stringPtr(message))
}

func (s *Service) failDetached(id, message string) {
	s.fail(id, message)
}

func (s *Service) complete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	message := "The agent is installed and its first heartbeat was received."
	return s.store.appendEvent(ctx, id, "complete", message, 100, "succeeded", nil)
}
