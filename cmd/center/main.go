// Command center runs the command center: it receives results from nodes,
// stores them, and serves the web UI. Implementation lands here in later
// tickets; this scaffold only loads and reports its configuration.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/jooservices/speedtest/internal/shared/config"
)

func main() {
	os.Exit(run(config.LoadCenter, os.Stdout, os.Stderr))
}

// run loads the command-center configuration and reports what was loaded. The
// HTTP server and database access are not implemented yet, so it only prints
// what would happen. It returns the process exit code: 0 on success, 1 on an
// invalid configuration.
func run(load func() (config.CenterConfig, error), stdout, stderr io.Writer) int {
	cfg, err := load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "center: invalid configuration: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "center: configuration loaded\n")
	_, _ = fmt.Fprintf(stdout, "center: would listen on %s (HTTP server not implemented yet)\n", cfg.HTTPAddr)
	// The password is intentionally not printed.
	_, _ = fmt.Fprintf(stdout, "center: database host %s port %d\n", cfg.Database.Host, cfg.Database.Port)
	_, _ = fmt.Fprintf(stdout, "center: database name %s user %s\n", cfg.Database.Name, cfg.Database.User)
	_, _ = fmt.Fprintf(stdout, "center: database sslmode %s\n", cfg.Database.SSLMode)

	return 0
}
