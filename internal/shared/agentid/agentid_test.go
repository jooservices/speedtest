package agentid

import (
	"runtime"
	"testing"
)

func TestCurrentAndParse(t *testing.T) {
	if got := Current("1.2.3"); got != "speedtest-agent/1.2.3 ("+runtime.GOOS+"/"+runtime.GOARCH+")" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ input, display, platform string }{
		{"speedtest-agent/1.2.3", "speedtest-agent/1.2.3", "linux-amd64"},
		{"speedtest-agent/1.2.3 (linux/arm64)", "speedtest-agent/1.2.3", "linux-arm64"},
		{"speedtest-agent/1.2.3 (linux/amd64)", "speedtest-agent/1.2.3", "linux-amd64"},
		{"speedtest-agent/1.2.3 (darwin/arm64)", "speedtest-agent/1.2.3", ""},
		{"speedtest-agent/1.2.3 (linux/arm64", "speedtest-agent/1.2.3", ""},
		{"speedtest-agent/1.2.3 (linux)", "speedtest-agent/1.2.3", ""},
	} {
		d, p := Parse(tc.input)
		if d != tc.display || p != tc.platform {
			t.Errorf("%q: %q %q", tc.input, d, p)
		}
	}
}
