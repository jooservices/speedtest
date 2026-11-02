package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
)

var fakeBuild struct {
	sync.Once
	image []byte
}

// BuildFakeSpeedtest compiles the fake once per test process and returns an
// executable owned by t. Each caller gets its own cleaned-up temporary copy,
// so one test's cleanup cannot invalidate another test's executable.
func BuildFakeSpeedtest(t testing.TB) string {
	t.Helper()
	fakeBuild.Do(func() {
		_, source, _, _ := runtime.Caller(0)
		root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
		output := filepath.Join(t.TempDir(), "fake-speedtest")
		cmd := exec.Command("go", "build", "-o", output, "./internal/agent/testdata/fake-speedtest")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build fake-speedtest: %v\n%s", err, out)
		}
		var err error
		fakeBuild.image, err = os.ReadFile(output)
		if err != nil {
			t.Fatalf("read fake-speedtest: %v", err)
		}
	})
	output := filepath.Join(t.TempDir(), "fake-speedtest")
	// Exclude concurrent fork while the executable is open for writing. A child
	// can otherwise briefly inherit the writable descriptor before exec closes it.
	syscall.ForkLock.RLock()
	err := os.WriteFile(output, fakeBuild.image, 0700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatalf("write fake-speedtest: %v", err)
	}
	return output
}

// RunFakeSpeedtest runs a scenario with optional CLI value overrides. The caller
// controls timeout/cancellation through ctx; failures return stdout and an error.
func RunFakeSpeedtest(t testing.TB, ctx context.Context, scenario string, overrides ...string) ([]byte, error) {
	t.Helper()
	args := append([]string{"--scenario", scenario}, overrides...)
	return exec.CommandContext(ctx, BuildFakeSpeedtest(t), args...).Output()
}
