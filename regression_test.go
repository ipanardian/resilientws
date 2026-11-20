package resilientws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func localSocket(t *testing.T, handler func(*websocket.Conn)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u := websocket.Upgrader{}
		conn, err := u.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func drainSocket(conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		var zero T
		return zero
	}
}

func TestConcurrentWritesWithQueueAndDeadline(t *testing.T) {
	const senders = 6
	const each = 40
	var count atomic.Int32
	received := make(chan struct{})
	url := localSocket(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if count.Add(1) == senders*each+1 {
				close(received)
			}
		}
	})
	ws := &Resws{WriteDeadline: time.Second, MessageQueueSize: 1}
	if err := ws.Send([]byte("offline")); err != nil {
		t.Fatal(err)
	}
	ws.Dial(url)
	defer ws.Close()

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				var err error
				switch j % 4 {
				case 0:
					err = ws.Send([]byte("send"))
				case 1:
					err = ws.SendJSON("send JSON")
				case 2:
					err = ws.WriteMessage(websocket.TextMessage, []byte("write"))
				case 3:
					err = ws.WriteJSON("write JSON")
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	receive(t, received, "all outbound frames")
}
