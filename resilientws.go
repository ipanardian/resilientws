package resilientws

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
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

	url            string
	dialer         *websocket.Dialer
	httpResp       *http.Response
	mu             sync.RWMutex
	messageQueue   [][]byte
	messageQueueMu sync.Mutex
	eventHandlers  map[EventType][]func(Event)
	isConnected    bool
	lastConnect    time.Time
	lastErr        error
	connectedCh    chan struct{}
	stopCh         chan struct{}
	closeOnce      sync.Once

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
	r.stopCh = make(chan struct{})

	if r.RecBackoffMin == 0 {
		r.RecBackoffMin = 2 * time.Second
	}
	if r.RecBackoffMax == 0 {
		r.RecBackoffMax = 30 * time.Second
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
	r.mu.RLock()
	defer r.mu.RUnlock()

	r.onErrorFn = fn
}

// OnConnected sets the connected handler
func (r *Resws) OnConnected(fn func(url string)) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	r.onConnectedFn = fn
}

// OnReconnecting sets the reconnection handler
func (r *Resws) OnReconnecting(fn func(time.Duration)) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	r.onReconnectingFn = fn
}

// SetIsConnected sets the connection state
func (r *Resws) SetIsConnected(isConnected bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	r.isConnected = isConnected
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
	r.mu.Lock()
	if r.Conn == nil {
		r.mu.Unlock()
		return
	}

	r.Conn.Close()
	r.Conn = nil
	r.mu.Unlock()

	r.SetIsConnected(false)
	r.emitEvent(Event{Type: EventClose})

	r.closeOnce.Do(func() {
		close(r.stopCh)
	})
}

// CloseAndReconnect closes the connection and starts a reconnection attempt
func (r *Resws) CloseAndReconnect() {
	r.mu.Lock()
	if r.Conn != nil {
		_ = r.Conn.Close()
		r.Conn = nil
	}
	r.mu.Unlock()
	r.SetIsConnected(false)

	r.closeOnce.Do(func() {
		close(r.stopCh)
	})

	r.stopCh = make(chan struct{})
	r.closeOnce = sync.Once{}
	r.connectedCh = make(chan struct{})

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
	urlStr, err := r.parseURL(url)
	if err != nil {
		r.lastErr = err
		r.emitEvent(Event{Type: EventError, Error: err})
		return
	}

	r.setURL(urlStr)
	r.setDefaultConfig()

	r.connectedCh = make(chan struct{})

	go r.connect()

	timer := time.NewTimer(r.getHandshakeTimeout())
	defer timer.Stop()

	select {
	case <-timer.C:
		return
	case <-r.connectedCh:
		return
	}
}

// connect establishes a connection to the WebSocket server
func (r *Resws) connect() {
	recBackoff := r.RecBackoffMin

	for {
		select {
		case <-r.stopCh:
			return
		default:
			conn, resp, err := r.dialer.Dial(r.url, r.Headers)

			r.mu.Lock()
			r.Conn = conn
			r.isConnected = err == nil
			r.lastConnect = time.Now()
			r.httpResp = resp
			r.mu.Unlock()

			if err == nil {
				if !r.NonVerbose {
					r.Logger.Info("Connection was successfully established: %s", r.url)
				}

				// Start message queue processor first to ensure no messages are missed
				go r.processMessageQueue()

				if r.SubscribeHandler != nil {
					if err := r.SubscribeHandler(); err != nil {
						r.Logger.Error("Subscribe handler failed: %v", err)
						return
					}
					if !r.NonVerbose {
						r.Logger.Info("Subscribe handler was successfully executed")
					}
				}

				if r.PingHandler != nil {
					go r.heartbeat(conn)
				}
				if r.MessageHandler != nil {
					go r.reader(conn)
				}

				r.emitEvent(Event{Type: EventConnected})
				close(r.connectedCh)
				return
			}

			r.lastErr = err
			if r.onErrorFn != nil {
				r.emitEvent(Event{Type: EventError, Error: err})
			}
			if !r.NonVerbose {
				r.Logger.Info("will reconnect in %v", recBackoff)
			}
			if r.onReconnectingFn != nil {
				r.emitEvent(Event{Type: EventReconnecting, Data: recBackoff})
			}

			time.Sleep(recBackoff)
			if recBackoff < r.RecBackoffMax {
				recBackoff *= 2
			}
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
		err := conn.WriteMessage(websocket.TextMessage, msg)
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

// processMessageQueue processes messages from the queue and sends them to the WebSocket server
func (r *Resws) processMessageQueue() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.mu.RLock()
			conn := r.Conn
			isConnected := r.isConnected
			r.mu.RUnlock()

			if !isConnected || conn == nil {
				continue
			}

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
			if r.WriteDeadline > 0 {
				conn.SetWriteDeadline(time.Now().Add(r.WriteDeadline))
			}
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
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
func (r *Resws) reader(conn *websocket.Conn) {
	for {
		select {
		case <-r.stopCh:
			return
		default:
			if conn == nil {
				return
			}
			if r.ReadDeadline > 0 {
				conn.SetReadDeadline(time.Now().Add(r.ReadDeadline))
			}
			msgType, msg, err := conn.ReadMessage()
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				r.Close()
				return
			}
			if err != nil {
				r.mu.Lock()
				if r.Conn == conn {
					r.Conn = nil
				}
				r.mu.Unlock()
				conn.Close()
				r.emitEvent(Event{Type: EventError, Error: err})
				<-time.After(1000 * time.Millisecond)
				r.CloseAndReconnect()
				return
			}
			switch msgType {
			case websocket.TextMessage, websocket.BinaryMessage:
				r.MessageHandler(msgType, msg)
			case websocket.CloseMessage:
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
		r.CloseAndReconnect()
	}

	return
}

// heartbeat sends ping messages to the server to keep the connection alive
func (r *Resws) heartbeat(conn *websocket.Conn) {
	ticker := time.NewTicker(r.PingInterval)
	defer ticker.Stop()

	_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
	conn.SetPongHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
		return nil
	})

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(r.PingInterval)); err != nil {
				return
			}
			r.PingHandler()
		}
	}
}
