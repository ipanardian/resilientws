package resilientws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// lockedWrite serialises writes through writeMu and applies WriteDeadline when configured.
// Callers must not hold r.mu when calling this.
func (r *Resws) lockedWrite(conn *websocket.Conn, fn func() error) error {
	r.mu.RLock()
	deadline := r.WriteDeadline
	r.mu.RUnlock()

	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if deadline > 0 {
		conn.SetWriteDeadline(time.Now().Add(deadline))
	}
	return fn()
}

// Send sends a message to the WebSocket server with a fallback queue
func (r *Resws) Send(msg []byte) error {
	conn := r.getConn()
	if conn != nil {
		err := r.lockedWrite(conn, func() error {
			return conn.WriteMessage(websocket.TextMessage, msg)
		})
		if err == nil {
			return nil
		}
	}

	return r.queueMessage(msg)
}

func (r *Resws) queueMessage(msg []byte) error {
	r.messageQueueMu.Lock()
	defer r.messageQueueMu.Unlock()

	if len(r.messageQueue) >= r.MessageQueueSize {
		return fmt.Errorf("message queue is full")
	}

	r.messageQueue = append(r.messageQueue, msg)

	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	r.Logger.Debug("Message queued for later delivery")
	return nil
}

// SendJSON sends a JSON message to the WebSocket server with a fallback queue
func (r *Resws) SendJSON(v any) (err error) {
	conn := r.getConn()
	if conn != nil {
		err = r.lockedWrite(conn, func() error {
			return conn.WriteJSON(v)
		})
		if err == nil {
			return nil
		}
	}

	return r.queueMessageJSON(v)
}

func (r *Resws) queueMessageJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}

	r.messageQueueMu.Lock()
	defer r.messageQueueMu.Unlock()

	if len(r.messageQueue) >= r.MessageQueueSize {
		return fmt.Errorf("message queue is full")
	}

	r.messageQueue = append(r.messageQueue, b)

	if r.Logger == nil {
		r.Logger = &defaultLogger{}
	}
	r.Logger.Debug("Message queued for later delivery")
	return nil
}

// processMessageQueue processes messages from the queue and sends them to the WebSocket server
func (r *Resws) processMessageQueue(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conn, ok := r.getConnForQueue()
			if !ok {
				continue
			}

			msg, ok := r.dequeueMessage()
			if !ok {
				continue
			}

			r.sendQueuedMessage(conn, msg)
		}
	}
}

func (r *Resws) getConnForQueue() (*websocket.Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.Conn, r.isConnected && r.Conn != nil
}

func (r *Resws) dequeueMessage() ([]byte, bool) {
	r.messageQueueMu.Lock()
	defer r.messageQueueMu.Unlock()

	if len(r.messageQueue) == 0 {
		return nil, false
	}

	msg := r.messageQueue[0]
	r.messageQueue = r.messageQueue[1:]
	return msg, true
}

func (r *Resws) sendQueuedMessage(conn *websocket.Conn, msg []byte) {
	err := r.lockedWrite(conn, func() error {
		return conn.WriteMessage(websocket.TextMessage, msg)
	})

	if err != nil {
		r.Logger.Error("Failed to send queued message: %v", err)
		r.requeueMessage(msg)
		return
	}

	r.Logger.Debug("Successfully sent queued message")
}

func (r *Resws) requeueMessage(msg []byte) {
	r.messageQueueMu.Lock()
	defer r.messageQueueMu.Unlock()

	if len(r.messageQueue) < r.MessageQueueSize {
		r.messageQueue = append(r.messageQueue, msg)
		r.Logger.Debug("Requeued failed message")
		return
	}

	r.Logger.Error("Failed to requeue message: queue full")
}

// reader loops and reads messages from the WebSocket connection and emits them as events
func (r *Resws) reader(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn := r.getConn()
			if conn == nil {
				return
			}

			if r.ReadDeadline > 0 {
				conn.SetReadDeadline(time.Now().Add(r.ReadDeadline))
			}

			msgType, msg, err := conn.ReadMessage()
			if r.handleReadError(err, conn) {
				return
			}

			if !r.handleMessage(msgType, msg) {
				return
			}
		}
	}
}

func (r *Resws) handleReadError(err error, conn *websocket.Conn) bool {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation) {
		return true
	}

	if err == nil {
		return false
	}

	r.mu.Lock()
	reconnect := r.shouldReconnect
	if r.Conn == conn {
		r.Conn = nil
	}
	r.mu.Unlock()

	r.emitEvent(Event{Type: EventError, Error: err})

	if reconnect {
		time.Sleep(100 * time.Millisecond)
		r.CloseAndReconnect()
	}

	return true
}

// handleMessage dispatches the message and returns false if the reader loop should stop.
func (r *Resws) handleMessage(msgType int, msg []byte) bool {
	switch msgType {
	case websocket.TextMessage, websocket.BinaryMessage:
		r.MessageHandler(msgType, msg)
	case websocket.CloseMessage:
		return false
	}
	return true
}

// ReadMessage manually reads a message from the websocket connection
func (r *Resws) ReadMessage() (msgType int, msg []byte, err error) {
	conn, ok := r.checkConnection()
	if !ok {
		return 0, nil, errNotConnected
	}

	msgType, msg, err = conn.ReadMessage()
	if r.handleReadCloseError(err) {
		return msgType, msg, nil
	}

	r.handleReadReconnect(err)

	return
}

func (r *Resws) handleReadCloseError(err error) bool {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
		return true
	}
	return false
}

func (r *Resws) handleReadReconnect(err error) {
	if err == nil {
		return
	}

	r.mu.Lock()
	reconnect := r.shouldReconnect
	r.mu.Unlock()

	if reconnect {
		r.CloseAndReconnect()
	}
}

// ReadJSON manually reads a JSON message from the websocket connection
func (r *Resws) ReadJSON(v any) (err error) {
	conn, ok := r.checkConnection()
	if !ok {
		return errNotConnected
	}

	err = conn.ReadJSON(v)
	if r.handleReadCloseError(err) {
		return
	}

	r.handleReadReconnect(err)

	return
}

// WriteMessage manually writes a message to the websocket connection
func (r *Resws) WriteMessage(msgType int, msg []byte) (err error) {
	conn, ok := r.checkConnection()
	if !ok {
		return errNotConnected
	}

	err = r.lockedWrite(conn, func() error {
		return conn.WriteMessage(msgType, msg)
	})

	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
	}

	return
}

// WriteJSON manually writes a JSON message to the websocket connection
func (r *Resws) WriteJSON(v any) (err error) {
	conn, ok := r.checkConnection()
	if !ok {
		return errNotConnected
	}

	err = r.lockedWrite(conn, func() error {
		return conn.WriteJSON(v)
	})

	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		r.Close()
	}

	return
}
