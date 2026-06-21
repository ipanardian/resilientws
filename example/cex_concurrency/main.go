package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ipanardian/resilientws"
)

type exchangeConfig struct {
	name         string
	url          string
	pairs        []string
	subscribe    func([]string) []any
	messageLabel func([]byte) string
	pongReply    func([]byte) []byte
	pingInterval time.Duration
}

type exchangeStats struct {
	connected    atomic.Int64
	reconnecting atomic.Int64
	errors       atomic.Int64
	messages     atomic.Int64
	subscribes   atomic.Int64
	pongs        atomic.Int64
	lastMessage  atomic.Value
}

type logAdapter struct {
	prefix string
}

func (l logAdapter) Debug(msg string, args ...any) {
	log.Printf("%s DEBUG: %s", l.prefix, fmt.Sprintf(msg, args...))
}
func (l logAdapter) Info(msg string, args ...any) {
	log.Printf("%s INFO: %s", l.prefix, fmt.Sprintf(msg, args...))
}
func (l logAdapter) Error(msg string, args ...any) {
	log.Printf("%s ERROR: %s", l.prefix, fmt.Sprintf(msg, args...))
}

func main() {
	duration := flag.Duration("duration", 45*time.Second, "total runtime")
	forceReconnect := flag.Bool("force-reconnect", true, "force one reconnect halfway through")
	exchangesCSV := flag.String("exchanges", "coinbase,crypto_com,hitbtc,bingx", "comma-separated exchange list")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	configs := selectedExchanges(*exchangesCSV)
	if len(configs) == 0 {
		log.Fatal("no exchanges selected")
	}

	var wg sync.WaitGroup
	results := make(chan runResult, len(configs))
	for _, cfg := range configs {
		wg.Add(1)
		go func(cfg exchangeConfig) {
			defer wg.Done()
			results <- runExchange(ctx, cfg, *forceReconnect, *duration)
		}(cfg)
	}

	wg.Wait()
	close(results)

	failed := false
	for result := range results {
		if result.err != nil {
			failed = true
			log.Printf("%s FAILED: %v", result.name, result.err)
			continue
		}
		log.Printf("%s OK: connected=%d reconnecting=%d messages=%d subscribes=%d pongs=%d last=%q",
			result.name,
			result.stats.connected.Load(),
			result.stats.reconnecting.Load(),
			result.stats.messages.Load(),
			result.stats.subscribes.Load(),
			result.stats.pongs.Load(),
			stringValue(result.stats.lastMessage.Load()),
		)
	}

	if failed {
		log.Fatal("one or more exchanges failed")
	}
}

type runResult struct {
	name  string
	stats *exchangeStats
	err   error
}

func runExchange(ctx context.Context, cfg exchangeConfig, forceReconnect bool, duration time.Duration) runResult {
	stats := &exchangeStats{}
	var ws *resilientws.Resws

	ws = &resilientws.Resws{
		RecBackoffMin:    500 * time.Millisecond,
		RecBackoffMax:    5 * time.Second,
		RecBackoffFactor: 1.5,
		BackoffType:      resilientws.BackoffTypeJitter,
		PingInterval:     cfg.pingInterval,
		PongTimeout:      20 * time.Second,
		MessageQueueSize: 128,
		Logger:           logAdapter{prefix: cfg.name},
		NonVerbose:       true,
	}

	ws.SubscribeHandler = func() error {
		for _, msg := range cfg.subscribe(cfg.pairs) {
			if err := ws.SendJSON(msg); err != nil {
				return fmt.Errorf("subscribe %s: %w", cfg.name, err)
			}
			stats.subscribes.Add(1)
		}
		return nil
	}

	ws.MessageHandler = func(msgType int, msg []byte) {
		if msgType == websocket.BinaryMessage {
			stats.messages.Add(1)
			stats.lastMessage.Store(fmt.Sprintf("binary:%d", len(msg)))
			return
		}

		if reply := cfg.pongReply(msg); reply != nil {
			stats.pongs.Add(1)
			if err := ws.Send(reply); err != nil {
				stats.errors.Add(1)
			}
			return
		}

		stats.messages.Add(1)
		stats.lastMessage.Store(cfg.messageLabel(msg))
	}

	ws.OnConnected(func(url string) {
		stats.connected.Add(1)
		log.Printf("%s connected: %s", cfg.name, url)
	})
	ws.OnReconnecting(func(d time.Duration) {
		stats.reconnecting.Add(1)
		log.Printf("%s reconnecting in %s", cfg.name, d)
	})
	ws.OnError(func(err error) {
		stats.errors.Add(1)
		log.Printf("%s error: %v", cfg.name, err)
	})

	ws.Dial(cfg.url)

	if forceReconnect {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(max(duration/2, 5*time.Second)):
				log.Printf("%s forcing reconnect", cfg.name)
				ws.CloseAndReconnect()
			}
		}()
	}

	<-ctx.Done()
	ws.Close()

	if stats.connected.Load() == 0 {
		return runResult{name: cfg.name, stats: stats, err: fmt.Errorf("never connected")}
	}
	if stats.subscribes.Load() == 0 {
		return runResult{name: cfg.name, stats: stats, err: fmt.Errorf("subscribe handler did not send")}
	}
	if stats.messages.Load() == 0 {
		return runResult{name: cfg.name, stats: stats, err: fmt.Errorf("no market data received")}
	}
	return runResult{name: cfg.name, stats: stats}
}

