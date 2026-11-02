// Command agent runs a speed-test node. It loads its own configuration from
// the environment and reports results to the command center. A node knows only
// its own configuration and never what it is attached to.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/jooservices/speedtest/internal/shared/config"
)

func main() {
	os.Exit(run(config.LoadAgent, os.Stdout, os.Stderr))
}

// run loads the node configuration and reports what was loaded. The test loop
// is not implemented yet, so it only prints what would happen. It returns the
// process exit code: 0 on success, 1 on an invalid configuration.
func run(load func() (config.AgentConfig, error), stdout, stderr io.Writer) int {
	cfg, err := load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent: invalid configuration: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "agent: configuration loaded\n")
	_, _ = fmt.Fprintf(stdout, "agent: node %q (test loop not implemented yet)\n", cfg.Name)
	_, _ = fmt.Fprintf(stdout, "agent: expected download %d Mbps\n", cfg.ExpectedDownloadMbps)
	_, _ = fmt.Fprintf(stdout, "agent: expected upload %d Mbps\n", cfg.ExpectedUploadMbps)
	_, _ = fmt.Fprintf(stdout, "agent: would test every %s\n", cfg.Interval)
	_, _ = fmt.Fprintf(stdout, "agent: would report to %s\n", cfg.CenterURL)

	return 0
}
