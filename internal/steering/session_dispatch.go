// internal/steering/session_dispatch.go
package steering

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"
)

// activeClassifierPool is nil unless EnableClassifierSessions() ran (production).
var activeClassifierPool *classifierPool

// EnableClassifierSessions turns on Haiku session reuse for the cheap stages.
// Idempotent. No-op when FLOW_STEERING_SESSION_REUSE=0.
func EnableClassifierSessions() {
	if !steeringEnvBool("FLOW_STEERING_SESSION_REUSE", true) {
		return
	}
	if activeClassifierPool == nil {
		activeClassifierPool = newClassifierPool(sessionMaxTurns(), sessionTTL())
	}
}

// runClassifier dispatches one cheap-stage call: through the active session
// pool when enabled, else the one-shot classifierRunner (byte-identical prompt).
func runClassifier(ctx context.Context, mode, prime, payload, primeKey string) (string, error) {
	if activeClassifierPool != nil {
		return activeClassifierPool.run(ctx, mode, prime, payload, primeKey)
	}
	return classifierRunner(ctx, prime+"\n\n"+payload)
}

// steeringEnvBool reads os.Getenv(key). "1","true","yes","on" → true;
// "0","false","no","off" → false; empty/unknown → def. Case-insensitive.
func steeringEnvBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// sessionMaxTurns returns the max turns from FLOW_STEERING_SESSION_MAX_TURNS,
// defaulting to 40. Non-positive or unparseable values use the default.
func sessionMaxTurns() int {
	v := strings.TrimSpace(os.Getenv("FLOW_STEERING_SESSION_MAX_TURNS"))
	if v == "" {
		return 40
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 40
	}
	return n
}

// sessionTTL returns the session TTL from FLOW_STEERING_SESSION_TTL parsed via
// time.ParseDuration, defaulting to 20 minutes. Unparseable values use the default.
func sessionTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("FLOW_STEERING_SESSION_TTL"))
	if v == "" {
		return 20 * time.Minute
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 20 * time.Minute
	}
	return d
}

// shortHash returns the fnv32a hash of s, hex-encoded, first 12 characters.
// Uses fnv64a (16 hex chars) so slicing to 12 is safe. Used to derive a stable
// primeKey from a taskIndex string.
func shortHash(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())[:12]
}
