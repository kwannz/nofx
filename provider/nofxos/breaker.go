package nofxos

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// BreakerCooldown is how long the client stays disabled after the upstream
// rejects the API key (revoked / deprecated / invalid / expired). While the
// breaker is open every request fails immediately with ErrUnavailable and no
// HTTP traffic is generated.
const BreakerCooldown = 30 * time.Minute

// maxBreakerReason caps the upstream message kept in the breaker state and
// quoted in the (single) warning log line.
const maxBreakerReason = 300

// ErrUnavailable is the sentinel for "the NofxOS API is disabled by the
// circuit breaker". The concrete error returned by the client is an
// *UnavailableError, so test with errors.Is(err, ErrUnavailable) or
// IsUnavailable(err); the sentinel also survives fmt.Errorf("...: %w", err).
var ErrUnavailable = errors.New("nofxos: API unavailable")

// UnavailableError is returned instead of performing an HTTP request while the
// breaker for the client's auth key is open (and by the call that tripped it).
type UnavailableError struct {
	// Reason is the (key-redacted) upstream message that tripped the breaker.
	Reason string
	// Until is when the breaker closes and requests are attempted again.
	Until time.Time
	// Cause is the underlying HTTP error for the call that tripped the breaker
	// (nil for calls rejected by an already-open breaker).
	Cause error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s: upstream rejected the API key (%s); disabled until %s",
		ErrUnavailable.Error(), e.Reason, e.Until.Format("15:04:05"))
}

// Is makes errors.Is(err, ErrUnavailable) true.
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Unwrap exposes the HTTP error of the call that tripped the breaker.
func (e *UnavailableError) Unwrap() error { return e.Cause }

// IsUnavailable reports whether err (or anything it wraps) is ErrUnavailable.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// breakerState is one auth key's circuit breaker.
type breakerState struct {
	until  time.Time
	reason string
}

var (
	// breakers is shared by every Client, keyed by auth key, so that
	// DefaultClient() and the per-engine clients built from the same key trip
	// and recover together.
	breakerMu sync.Mutex
	breakers  = map[string]*breakerState{}

	// Test seams.
	breakerNow      = time.Now
	breakerCooldown = BreakerCooldown
)

// Available returns nil when requests may be attempted, or an
// *UnavailableError (matching ErrUnavailable) while the breaker for this
// client's auth key is open. It never performs I/O.
func (c *Client) Available() error {
	return checkBreaker(c.GetAuthKey())
}

func checkBreaker(authKey string) error {
	breakerMu.Lock()
	defer breakerMu.Unlock()
	st, ok := breakers[authKey]
	if !ok {
		return nil
	}
	if !breakerNow().Before(st.until) {
		delete(breakers, authKey) // cooldown elapsed: half-open, let the next call probe
		return nil
	}
	return &UnavailableError{Reason: st.reason, Until: st.until}
}

// tripBreaker opens the breaker for authKey and returns the error for the call
// that observed the rejection. Only the transition closed -> open logs (one
// warning); concurrent in-flight failures that arrive while it is already open
// neither log nor extend the cooldown.
func tripBreaker(authKey, reason string, cause error) error {
	reason = sanitizeReason(reason, authKey)

	breakerMu.Lock()
	now := breakerNow()
	st, open := breakers[authKey]
	if open && now.Before(st.until) {
		err := &UnavailableError{Reason: st.reason, Until: st.until, Cause: cause}
		breakerMu.Unlock()
		return err
	}
	until := now.Add(breakerCooldown)
	breakers[authKey] = &breakerState{until: until, reason: reason}
	breakerMu.Unlock()

	logger.Warnf("⚠️  NofxOS API disabled for %s: upstream rejected the API key: %s. "+
		"Quant data, market-wide rankings and AI500/OI-top coin sources are unavailable until the key is replaced "+
		"(Strategy Studio -> Indicators -> NofxOS API key).", breakerCooldown, reason)
	return &UnavailableError{Reason: reason, Until: until, Cause: cause}
}

// sanitizeReason removes the auth key from an upstream message and bounds its size.
func sanitizeReason(reason, authKey string) string {
	reason = strings.TrimSpace(reason)
	if authKey != "" {
		reason = strings.ReplaceAll(reason, authKey, "[redacted]")
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > maxBreakerReason {
		reason = reason[:maxBreakerReason] + "..."
	}
	if reason == "" {
		reason = "no message"
	}
	return reason
}

// upstreamEnvelope is the common error envelope: {"success":false,"error":"..."}.
type upstreamEnvelope struct {
	Success *bool       `json:"success"`
	Error   interface{} `json:"error"`
	Message interface{} `json:"message"`
}

// keyRejection inspects a response and reports whether the upstream rejected
// the API key itself (as opposed to a per-request failure such as an unknown
// symbol), returning the upstream message when it did.
//
//   - HTTP 401/403 always means a rejected key.
//   - Any other response whose JSON body is {"success":false, "error":"..."} trips
//     only when the message talks about the key being deprecated / invalid /
//     expired / revoked. Requiring "key" (or auth/token) in the message keeps
//     per-request errors such as "invalid symbol" from disabling the client.
func keyRejection(status int, body []byte) (string, bool) {
	var env upstreamEnvelope
	msg := ""
	if err := json.Unmarshal(body, &env); err == nil {
		msg = envelopeMessage(env)
	}

	if status == 401 || status == 403 {
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", status)
		}
		return msg, true
	}

	if env.Success == nil || *env.Success || msg == "" {
		return "", false
	}
	lower := strings.ToLower(msg)
	if containsAny(lower, "deprecated", "invalid", "expired", "revoked") &&
		containsAny(lower, "key", "auth", "token") {
		return msg, true
	}
	return "", false
}

func envelopeMessage(env upstreamEnvelope) string {
	if s := stringify(env.Error); s != "" {
		return s
	}
	return stringify(env.Message)
}

func stringify(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
