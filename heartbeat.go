package resilientws

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
)

// heartbeat sends ping messages to the server to keep the connection alive
func (r *Resws) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(r.PingInterval)
	defer ticker.Stop()

	r.setupPongHandler()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.sendPing() {
				return
			}
			r.PingHandler()
		}
	}
}

func (r *Resws) setupPongHandler() {
	conn := r.getConn()
	if conn == nil || r.PongTimeout <= 0 {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
	conn.SetPongHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(r.PongTimeout))
		return nil
	})
}

func (r *Resws) sendPing() bool {
	conn := r.getConn()
	if conn == nil {
		return false
	}

	err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(r.PingInterval))
	return err == nil
}
