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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Resws struct {
	// RecBackoffMin is the minimum backoff duration between reconnection attempts
	RecBackoffMin time.Duration

	// RecBackoffMax is the maximum backoff duration between reconnection attempts
	RecBackoffMax time.Duration

	// RecBackoffFactor is the factor by which the backoff duration is multiplied
	RecBackoffFactor float64

	// BackoffType is the type of backoff to use
	BackoffType BackoffType

	// StableConnectionDuration is the duration a connection must be stable before resetting backoff
	StableConnectionDuration time.Duration

	// Handshake timeout
	HandshakeTimeout time.Duration

	// Headers to be sent with the connection
	Headers http.Header

	// Ping interval
	PingInterval time.Duration

	// Pong timeout
	PongTimeout time.Duration

	// Read deadline
	ReadDeadline time.Duration

	// Write deadline
	WriteDeadline time.Duration

	// Message queue size
	MessageQueueSize int

	// TLS configuration
	TLSConfig *tls.Config

	// Proxy configuration
	Proxy func(*http.Request) (*url.URL, error)

	// Logger
	Logger Logger

	// Non-verbose mode
	NonVerbose bool

	// Subscribe handler
	SubscribeHandler func() error

	// Message handler
	MessageHandler func(int, []byte)

	// Ping handler
	PingHandler func()

	httpResp       *http.Response
	mu             sync.RWMutex
	writeMu        sync.Mutex
	messageQueue   [][]byte
	messageQueueMu sync.Mutex
	queueDrainMu   sync.Mutex
	isConnected    bool
	lastConnect    time.Time
	lastErr        error
	lifecycleMu    sync.Mutex
	session        *dialSession
	connCancel     context.CancelFunc
	reconnect      chan struct{}

	// Backoff state that persists across reconnections
	reconnectAttempts int
	lastReconnectTime time.Time
	backoffMu         sync.RWMutex

	// Event handlers
	onReconnectingFn func(time.Duration)
	onConnectedFn    func(string)
	onErrorFn        func(error)

	*websocket.Conn
}

type dialSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	ready  chan struct{}
}

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

type defaultLogger struct{}

func (l *defaultLogger) Debug(msg string, args ...any) {
	log.Printf("DEBUG: "+msg, args...)
}

func (l *defaultLogger) Info(msg string, args ...any) {
	log.Printf("INFO: "+msg, args...)
}

func (l *defaultLogger) Error(msg string, args ...any) {
	log.Printf("ERROR: "+msg, args...)
}

type BackoffType int

const (
	BackoffTypeJitter BackoffType = iota
	BackoffTypeFixed
)

type Event struct {
	Type        EventType
	Message     []byte
	MessageType int
	Data        any
	Error       error
}

type EventType int

const (
	EventMessage EventType = iota
	EventConnected
	EventReconnecting
	EventError
	EventClose
)

var errNotConnected = errors.New("websocket: not connected")

