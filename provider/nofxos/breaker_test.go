package nofxos

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/logger"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeClock is a manually advanced clock for the breaker.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// breakerEnv isolates one test: loopback HTTP, a fake breaker clock, a short cooldown, an
// empty breaker table and a captured logger. Everything is restored on cleanup.
type breakerEnv struct {
	clock *fakeClock
	logs  *syncBuffer
}

const testCooldown = 10 * time.Minute

func newBreakerEnv(t *testing.T) *breakerEnv {
	t.Helper()

	oldGet, oldNow, oldCooldown := httpGet, breakerNow, breakerCooldown
	clock := &fakeClock{now: time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)}
	httpGet = func(u string, timeout time.Duration) (*http.Response, error) {
		// security.SafeGet blocks loopback; the tests only talk to httptest servers.
		return (&http.Client{Timeout: timeout}).Get(u)
	}
	breakerNow = clock.Now
	breakerCooldown = testCooldown

	breakerMu.Lock()
	breakers = map[string]*breakerState{}
	breakerMu.Unlock()

	logs := &syncBuffer{}
	oldOut := logger.Log.Out
	logger.Log.SetOutput(logs)

	t.Cleanup(func() {
		logger.Log.SetOutput(oldOut)
		httpGet, breakerNow, breakerCooldown = oldGet, oldNow, oldCooldown
		breakerMu.Lock()
		breakers = map[string]*breakerState{}
		breakerMu.Unlock()
	})
	return &breakerEnv{clock: clock, logs: logs}
}

// warnings counts how many breaker-trip warnings were logged.
func (e *breakerEnv) warnings() int {
	return strings.Count(e.logs.String(), "NofxOS API disabled")
}

// stubServer serves handler and counts the requests it receives.
func stubServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func writeBody(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const (
	okCoinBody = `{"success":true,"data":{"symbol":"BTC","price":100000.5}}`
)

func deprecatedBody(key string) string {
	return fmt.Sprintf(`{"success":false,"error":"The public API key (%s) has been deprecated. Please create your own key at https://nofxos.ai"}`, key)
}

func TestBreakerTripsOnDeprecatedKey(t *testing.T) {
	env := newBreakerEnv(t)
	const key = "cm_deprecated_key_0001"
	srv, hits := stubServer(t, writeBody(http.StatusOK, deprecatedBody(key)))
	c := NewClient(srv.URL, key)

	_, err := c.GetCoinData("BTC", "")
	if !IsUnavailable(err) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first call: want ErrUnavailable, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("the call that trips the breaker should still expose the HTTP error, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("first call: want 1 HTTP request, got %d", got)
	}

	// Second call is rejected locally.
	_, err = c.GetCoinData("ETH", "")
	if !IsUnavailable(err) {
		t.Fatalf("second call: want ErrUnavailable, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("second call must not hit the network: want 1 request total, got %d", got)
	}
	if c.Available() == nil {
		t.Fatalf("Available() should report the open breaker")
	}

	if env.warnings() != 1 {
		t.Fatalf("want exactly one trip warning, got %d:\n%s", env.warnings(), env.logs.String())
	}
	logs := env.logs.String()
	if !strings.Contains(logs, "deprecated") {
		t.Errorf("warning should include the upstream message:\n%s", logs)
	}
	if strings.Contains(logs, key) {
		t.Errorf("the API key must not be logged:\n%s", logs)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the API key must not appear in the ErrUnavailable message: %v", err)
	}
}

func TestBreakerTripsOnKeyRejections(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"401 empty body", http.StatusUnauthorized, ``},
		{"401 json", http.StatusUnauthorized, `{"success":false,"error":"unauthorized"}`},
		{"403 html", http.StatusForbidden, `<html>Forbidden</html>`},
		{"200 invalid key", http.StatusOK, `{"success":false,"error":"Invalid API key"}`},
		{"200 expired key", http.StatusOK, `{"success":false,"error":"Your key has expired"}`},
		{"200 revoked token", http.StatusOK, `{"success":false,"error":"auth token revoked"}`},
		{"410 deprecated", http.StatusGone, `{"success":false,"error":"The public API key has been DEPRECATED"}`},
		{"200 message field", http.StatusOK, `{"success":false,"message":"api key invalid"}`},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newBreakerEnv(t)
			srv, hits := stubServer(t, writeBody(tc.status, tc.body))
			c := NewClient(srv.URL, fmt.Sprintf("cm_reject_%d", i))

			if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
				t.Fatalf("want ErrUnavailable, got %v", err)
			}
			if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
				t.Fatalf("want ErrUnavailable on second call, got %v", err)
			}
			if got := atomic.LoadInt32(hits); got != 1 {
				t.Fatalf("want 1 HTTP request, got %d", got)
			}
			if env.warnings() != 1 {
				t.Fatalf("want one warning, got %d", env.warnings())
			}
		})
	}
}

