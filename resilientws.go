/*
 * The MIT License (MIT)
 *
 * Copyright (c) 2025 ipanardian, <github.com/ipanardian>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, Subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or Substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

// Package resilientws provides a resilient WebSocket client that automatically reconnects and re-subscribes when the connection is lost
package resilientws

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Dial establishes a connection to the WebSocket server
func (r *Resws) Dial(urlStr string) {
	r.ctx, r.cancel = context.WithCancel(context.Background())

	parsed, err := r.parseURL(urlStr)
	if err != nil {
		r.lastErr = err
		r.emitEvent(Event{Type: EventError, Error: err})
		return
	}

	r.mu.Lock()
	r.connectedCh = make(chan struct{}, 1)
	r.connOnce = new(sync.Once)
	r.closeOnce = sync.Once{}
	r.mu.Unlock()

	r.setURL(parsed)
	r.setDefaultConfig()

	go r.connect()

	timer := time.NewTimer(r.getHandshakeTimeout())
	defer timer.Stop()

	r.mu.RLock()
	connectedCh := r.connectedCh
	r.mu.RUnlock()

	select {
	case <-timer.C:
		return
	case <-connectedCh:
		return
	}
}

// OnError sets the error handler
func (r *Resws) OnError(fn func(err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onErrorFn = fn
}

// OnConnected sets the connected handler
func (r *Resws) OnConnected(fn func(url string)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onConnectedFn = fn
}

// OnReconnecting sets the reconnection handler
func (r *Resws) OnReconnecting(fn func(time.Duration)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onReconnectingFn = fn
}

// Close closes the connection
func (r *Resws) Close() {
	r.closeOnce.Do(func() {
		// Prevent any future reconnect attempts
		r.mu.Lock()
		r.shouldReconnect = false
		conn := r.Conn
		r.Conn = nil
		connCancel := r.connCancel
		r.connCancel = nil
		cancel := r.cancel
		r.mu.Unlock()

		if cancel != nil {
			cancel()
		}

		if connCancel != nil {
			connCancel()
		}

		if conn != nil {
			// Attempt graceful close
			deadline := time.Now().Add(250 * time.Millisecond)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		}

		r.connWg.Wait()

		r.setIsConnected(false)
		r.emitEvent(Event{Type: EventClose})
	})
}

// CloseAndReconnect closes the connection and starts a reconnection attempt with backoff
func (r *Resws) CloseAndReconnect() {
	r.mu.Lock()
	conn := r.Conn
	r.Conn = nil
	if r.connCancel != nil {
		r.connCancel()
	}
	r.connectedCh = make(chan struct{}, 1)
	r.connOnce = new(sync.Once)
	r.mu.Unlock()

	if conn != nil {
		// Attempt graceful close outside the lock — WriteControl is concurrent-safe
		deadline := time.Now().Add(250 * time.Millisecond)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close()
	}

	r.setIsConnected(false)

	// Wait for the connection to close
	time.Sleep(100 * time.Millisecond)

	// Increment reconnect attempts for this reconnection
	r.incrementReconnectAttempts()

	// Apply backoff before reconnecting
	backoffDuration := r.getReconnectBackoff()
	if backoffDuration > 0 {
		if !r.NonVerbose {
			r.Logger.Info("Will reconnect in %v (attempt %d)", backoffDuration, r.getReconnectAttempts())
		}
		if r.onReconnectingFn != nil {
			r.emitEvent(Event{Type: EventReconnecting, Data: backoffDuration})
		}
		time.Sleep(backoffDuration)
	}

	go r.connect()
}

// IsConnected returns the connection state
func (r *Resws) IsConnected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.isConnected
}

// LastConnectTime returns the last connection time
func (r *Resws) LastConnectTime() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.lastConnect
}

// LastError returns the last error
func (r *Resws) LastError() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.lastErr
}

// GetHTTPResponse returns the HTTP response
func (r *Resws) GetHTTPResponse() *http.Response {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.httpResp
}

// setDefaultConfig sets the default configuration for the WebSocket client
func (r *Resws) setDefaultConfig() {
	// shouldReconnect is a flag to prevent reconnecting when Close() is called
	r.shouldReconnect = true

	if r.RecBackoffMin == 0 {
		r.RecBackoffMin = 1000 * time.Millisecond
	}
	if r.RecBackoffMax == 0 {
		r.RecBackoffMax = 30 * time.Second
	}
	if r.RecBackoffFactor == 0 {
		r.RecBackoffFactor = 1.5
	}
	if r.HandshakeTimeout == 0 {
		r.HandshakeTimeout = 2 * time.Second
	}
	if r.StableConnectionDuration == 0 {
		r.StableConnectionDuration = 30 * time.Second
	}
	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	if r.PingInterval == 0 {
		r.PingInterval = 15 * time.Second
	}
	r.dialer = &websocket.Dialer{
		TLSClientConfig:  r.TLSConfig,
		Proxy:            r.Proxy,
		HandshakeTimeout: r.getHandshakeTimeout(),
	}
}

// setURL sets the URL of the WebSocket server
func (r *Resws) setURL(urlStr string) {
	r.url = urlStr
}

// parseURL parses the URL of the WebSocket server
func (r *Resws) parseURL(urlStr string) (string, error) {
	if strings.TrimSpace(urlStr) == "" {
		return "", fmt.Errorf("url cannot be empty")
	}

	if len(urlStr) < 5 {
		return "", fmt.Errorf("url too short")
	}

	u, err := url.Parse(urlStr)

	if err != nil {
		return "", fmt.Errorf("url: %s", err.Error())
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("url: websocket uris must start with ws or wss scheme")
	}

	if u.User != nil {
		return "", fmt.Errorf("url: user name and password are not allowed in websocket URIs")
	}

	return urlStr, nil
}

// emitEvent emits an event to the event handlers
func (r *Resws) emitEvent(event Event) {
	switch event.Type {
	case EventConnected:
		if r.onConnectedFn != nil {
			r.onConnectedFn(r.url)
		}
	case EventReconnecting:
		if r.onReconnectingFn != nil {
			if d, ok := event.Data.(time.Duration); ok {
				r.onReconnectingFn(d)
			}
		}
	case EventError:
		if r.onErrorFn != nil {
			r.onErrorFn(event.Error)
		}
	}
}

// setIsConnected sets the connection state
func (r *Resws) setIsConnected(isConnected bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.isConnected = isConnected
}

// signalConnected signals the connection state
func (r *Resws) signalConnected() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.connOnce.Do(func() {
		select {
		case r.connectedCh <- struct{}{}:
		default:
		}
	})
}

// getHandshakeTimeout returns the handshake timeout
func (r *Resws) getHandshakeTimeout() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.HandshakeTimeout
}
