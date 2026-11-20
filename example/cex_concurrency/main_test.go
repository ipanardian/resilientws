package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRunExchangeWithoutPongReply(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ticker"}`)); err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := exchangeConfig{
		name: "test", url: "ws" + strings.TrimPrefix(server.URL, "http"),
		subscribe:    func([]string) []any { return []any{map[string]string{"type": "subscribe"}} },
		messageLabel: func([]byte) string { cancel(); return "ticker" },
	}
	result := runExchange(ctx, cfg, false, time.Second)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got := result.stats.messages.Load(); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
}
