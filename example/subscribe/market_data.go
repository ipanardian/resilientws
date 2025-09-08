package main

import (
	"context"
	"log"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ipanardian/resilientws"
)

var ws *resilientws.Resws

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(30 * time.Second)
		log.Println("Closing connection after 10 seconds")
		cancel()
	}()

	go Run()

	<-ctx.Done()
	ws.Close()
	log.Println("Bye-bye")
}

func Run() {
	ws = &resilientws.Resws{
		PingInterval:     10 * time.Second,
		PingHandler:      PingHandler,
		SubscribeHandler: SubscribeHandler,
		MessageHandler:   MessageHandler,
		WriteDeadline:    5 * time.Second,
		NonVerbose:       true,
	}

	ws.OnConnected(func(url string) {
		log.Println("Successfully connected to", url)
	})
	ws.OnReconnecting(func(d time.Duration) {
		log.Println("Will reconnect in", d)
		if !ws.LastConnectTime().IsZero() {
			log.Println("LastConnectTime:", ws.LastConnectTime())
		}
	})
	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	u := url.URL{Scheme: "wss", Host: "stream.binance.com:9443", Path: "/ws"}
	ws.Dial(u.String())
}

func PingHandler() {
	if !ws.IsConnected() {
		return
	}
	err := ws.WriteMessage(websocket.PingMessage, []byte{})
	if err != nil {
		log.Println("Ping error:", err)
	}
	log.Println("Sent ping")
}

func SubscribeHandler() error {
	subscribe := map[string]any{
		"method": "SUBSCRIBE",
		"params": []string{"btcusdt@aggTrade"},
		"id":     1,
	}
	err := ws.WriteJSON(subscribe)
	if err != nil {
		return err
	}

	return nil
}

func MessageHandler(_ int, msg []byte) {
	log.Println("Received message:", string(msg))
}
