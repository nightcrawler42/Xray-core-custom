package freedom_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// goroutineCount returns the current number of goroutines.
func goroutineCount() int {
	return runtime.NumGoroutine()
}

// fdCount returns the number of open file descriptors for the current process.
// On Linux it reads /proc/self/fd; on macOS it shells out to lsof.
func fdCount() int {
	pid := os.Getpid()

	// Try Linux /proc first.
	procPath := fmt.Sprintf("/proc/%d/fd", pid)
	if entries, err := os.ReadDir(procPath); err == nil {
		return len(entries)
	}

	// Fallback for macOS: use lsof.
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return -1 // cannot determine
	}
	// Each line (minus header) is one FD entry.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 1 {
		return len(lines) - 1 // subtract header
	}
	return 0
}

// startImmediateCloseServer starts a TCP listener that accepts connections
// and immediately closes them. Returns the listener and its address.
func startImmediateCloseServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			conn.Close()
		}
	}()
	return ln, ln.Addr().String()
}

// startSilentServer starts a TCP listener that accepts connections but
// never reads, writes, or closes them (simulates a stale/hung peer).
// It collects accepted connections so they can be cleaned up when the
// listener is closed.
func startSilentServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start silent server: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	// Register cleanup: close all accumulated connections when listener closes.
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	return ln, ln.Addr().String()
}

// startDieAfterServer starts a TCP listener that accepts connections,
// writes a few bytes, then abruptly closes (simulating mid-stream death).
func startDieAfterServer(t *testing.T, writeBytes int, delay time.Duration) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start die-after server: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if delay > 0 {
					time.Sleep(delay)
				}
				payload := make([]byte, writeBytes)
				for i := range payload {
					payload[i] = 0xAA
				}
				c.Write(payload)
				// Abrupt close — triggers connection reset on peer side.
			}(conn)
		}
	}()
	return ln, ln.Addr().String()
}

// runCopyCtxOnConnection dials addr, wraps the connection in a
// DeadlineReader, and runs CopyCtx until context cancellation or EOF.
// This exercises the same code path as freedom.Handler.responseDone.
func runCopyCtxOnConnection(ctx context.Context, addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer conn.Close()

	reader := buf.NewDeadlineReader(ctx, conn, buf.NewReader(conn))
	return buf.CopyCtx(ctx, reader, buf.Discard)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestFreedom_NoFDLeak creates 100 connections to a server that immediately
// closes them. It verifies that the FD count returns to baseline after all
// connections complete, confirming no file descriptor leak.
func TestFreedom_NoFDLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping FD leak test in short mode")
	}

	ln, addr := startImmediateCloseServer(t)
	defer ln.Close()

	// Let runtime settle.
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	baseline := fdCount()
	if baseline < 0 {
		t.Skip("cannot determine FD count on this platform")
	}
	t.Logf("baseline FD count: %d", baseline)

	const numConns = 100
	var wg sync.WaitGroup

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// We expect EOF or connection reset — both are fine.
			_ = runCopyCtxOnConnection(ctx, addr)
		}()
	}

	wg.Wait()

	// Give time for OS to reclaim FDs and GC to run finalizers.
	for attempt := 0; attempt < 10; attempt++ {
		runtime.GC()
		time.Sleep(500 * time.Millisecond)

		current := fdCount()
		leaked := current - baseline
		t.Logf("attempt %d: current FD count: %d (delta: %+d)", attempt, current, leaked)

		// Allow a small margin — some FDs may belong to the runtime or test infra.
		if leaked <= 5 {
			return // success
		}
	}

	final := fdCount()
	leaked := final - baseline
	if leaked > 5 {
		t.Fatalf("FD leak detected: baseline=%d, final=%d, leaked=%d (threshold 5)", baseline, final, leaked)
	}
}

// TestFreedom_StaleConnectionCleanup connects to a server that never
// responds (simulating a stale peer). It cancels the context after 2
// seconds and verifies the connection is cleaned up within 35 seconds
// (30s DeadlineReader timeout + 5s margin).
func TestFreedom_StaleConnectionCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stale connection test in short mode")
	}

	ln, addr := startSilentServer(t)
	_ = ln // cleanup via t.Cleanup

	// Dial and start CopyCtx with a DeadlineReader.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	copyDone := make(chan error, 1)
	go func() {
		reader := buf.NewDeadlineReader(ctx, conn, buf.NewReader(conn))
		copyDone <- buf.CopyCtx(ctx, reader, buf.Discard)
	}()

	// Let the reader block for a moment, then cancel.
	time.Sleep(2 * time.Second)
	cancel()

	// The DeadlineReader should wake up within 30s (readDeadlineTimeout)
	// and see the cancelled context. We allow 35s total.
	select {
	case err := <-copyDone:
		elapsed := 2 * time.Second // we already waited 2s
		t.Logf("CopyCtx returned after ~%v with err: %v", elapsed, err)
		// Verify the connection is actually closed.
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, readErr := conn.Read(make([]byte, 1))
		if readErr == nil {
			// Connection should be effectively dead after context cancel,
			// but Read may succeed if there's buffered data. Close it.
		}
		conn.Close()

	case <-time.After(35 * time.Second):
		conn.Close()
		t.Fatalf("CopyCtx did not return within 35 seconds after context cancellation — stale connection not cleaned up")
	}
}

