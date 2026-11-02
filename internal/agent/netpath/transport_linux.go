//go:build linux

package netpath

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

func newBoundTransport(path Path) *http.Transport {
	dialer := configuredDialer(path, &net.Dialer{Timeout: echoTimeout, KeepAlive: 30 * time.Second})
	resolver := boundResolver(path, dialer)
	dialer.Resolver = resolver
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1,
		IdleConnTimeout:       echoTimeout,
		TLSHandshakeTimeout:   echoTimeout,
		ResponseHeaderTimeout: echoTimeout,
		// The center IP must be observed from this node, never from an
		// ambient HTTP proxy with a different route.
		Proxy: nil,
	}
}

func configuredDialer(path Path, dialer *net.Dialer) *net.Dialer {
	if path.SourceIP != nil {
		dialer.LocalAddr = localAddress("tcp", path.SourceIP)
	}
	if path.Interface != "" {
		dialer.Control = func(_, _ string, raw syscall.RawConn) error {
			return bindToDevice(raw, path.Interface)
		}
	}
	return dialer
}

func boundResolver(path Path, dialer *net.Dialer) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dnsDialer := *dialer
			if path.SourceIP != nil {
				dnsDialer.LocalAddr = localAddress(network, path.SourceIP)
			}
			return dnsDialer.DialContext(ctx, network, address)
		},
	}
}

func bindToDevice(raw syscall.RawConn, name string) error {
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		socketErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, name)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return fmt.Errorf("bind socket to network_interface %q: %w", name, socketErr)
	}
	return nil
}

func localAddress(network string, ip net.IP) net.Addr {
	if strings.HasPrefix(network, "udp") {
		return &net.UDPAddr{IP: ip}
	}
	return &net.TCPAddr{IP: ip}
}
