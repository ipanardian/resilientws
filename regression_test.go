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

func TestCloseCancelsHandshake(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if attempts.Add(1) != 1 {
			t.Error("unexpected extra handshake")
			return
		}
		close(started)
		select {
		case <-req.Context().Done():
			close(canceled)
		case <-release:
			u := websocket.Upgrader{}
			conn, err := u.Upgrade(w, req, nil)
			if err == nil {
				defer conn.Close()
				drainSocket(conn)
			}
		}
	}))
	defer server.Close()
	defer close(release)

	ws := &Resws{HandshakeTimeout: 10 * time.Second}
	ws.OnConnected(func(string) { t.Error("OnConnected after Close") })
	dialReturned := make(chan struct{})
	go func() {
		ws.Dial("ws" + strings.TrimPrefix(server.URL, "http"))
		close(dialReturned)
	}()
	receive(t, started, "opening handshake")
	ws.Close()
	receive(t, canceled, "provider observing handshake cancellation")
	receive(t, dialReturned, "Dial returning")
	if ws.IsConnected() {
		t.Fatal("connected after closing an in-flight handshake")
	}
}

func TestReconnectWithOneProviderSlot(t *testing.T) {
	first := make(chan struct{}, 1)
	oldClosed := make(chan struct{}, 1)
	rejected := make(chan struct{}, 1)
	releaseSlot := make(chan struct{})
	slotReleased := make(chan struct{})
	answerRejection := make(chan struct{})
	reconnected := make(chan struct{}, 1)
	var active atomic.Bool
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := attempts.Add(1)
		if !active.CompareAndSwap(false, true) {
			if n == 2 {
				rejected <- struct{}{}
				select {
				case <-answerRejection:
				case <-req.Context().Done():
				}
			} else {
				t.Errorf("attempt %d while the provider slot is held", n)
			}
			http.Error(w, "max connections reached", http.StatusTooManyRequests)
			return
		}
		if n != 1 {
			defer active.Store(false)
		}
		u := websocket.Upgrader{}
		conn, err := u.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if n == 1 {
			first <- struct{}{}
			drainSocket(conn)
			oldClosed <- struct{}{}
			<-releaseSlot
			active.Store(false)
			close(slotReleased)
			return
		}
		reconnected <- struct{}{}
		drainSocket(conn)
	}))
	defer server.Close()
	var releaseOnce, answerOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlot) }) }
	answer := func() { answerOnce.Do(func() { close(answerRejection) }) }
	defer release()
	defer answer()

	ws := &Resws{HandshakeTimeout: 2 * time.Second, BackoffType: BackoffTypeFixed,
		RecBackoffMin: 150 * time.Millisecond, RecBackoffMax: time.Second}
	backoff := make(chan time.Duration, 4)
	ws.OnReconnecting(func(d time.Duration) { backoff <- d })
	ws.Dial("ws" + strings.TrimPrefix(server.URL, "http"))
	defer ws.Close()
	receive(t, first, "first connection")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ws.CloseAndReconnect() }()
	}
	receive(t, oldClosed, "provider observing old connection close")
	receive(t, rejected, "provider rejecting a replacement while the slot is held")
	wg.Wait()
	if n := attempts.Load(); n != 2 {
		t.Fatalf("got %d attempts while the replacement handshake was pending, want 2", n)
	}
	release()
	receive(t, slotReleased, "provider releasing slot")
	answer()
	if d := receive(t, backoff, "retry backoff"); d < ws.RecBackoffMin {
		t.Fatalf("retry delay = %v, want at least %v", d, ws.RecBackoffMin)
	}
	receive(t, reconnected, "connection after provider releases slot")
	if n := attempts.Load(); n != 3 {
		t.Fatalf("got %d total attempts, want 3", n)
	}
}

func TestCloseFromConnectionCallback(t *testing.T) {
	for _, kind := range []string{"message", "ping"} {
		t.Run(kind, func(t *testing.T) {
			var attempts atomic.Int32
			serverClosed := make(chan struct{}, 1)
			url := localSocket(t, func(conn *websocket.Conn) {
				if attempts.Add(1) != 1 {
					t.Error("unexpected replacement after Close")
				}
				if kind == "message" {
					if err := conn.WriteMessage(websocket.TextMessage, []byte("update")); err != nil {
						t.Error(err)
						return
					}
				}
				_, _, err := conn.ReadMessage()
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					t.Errorf("expected normal close, got %v", err)
				}
				serverClosed <- struct{}{}
			})

			returned := make(chan struct{}, 1)
			ws := &Resws{PingInterval: 10 * time.Millisecond}
			closeFromCallback := func() {
				ws.Close()
				returned <- struct{}{}
			}
			switch kind {
			case "message":
				ws.MessageHandler = func(int, []byte) { closeFromCallback() }
			case "ping":
				ws.PingHandler = closeFromCallback
			}
			ws.Dial(url)
			receive(t, returned, "Close returning from "+kind+" callback")
			receive(t, serverClosed, "provider observing close frame")
			if ws.IsConnected() {
				t.Fatal("connected after Close returned")
			}
			ws.Close()
		})
	}
}
