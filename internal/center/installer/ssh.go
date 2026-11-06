package installer

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const handshakeTimeout = 10 * time.Second

func probeHostKey(ctx context.Context, input ProbeInput) (string, error) {
	input, err := input.normalize()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(input.Host, strconv.Itoa(input.Port)))
	if err != nil {
		return "", ErrDisconnect
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	var fingerprint string
	config := &ssh.ClientConfig{
		User: "speedtest-probe",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: handshakeTimeout,
	}
	clientConn, channels, requests, _ := ssh.NewClientConn(conn, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), config)
	if clientConn != nil {
		client := ssh.NewClient(clientConn, channels, requests)
		_ = client.Close()
	}
	if fingerprint == "" {
		return "", ErrDisconnect
	}
	return fingerprint, nil
}

func connectSSH(ctx context.Context, input SSHInput, expectedFingerprint string) (*ssh.Client, func(), error) {
	input, err := input.normalize()
	if err != nil {
		return nil, nil, ErrInvalid
	}
	var auth ssh.AuthMethod
	switch input.Auth {
	case "password":
		auth = ssh.Password(input.Secret)
	case "key":
		signer, parseErr := ssh.ParsePrivateKey([]byte(input.Secret))
		if parseErr != nil {
			return nil, nil, ErrInvalid
		}
		auth = ssh.PublicKeys(signer)
	default:
		return nil, nil, ErrInvalid
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, handshakeTimeout)
	address := net.JoinHostPort(input.Host, strconv.Itoa(input.Port))
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	cancelDial()
	if err != nil {
		return nil, nil, ErrDisconnect
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	var hostKeyErr error
	config := &ssh.ClientConfig{
		User:    input.User,
		Auth:    []ssh.AuthMethod{auth},
		Timeout: handshakeTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			observed := ssh.FingerprintSHA256(key)
			if subtle.ConstantTimeCompare([]byte(observed), []byte(expectedFingerprint)) != 1 {
				hostKeyErr = ErrHostKeyChange
				return hostKeyErr
			}
			return nil
		},
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		_ = conn.Close()
		if errors.Is(hostKeyErr, ErrHostKeyChange) {
			return nil, nil, ErrHostKeyChange
		}
		if strings.Contains(strings.ToLower(err.Error()), "unable to authenticate") {
			return nil, nil, ErrAuth
		}
		return nil, nil, ErrDisconnect
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, channels, requests)
	return client, closeSSHOnCancel(ctx, client), nil
}

func closeSSHOnCancel(ctx context.Context, client *ssh.Client) func() {
	clientCtx, cancelClient := context.WithCancel(ctx)
	stopped := make(chan struct{})
	var once sync.Once
	closeClient := func() {
		once.Do(func() {
			close(stopped)
			cancelClient()
			_ = client.Close()
		})
	}
	go func() {
		select {
		case <-clientCtx.Done():
			_ = client.Close()
		case <-stopped:
		}
	}()
	return closeClient
}