func selectedExchanges(csv string) []exchangeConfig {
	all := map[string]exchangeConfig{
		"coinbase":   coinbaseConfig(),
		"crypto_com": cryptoComConfig(),
		"hitbtc":     hitbtcConfig(),
		"bingx":      bingxConfig(),
	}

	keys := strings.Split(csv, ",")
	configs := make([]exchangeConfig, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		cfg, ok := all[key]
		if !ok {
			valid := make([]string, 0, len(all))
			for k := range all {
				valid = append(valid, k)
			}
			sort.Strings(valid)
			log.Fatalf("unknown exchange %q; valid: %s", key, strings.Join(valid, ","))
		}
		configs = append(configs, cfg)
	}
	return configs
}

func coinbaseConfig() exchangeConfig {
	return exchangeConfig{
		name:         "coinbase",
		url:          "wss://ws-feed.exchange.coinbase.com",
		pairs:        []string{"BTC-USD", "ETH-USD"},
		pingInterval: 10 * time.Second,
		subscribe: func(pairs []string) []any {
			return []any{map[string]any{
				"type":        "subscribe",
				"channels":    []string{"ticker"},
				"product_ids": pairs,
			}}
		},
		messageLabel: jsonFieldLabel("type", "product_id"),
	}
}

func cryptoComConfig() exchangeConfig {
	return exchangeConfig{
		name:         "crypto_com",
		url:          "wss://stream.crypto.com/exchange/v1/market",
		pairs:        []string{"BTC_USD", "ETH_USD"},
		pingInterval: 15 * time.Second,
		subscribe: func(pairs []string) []any {
			channels := make([]string, 0, len(pairs))
			for _, pair := range pairs {
				channels = append(channels, fmt.Sprintf("book.%s.10", pair))
			}
			return []any{map[string]any{
				"id":     time.Now().UnixNano(),
				"method": "subscribe",
				"params": map[string]any{
					"channels":               channels,
					"book_subscription_type": "SNAPSHOT",
					"book_update_frequency":  500,
				},
			}}
		},
		pongReply: func(msg []byte) []byte {
			var payload struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(msg, &payload); err != nil || payload.Method != "public/heartbeat" {
				return nil
			}
			reply, _ := json.Marshal(map[string]any{"id": payload.ID, "method": "public/respond-heartbeat"})
			return reply
		},
		messageLabel: jsonFieldLabel("method", "result.instrument_name"),
	}
}

func hitbtcConfig() exchangeConfig {
	return exchangeConfig{
		name:         "hitbtc",
		url:          "wss://api.hitbtc.com/api/3/ws/public",
		pairs:        []string{"BTCUSDT", "ETHUSDT"},
		pingInterval: 15 * time.Second,
		subscribe: func(pairs []string) []any {
			return []any{map[string]any{
				"method": "subscribe",
				"ch":     "orderbook/top/100ms",
				"params": map[string]any{"symbols": pairs},
				"id":     time.Now().UnixNano(),
			}}
		},
		messageLabel: jsonFieldLabel("ch", "data.s"),
	}
}

func bingxConfig() exchangeConfig {
	return exchangeConfig{
		name:         "bingx",
		url:          "wss://open-api-swap.bingx.com/swap-market",
		pairs:        []string{"BTC-USDT", "ETH-USDT"},
		pingInterval: 30 * time.Second,
		subscribe: func(pairs []string) []any {
			messages := make([]any, 0, len(pairs))
			for _, pair := range pairs {
				messages = append(messages, map[string]any{
					"id":       fmt.Sprintf("%d-%s", time.Now().UnixNano(), pair),
					"reqType":  "sub",
					"dataType": fmt.Sprintf("%s@depth5@1000ms", pair),
				})
			}
			return messages
		},
		pongReply: func(msg []byte) []byte {
			if string(msg) == "Ping" {
				return []byte("Pong")
			}
			return nil
		},
		messageLabel: jsonFieldLabel("dataType", "code"),
	}
}

func jsonFieldLabel(fields ...string) func([]byte) string {
	return func(msg []byte) string {
		var payload map[string]any
		if err := json.Unmarshal(msg, &payload); err != nil {
			if len(msg) > 80 {
				return string(msg[:80])
			}
			return string(msg)
		}

		parts := make([]string, 0, len(fields))
		for _, field := range fields {
			if v, ok := nested(payload, strings.Split(field, ".")); ok {
				parts = append(parts, fmt.Sprintf("%s=%v", field, v))
			}
		}
		if len(parts) == 0 {
			return fmt.Sprintf("json:%d", len(msg))
		}
		return strings.Join(parts, " ")
	}
}

func nested(v any, path []string) (any, bool) {
	if len(path) == 0 {
		return v, true
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	next, ok := m[path[0]]
	if !ok {
		return nil, false
	}
	return nested(next, path[1:])
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
