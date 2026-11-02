// Command agent is the speed-test node. It can be enrolled once against a
// command center, then run as a background service. A node knows only its own
// configuration and never what it is attached to.
//
// Commands: enroll, run, version, uninstall. The credential and enrollment
// token are secrets and are never logged.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	agentconfig "github.com/jooservices/speedtest/internal/agent/config"
	"github.com/jooservices/speedtest/internal/agent/enroll"
)

// version is the agent version, overridable at build time with
// -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches the CLI subcommand and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		return runVersion(stdout)
	case "enroll":
		return runEnroll(ctx, args[1:], stdout, stderr)
	case "run":
		return runAgent(ctx, args[1:], stdout, stderr)
	case "uninstall":
		return runUninstall(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "agent: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: agent <command> [options]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	_, _ = fmt.Fprintln(w, "  enroll --center <url> --token <token> --name <name> [--host <host>]")
	_, _ = fmt.Fprintln(w, "      enroll this node once and store its credential")
	_, _ = fmt.Fprintln(w, "  run")
	_, _ = fmt.Fprintln(w, "      run the node as a background service")
	_, _ = fmt.Fprintln(w, "  version")
	_, _ = fmt.Fprintln(w, "      print the agent version")
	_, _ = fmt.Fprintln(w, "  uninstall")
	_, _ = fmt.Fprintln(w, "      remove the agent configuration and credential")
}

func runVersion(stdout io.Writer) int {
	_, _ = fmt.Fprintf(stdout, "speedtest-agent %s\n", version)
	return 0
}

func runEnroll(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	center := fs.String("center", "", "command-center URL")
	token := fs.String("token", "", "one-time enrollment token")
	name := fs.String("name", "", "node name")
	host := fs.String("host", "", "optional node host")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "agent enroll: unexpected arguments:", fs.Args())
		return 2
	}

	baseURL, err := url.Parse(*center)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		_, _ = fmt.Fprintln(stderr, "agent enroll: --center must be an absolute http or https URL")
		return 2
	}

	credentialPath := getenv("SPEEDTEST_AGENT_CREDENTIAL", enroll.DefaultCredentialPath)

	client := enroll.Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	result, err := client.Enroll(ctx, enroll.Request{
		Token: *token,
		Name:  *name,
		Host:  *host,
		Agent: "speedtest-agent/" + version,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent enroll: %v\n", err)
		return 1
	}

	if err := enroll.StoreCredential(credentialPath, result.Credential); err != nil {
		_, _ = fmt.Fprintf(stderr, "agent enroll: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "agent: enrolled node %q (%s)\n", *name, result.NodeID)
	_, _ = fmt.Fprintf(stdout, "agent: credential stored at %s\n", credentialPath)
	return 0
}

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "agent run: unexpected arguments:", fs.Args())
		return 2
	}

	logger := slog.New(slog.NewJSONHandler(stdout, nil))

	configPath := getenv("SPEEDTEST_AGENT_CONFIG", agentconfig.DefaultPath)
	credentialPath := getenv("SPEEDTEST_AGENT_CREDENTIAL", enroll.DefaultCredentialPath)

	cfg, err := agentconfig.Load(configPath)
	if err != nil {
		logger.Error("invalid configuration", "config", configPath, "error", err.Error())
		return 1
	}

	// LoadCredential also refuses to start when the file is world-readable.
	if _, err := enroll.LoadCredential(credentialPath); err != nil {
		logger.Error("invalid credential", "credential", credentialPath, "error", err.Error())
		return 1
	}

	logger.Info("agent started",
		"version", version,
		"name", cfg.Name,
		"expected_download_mbps", cfg.ExpectedDownloadMbps,
		"expected_upload_mbps", cfg.ExpectedUploadMbps,
		"interval", cfg.Interval.String(),
	)

	<-ctx.Done()
	logger.Info("agent stopped", "name", cfg.Name)
	return 0
}

func runUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "agent uninstall: unexpected arguments:", fs.Args())
		return 2
	}

	configPath := getenv("SPEEDTEST_AGENT_CONFIG", agentconfig.DefaultPath)
	credentialPath := getenv("SPEEDTEST_AGENT_CREDENTIAL", enroll.DefaultCredentialPath)

	failed := false
	for _, p := range []string{credentialPath, configPath} {
		err := os.Remove(p)
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(stdout, "agent: removed %s\n", p)
		case os.IsNotExist(err):
			// already gone; nothing to do
		default:
			_, _ = fmt.Fprintf(stderr, "agent uninstall: remove %s: %v\n", p, err)
			failed = true
		}
	}
	if failed {
		return 1
	}

	// Best-effort: drop the agent's own directory when it is now empty.
	removeDirIfEmpty(filepath.Dir(credentialPath))
	removeDirIfEmpty(filepath.Dir(configPath))

	_, _ = fmt.Fprintln(stdout, "agent: uninstalled")
	return 0
}

func removeDirIfEmpty(dir string) {
	if dir == "" || dir == "." || dir == string(filepath.Separator) {
		return
	}
	if filepath.Base(filepath.Clean(dir)) != "speedtest-agent" {
		return
	}
	_ = os.Remove(dir)
}

// getenv returns the non-empty value of key, or def when unset or empty.
func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
