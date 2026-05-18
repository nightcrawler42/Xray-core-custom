package buf_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/xtls/xray-core/common/buf"
)

// testDeadlineTimeout is the poll interval used in tests instead of the
// production 30-second default. Keeps test runtime under a few seconds.
const testDeadlineTimeout = 500 * time.Millisecond

// newTestDeadlineReader creates a DeadlineReader with a short poll interval
// suitable for tests. It returns the Reader interface (for ReadMultiBuffer)
// and uses SetDeadlineTimeout to override the 30s production default.
func newTestDeadlineReader(ctx context.Context, conn net.Conn, reader Reader) Reader {
	r := NewDeadlineReader(ctx, conn, reader)
	if dr, ok := r.(*DeadlineReader); ok {
		dr.SetDeadlineTimeout(testDeadlineTimeout)
	}
	return r
}

// TestDeadlineReader_NormalRead verifies that data available immediately is
// returned without artificial delay.
func TestDeadlineReader_NormalRead(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	payload := []byte("hello deadline reader")

	// Write data before the read so it's immediately available.
	go func() {
		server.Write(payload)
	}()

	ctx := context.Background()
	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	mb, err := reader.ReadMultiBuffer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mb.IsEmpty() {
		t.Fatal("expected non-empty MultiBuffer")
	}

	got := make([]byte, mb.Len())
	mb.Copy(got)
	ReleaseMulti(mb)

	if !bytes.Equal(got, payload) {
		t.Fatalf("expected %q, got %q", payload, got)
	}
}

// TestDeadlineReader_ContextCancelUnblocks is the critical test: a stalled
// connection (net.Pipe with no writer) should unblock when the context is
// cancelled. With the test timeout of 500ms, the reader wakes up quickly,
// checks the cancelled context, and returns context.Canceled.
func TestDeadlineReader_ContextCancelUnblocks(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	done := make(chan error, 1)
	go func() {
		_, err := reader.ReadMultiBuffer()
		done <- err
	}()

	// Cancel after 1 second. The reader's 500ms poll interval means it
	// will notice the cancellation within ~500ms of the cancel() call.
	time.Sleep(1 * time.Second)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DeadlineReader did not unblock after context cancel within 5s")
	}
}

// TestDeadlineReader_NilConn verifies that passing a nil conn returns the raw
// reader unwrapped (no DeadlineReader wrapping).
func TestDeadlineReader_NilConn(t *testing.T) {
	raw := NewReader(strings.NewReader("test"))
	result := NewDeadlineReader(context.Background(), nil, raw)

	// When conn is nil, NewDeadlineReader should return the raw reader as-is.
	if result != raw {
		t.Fatal("expected NewDeadlineReader(ctx, nil, reader) to return the raw reader")
	}
}

// TestDeadlineReader_RealTimeout uses a context with a short deadline to
// verify that the reader returns context.DeadlineExceeded when the deadline
// passes on a stalled connection.
func TestDeadlineReader_RealTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Use a short-lived context: 1 second deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := reader.ReadMultiBuffer()
		done <- err
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != context.DeadlineExceeded {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
		// With 500ms poll interval and 1s context deadline, should unblock
		// within ~1.5s (1s deadline + up to 500ms poll lag).
		if elapsed > 5*time.Second {
			t.Fatalf("took too long: %v", elapsed)
		}
		t.Logf("unblocked in %v", elapsed)
	case <-time.After(10 * time.Second):
		t.Fatal("DeadlineReader did not unblock after context deadline")
	}
}

// TestDeadlineReader_EOF verifies that when the remote side closes the
// connection, the reader returns io.EOF (not a timeout error).
func TestDeadlineReader_EOF(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	ctx := context.Background()
	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	// Close the server side to trigger EOF on the client.
	go func() {
		time.Sleep(100 * time.Millisecond)
		server.Close()
	}()

	_, err := reader.ReadMultiBuffer()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if err != io.EOF && !strings.Contains(err.Error(), "EOF") &&
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected EOF or closed error, got %v", err)
	}
}

// TestDeadlineReader_MultipleReads verifies that multiple successful reads
// work correctly, and that a subsequent context cancel is still detected.
func TestDeadlineReader_MultipleReads(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	messages := []string{"first", "second", "third"}
	var received []string

	for _, msg := range messages {
		payload := []byte(msg)
		go func() {
			server.Write(payload)
		}()

		mb, err := reader.ReadMultiBuffer()
		if err != nil {
			t.Fatalf("unexpected error on read: %v", err)
		}
		got := make([]byte, mb.Len())
		mb.Copy(got)
		ReleaseMulti(mb)
		received = append(received, string(got))
	}

	if len(received) != len(messages) {
		t.Fatalf("expected %d messages, got %d", len(messages), len(received))
	}
	for i, msg := range messages {
		if received[i] != msg {
			t.Fatalf("message %d: expected %q, got %q", i, msg, received[i])
		}
	}

	// Now cancel and verify the next read returns context.Canceled.
	cancel()
	_, err := reader.ReadMultiBuffer()
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled after cancel, got %v", err)
	}
}

// TestDeadlineReader_TimeoutRetry verifies that when a read deadline fires
// but the context is still alive, the reader retries and eventually returns
// data that arrives later.
func TestDeadlineReader_TimeoutRetry(t *testing.T) {
	// Use a TCP connection so SetReadDeadline operates on a real socket.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	var serverConn net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var acceptErr error
		serverConn, acceptErr = ln.Accept()
		if acceptErr != nil {
			t.Errorf("accept error: %v", acceptErr)
		}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial error: %v", err)
	}
	defer client.Close()
	wg.Wait()
	defer serverConn.Close()

	ctx := context.Background()
	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	payload := []byte("delayed payload")

	// Write data after a delay longer than the 500ms poll interval.
	// The reader will timeout at least once, find the context still alive,
	// retry, and eventually receive the data.
	go func() {
		time.Sleep(800 * time.Millisecond)
		serverConn.Write(payload)
	}()

	mb, err := reader.ReadMultiBuffer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mb.IsEmpty() {
		t.Fatal("expected data, got empty MultiBuffer")
	}

	got := make([]byte, mb.Len())
	mb.Copy(got)
	ReleaseMulti(mb)

	if !bytes.Equal(got, payload) {
		t.Fatalf("expected %q, got %q", payload, got)
	}
}

// TestDeadlineReader_AlreadyCancelledContext verifies that if the context is
// already cancelled before ReadMultiBuffer is called, it returns immediately.
func TestDeadlineReader_AlreadyCancelledContext(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	reader := newTestDeadlineReader(ctx, client, NewReader(client))

	start := time.Now()
	_, err := reader.ReadMultiBuffer()
	elapsed := time.Since(start)

	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("should have returned immediately, took %v", elapsed)
	}
}