// setDefaultConfig sets the default configuration for the WebSocket client
func (r *Resws) setDefaultConfig() {
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
	r.mu.RLock()
	connected, reconnecting, onError := r.onConnectedFn, r.onReconnectingFn, r.onErrorFn
	r.mu.RUnlock()
	switch event.Type {
	case EventConnected:
		if connected != nil {
			connected(event.Data.(string))
		}
	case EventReconnecting:
		if reconnecting != nil {
			reconnecting(event.Data.(time.Duration))
		}
	case EventError:
		if onError != nil {
			onError(event.Error)
		}
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

// Close stops reconnecting and closes the current socket. It can be called
// from a callback and does not wait for callbacks already running.
func (r *Resws) Close() {
	r.mu.Lock()
	if r.session != nil {
		r.session.cancel()
	}
	r.detachSocketLocked()
	r.mu.Unlock()
}

// detachSocketLocked closes the owned socket before the next handshake can start.
func (r *Resws) detachSocketLocked() {
	if r.connCancel != nil {
		r.connCancel()
		r.connCancel = nil
	}
	if conn := r.Conn; conn != nil {
		deadline := time.Now().Add(250 * time.Millisecond)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
		_ = conn.Close()
	}
	r.Conn = nil
	r.reconnect = nil
	r.isConnected = false
}

// CloseAndReconnect schedules a single replacement for the current connection.
func (r *Resws) CloseAndReconnect() {
	r.requestReconnect(nil)
}

func (r *Resws) requestReconnect(conn *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reconnect == nil || (conn != nil && conn != r.Conn) {
		return
	}
	select {
	case r.reconnect <- struct{}{}:
	default:
	}
}

// Dial starts a connection lifecycle if one is not already running.
// It waits for the first successful connection, cancellation, or the handshake timeout.
func (r *Resws) Dial(rawURL string) {
	urlStr, err := r.parseURL(rawURL)
	if err != nil {
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		r.emitEvent(Event{Type: EventError, Error: err})
		return
	}

	r.mu.Lock()
	s := r.session
	if s == nil || s.ctx.Err() != nil {
		r.setDefaultConfig()
		ctx, cancel := context.WithCancel(context.Background())
		s = &dialSession{ctx: ctx, cancel: cancel, ready: make(chan struct{})}
		r.session = s
		dialer := websocket.Dialer{
			TLSClientConfig:  r.TLSConfig,
			Proxy:            r.Proxy,
			HandshakeTimeout: r.HandshakeTimeout,
		}
		go r.connect(s, urlStr, dialer)
	}
	timeout := r.HandshakeTimeout
	r.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.ready:
	case <-s.ctx.Done():
	}
}

// dialSocket cancels the underlying transport even while Gorilla is waiting
// for the HTTP upgrade response after TCP is established.
func dialSocket(ctx context.Context, dialer websocket.Dialer, target string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	var stop func() bool
	var netDialer net.Dialer
	dialer.NetDialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := netDialer.DialContext(dialCtx, network, address)
		if err == nil {
			stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
		}
		return conn, err
	}
	conn, resp, err := dialer.DialContext(ctx, target, headers)
	if stop != nil {
		stop()
	}
	return conn, resp, err
}

// connect owns all handshakes and socket replacement for a session. A fresh
// Dial waits for a canceled session to finish before starting its own handshake.
func (r *Resws) connect(s *dialSession, target string, dialer websocket.Dialer) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	nextDelay := r.RecBackoffMin
	attempt := 0
	ready := s.ready
	for s.ctx.Err() == nil {
		conn, resp, err := dialSocket(s.ctx, dialer, target, r.Headers)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			r.mu.Lock()
			r.httpResp = resp
			r.lastErr = err
			r.mu.Unlock()
			r.emitEvent(Event{Type: EventError, Error: err})
			if !r.waitReconnect(s.ctx, nextDelay) {
				return
			}
			nextDelay = r.backoff(attempt)
			attempt++
			continue
		}

		r.mu.Lock()
		if s.ctx.Err() != nil {
			r.mu.Unlock()
			_ = conn.Close()
			return
		}
		connCtx, cancel := context.WithCancel(s.ctx)
		reconnect := make(chan struct{}, 1)
		r.connCancel = cancel
		r.reconnect = reconnect
		r.Conn = conn
		r.lastConnect = time.Now()
		r.isConnected = true

		go r.monitorConnectionStability(connCtx)
		go r.processMessageQueue(connCtx, conn, target)
		if r.PingHandler != nil {
			go r.heartbeat(connCtx, conn)
		}
		if r.MessageHandler != nil {
			go r.reader(connCtx, conn)
		}
		if ready != nil {
			close(ready)
			ready = nil
		}
		r.mu.Unlock()

		if !r.NonVerbose {
			r.Logger.Info("Connection was successfully established: %s", target)
		}
		go func() {
			if connCtx.Err() != nil {
				return
			}
			r.emitEvent(Event{Type: EventConnected, Data: target})
			if r.SubscribeHandler != nil && r.retrySubscribeHandler(connCtx) != nil && connCtx.Err() == nil {
				r.requestReconnect(conn)
			}
		}()

		select {
		case <-s.ctx.Done():
			return
		case <-reconnect:
		}

		r.mu.Lock()
		if r.Conn == conn {
			r.detachSocketLocked()
		}
		r.mu.Unlock()

		r.incrementReconnectAttempts()
		if !r.waitReconnect(s.ctx, r.getReconnectBackoff()) {
			return
		}
		nextDelay, attempt = r.RecBackoffMin, 0
	}
}

