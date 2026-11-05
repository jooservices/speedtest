// Package agentid encodes the agent version and platform without changing the legacy version display.
package agentid

import (
	"runtime"
	"strings"
)

// Current identifies this executable in enrollment and control requests.
func Current(version string) string {
	return "speedtest-agent/" + version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// Parse preserves the legacy display string and defaults old agents to linux/amd64.
func Parse(value string) (display, platform string) {
	display, platform = value, "linux-amd64"
	if before, after, ok := strings.Cut(value, " ("); ok {
		display = before
		platform = ""
		if strings.HasSuffix(after, ")") {
			parts := strings.Split(strings.TrimSuffix(after, ")"), "/")
			if len(parts) == 2 && parts[0] == "linux" && (parts[1] == "amd64" || parts[1] == "arm64") {
				platform = parts[0] + "-" + parts[1]
			}
		}
	}
	return display, platform
}
