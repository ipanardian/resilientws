package resilientws

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
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
	messageQueue    [][]byte
	messageQueueMu  sync.Mutex
	writeMu         sync.Mutex
	isConnected     bool
	lastConnect     time.Time
	lastErr         error
	shouldReconnect bool
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
