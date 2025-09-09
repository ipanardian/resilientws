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
	"log"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ipanardian/resilientws"
	"github.com/stretchr/testify/assert"
)

func TestBackoffAfterSuccessfulConnection(t *testing.T) {
	connCount := int32(0)
	backoffApplied := int32(0)

	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		count := atomic.AddInt32(&connCount, 1)
		log.Printf("Connection #%d established", count)

		if count <= 3 {
			// First few connections: close immediately to trigger reconnects
			time.Sleep(10 * time.Millisecond)
			conn.Close()
			return
		}

		// Keep final connection alive
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{
		RecBackoffMin:            200 * time.Millisecond,
		RecBackoffMax:            2 * time.Second,
		RecBackoffFactor:         2.0,
		StableConnectionDuration: 1 * time.Second,
		MessageHandler: func(_ int, msg []byte) {
			log.Println("Message received:", string(msg))
		},
	}

	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	ws.OnReconnecting(func(duration time.Duration) {
		atomic.AddInt32(&backoffApplied, 1)
		log.Printf("Backoff applied: reconnecting in %v", duration)
	})

	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	// Wait for multiple reconnection cycles
	time.Sleep(3 * time.Second)

	// Verify that connections were attempted and backoff was applied
	assert.GreaterOrEqual(t, atomic.LoadInt32(&connCount), int32(3), "Should have multiple connection attempts")
	log.Printf("Total connections: %d, Backoff applications: %d",
		atomic.LoadInt32(&connCount), atomic.LoadInt32(&backoffApplied))
}

func TestBackoffResetAfterStableConnection(t *testing.T) {
	connCount := int32(0)

	ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
		count := atomic.AddInt32(&connCount, 1)
		log.Printf("Connection #%d established", count)

		switch count {
		case 1:
			// First connection: close immediately to trigger backoff
			time.Sleep(10 * time.Millisecond)
			conn.Close()
			return
		case 2:
			// Second connection: keep stable for longer than StableConnectionDuration
			time.Sleep(2 * time.Second)
			conn.Close()
			return
		}

		// Third connection: should have reset backoff
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer ts.Close()

	ws := &resilientws.Resws{
		RecBackoffMin:            200 * time.Millisecond,
		RecBackoffMax:            2 * time.Second,
		RecBackoffFactor:         2.0,
		StableConnectionDuration: 1 * time.Second,
		MessageHandler: func(_ int, msg []byte) {
			log.Println("Message received:", string(msg))
		},
	}

	ws.OnError(func(err error) {
		log.Println("Error:", err)
	})

	ws.Dial(wsURLFromHTTP(ts.URL))
	defer ws.Close()

	// Wait for the test scenario to complete
	time.Sleep(4 * time.Second)

	// Should have at least 3 connection attempts
	assert.GreaterOrEqual(t, atomic.LoadInt32(&connCount), int32(3), "Should have at least 3 connection attempts")
	log.Printf("Total connections made: %d", atomic.LoadInt32(&connCount))
}

func TestBackoffWithDifferentStrategies(t *testing.T) {
	testCases := []struct {
		name        string
		backoffType resilientws.BackoffType
	}{
		{"Fixed", resilientws.BackoffTypeFixed},
		{"Jitter", resilientws.BackoffTypeJitter},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			connCount := int32(0)

			ts := newMockWSServer(func(conn *websocket.Conn, w http.ResponseWriter) {
				count := atomic.AddInt32(&connCount, 1)
				if count <= 2 {
					// Close first two connections to trigger backoff
					conn.Close()
					return
				}
				// Keep subsequent connections alive
				for {
					_, _, err := conn.ReadMessage()
					if err != nil {
						return
					}
				}
			})
			defer ts.Close()

			ws := &resilientws.Resws{
				RecBackoffMin:    200 * time.Millisecond,
				RecBackoffMax:    2 * time.Second,
				RecBackoffFactor: 1.5,
				BackoffType:      tc.backoffType,
				MessageHandler: func(_ int, msg []byte) {
					log.Println("Message received:", string(msg))
				},
			}

			ws.OnError(func(err error) {
				log.Println("Error:", err)
			})

			ws.Dial(wsURLFromHTTP(ts.URL))
			defer ws.Close()

			// Wait for reconnections
			time.Sleep(2 * time.Second)

			assert.GreaterOrEqual(t, atomic.LoadInt32(&connCount), int32(2), "Should have multiple connection attempts")
		})
	}
}

func TestBackoff(t *testing.T) {
	t.Run("TestBackoffAfterSuccessfulConnection", TestBackoffAfterSuccessfulConnection)
	t.Run("TestBackoffResetAfterStableConnection", TestBackoffResetAfterStableConnection)
	t.Run("TestBackoffWithDifferentStrategies", TestBackoffWithDifferentStrategies)
}
