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
		MessageHandler:   MessageHandler,
		PingHandler:      PingHandler,
		PingInterval:     5 * time.Second,
		MessageQueueSize: 1,
	}

	// sends a message after connection is established with onConnected event
	ws.OnConnected(func(url string) {
		err := ws.WriteMessage(websocket.TextMessage, []byte("Hello"))
		if err != nil {
			log.Println("Write message error:", err)
		}
		log.Println("Hello sent")
	})

	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	// sends a message while connection is not established
	// this message will be queued and sent after connection is established
	ws.Send([]byte("This is a queued message"))

	u := url.URL{Scheme: "wss", Host: "echo.websocket.events"}
	ws.Dial(u.String())
}

func PingHandler() {
	log.Println("Send ping")
	err := ws.Send([]byte(`ping`))
	if err != nil {
		log.Println("Ping error:", err)
	}
}

func MessageHandler(_ int, msg []byte) {
	log.Println("Received message:", string(msg))
}