func (r *Resws) waitReconnect(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if delay <= 0 {
		return true
	}
	if !r.NonVerbose {
		r.Logger.Info("Will reconnect in %v", delay)
	}
	r.emitEvent(Event{Type: EventReconnecting, Data: delay})
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Resws) retrySubscribeHandler(ctx context.Context) error {
	r.mu.RLock()
	backoff := r.RecBackoffMin
	max := r.RecBackoffMax
	r.mu.RUnlock()

	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := r.SubscribeHandler(); err != nil {
			r.Logger.Error("Subscribe handler failed: %v", err)
			r.emitEvent(Event{Type: EventError, Error: err})

			if backoff >= max {
				return fmt.Errorf("subscribe handler failed after max retries: %w", err)
			}

			if !r.NonVerbose {
				r.Logger.Info("Retrying subscribe handler in %v", backoff)
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}

			backoff = r.backoff(attempt)
			attempt++
		} else {
			if !r.NonVerbose {
				r.Logger.Info("Subscribe handler executed successfully")
			}
			return nil
		}
	}
}

// write serializes Gorilla's data writes and deadline updates.
func (r *Resws) write(conn *websocket.Conn, fn func() error) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.WriteDeadline > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(r.WriteDeadline)); err != nil {
			return err
		}
	}
	return fn()
}

// Send sends a message to the WebSocket server with a fallback queue
func (r *Resws) Send(msg []byte) error {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()

	// If we have a connection, try to send directly
	if conn != nil {
		err := r.write(conn, func() error { return conn.WriteMessage(websocket.TextMessage, msg) })
		if err == nil {
			return nil
		}
	}

	// If no connection or send failed, queue the message
	r.messageQueueMu.Lock()
	if len(r.messageQueue) >= r.MessageQueueSize {
		r.messageQueueMu.Unlock()
		return fmt.Errorf("message queue is full")
	}
	r.messageQueue = append(r.messageQueue, msg)
	r.messageQueueMu.Unlock()

	r.logQueuedMessage()
	return nil
}

func (r *Resws) SendJSON(v any) (err error) {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()

	// If we have a connection, try to send directly
	if conn != nil {
		err = r.write(conn, func() error { return conn.WriteJSON(v) })
		if err == nil {
			return nil
		}
	}

	// If no connection or send failed, queue the message
	r.messageQueueMu.Lock()
	if len(r.messageQueue) >= r.MessageQueueSize {
		r.messageQueueMu.Unlock()
		return fmt.Errorf("message queue is full")
	}
	b, err := json.Marshal(v)
	if err != nil {
		r.messageQueueMu.Unlock()
		return err
	}
	r.messageQueue = append(r.messageQueue, b)
	r.messageQueueMu.Unlock()

	r.logQueuedMessage()
	return nil
}

func (r *Resws) logQueuedMessage() {
	r.mu.RLock()
	logger := r.Logger
	r.mu.RUnlock()
	if logger == nil {
		logger = &defaultLogger{}
	}
	logger.Debug("Message queued for later delivery")
}

