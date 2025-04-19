/*
 * The MIT License (MIT)
 *
 * Copyright (c) 2025 ipanardian, <github.com/ipanardian>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, Subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or Substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package test

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ipanardian/resilientws"
	"github.com/stretchr/testify/assert"
)

func newMockWSServer(handler func(*websocket.Conn, http.ResponseWriter)) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handler(conn, w)
	}))
	return s
}

func wsURLFromHTTP(urlStr string) string {
	u, _ := url.Parse(urlStr)
	u.Scheme = "ws"
	return u.String()
}

func TestConnectionState(t *testing.T) {
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{}

	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected before start")

	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, true, ws.IsConnected(), "Should be connected after start")

	ws.Close()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected after close")
}

func TestReconnectAfterServerDisconnect(t *testing.T) {
	connCount := int32(0)
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		atomic.AddInt32(&connCount, atomic.LoadInt32(&connCount)+1)
		if atomic.LoadInt32(&connCount) == 1 {
			conn.Close()
			return
		}
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer ts.Close()

	messageHandlerFn := func(_ int, msg []byte) {
		log.Println("Message received:", string(msg))
	}

	ws := &resilientws.Resws{
		MessageHandler: messageHandlerFn,
	}

	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected before start")

	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	time.Sleep(1100 * time.Millisecond)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&connCount), int32(1), "Should have reconnected")
	assert.Equal(t, true, ws.IsConnected(), "Should be connected after start")

	ws.Close()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected after close")
}

func TestReconnectOnInitialFailure(t *testing.T) {
	connCount := int32(0)
	upgrader := websocket.Upgrader{}
	shouldServe := atomic.Bool{}

	// Server that starts later
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldServe.Load() {
			http.Error(w, "not ready", http.StatusInternalServerError)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		atomic.AddInt32(&connCount, 1)
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				break
			}
		}
	}))

	messageHandlerFn := func(_ int, msg []byte) {
		log.Println("Message received:", string(msg))
	}

	ws := &resilientws.Resws{
		MessageHandler: messageHandlerFn,
	}

	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	go func() {
		log.Println("Dialing...")
		url := "ws://" + server.Listener.Addr().String()
		ws.Dial(url)
	}()
	defer ws.Close()

	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected after start")

	// wait for timeout
	time.Sleep(8000 * time.Millisecond)

	// start server
	shouldServe.Store(true)
	server.Start()
	defer server.Close()

	// wait for connection
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&connCount) == 0 {
		t.Errorf("expected successful connect after retry, got connCount=0")
	}

	// wait for reconnection
	time.Sleep(500 * time.Millisecond)

	assert.GreaterOrEqual(t, atomic.LoadInt32(&connCount), int32(1), "Should have reconnected")
	assert.Equal(t, true, ws.IsConnected(), "Should be reconnected")
}

func TestPingHandler(t *testing.T) {
	pingReceived := int32(0)
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		conn.SetPingHandler(func(appData string) error {
			return conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(time.Second))
		})
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{
		PingInterval: 100 * time.Millisecond,
	}
	ws.PingHandler = func() {
		atomic.StoreInt32(&pingReceived, 1)
	}

	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&pingReceived))
}

func TestSubscribeHandler(t *testing.T) {
	var ws *resilientws.Resws
	messageReceived := make(chan []byte, 1)
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messageReceived <- msg
		}
	})
	defer ts.Close()

	testMsg := []byte("test message")

	SubscribeHandlerFn := func() error {
		err := ws.Send(testMsg)
		if err != nil {
			return err
		}
		return nil
	}

	ws = &resilientws.Resws{
		SubscribeHandler: SubscribeHandlerFn,
	}

	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	time.Sleep(100 * time.Millisecond)

	select {
	case msg := <-messageReceived:
		assert.Equal(t, testMsg, msg)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for message")
	}
}

func TestSendMessage(t *testing.T) {
	messageReceived := make(chan []byte, 1)
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messageReceived <- msg
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{}
	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	testMsg := []byte("test message")
	err := ws.Send(testMsg)
	assert.NoError(t, err)

	select {
	case msg := <-messageReceived:
		assert.Equal(t, testMsg, msg)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for message")
	}
}

func TestSendJSON(t *testing.T) {
	messageReceived := make(chan []byte, 1)
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			messageReceived <- msg
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{}
	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	testMsg := map[string]string{"message": "test message"}
	err := ws.SendJSON(testMsg)
	assert.NoError(t, err)

	select {
	case msg := <-messageReceived:
		var receivedMsg map[string]string
		err := json.Unmarshal(msg, &receivedMsg)
		assert.NoError(t, err)
		assert.Equal(t, testMsg, receivedMsg)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for message")
	}
}

func TestMessageQueue(t *testing.T) {
	var msgReceived sync.WaitGroup
	msgReceived.Add(1)

	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			assert.Equal(t, []byte("queued message"), msg)
			msgReceived.Done()
			return
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{
		MessageQueueSize: 1,
	}

	// Initialize config before sending
	ws.Dial(wsURLFromHTTP(ts.URL))

	// Close connection to simulate disconnected state
	ws.Close()
	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected after close")

	// Send while disconnected
	err := ws.Send([]byte("queued message"))
	assert.NoError(t, err)

	// Reconnect
	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	if !assert.Eventually(t, func() bool {
		msgReceived.Wait()
		return true
	}, 2*time.Second, 100*time.Millisecond) {
		t.Fatal("Timeout waiting for queued message")
	}
}

func TestReadAndWriteMessage(t *testing.T) {
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			assert.Equal(t, []byte("Hello"), msg)

			// Send a response
			err = conn.WriteMessage(websocket.TextMessage, []byte("World!"))
			if err != nil {
				t.Fatal(err)
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{}
	ws.OnConnected(func(url string) {
		err := ws.WriteMessage(websocket.TextMessage, []byte("Hello"))
		if err != nil {
			t.Fatal(err)
		}
	})
	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	// Read the message
	time.Sleep(100 * time.Millisecond)
	msgType, msg, err := ws.ReadMessage()
	assert.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, msgType)
	assert.Equal(t, []byte("World!"), msg)
}

func TestReadAndWriteJSON(t *testing.T) {
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		var v map[string]string
		for {
			err := conn.ReadJSON(&v)
			if err != nil {
				return
			}
			assert.Equal(t, map[string]string{"message": "Hello"}, v)

			// Send a response
			err = conn.WriteJSON(map[string]string{"message": "World!"})
			if err != nil {
				t.Fatal(err)
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{}
	ws.OnConnected(func(url string) {
		err := ws.WriteJSON(map[string]string{"message": "Hello"})
		if err != nil {
			t.Fatal(err)
		}
	})
	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	// Read the message
	time.Sleep(100 * time.Millisecond)
	var v map[string]string
	err := ws.ReadJSON(&v)
	assert.NoError(t, err)
	assert.Equal(t, map[string]string{"message": "World!"}, v)
}

func TestCloseResilientws(t *testing.T) {
	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{}
	ws.Dial(wsURLFromHTTP(ts.URL))
	ws.Close()

	assert.Equal(t, false, ws.IsConnected(), "Should be disconnected after close")
}

func TestWebSocketws(t *testing.T) {
	t.Run("PingHandler", TestPingHandler)

	t.Run("SubscribeHandler", TestSubscribeHandler)

	t.Run("ReconnectAfterServerDisconnect", TestReconnectAfterServerDisconnect)

	t.Run("ReconnectOnInitialFailure", TestReconnectOnInitialFailure)

	t.Run("SendMessage", TestSendMessage)

	t.Run("SendJSON", TestSendJSON)

	t.Run("MessageQueue", TestMessageQueue)

	t.Run("ReadAndWriteMessage", TestReadAndWriteMessage)

	t.Run("ReadAndWriteJSON", TestReadAndWriteJSON)

}
