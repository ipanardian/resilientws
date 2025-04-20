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

package resilientws

import (
	"context"
	"crypto/tls"
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
	messageQueue    [][]byte
	messageQueueMu  sync.Mutex
	eventHandlers   map[EventType][]func(Event)
	isConnected     bool
	lastConnect     time.Time
	lastErr         error
	shouldReconnect bool
	connectedCh     chan struct{}
	connOnce        *sync.Once

	ctx        context.Context
	cancel     context.CancelFunc
	connCtx    context.Context
	connCancel context.CancelFunc

	onReconnectingFn func(time.Duration)
	onConnectedFn    func(string)
	onErrorFn        func(error)

	*websocket.Conn
}

type Logger interface {
	Debug(msg string, args ...interface{})
	Info(msg string, args ...interface{})
	Error(msg string, args ...interface{})
}

type defaultLogger struct{}

func (l *defaultLogger) Debug(msg string, args ...interface{}) {
	log.Printf("DEBUG: "+msg, args...)
}

func (l *defaultLogger) Info(msg string, args ...interface{}) {
	log.Printf("INFO: "+msg, args...)
}

func (l *defaultLogger) Error(msg string, args ...interface{}) {
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
	Data        interface{}
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
	r.eventHandlers = make(map[EventType][]func(Event))

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
	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	if r.PingInterval == 0 {
		r.PingInterval = 15 * time.Second
	}
	if r.PongTimeout == 0 {
		r.PongTimeout = 20 * time.Second
	}
	if r.ReadDeadline == 0 {
		r.ReadDeadline = 15 * time.Second
	}
	if r.WriteDeadline == 0 {
		r.WriteDeadline = 15 * time.Second
	}
	if r.MessageQueueSize == 0 {
		r.MessageQueueSize = 10
	}
	if !r.shouldReconnect {
		r.shouldReconnect = true
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
			r.onReconnectingFn(event.Data.(time.Duration))
		}
	case EventError:
		if r.onErrorFn != nil {
			r.onErrorFn(event.Error)
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

// Close closes the connection
func (r *Resws) Close() {
	r.cancel()

	r.mu.Lock()
	if r.Conn == nil {
		r.mu.Unlock()
		return
	}

	r.Conn.Close()
	r.Conn = nil
	r.shouldReconnect = false

	if r.connCancel != nil {
		r.connCancel()
	}
	r.mu.Unlock()

	r.setIsConnected(false)
}

// CloseAndReconnect closes the connection and starts a reconnection attempt
func (r *Resws) CloseAndReconnect() {
	r.mu.Lock()
	if r.Conn != nil {
		_ = r.Conn.Close()
		r.Conn = nil
	}
	if r.connCancel != nil {
		r.connCancel()
	}

	r.connectedCh = make(chan struct{}, 1)
	r.connOnce = new(sync.Once)
	r.mu.Unlock()

	r.setIsConnected(false)

	// Wait for the connection to close
	time.Sleep(100 * time.Millisecond)

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
	r.ctx, r.cancel = context.WithCancel(context.Background())

	urlStr, err := r.parseURL(url)
	if err != nil {
		r.lastErr = err
		r.emitEvent(Event{Type: EventError, Error: err})
		return
	}

	r.mu.Lock()
	r.connectedCh = make(chan struct{}, 1)
	r.connOnce = new(sync.Once)
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
	r.connCtx, r.connCancel = context.WithCancel(r.ctx)

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
			r.mu.Unlock()

			if !r.NonVerbose {
				r.Logger.Info("Connection was successfully established: %s", r.url)
			}

			r.signalConnected()
			r.setIsConnected(true)

			// Start message queue processor
			go r.processMessageQueue(r.connCtx)

			// Retry subscribe handler with backoff
			if r.SubscribeHandler != nil {
				if err := r.retrySubscribeHandler(); err != nil {
					r.CloseAndReconnect()
					return
				}
			}

			if r.PingHandler != nil {
				go r.heartbeat(r.connCtx)
			}
			if r.MessageHandler != nil {
				go r.reader(r.connCtx)
			}

			r.emitEvent(Event{Type: EventConnected})

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
	r.mu.RLock()
	backoff := r.RecBackoffMin
	max := r.RecBackoffMax
	r.mu.RUnlock()

	attempt := 0

	for {
		if err := r.SubscribeHandler(); err != nil {
			r.Logger.Error("Subscribe handler failed: %v", err)
			r.emitEvent(Event{Type: EventError, Error: err})

			if backoff >= max {
				return fmt.Errorf("subscribe handler failed after max retries: %w", err)
			}

			if !r.NonVerbose {
				r.Logger.Info("Retrying subscribe handler in %v", backoff)
			}
			time.Sleep(backoff)
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
		if r.WriteDeadline > 0 {
			conn.SetWriteDeadline(time.Now().Add(r.WriteDeadline))
		}
		r.mu.Lock()
		err := conn.WriteMessage(websocket.TextMessage, msg)
		r.mu.Unlock()
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

	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	r.Logger.Debug("Message queued for later delivery")
	return nil
}

func (r *Resws) SendJSON(v any) (err error) {
	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()

	// If we have a connection, try to send directly
	if conn != nil {
		if r.WriteDeadline > 0 {
			conn.SetWriteDeadline(time.Now().Add(r.WriteDeadline))
		}
		r.mu.Lock()
		err = conn.WriteJSON(v)
		r.mu.Unlock()
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
	r.messageQueue = append(r.messageQueue, v.([]byte))
	r.messageQueueMu.Unlock()

	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	r.Logger.Debug("Message queued for later delivery")
	return nil
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
			r.mu.RLock()
			if r.WriteDeadline > 0 {
				conn.SetWriteDeadline(time.Now().Add(r.WriteDeadline))
			}
			r.mu.RUnlock()
			err := conn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				r.Logger.Error("Failed to send queued message: %v", err)
				// If we failed to send, try to requeue the message
				r.messageQueueMu.Lock()
				if len(r.messageQueue) < r.MessageQueueSize {
					r.messageQueue = append(r.messageQueue, msg)
					r.Logger.Debug("Requeued failed message")
				} else {
					r.Logger.Error("Failed to requeue message: queue full")
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
			if r.ReadDeadline > 0 {
				conn.SetReadDeadline(time.Now().Add(r.ReadDeadline))
			}
			msgType, msg, err := conn.ReadMessage()
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation) {
				r.Close()
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
					// Wait for a second before reconnecting
					time.Sleep(1 * time.Second)
					r.CloseAndReconnect()
				}
				return
			}
			switch msgType {
			case websocket.TextMessage, websocket.BinaryMessage:
				r.MessageHandler(msgType, msg)
			case websocket.CloseMessage:
				// TODO: handle close message
				r.emitEvent(Event{Type: EventClose})
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
	msgType, msg, err = r.Conn.ReadMessage()
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
	err = r.Conn.ReadJSON(v)
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
	if !r.IsConnected() {
		return
	}
	r.mu.Lock()
	err = r.Conn.WriteMessage(msgType, msg)
	r.mu.Unlock()
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}

	return
}

// WriteJSON manually writes a JSON message to the websocket connection
func (r *Resws) WriteJSON(v any) (err error) {
	err = errNotConnected
	if !r.IsConnected() {
		return
	}
	r.mu.Lock()
	err = r.Conn.WriteJSON(v)
	r.mu.Unlock()
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return
	}

	return
}

// heartbeat sends ping messages to the server to keep the connection alive
func (r *Resws) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(r.PingInterval)
	defer ticker.Stop()

	r.mu.RLock()
	conn := r.Conn
	r.mu.RUnlock()

	_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
	conn.SetPongHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
		return nil
	})

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
