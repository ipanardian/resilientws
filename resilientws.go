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

	url             string
	dialer          *websocket.Dialer
	httpResp        *http.Response
	mu              sync.RWMutex
	writeMu         sync.Mutex
	messageQueue    [][]byte
	messageQueueMu  sync.Mutex
	isConnected     bool
	lastConnect     time.Time
	lastErr         error
	shouldReconnect bool
	active          bool
	reconnecting    bool
	connectedCh     chan struct{}
	connOnce        *sync.Once
	closeOnce       sync.Once

	// Backoff state that persists across reconnections
	reconnectAttempts int
	lastReconnectTime time.Time
	backoffMu         sync.RWMutex

	// Context for connection management
	ctx        context.Context
	cancel     context.CancelFunc
	connCtx    context.Context
	connCancel context.CancelFunc

	connWg sync.WaitGroup

	// Event handlers
	onReconnectingFn func(time.Duration)
	onConnectedFn    func(string)
	onErrorFn        func(error)

	*websocket.Conn
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
	if r.MessageQueueSize == 0 {
		r.MessageQueueSize = 100
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
	r.mu.RLock()
	onConnectedFn := r.onConnectedFn
	onReconnectingFn := r.onReconnectingFn
	onErrorFn := r.onErrorFn
	url := r.url
	r.mu.RUnlock()

	switch event.Type {
	case EventConnected:
		if onConnectedFn != nil {
			onConnectedFn(url)
		}
	case EventReconnecting:
		if onReconnectingFn != nil {
			onReconnectingFn(event.Data.(time.Duration))
		}
	case EventError:
		if onErrorFn != nil {
			onErrorFn(event.Error)
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

// sleepWithContext sleeps for d, but returns early if r.ctx is cancelled.
// Returns true if cancelled, false if the full duration elapsed.
func (r *Resws) sleepWithContext(d time.Duration) bool {
	r.mu.RLock()
	ctx := r.ctx
	r.mu.RUnlock()

	if ctx == nil {
		time.Sleep(d)
		return false
	}
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func (r *Resws) doWrite(conn *websocket.Conn, msgType int, data []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	r.mu.RLock()
	deadline := r.WriteDeadline
	r.mu.RUnlock()

	if deadline > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(deadline))
	}
	return conn.WriteMessage(msgType, data)
}

func (r *Resws) doWriteControl(conn *websocket.Conn, msgType int, data []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	r.mu.RLock()
	pingInterval := r.PingInterval
	r.mu.RUnlock()

	return conn.WriteControl(msgType, data, time.Now().Add(pingInterval))
}

func (r *Resws) doWriteCloseFrame(conn *websocket.Conn) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	deadline := time.Now().Add(250 * time.Millisecond)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
}

func (r *Resws) waitGoroutines(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		r.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		if r.Logger != nil {
			r.Logger.Error("goroutine cleanup timed out after %v", timeout)
		}
	}
}

// Close closes the connection
func (r *Resws) Close() {
	r.closeOnce.Do(func() {
		// Prevent any future reconnect attempts
		r.mu.Lock()
		r.shouldReconnect = false
		r.active = false
		r.reconnecting = false
		conn := r.Conn
		r.Conn = nil
		connCancel := r.connCancel
		r.connCancel = nil
		cancel := r.cancel
		r.cancel = nil
		r.mu.Unlock()

		if cancel != nil {
			cancel()
		}

		if connCancel != nil {
			connCancel()
		}

		if conn != nil {
			// Attempt graceful close
			r.doWriteCloseFrame(conn)
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		}

		r.waitGoroutines(10 * time.Second)

		r.setIsConnected(false)
		r.emitEvent(Event{Type: EventClose})
	})
}

