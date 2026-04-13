package resilientws

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// connect establishes a connection to the WebSocket server
func (r *Resws) connect() {
	r.cancelExistingConn()
	r.connWg.Wait()
	r.setupConnContext()

	recBackoff := r.getRecBackoffMin()
	attempt := 0

	for {
		select {
		case <-r.ctx.Done():
			return
		default:
			conn, resp, err := r.dialer.Dial(r.url, r.Headers)
			if err != nil {
				r.handleDialFailure(resp, err, recBackoff)
				recBackoff = r.backoff(attempt)
				attempt++
				continue
			}

			if r.handleConnectionSuccess(conn) {
				return
			}
		}
	}
}

func (r *Resws) cancelExistingConn() {
	r.mu.Lock()
	if r.connCancel != nil {
		r.connCancel()
	}
	r.mu.Unlock()
}

func (r *Resws) setupConnContext() {
	r.mu.Lock()
	r.connCtx, r.connCancel = context.WithCancel(r.ctx)
	r.mu.Unlock()
}

func (r *Resws) getRecBackoffMin() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.RecBackoffMin
}

func (r *Resws) handleConnectionSuccess(conn *websocket.Conn) bool {
	r.mu.Lock()
	r.Conn = conn
	r.lastConnect = time.Now()
	r.mu.Unlock()

	if !r.NonVerbose {
		r.Logger.Info("Connection was successfully established: %s", r.url)
	}

	r.signalConnected()
	r.setIsConnected(true)
	r.startConnectionWorkers()
	r.emitEvent(Event{Type: EventConnected})

	return r.retrySubscribeIfNeeded()
}

func (r *Resws) startConnectionWorkers() {
	r.connWg.Add(1)
	go func() {
		defer r.connWg.Done()
		r.monitorConnectionStability(r.connCtx)
	}()

	r.connWg.Add(1)
	go func() {
		defer r.connWg.Done()
		r.processMessageQueue(r.connCtx)
	}()

	if r.PingHandler != nil {
		r.connWg.Add(1)
		go func() {
			defer r.connWg.Done()
			r.heartbeat(r.connCtx)
		}()
	}

	if r.MessageHandler != nil {
		r.connWg.Add(1)
		go func() {
			defer r.connWg.Done()
			r.reader(r.connCtx)
		}()
	}
}

func (r *Resws) retrySubscribeIfNeeded() bool {
	if r.SubscribeHandler == nil {
		return true
	}

	if err := r.retrySubscribeHandler(); err != nil {
		r.CloseAndReconnect()
		return false
	}

	return true
}

func (r *Resws) handleDialFailure(resp *http.Response, err error, backoff time.Duration) {
	r.mu.Lock()
	r.Conn = nil
	r.httpResp = resp
	r.lastErr = err
	r.shouldReconnect = true
	r.mu.Unlock()

	r.setIsConnected(false)

	if r.onErrorFn != nil {
		r.emitEvent(Event{Type: EventError, Error: err})
	}
	if !r.NonVerbose {
		r.Logger.Info("Will reconnect in %v", backoff)
	}
	if r.onReconnectingFn != nil {
		r.emitEvent(Event{Type: EventReconnecting, Data: backoff})
	}

	time.Sleep(backoff)
}

func (r *Resws) retrySubscribeHandler() error {
	_, max := r.getSubscribeBackoffConfig()
	attempt := 0

	for {
		select {
		case <-r.connCtx.Done():
			return r.connCtx.Err()
		default:
		}

		if err := r.SubscribeHandler(); err != nil {
			nextBackoff := r.backoff(attempt)
			if retryErr := r.handleSubscribeError(err, nextBackoff, max, attempt); retryErr != nil {
				return retryErr
			}
			attempt++
			continue
		}

		if !r.NonVerbose {
			r.Logger.Info("Subscribe handler executed successfully")
		}
		return nil
	}
}

func (r *Resws) getSubscribeBackoffConfig() (min, max time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.RecBackoffMin, r.RecBackoffMax
}

func (r *Resws) handleSubscribeError(err error, backoff, max time.Duration, attempt int) error {
	r.Logger.Error("Subscribe handler failed: %v", err)
	r.emitEvent(Event{Type: EventError, Error: err})

	if attempt > 0 && backoff >= max {
		return fmt.Errorf("subscribe handler failed after max retries: %w", err)
	}

	if !r.NonVerbose {
		r.Logger.Info("Retrying subscribe handler in %v", backoff)
	}

	select {
	case <-r.connCtx.Done():
		return r.connCtx.Err()
	case <-time.After(backoff):
		return nil
	}
}
