package netpath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	echoTimeout = 10 * time.Second
	echoMaxBody = 4096
)

type echoResponse struct {
	Success bool `json:"success"`
	Code    int  `json:"code"`
	Data    struct {
		IP string `json:"ip"`
	} `json:"data"`
}

func (r *Runner) centerPublicIP(ctx context.Context, path Path) (net.IP, error) {
	endpoint := r.centerURL.JoinPath("api", "agent", "ip")
	transport := newBoundTransport(path)
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   echoTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create center IP request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+r.credential)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request center IP: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("center IP endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, echoMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read center IP response: %w", err)
	}
	if len(body) > echoMaxBody {
		return nil, errors.New("center IP response exceeded size limit")
	}
	var envelope echoResponse
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, errors.New("center IP response was invalid JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("center IP response contained trailing data")
	}
	if !envelope.Success || envelope.Code != http.StatusOK {
		return nil, errors.New("center IP response was not a success envelope")
	}
	ip := net.ParseIP(envelope.Data.IP)
	if ip == nil || strings.TrimSpace(envelope.Data.IP) != envelope.Data.IP {
		return nil, errors.New("center IP response did not contain an IP literal")
	}
	return ip, nil
}