// CloseAndReconnect closes the connection and starts a reconnection attempt with backoff
func (r *Resws) CloseAndReconnect() {
	r.mu.Lock()
	if r.reconnecting || !r.shouldReconnect {
		r.mu.Unlock()
		return
	}
	r.reconnecting = true
	conn := r.Conn
	r.Conn = nil
	r.isConnected = false
	if r.connCancel != nil {
		r.connCancel()
	}

	r.connectedCh = make(chan struct{}, 1)
	r.connOnce = new(sync.Once)
	r.mu.Unlock()

	if conn != nil {
		r.doWriteCloseFrame(conn)
		r.sleepWithContext(100 * time.Millisecond)
		_ = conn.Close()
	}

	if r.sleepWithContext(100 * time.Millisecond) {
		r.mu.Lock()
		r.reconnecting = false
		r.mu.Unlock()
		return
	}

	// Increment reconnect attempts for this reconnection
	r.incrementReconnectAttempts()

	// Apply backoff before reconnecting
	backoffDuration := r.getReconnectBackoff()
	if backoffDuration > 0 {
		if !r.NonVerbose {
			r.Logger.Info("Will reconnect in %v (attempt %d)", backoffDuration, r.getReconnectAttempts())
		}
		r.emitEvent(Event{Type: EventReconnecting, Data: backoffDuration})
		if r.sleepWithContext(backoffDuration) {
			r.mu.Lock()
			r.reconnecting = false
			r.mu.Unlock()
			return
		}
	}

	go r.connect()
}

// getHandshakeTimeout returns the handshake timeout
func (r *Resws) getHandshakeTimeout() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.HandshakeTimeout
}

// Dial establishes a connection to the WebSocket server
func (r *Resws) Dial(url string) {
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return
	}
	r.active = true
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.mu.Unlock()

	urlStr, err := r.parseURL(url)
	if err != nil {
		r.mu.Lock()
		r.active = false
		r.lastErr = err
		r.mu.Unlock()
		r.emitEvent(Event{Type: EventError, Error: err})
		return
	}

	r.mu.Lock()
	r.connectedCh = make(chan struct{}, 1)
	r.connOnce = new(sync.Once)
	r.closeOnce = sync.Once{}
	r.mu.Unlock()

	r.setURL(urlStr)
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

// connect establishes a connection to the WebSocket server
func (r *Resws) connect() {
	r.mu.Lock()
	if r.connCancel != nil {
		r.connCancel()
	}
	r.mu.Unlock()

	r.waitGoroutines(10 * time.Second)

	r.mu.Lock()
	r.connCtx, r.connCancel = context.WithCancel(r.ctx)
	r.mu.Unlock()

	r.mu.RLock()
	recBackoff := r.RecBackoffMin
	r.mu.RUnlock()

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

			r.mu.Lock()
			r.Conn = conn
			r.lastConnect = time.Now()
			r.reconnecting = false
			r.mu.Unlock()

			if !r.NonVerbose {
				r.Logger.Info("Connection was successfully established: %s", r.url)
			}

			r.signalConnected()
			r.setIsConnected(true)

			// Start connection stability monitor
			r.connWg.Add(1)
			go func() {
				defer r.connWg.Done()
				r.monitorConnectionStability(r.connCtx)
			}()

			// Start message queue processor
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

			r.emitEvent(Event{Type: EventConnected})

			// Retry subscribe handler with backoff after connection is fully established
			if r.SubscribeHandler != nil {
				if err := r.retrySubscribeHandler(); err != nil {
					r.CloseAndReconnect()
					return
				}
			}

			return
		}
	}
}

func (r *Resws) handleDialFailure(resp *http.Response, err error, backoff time.Duration) {
	r.mu.Lock()
	r.Conn = nil
	r.httpResp = resp
	r.lastErr = err
	r.shouldReconnect = true
	r.mu.Unlock()

	r.setIsConnected(false)

	r.emitEvent(Event{Type: EventError, Error: err})
	if !r.NonVerbose {
		r.Logger.Info("Will reconnect in %v", backoff)
	}
	r.emitEvent(Event{Type: EventReconnecting, Data: backoff})

	r.sleepWithContext(backoff)
}

