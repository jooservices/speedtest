//go:build !linux

package netpath

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

func newBoundTransport(path Path) *http.Transport {
	dialer := &net.Dialer{Timeout: echoTimeout}
	if path.SourceIP != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: path.SourceIP}
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if path.Interface != "" {
				return nil, fmt.Errorf("network_interface binding is supported only on Linux")
			}
			return dialer.DialContext(ctx, network, address)
		},
		Proxy: nil,
	}
}
