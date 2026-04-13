package resilientws

import (
	"context"
	"math/rand/v2"
	"time"
)

func (r *Resws) backoff(attempt int) time.Duration {
	min := r.RecBackoffMin
	max := r.RecBackoffMax

	if min >= max {
		return max
	}

	backoffFactor := r.RecBackoffFactor
	if backoffFactor == 0 {
		backoffFactor = 1.5
	}

	if attempt > 30 {
		attempt = 30
	}

	backoff := r.calculateBackoff(min, max, attempt)
	backoff = time.Duration(float64(backoff) * backoffFactor)
	backoff = r.applyJitter(backoff)

	return r.clampBackoff(backoff, min, max)
}

func (r *Resws) calculateBackoff(min, max time.Duration, attempt int) time.Duration {
	backoff := min
	for i := 0; i < attempt; i++ {
		backoff *= 2
		if backoff > max || backoff < 0 {
			return max
		}
	}
	return backoff
}

func (r *Resws) applyJitter(backoff time.Duration) time.Duration {
	if r.BackoffType == BackoffTypeJitter {
		backoff = time.Duration(float64(backoff) * (1 + 0.1*rand.Float64()))
	}
	return backoff
}

func (r *Resws) clampBackoff(backoff, min, max time.Duration) time.Duration {
	if backoff < min {
		return min
	}
	if backoff > max {
		return max
	}
	return backoff.Round(100 * time.Millisecond)
}

// getReconnectBackoff calculates backoff duration for reconnection attempts
func (r *Resws) getReconnectBackoff() time.Duration {
	r.backoffMu.RLock()
	attempts := r.reconnectAttempts
	r.backoffMu.RUnlock()

	if attempts <= 1 {
		return 0 // First connection attempt, no backoff
	}

	return r.backoff(attempts - 1)
}

// incrementReconnectAttempts increments the reconnection attempt counter
func (r *Resws) incrementReconnectAttempts() {
	r.backoffMu.Lock()
	r.reconnectAttempts++
	r.lastReconnectTime = time.Now()
	r.backoffMu.Unlock()
}

// getReconnectAttempts returns the current reconnection attempt count
func (r *Resws) getReconnectAttempts() int {
	r.backoffMu.RLock()
	defer r.backoffMu.RUnlock()
	return r.reconnectAttempts
}

// resetReconnectAttempts resets the reconnection attempt counter
func (r *Resws) resetReconnectAttempts() {
	r.backoffMu.Lock()
	r.reconnectAttempts = 0
	r.backoffMu.Unlock()
}

// monitorConnectionStability monitors connection stability and resets backoff after stable period
func (r *Resws) monitorConnectionStability(ctx context.Context) {
	timer := time.NewTimer(r.StableConnectionDuration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		if r.IsConnected() {
			r.resetReconnectAttempts()
			if !r.NonVerbose {
				r.Logger.Debug("Connection stable for %v, reset backoff counter", r.StableConnectionDuration)
			}
		}
	}
}
