package proxy

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// TestSetDeadlineUnblocksRead verifies that calling SetReadDeadline(time.Now())
// on a real TCP connection unblocks a goroutine that is stuck in Read().
// This is the core mechanism used by the CopyRawConnIfExist ctx-watcher fix:
// when the context is cancelled, the watcher sets a past deadline on the reader
// conn so that the splice (ReadFrom) returns immediately.
func TestSetDeadlineUnblocksRead(t *testing.T) {
	// Use real TCP connections because net.Pipe does not support deadlines.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer client.Close()

	server, err := ln.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer server.Close()

	// Start a goroutine that blocks on Read.  The server side never sends
	// anything, so this will block until the deadline is set.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, err := client.Read(buf)
		done <- err
	}()

	// Give the goroutine time to enter the Read syscall.
	time.Sleep(500 * time.Millisecond)

	// Set deadline to the past — this should unblock the Read.
	client.SetReadDeadline(time.Now())

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from Read after SetReadDeadline, got nil")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expected os.ErrDeadlineExceeded, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SetReadDeadline(time.Now()) did not unblock Read within 3 seconds")
	}
}

// TestClearDeadlineAfterUnblock verifies the full pattern used in the splice
// fix: set a past deadline to unblock, then clear it (zero time) so the conn
// is usable again.
func TestClearDeadlineAfterUnblock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer client.Close()

	server, err := ln.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer server.Close()

	// Force a timeout.
	client.SetReadDeadline(time.Now())
	buf := make([]byte, 1024)
	_, err = client.Read(buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got: %v", err)
	}

	// Clear the deadline — conn should be usable again.
	client.SetReadDeadline(time.Time{})

	// Write from server side, read from client side.
	payload := []byte("hello after clear")
	go func() {
		server.Write(payload)
	}()

	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("Read after clearing deadline failed: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("expected %q, got %q", payload, buf[:n])
	}
}

// TestSpliceCtxCancelPattern simulates the exact pattern from the
// CopyRawConnIfExist fix: one goroutine does a blocking read (simulating
// splice/ReadFrom), while a watcher goroutine waits for a "cancel" signal and
// then sets a past deadline to unblock the reader.
func TestSpliceCtxCancelPattern(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer client.Close()

	server, err := ln.Accept()
	if err != nil {
		t.Fatalf("failed to accept: %v", err)
	}
	defer server.Close()

	// Simulate the ctx.Done() + spliceDone pattern from the fix.
	ctxDone := make(chan struct{})    // simulates ctx.Done()
	spliceDone := make(chan struct{}) // signals splice completed

	// Watcher goroutine — mirrors the goroutine in CopyRawConnIfExist.
	go func() {
		select {
		case <-ctxDone:
			client.SetReadDeadline(time.Now())
		case <-spliceDone:
			// Splice finished normally, nothing to do.
		}
	}()

	// Reader goroutine — simulates the splice/ReadFrom blocking call.
	readResult := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, err := client.Read(buf)
		readResult <- err
	}()

	// Give everything time to settle into blocking state.
	time.Sleep(500 * time.Millisecond)

	// "Cancel" the context.
	close(ctxDone)

	select {
	case err := <-readResult:
		// Signal the watcher that splice is done (cleanup).
		close(spliceDone)

		// Clear the deadline (mirrors the fix).
		client.SetReadDeadline(time.Time{})

		if err == nil {
			t.Fatal("expected error after ctx cancel forced deadline, got nil")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expected os.ErrDeadlineExceeded, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx cancel did not unblock the read within 3 seconds")
	}
}
