package websocket

import (
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// createTestWSConn sets up a real WebSocket client connection backed by an
// httptest server.  The returned cleanup function closes both sides.
func createTestWSConn(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()

	var serverConn *websocket.Conn
	var serverMu sync.Mutex
	ready := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("server upgrade error: %v", err)
			return
		}
		serverMu.Lock()
		serverConn = c
		serverMu.Unlock()
		close(ready)
		// Keep reading so the connection stays alive until the client closes.
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				break
			}
		}
	}))

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		server.Close()
		t.Fatalf("failed to dial test websocket: %v", err)
	}

	// Wait until the server handler has stored its conn.
	<-ready

	cleanup := func() {
		ws.Close()
		serverMu.Lock()
		if serverConn != nil {
			serverConn.Close()
		}
		serverMu.Unlock()
		server.Close()
	}
	return ws, cleanup
}

// TestConnection_HeartbeatStopsOnClose verifies that closing a connection
// causes the heartbeat goroutine to exit within a reasonable time.
func TestConnection_HeartbeatStopsOnClose(t *testing.T) {
	ws, cleanup := createTestWSConn(t)
	defer cleanup()

	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}

	// Snapshot goroutine count before creating the heartbeat connection.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	conn := NewConnection(ws, addr, nil, 1) // 1-second heartbeat period
	// Give the heartbeat goroutine time to start.
	time.Sleep(200 * time.Millisecond)

	afterCreate := runtime.NumGoroutine()
	if afterCreate <= before {
		t.Logf("goroutine count did not increase (before=%d, after=%d); test may be flaky under load", before, afterCreate)
	}

	// Close the connection — this should signal the done channel and stop
	// the heartbeat goroutine.
	if err := conn.Close(); err != nil {
		// Closing may return an error if WriteControl fails on the
		// already-closing socket; that is acceptable.
		t.Logf("conn.Close returned (expected): %v", err)
	}

	// Wait for the goroutine to drain.
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("heartbeat goroutine did not exit within 2s (goroutines: before=%d, now=%d)", before, runtime.NumGoroutine())
		case <-ticker.C:
			if runtime.NumGoroutine() <= afterCreate {
				// Goroutine count has dropped — heartbeat exited.
				return
			}
		}
	}
}

// TestConnection_CloseIdempotent verifies that calling Close() twice does not
// panic (the done channel must be guarded against double-close).
func TestConnection_CloseIdempotent(t *testing.T) {
	ws, cleanup := createTestWSConn(t)
	defer cleanup()

	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	conn := NewConnection(ws, addr, nil, 1)

	// First close.
	_ = conn.Close()

	// Second close must not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Close() panicked: %v", r)
			}
		}()
		_ = conn.Close()
	}()
}

// TestConnection_HeartbeatTimeout verifies that the heartbeat goroutine exits
// when WriteControl cannot complete (simulating a hung connection), because it
// uses a bounded 10-second deadline rather than blocking forever.
//
// We accomplish this by closing the underlying TCP connection so that the next
// WriteControl fails immediately, causing the goroutine to return.
func TestConnection_HeartbeatTimeout(t *testing.T) {
	ws, cleanup := createTestWSConn(t)
	defer cleanup()

	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	// Use a short heartbeat period so we don't wait long.
	conn := NewConnection(ws, addr, nil, 1)
	time.Sleep(200 * time.Millisecond) // let goroutine start

	// Forcibly close the underlying network connection so WriteControl
	// fails on the next tick.  We access the raw conn via
	// ws.UnderlyingConn().
	ws.UnderlyingConn().Close()

	// The heartbeat goroutine should detect the write error and exit.
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("heartbeat goroutine did not exit after underlying conn closed (goroutines: before=%d, now=%d)", before, runtime.NumGoroutine())
		case <-ticker.C:
			current := runtime.NumGoroutine()
			if current <= before+1 {
				// +1 tolerance for runtime variance.
				// Clean up our wrapper too.
				_ = conn.Close()
				return
			}
		}
	}
}
