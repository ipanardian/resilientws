package main

import (
    "log"
    "net/http"
    "time"

    resilientws "github.com/ipanardian/resilientws"
)

// Minimal example: pass Authorization header directly via Resws.Headers.
func main() {
    // Build your headers once and pass them to Resws.
    header := http.Header{}
    header.Set("Authorization", "Bearer YOUR_TOKEN_OR_API_KEY")

    ws := &resilientws.Resws{
        RecBackoffMin:    1 * time.Second,
        RecBackoffMax:    30 * time.Second,
        RecBackoffFactor: 1.5,
        BackoffType:      resilientws.BackoffTypeJitter,
        PingInterval:     15 * time.Second,
        PongTimeout:      20 * time.Second,
        Headers:          header,
    }

    ws.MessageHandler = func(msgType int, msg []byte) {
        log.Printf("recv: %s", string(msg))
    }
    ws.OnConnected(func(url string) { log.Printf("connected to %s", url) })
    ws.OnError(func(err error) { log.Printf("error: %v", err) })

    ws.Dial("wss://example.com/secure-stream")
    defer ws.Close()

    select {}
}
