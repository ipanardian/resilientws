package main

import (
	"context"
	"log"
	"net/url"
	"time"

	"github.com/ipanardian/resilientws"
)

var ws *resilientws.Resws

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(10 * time.Second)
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
		PingInterval:     5 * time.Second,
		SubscribeHandler: SubscribeHandler,
		MessageHandler:   MessageHandler,
		ReadDeadline:     5 * time.Second,
		WriteDeadline:    5 * time.Second,
		NonVerbose:       true,
	}

	ws.OnConnected(func(url string) {
		log.Println("Successfully connected to", url)
	})
	ws.OnReconnecting(func(d time.Duration) {
		log.Println("Will reconnect in", d)
		log.Println("LastConnectTime:", ws.LastConnectTime())
	})
	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	u := url.URL{Scheme: "wss", Host: "ws-feed.exchange.coinbase.com"}
	ws.Dial(u.String())
}

func PingHandler() {
	log.Println("Send ping")
	err := ws.Send([]byte(`ping`))
	if err != nil {
		log.Println("Ping error:", err)
	}
}

func SubscribeHandler() error {
	subscribe := `{"type":"subscribe","channels":["ticker"],"product_ids":["BTC-USD"]}`
	err := ws.Send([]byte(subscribe))
	if err != nil {
		return err
	}

	return nil
}

func MessageHandler(_ int, msg []byte) {
	log.Println("Received message:", string(msg))
}
