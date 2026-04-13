package resilientws

import "github.com/gorilla/websocket"

// getConn returns the current connection under a read lock.
func (r *Resws) getConn() *websocket.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Conn
}

// checkConnection snapshots conn and connected state atomically.
func (r *Resws) checkConnection() (*websocket.Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Conn, r.isConnected && r.Conn != nil
}