func TestBreakerDoesNotTripOnSuccessOrPerRequestErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"success", http.StatusOK, okCoinBody},
		{"unknown symbol 404", http.StatusNotFound, `{"success":false,"error":"coin not found"}`},
		{"invalid symbol 200", http.StatusOK, `{"success":false,"error":"invalid symbol FOO"}`},
		{"rate limited", http.StatusTooManyRequests, `{"success":false,"error":"rate limit exceeded"}`},
		{"server error", http.StatusInternalServerError, `internal error`},
		{"deprecated endpoint without key context", http.StatusOK, `{"success":false,"error":"endpoint deprecated"}`},
		{"success true with key words", http.StatusOK, `{"success":true,"note":"invalid key ignored","data":{}}`},
		{"non-json body", http.StatusOK, `not json at all, key expired?`},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newBreakerEnv(t)
			srv, hits := stubServer(t, writeBody(tc.status, tc.body))
			c := NewClient(srv.URL, fmt.Sprintf("cm_ok_%d", i))

			const calls = 3
			for n := 0; n < calls; n++ {
				_, err := c.doRequest("/api/coin/BTC")
				if IsUnavailable(err) {
					t.Fatalf("call %d: breaker must not trip, got %v", n, err)
				}
			}
			if got := atomic.LoadInt32(hits); got != calls {
				t.Fatalf("every call must reach the server: want %d, got %d", calls, got)
			}
			if c.Available() != nil {
				t.Fatalf("Available() should be nil")
			}
			if env.warnings() != 0 {
				t.Fatalf("no trip warning expected:\n%s", env.logs.String())
			}
		})
	}
}

func TestBreakerReopensAfterCooldown(t *testing.T) {
	env := newBreakerEnv(t)
	const key = "cm_cooldown_key"

	var deprecated atomic.Bool
	deprecated.Store(true)
	srv, hits := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if deprecated.Load() {
			writeBody(http.StatusOK, deprecatedBody(key))(w, r)
			return
		}
		writeBody(http.StatusOK, okCoinBody)(w, r)
	})
	c := NewClient(srv.URL, key)

	if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
		t.Fatalf("want trip, got %v", err)
	}

	// Still closed just before the cooldown ends.
	env.clock.Advance(testCooldown - time.Second)
	if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
		t.Fatalf("breaker should still be open, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("no request while open: want 1, got %d", got)
	}

	// Cooldown over, key still rejected: probe fails and re-trips (second warning).
	env.clock.Advance(2 * time.Second)
	if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
		t.Fatalf("probe against a still-deprecated key should trip again, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("one probe request after the cooldown: want 2 total, got %d", got)
	}
	if env.warnings() != 2 {
		t.Fatalf("want a warning per trip (2), got %d", env.warnings())
	}

	// Upstream recovers: after the next cooldown the client works again.
	deprecated.Store(false)
	env.clock.Advance(testCooldown + time.Second)
	data, err := c.GetCoinData("BTC", "")
	if err != nil {
		t.Fatalf("after recovery: unexpected error %v", err)
	}
	if data == nil || data.Price != 100000.5 {
		t.Fatalf("after recovery: unexpected data %+v", data)
	}
	if c.Available() != nil {
		t.Fatalf("breaker should be closed after a successful probe")
	}
	if _, err := c.GetCoinData("BTC", ""); err != nil {
		t.Fatalf("normal operation after recovery: %v", err)
	}
}

