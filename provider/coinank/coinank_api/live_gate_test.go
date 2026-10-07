package coinank_api

import (
	"os"
	"testing"
)

// requireLiveTests skips a test unless NOFX_LIVE_TESTS=1 is set. It guards tests
// that need real network access, third-party API keys or live market data: CI
// runs `go test -race ./...` hermetically, so such tests are opt-in.
func requireLiveTests(t *testing.T) {
	t.Helper()
	if os.Getenv("NOFX_LIVE_TESTS") != "1" {
		t.Skip("live network test: set NOFX_LIVE_TESTS=1 to run")
	}
}
