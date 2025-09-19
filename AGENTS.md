# AGENTS Guide: resilientws

This document is tailored for AI agents and code assistants to quickly understand and use `resilientws` effectively. It summarizes capabilities, configuration parameters, lifecycle events, and provides minimal examples to answer common questions.

Repository: https://github.com/ipanardian/resilientws

## Overview
`resilientws` is a lightweight Go WebSocket client designed for production resilience:
- Automatic reconnection with production-ready backoff.
- Persistent backoff state across connection cycles; reset after stable connections.
- Subscribe handler re-run after every successful (re)connect.
- Thread-safe message queue that buffers sends while disconnected.
- Ping/Pong heartbeat.
- Event callbacks for connection lifecycle and errors.

## Core Types and Fields
Main struct: `resilientws.Resws`

Configuration fields:
- `RecBackoffMin time.Duration` — Minimum backoff between reconnection attempts (default: 1s).
- `RecBackoffMax time.Duration` — Maximum backoff between reconnection attempts (default: 30s).
- `RecBackoffFactor float64` — Multiplier applied per attempt (default: 1.5).
- `BackoffType BackoffType` — `BackoffTypeJitter` (default) or `BackoffTypeFixed`.
- `StableConnectionDuration time.Duration` — If a connection stays up for this long (default: 30s), backoff state resets.
- `HandshakeTimeout time.Duration` — Timeout for initial handshake (default: 2s).
- `Headers http.Header` — Request headers for the handshake (e.g., `Authorization`).
- `PingInterval time.Duration` — Interval for sending ping frames (default: 15s).
- `PongTimeout time.Duration` — Deadline extension after receiving a pong; 0 disables deadline.
- `ReadDeadline time.Duration` — Optional read deadline per read.
- `WriteDeadline time.Duration` — Optional write deadline per write.
- `MessageQueueSize int` — Buffered queue size for offline sends.
- `TLSConfig *tls.Config` — TLS options.
- `Proxy func(*http.Request) (*url.URL, error)` — HTTP proxy function.
- `Logger Logger` — Optional custom logger (see Logger interface below).
- `NonVerbose bool` — Reduce log verbosity.

Handlers:
- `SubscribeHandler func() error` — Called after every successful connect/reconnect. Return error to trigger internal retry with backoff; if retries exceed max window, the client will reconnect.
- `MessageHandler func(msgType int, msg []byte)` — Called for text/binary messages.
- `PingHandler func()` — Called on each ping tick; use for metrics/heartbeats.

Event callbacks:
- `OnConnected(func(url string))`
- `OnReconnecting(func(duration time.Duration))` — Called before each backoff sleep.
- `OnError(func(err error))`

Logger interface:
```go
type Logger interface {
    Debug(msg string, args ...interface{})
    Info(msg string, args ...interface{})
    Error(msg string, args ...interface{})
}
```

Key methods:
- `Dial(url string)` — Start connection lifecycle. Non-blocking; returns after handshake timeout or first connect signal.
- `Close()` — Shutdown and prevent further reconnects.
- `CloseAndReconnect()` — Force-close and schedule reconnect with backoff.
- `Send(msg []byte) error` — Send or enqueue if not connected.
- `SendJSON(v any) error` — JSON send or enqueue.
- `ReadMessage()`, `ReadJSON(v any)` — Manual reads.
- `WriteMessage(mt int, data []byte)`, `WriteJSON(v any)` — Manual writes.
- `IsConnected() bool`
- `LastConnectTime() time.Time`
- `LastError() error`
- `GetHTTPResponse() *http.Response`

## Backoff and Stability
- Backoff is applied both on initial connect failures and on post-connect disconnects.
- Backoff grows with attempts and respects `RecBackoffMin/Max/Factor` and `BackoffType`.
- After a connection remains stable for `StableConnectionDuration`, backoff state resets to avoid permanent long delays.

## Message Queue
- If not connected, `Send`/`SendJSON` enqueue messages up to `MessageQueueSize`.
- Upon reconnection, a background worker drains the queue and sends messages in order.

## Heartbeat
- If `PingHandler` is set, a heartbeat loop will send periodic pings at `PingInterval`.
- If `PongTimeout > 0`, read deadlines are updated on pong to detect stale connections.

## Minimal Copy-Paste Examples

### 0) Basic
Path: `example/basic/basic.go`
```go
ws := &resilientws.Resws{
    MessageHandler:   MessageHandler,
    PingHandler:      PingHandler,
    PingInterval:     5 * time.Second,
    MessageQueueSize: 1,
}

// OnConnected is called after the connection is established
ws.OnConnected(func(url string) {
   log.Println("Connected to", url)

   // Send a message after connection is established
   _ = ws.Send([]byte("Hello"))
})

// OnReconnecting is called before each backoff sleep
ws.OnReconnecting(func(duration time.Duration) {
   log.Println("Reconnecting in", duration)
})

// OnError is called when an error occurs
ws.OnError(func(err error) {
   log.Println("Error:", err)
})

// Dial starts the connection lifecycle
ws.Dial("wss://echo.websocket.org")

// MessageHandler is called when a message is received
func MessageHandler(_ int, msg []byte) {
   log.Println("Received message:", string(msg))
}

// PingHandler is called on each ping tick
func PingHandler() {
   _ = ws.Send([]byte(`ping`))
}
```