func (r *Resws) retrySubscribeHandler() error {
	r.mu.RLock()
	backoff := r.RecBackoffMin
	max := r.RecBackoffMax
	r.mu.RUnlock()

	attempt := 0

	for {
		select {
		case <-r.connCtx.Done():
			return r.connCtx.Err()
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
			case <-r.connCtx.Done():
				return r.connCtx.Err()
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

// Send sends a message to the WebSocket server with a fallback queue
func (r *Resws) Send(msg []byte) error {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()

	// If we have a connection, try to send directly
	if conn != nil {
		if err := r.doWrite(conn, websocket.TextMessage, msg); err == nil {
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

	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	r.Logger.Debug("Message queued for later delivery")
	return nil
}

func (r *Resws) SendJSON(v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.Send(b)
}

// processMessageQueue processes messages from the queue and sends them to the WebSocket server
func (r *Resws) processMessageQueue(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.RLock()
			conn := r.Conn
			isConnected := r.isConnected
			if !isConnected || conn == nil {
				r.mu.RUnlock()
				continue
			}
			r.mu.RUnlock()

			r.messageQueueMu.Lock()
			queueLen := len(r.messageQueue)
			if queueLen == 0 {
				r.messageQueueMu.Unlock()
				continue
			}

			// Get the first message
			msg := r.messageQueue[0]
			// Remove it from the queue
			r.messageQueue = r.messageQueue[1:]
			r.messageQueueMu.Unlock()

			// Try to send the message
			if err := r.doWrite(conn, websocket.TextMessage, msg); err != nil {
				r.Logger.Error("[%s] Failed to send queued message, stopping queue processor: %v", r.url, err)
				r.messageQueueMu.Lock()
				if len(r.messageQueue) < r.MessageQueueSize {
					r.messageQueue = append([][]byte{msg}, r.messageQueue...)
				} else {
					r.Logger.Error("[%s] Failed to requeue message: queue full", r.url)
				}
				r.messageQueueMu.Unlock()
			} else {
				r.Logger.Debug("Successfully sent queued message")
			}
		}
	}
}

// reader loops and reads messages from the WebSocket connection and emits them as events
func (r *Resws) reader(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			r.mu.RLock()
			conn := r.Conn
			r.mu.RUnlock()

			if conn == nil {
				return
			}
			pingHandlerSet := r.PingHandler != nil
			if r.ReadDeadline > time.Duration(0) && !pingHandlerSet {
				_ = conn.SetReadDeadline(time.Now().Add(r.ReadDeadline))
			}
			msgType, msg, err := conn.ReadMessage()
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation) {
				return
			}
			if err != nil {
				r.mu.Lock()
				reconnect := r.shouldReconnect
				if r.Conn == conn {
					r.Conn = nil
				}
				r.mu.Unlock()
				r.emitEvent(Event{Type: EventError, Error: err})
				if reconnect {
					// Wait for a momment before reconnecting
					time.Sleep(100 * time.Millisecond)
					r.CloseAndReconnect()
				}
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
	err = errNotConnected
	if !r.IsConnected() {
		return
	}
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		return
	}
	msgType, msg, err = conn.ReadMessage()
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return msgType, msg, nil
	}
	if err != nil {
		r.mu.Lock()
		reconnect := r.shouldReconnect
		r.mu.Unlock()
		if reconnect {
			r.CloseAndReconnect()
		}
	}

	return
}

// ReadJSON manually reads a JSON message from the websocket connection
func (r *Resws) ReadJSON(v any) (err error) {
	err = errNotConnected
	if !r.IsConnected() {
		return
	}
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		return
	}
	err = conn.ReadJSON(v)
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}
	if err != nil {
		r.mu.Lock()
		reconnect := r.shouldReconnect
		r.mu.Unlock()
		if reconnect {
			r.CloseAndReconnect()
		}
	}

	return
}

// WriteMessage manually writes a message to the websocket connection
func (r *Resws) WriteMessage(msgType int, msg []byte) (err error) {
	err = errNotConnected
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()
	if conn == nil {
		return
	}
	err = r.doWrite(conn, msgType, msg)
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}

	return
}

// WriteJSON manually writes a JSON message to the websocket connection
func (r *Resws) WriteJSON(v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.WriteMessage(websocket.TextMessage, b)
}

// heartbeat sends ping messages to the server to keep the connection alive
func (r *Resws) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(r.PingInterval)
	defer ticker.Stop()

	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()

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
			r.mu.RLock()
			currentConn := r.Conn
			r.mu.RUnlock()

			if currentConn == nil {
				return
			}

			if err := r.doWriteControl(currentConn, websocket.PingMessage, []byte{}); err != nil {
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
