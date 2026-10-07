// Package testutil holds small helpers shared by tests across the repo.
package testutil

import (
	"os"
	"testing"
)

// LiveEnvVar is the environment variable that opts in to live-network tests.
const LiveEnvVar = "NOFX_LIVE_TESTS"

// LiveEnabled reports whether live-network tests were explicitly enabled
// (NOFX_LIVE_TESTS=1).
func LiveEnabled() bool {
	return os.Getenv(LiveEnvVar) == "1"
}

// RequireLive skips the calling test unless NOFX_LIVE_TESTS=1.
//
// Use it as the first statement of every test that talks to a real exchange,
// data provider or LLM endpoint. CI runs `go test -race ./...` without -short,
// so such tests must be opt-in to keep the default run hermetic and non-flaky.
// Run them with `make test-live` or `NOFX_LIVE_TESTS=1 go test ...`.
func RequireLive(t testing.TB) {
	t.Helper()
	if !LiveEnabled() {
		t.Skipf("live network test: set %s=1 to run", LiveEnvVar)
	}
}