func TestBreakerSharedAcrossClientsAndKeyedByAuthKey(t *testing.T) {
	newBreakerEnv(t)
	srv, hits := stubServer(t, writeBody(http.StatusOK, deprecatedBody(DefaultAuthKey)))

	// A per-engine client built from the default key trips the breaker...
	engineClient := NewClient(srv.URL, DefaultAuthKey)
	if _, err := engineClient.GetCoinData("BTC", ""); !IsUnavailable(err) {
		t.Fatalf("want trip, got %v", err)
	}

	// ...which also disables every other client using that key, DefaultClient() included,
	// without any HTTP request (DefaultClient points at the real host: reaching it would be a
	// network call, so a nil error or a non-ErrUnavailable error fails the test).
	other := NewClient(srv.URL, DefaultAuthKey)
	if _, err := other.GetCoinData("ETH", ""); !IsUnavailable(err) {
		t.Fatalf("second client with the same key: want ErrUnavailable, got %v", err)
	}
	if _, err := DefaultClient().GetCoinData("ETH", ""); !IsUnavailable(err) {
		t.Fatalf("DefaultClient(): want ErrUnavailable, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("want 1 HTTP request in total, got %d", got)
	}

	// A different key is unaffected.
	okSrv, okHits := stubServer(t, writeBody(http.StatusOK, okCoinBody))
	custom := NewClient(okSrv.URL, "cm_user_supplied_key")
	if _, err := custom.GetCoinData("BTC", ""); err != nil {
		t.Fatalf("a client with another key must keep working, got %v", err)
	}
	if atomic.LoadInt32(okHits) != 1 {
		t.Fatalf("expected the custom-key request to reach its server")
	}

	// Switching the key via SetConfig moves the client out of the tripped breaker.
	engineClient.SetConfig(okSrv.URL, "cm_replacement_key")
	if _, err := engineClient.GetCoinData("BTC", ""); err != nil {
		t.Fatalf("after replacing the key the client should work, got %v", err)
	}
}

func TestBreakerConcurrentTripLogsOnce(t *testing.T) {
	env := newBreakerEnv(t)
	const key = "cm_concurrent_key"
	srv, hits := stubServer(t, writeBody(http.StatusOK, deprecatedBody(key)))
	c := NewClient(srv.URL, key)

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
				t.Errorf("want ErrUnavailable, got %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if env.warnings() != 1 {
		t.Fatalf("want exactly one warning under concurrency, got %d", env.warnings())
	}
	if got := atomic.LoadInt32(hits); got < 1 || got > workers {
		t.Fatalf("unexpected request count %d", got)
	}
	before := atomic.LoadInt32(hits)
	if _, err := c.GetCoinData("BTC", ""); !IsUnavailable(err) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if atomic.LoadInt32(hits) != before {
		t.Fatalf("no further HTTP requests once the breaker is open")
	}
}

// Higher-level calls must fail fast and quietly once the breaker is open.
func TestUnavailableShortCircuitsCallers(t *testing.T) {
	env := newBreakerEnv(t)
	const key = "cm_callers_key"
	srv, hits := stubServer(t, writeBody(http.StatusOK, deprecatedBody(key)))
	c := NewClient(srv.URL, key)

	// GetCoinDataBatch: stops at the first ErrUnavailable instead of one request per symbol.
	if got := c.GetCoinDataBatch([]string{"BTC", "ETH", "SOL", "BNB"}, ""); len(got) != 0 {
		t.Fatalf("batch should be empty, got %v", got)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("batch: want 1 request, got %d", got)
	}

	// With the breaker open nothing else may touch the network or the legacy per-call logs.
	start := time.Now()
	if _, err := c.GetOIRanking("1h", 10); !IsUnavailable(err) {
		t.Errorf("GetOIRanking: want ErrUnavailable, got %v", err)
	}
	if _, err := c.GetOITopPositions(); !IsUnavailable(err) {
		t.Errorf("GetOITopPositions: want ErrUnavailable, got %v", err)
	}
	if _, err := c.GetNetFlowRanking("1h", 10); !IsUnavailable(err) {
		t.Errorf("GetNetFlowRanking: want ErrUnavailable, got %v", err)
	}
	if _, err := c.GetPriceRanking("1h", 10); !IsUnavailable(err) {
		t.Errorf("GetPriceRanking: want ErrUnavailable, got %v", err)
	}
	if _, err := c.GetTopRatedCoins(5); !IsUnavailable(err) {
		t.Errorf("GetTopRatedCoins: want ErrUnavailable, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("AI500 must not sleep/retry while the breaker is open (took %v)", elapsed)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("no HTTP while open: want 1 request in total, got %d", got)
	}
	if env.warnings() != 1 {
		t.Errorf("want one warning, got %d", env.warnings())
	}
}

// Without a tripped breaker the first failing AI500 call must not be retried either.
func TestAI500DoesNotRetryOnRejectedKey(t *testing.T) {
	newBreakerEnv(t)
	const key = "cm_ai500_key"
	srv, hits := stubServer(t, writeBody(http.StatusUnauthorized, `{"success":false,"error":"unauthorized"}`))
	c := NewClient(srv.URL, key)

	start := time.Now()
	if _, err := c.GetAI500List(); !IsUnavailable(err) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("want a single request (no retries), got %d", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("no retry sleep expected, took %v", elapsed)
	}
}

func TestSanitizeReason(t *testing.T) {
	got := sanitizeReason("  The public API key (cm_secret) has\n been deprecated  ", "cm_secret")
	if strings.Contains(got, "cm_secret") || !strings.Contains(got, "deprecated") {
		t.Fatalf("unexpected sanitized reason %q", got)
	}
	if got := sanitizeReason("", "k"); got != "no message" {
		t.Fatalf("empty reason: got %q", got)
	}
	long := sanitizeReason(strings.Repeat("x", 5000), "k")
	if len(long) > maxBreakerReason+3 {
		t.Fatalf("reason not truncated: %d bytes", len(long))
	}
}
