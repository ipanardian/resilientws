package main

import (
    "log"
    "sync"
    "time"

    resilientws "github.com/ipanardian/resilientws"
)

// Minimal example: manage a thread-safe set of symbols and resubscribe on reconnect.
func main() {
    // Thread-safe set of symbols (map + mutex).
    var (
        mu      sync.RWMutex
        symbols = map[string]struct{}{}
    )

    add := func(s string) {
        mu.Lock(); symbols[s] = struct{}{}; mu.Unlock()
    }
    remove := func(s string) {
        mu.Lock(); delete(symbols, s); mu.Unlock()
    }
    snapshot := func() []string {
        mu.RLock(); defer mu.RUnlock()
        out := make([]string, 0, len(symbols))
        for s := range symbols { out = append(out, s) }
        return out
    }

    ws := &resilientws.Resws{
        RecBackoffMin:    1 * time.Second,
        RecBackoffMax:    30 * time.Second,
        RecBackoffFactor: 1.5,
        BackoffType:      resilientws.BackoffTypeJitter,
        MessageQueueSize: 256,
        PingInterval:     15 * time.Second,
    }

    // Subscribe only current symbols after each (re)connection.
    ws.SubscribeHandler = func() error {
        syms := snapshot()
        if len(syms) == 0 { return nil }
        log.Printf("(re)subscribing %v", syms)
        // Use a minimal inline payload; adapt keys to your server protocol.
        return ws.SendJSON(map[string]any{"type": "subscribe", "symbols": syms})
    }

    // Optional: print incoming messages
    ws.MessageHandler = func(_ int, msg []byte) { log.Printf("recv: %s", string(msg)) }

    ws.Dial("wss://example.com/stream")
    defer ws.Close()

    // Simple runtime mutations
    add("AAPL")
    _ = ws.SendJSON(map[string]any{"type": "subscribe", "symbols": []string{"AAPL"}})

    time.Sleep(2 * time.Second)
    add("MSFT")
    _ = ws.SendJSON(map[string]any{"type": "subscribe", "symbols": []string{"MSFT"}})

    time.Sleep(2 * time.Second)
    remove("AAPL")
    _ = ws.SendJSON(map[string]any{"type": "unsubscribe", "symbols": []string{"AAPL"}})

    select {}
}
