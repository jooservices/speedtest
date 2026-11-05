package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jooservices/speedtest/internal/agent/control"
	"github.com/jooservices/speedtest/internal/agent/enroll"
	"github.com/jooservices/speedtest/internal/agent/update"
	"github.com/jooservices/speedtest/internal/shared/agentid"
)

// updateHTTPClient downloads releases; tests replace it to trust their own TLS server.
var updateHTTPClient = &http.Client{Timeout: 2 * time.Minute}

// runUpdateApply is the privileged step the service unit runs before every start (ExecStartPre=-+). It installs
// a requested update, confirms a good one or rolls a bad one back. It always exits 0 unless its own state
// cannot be saved, and the unit ignores its failure, so a broken update step never keeps the agent from starting.
func runUpdateApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintln(stderr, "agent update-apply: unexpected arguments:", args)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	applier := &update.Applier{
		Paths:   update.DefaultPaths(),
		Version: version,
		Logger:  logger,
		Source:  releaseSource(),
		HTTP:    updateHTTPClient,
	}
	if err := applier.Apply(ctx); err != nil {
		logger.Error("agent update step failed", "error", err.Error())
		return 1
	}
	return 0
}

// releaseSource reads the advertised release from the command center with the node's own credential.
func releaseSource() func(context.Context) (*update.Release, error) {
	return func(ctx context.Context) (*update.Release, error) {
		centerRaw := getenv("SPEEDTEST_AGENT_CENTER", "")
		centerURL, err := url.Parse(centerRaw)
		if err != nil || centerRaw == "" || (centerURL.Scheme != "http" && centerURL.Scheme != "https") || centerURL.Host == "" {
			return nil, fmt.Errorf("SPEEDTEST_AGENT_CENTER is not a valid URL")
		}
		credential, err := enroll.LoadCredential(getenv("SPEEDTEST_AGENT_CREDENTIAL", enroll.DefaultCredentialPath))
		if err != nil {
			return nil, fmt.Errorf("credential unavailable")
		}
		client := control.Client{BaseURL: centerURL, Credential: credential, Agent: agentid.Current(version)}
		cfg, _, _, err := client.Config(ctx, "")
		if err != nil {
			return nil, fmt.Errorf("configuration unavailable")
		}
		return cfg.Release, nil
	}
}