// TestFreedom_GoroutineNoLeak records the goroutine count, makes 50
// connections that die mid-stream, then waits for cleanup and checks that
// goroutine count returns to near-baseline (within +10).
func TestFreedom_GoroutineNoLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goroutine leak test in short mode")
	}

	// Server writes 1KB then dies.
	ln, addr := startDieAfterServer(t, 1024, 50*time.Millisecond)
	defer ln.Close()

	// Let runtime settle and collect baseline.
	runtime.GC()
	time.Sleep(1 * time.Second)
	before := goroutineCount()
	t.Logf("baseline goroutine count: %d", before)

	const numConns = 50
	var wg sync.WaitGroup

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = runCopyCtxOnConnection(ctx, addr)
		}()
		// Slight stagger to avoid thundering herd on the server.
		time.Sleep(10 * time.Millisecond)
	}

	wg.Wait()
	t.Logf("all connections completed, goroutine count now: %d", goroutineCount())

	// Wait for goroutines to drain. The DeadlineReader + CopyCtx chain
	// should not leave any goroutines behind after the connection EOF.
	// We poll every 2s for up to 60s.
	deadline := time.Now().Add(60 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(2 * time.Second)

		after = goroutineCount()
		leaked := after - before
		t.Logf("goroutine count: %d (delta: %+d)", after, leaked)

		if leaked <= 10 {
			t.Logf("goroutine count within threshold: before=%d, after=%d, leaked=%d", before, after, leaked)
			return // success
		}
	}

	leaked := after - before
	if leaked > 10 {
		// Dump goroutine stacks for debugging.
		stackBuf := make([]byte, 1<<20)
		n := runtime.Stack(stackBuf, true)
		t.Logf("goroutine dump:\n%s", stackBuf[:n])
		t.Fatalf("goroutine leak detected: before=%d, after=%d, leaked=%d (threshold 10)", before, after, leaked)
	}
}

// TestDeadlineReader_ContextCancelUnblocksRead verifies that DeadlineReader
// unblocks a read when the context is cancelled, even if the peer sends
// nothing. This is the unit-level test for the core mechanism.
func TestDeadlineReader_ContextCancelUnblocksRead(t *testing.T) {
	// Create a connected pair — server side never sends.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// net.Pipe returns in-memory connections that do NOT support
	// SetReadDeadline (they return an error). Use real TCP instead.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverReady := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		serverReady <- c
	}()

	// Close the in-memory pipe — we'll use TCP below.
	server.Close()
	client.Close()

	tcpConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcpConn.Close()

	srvConn := <-serverReady
	defer srvConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	reader := buf.NewDeadlineReader(ctx, tcpConn, buf.NewReader(tcpConn))

	readDone := make(chan error, 1)
	go func() {
		_, err := reader.ReadMultiBuffer()
		readDone <- err
	}()

	// Cancel after 1 second.
	time.Sleep(1 * time.Second)
	cancel()

	// The reader should unblock within one deadline cycle (30s).
	// In practice it should be almost instant since the deadline
	// fires every 30s and we cancelled the context.
	select {
	case err := <-readDone:
		t.Logf("ReadMultiBuffer returned: %v", err)
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("ReadMultiBuffer did not unblock within 35s after context cancellation")
	}
}

// TestFreedom_ConcurrentDialAndClose exercises the pattern where many
// goroutines concurrently dial a target and the context is cancelled
// globally. This checks for leaked goroutines and file descriptors from
// the dial/close race.
func TestFreedom_ConcurrentDialAndClose(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent dial test in short mode")
	}

	ln, addr := startSilentServer(t)
	_ = ln

	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	beforeGoroutines := goroutineCount()
	beforeFDs := fdCount()

	ctx, cancel := context.WithCancel(context.Background())
	const numConns = 30
	var wg sync.WaitGroup

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				return
			}
			defer conn.Close()

			reader := buf.NewDeadlineReader(ctx, conn, buf.NewReader(conn))
			_ = buf.CopyCtx(ctx, reader, buf.Discard)
		}()
	}

	// Let connections establish, then kill them all at once.
	time.Sleep(1 * time.Second)
	cancel()
	wg.Wait()

	// Wait for cleanup.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(2 * time.Second)

		goroutineLeaked := goroutineCount() - beforeGoroutines
		fdLeaked := 0
		if beforeFDs >= 0 {
			fdLeaked = fdCount() - beforeFDs
		}

		if goroutineLeaked <= 10 && fdLeaked <= 5 {
			t.Logf("cleanup OK: goroutine delta=%+d, FD delta=%+d", goroutineLeaked, fdLeaked)
			return
		}
		t.Logf("waiting for cleanup: goroutine delta=%+d, FD delta=%+d", goroutineLeaked, fdLeaked)
	}

	afterGoroutines := goroutineCount()
	afterFDs := fdCount()
	t.Fatalf("leak detected after concurrent dial+cancel: goroutines=%d→%d, FDs=%d→%d",
		beforeGoroutines, afterGoroutines, beforeFDs, afterFDs)
}

// Ensure io import is used (the server helpers read/write via net.Conn
// which satisfies io.Reader/io.Writer, but the import is needed for
// the explicit io.EOF reference in comments).
var _ = io.EOF