### 1) Subscribe Market Data (Binance)
Path: `example/subscribe/market_data.go`
```go
ws := &resilientws.Resws{
    PingInterval:     10 * time.Second,
    PingHandler:      PingHandler,
    SubscribeHandler: SubscribeHandler,
    MessageHandler:   MessageHandler,
    WriteDeadline:    5 * time.Second,
}

// Called after each (re)connect; sends subscription
func SubscribeHandler() error {
    sub := map[string]any{
        "method": "SUBSCRIBE",
        "params": []string{"btcusdt@aggTrade"},
        "id":     1,
    }
    return ws.WriteJSON(sub)
}

ws.Dial("wss://stream.binance.com:9443/ws")
```

### 1) Dynamic Subscriptions (thread-safe, resubscribe current set)
Path: `example/dynamic_subscriptions/dynamic.go`
```go
var (
    mu      sync.RWMutex
    symbols = map[string]struct{}{}
)

snapshot := func() []string {
    mu.RLock(); defer mu.RUnlock()
    out := make([]string, 0, len(symbols))
    for s := range symbols { out = append(out, s) }
    return out
}

ws := &resilientws.Resws{}
ws.SubscribeHandler = func() error {
    syms := snapshot()
    if len(syms) == 0 { return nil }
    return ws.SendJSON(map[string]any{"type": "subscribe", "symbols": syms})
}
```

### 2) Authorization Header
Path: `example/auth_header/auth_header.go`
```go
header := http.Header{}
header.Set("Authorization", "Bearer YOUR_TOKEN_OR_API_KEY")
ws := &resilientws.Resws{ Headers: header }
ws.Dial("wss://example.com/secure-stream")
```

### 3) Backoff Tuning and Stability Reset
```go
ws := &resilientws.Resws{
    RecBackoffMin:           1 * time.Second,
    RecBackoffMax:           30 * time.Second,
    RecBackoffFactor:        1.5,
    BackoffType:             resilientws.BackoffTypeJitter,
    StableConnectionDuration: 30 * time.Second,
}
ws.OnReconnecting(func(d time.Duration) { log.Printf("reconnecting in %v", d) })
```

### 4) Offline Send Queue
```go
ws := &resilientws.Resws{ MessageQueueSize: 256 }
// Enqueue while offline or during reconnect
_ = ws.SendJSON(map[string]any{"type": "subscribe", "symbols": []string{"AAPL"}})
// After reconnect, the queue auto-flushes
```

### 5) Heartbeat
```go
ws := &resilientws.Resws{ PingInterval: 15 * time.Second, PongTimeout: 20 * time.Second }
ws.PingHandler = func() { log.Printf("ping tick") }
```

### 6) Custom Logger
```go
type myLogger struct{}
func (l *myLogger) Debug(msg string, a ...any) { log.Printf("DEBUG: "+msg, a...) }
func (l *myLogger) Info(msg string, a ...any)  { log.Printf("INFO: "+msg, a...) }
func (l *myLogger) Error(msg string, a ...any) { log.Printf("ERROR: "+msg, a...) }

ws := &resilientws.Resws{ Logger: &myLogger{} }
```

## Common Q&A Patterns (for Agents)
- Dynamic subscriptions: maintain a thread-safe set and send a snapshot in `SubscribeHandler` after reconnect; apply runtime changes using `SendJSON` immediately.
- Passing headers: set `Resws.Headers` before `Dial`. For rotating tokens, update the header in `OnReconnecting` before the next handshake.
- Handling transient subscribe errors: have `SubscribeHandler` return an error; the client retries with backoff and may reconnect if failures persist.
- Avoid tight reconnect loops: rely on built-in backoff and stability reset; do not implement manual sleep loops.
- Graceful shutdown: call `Close()` to stop future reconnects and close the connection.

## Pseudocode Lifecycle
```text
Dial -> connect loop
  - attempt Dial with headers
  - on success: set connected, start stability timer, start heartbeat (optional), start reader (optional), start queue processor
  - run SubscribeHandler with retry/backoff
  - on read error: emit error, CloseAndReconnect() if shouldReconnect
Backoff across reconnects persists; reset after StableConnectionDuration
```

## References
- Code: `resilientws/resilientws.go`
- Examples:
  - `example/dynamic_subscriptions/dynamic.go`
  - `example/auth_header/auth_header.go`
- README: `README.md`
- Context7 metadata: `context7.json`