// processMessageQueue processes messages from the queue and sends them to the WebSocket server
func (r *Resws) processMessageQueue(ctx context.Context, conn *websocket.Conn, target string) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.flushQueueHead(ctx, conn); err != nil {
				r.requestReconnect(conn)
				r.Logger.Error("[%s] Failed to send queued message; retaining for reconnect: %v", target, err)
				return
			}
		}
	}
}

// Keep the head in the queue until it is written. During a reconnect only
// one worker can advance it, even if the old worker is finishing a write.
func (r *Resws) flushQueueHead(ctx context.Context, conn *websocket.Conn) error {
	r.queueDrainMu.Lock()
	defer r.queueDrainMu.Unlock()
	if ctx.Err() != nil {
		return nil
	}

	r.messageQueueMu.Lock()
	if len(r.messageQueue) == 0 {
		r.messageQueueMu.Unlock()
		return nil
	}
	msg := r.messageQueue[0]
	r.messageQueueMu.Unlock()

	if err := r.write(conn, func() error { return conn.WriteMessage(websocket.TextMessage, msg) }); err != nil {
		return err
	}
	r.messageQueueMu.Lock()
	r.messageQueue[0] = nil
	r.messageQueue = r.messageQueue[1:]
	r.messageQueueMu.Unlock()
	return nil
}

// reader loops and reads messages from the WebSocket connection and emits them as events
func (r *Resws) reader(ctx context.Context, conn *websocket.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if r.ReadDeadline > time.Duration(0) {
				conn.SetReadDeadline(time.Now().Add(r.ReadDeadline))
			}
			msgType, msg, err := conn.ReadMessage()
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation) {
				return
			}
			if err != nil {
				if ctx.Err() == nil {
					r.emitEvent(Event{Type: EventError, Error: err})
					r.requestReconnect(conn)
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
			switch msgType {
			case websocket.TextMessage, websocket.BinaryMessage:
				r.MessageHandler(msgType, msg)
			case websocket.CloseMessage:
				return
			}
		}
	}
}

// ReadMessage manually reads a message from the websocket connection
func (r *Resws) ReadMessage() (msgType int, msg []byte, err error) {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		err = errNotConnected
		return
	}
	msgType, msg, err = conn.ReadMessage()
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return msgType, msg, nil
	}
	if err != nil {
		r.requestReconnect(conn)
	}

	return
}

// ReadJSON manually reads a JSON message from the websocket connection
func (r *Resws) ReadJSON(v any) (err error) {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		err = errNotConnected
		return
	}
	err = conn.ReadJSON(v)
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}
	if err != nil {
		r.requestReconnect(conn)
	}

	return
}

// WriteMessage manually writes a message to the websocket connection
func (r *Resws) WriteMessage(msgType int, msg []byte) (err error) {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		return errNotConnected
	}
	err = r.write(conn, func() error { return conn.WriteMessage(msgType, msg) })
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}

	return
}

// WriteJSON manually writes a JSON message to the websocket connection
func (r *Resws) WriteJSON(v any) (err error) {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		return errNotConnected
	}
	err = r.write(conn, func() error { return conn.WriteJSON(v) })
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}

	return
}

// heartbeat sends ping messages to the server to keep the connection alive
func (r *Resws) heartbeat(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(r.PingInterval)
	defer ticker.Stop()

	if conn != nil && r.PongTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
		conn.SetPongHandler(func(appData string) error {
			_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
			return nil
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(r.PingInterval)); err != nil {
				return
			}
			r.PingHandler()
		}
	}
}

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
	backoff := min * time.Duration(1<<attempt)
	backoff = time.Duration(float64(backoff) * backoffFactor)
	if r.BackoffType == BackoffTypeJitter {
		backoff = time.Duration(float64(backoff) * (1 + 0.1*rand.Float64()))
	}

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
		// Connection has been stable, reset reconnect attempts
		if r.IsConnected() {
			r.resetReconnectAttempts()
			if !r.NonVerbose {
				r.Logger.Debug("Connection stable for %v, reset backoff counter", r.StableConnectionDuration)
			}
		}
	}
}
