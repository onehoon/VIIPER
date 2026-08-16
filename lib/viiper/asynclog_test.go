package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests exercise asyncLogWriter directly: FIFO ordering, a non-blocking producer even when
// the backing writer is deliberately stuck, bounded capacity with drop accounting, writer/backend
// errors never propagating, and Flush's bounded best-effort semantics. No real disk, no sleeps
// used as synchronization (only as bounded timeouts on channels/signals), no real hardware.

// gatedWriter is a fake io.Writer whose Write blocks until the test releases it, letting tests
// deterministically prove the producer side never waits on the backing writer.
type gatedWriter struct {
	mu      sync.Mutex
	release chan struct{}
	writes  [][]byte
	err     error
}

func newGatedWriter() *gatedWriter { return &gatedWriter{release: make(chan struct{})} }

func (g *gatedWriter) Write(p []byte) (int, error) {
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return 0, g.err
	}
	g.writes = append(g.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (g *gatedWriter) Open() { close(g.release) }

func (g *gatedWriter) snapshot() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte(nil), g.writes...)
}

func TestAsyncLogWriterProducerNeverWaitsOnAStuckBackingWriter(t *testing.T) {
	gw := newGatedWriter() // Never opened during this test: every backing write blocks forever.
	a := newAsyncLogWriter(gw, 8)

	done := make(chan struct{})
	go func() {
		_, _ = a.Write([]byte("attachment-timing operation=attach\n"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Write blocked on a stuck backing writer; producer must never wait on disk I/O")
	}
}

func TestAsyncLogWriterPreservesFIFOOrder(t *testing.T) {
	gw := newGatedWriter()
	a := newAsyncLogWriter(gw, 32)
	gw.Open() // Backing writer accepts immediately; test only needs ordering, not blocking.

	const n = 20
	for i := 0; i < n; i++ {
		_, _ = a.Write([]byte(fmt.Sprintf("record-%d\n", i)))
	}
	if !a.Flush() {
		t.Fatal("flush timed out")
	}

	writes := gw.snapshot()
	if len(writes) != n {
		t.Fatalf("backing writer received %d writes, want %d", len(writes), n)
	}
	for i, w := range writes {
		want := fmt.Sprintf("record-%d\n", i)
		if string(w) != want {
			t.Fatalf("write %d = %q, want %q (FIFO order not preserved)", i, w, want)
		}
	}
}

func TestAsyncLogWriterQueueSaturationDropsRatherThanBlocks(t *testing.T) {
	gw := newGatedWriter() // Kept closed: nothing drains, so the queue fills and stays full.
	const capacity = 4
	a := newAsyncLogWriter(gw, capacity)

	// The writer goroutine immediately dequeues one item into its blocked Write call, freeing one
	// queue slot; fill well past capacity to force real drops regardless of that timing detail.
	const attempts = capacity + 20
	done := make(chan struct{})
	go func() {
		for i := 0; i < attempts; i++ {
			_, _ = a.Write([]byte("record\n"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("producer blocked while the queue was saturated; enqueue must be non-blocking")
	}

	if got := a.dropped.Load(); got == 0 {
		t.Fatal("expected at least one dropped record once the bounded queue saturated")
	}
}

func TestAsyncLogWriterDropAccountingSurfacesOnceCapacityReturns(t *testing.T) {
	gw := newGatedWriter()
	const capacity = 2
	a := newAsyncLogWriter(gw, capacity)

	// Saturate without draining.
	for i := 0; i < capacity+10; i++ {
		_, _ = a.Write([]byte("record\n"))
	}
	if a.dropped.Load() == 0 {
		t.Fatal("expected drops before the backing writer is opened")
	}

	gw.Open()
	if !a.Flush() {
		t.Fatal("flush timed out")
	}

	found := false
	for _, w := range gw.snapshot() {
		if strings.Contains(string(w), "libVIIPER logging backlog droppedLogRecords=") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a backlog notice to be written once the backing writer had capacity")
	}
	if got := a.dropped.Load(); got != 0 {
		t.Fatalf("dropped counter = %d after being reported, want reset to 0", got)
	}
}

func TestAsyncLogWriterBackingErrorsDoNotPropagate(t *testing.T) {
	gw := newGatedWriter()
	gw.err = errors.New("simulated disk failure")
	gw.Open()
	a := newAsyncLogWriter(gw, 8)

	n, err := a.Write([]byte("record\n"))
	if err != nil {
		t.Fatalf("Write returned an error (%v); backing-writer failures must never propagate to the caller", err)
	}
	if n == 0 {
		t.Fatal("Write reported 0 bytes written")
	}
	if !a.Flush() {
		t.Fatal("flush timed out despite the backing writer being open (errors must not stall the writer goroutine)")
	}
}

func TestAsyncLogWriterFlushDrainsWhenBackingWriterHealthy(t *testing.T) {
	gw := newGatedWriter()
	a := newAsyncLogWriter(gw, 8)
	gw.Open()

	_, _ = a.Write([]byte("record\n"))
	if !a.Flush() {
		t.Fatal("flush should succeed once the backing writer is healthy")
	}
	if len(gw.snapshot()) != 1 {
		t.Fatalf("backing writer received %d writes after flush, want 1", len(gw.snapshot()))
	}
}

func TestAsyncLogWriterFlushTimeoutOnStuckWriterNeverPanics(t *testing.T) {
	gw := newGatedWriter() // Never opened: Flush must time out, not hang or panic.
	a := newAsyncLogWriter(gw, 8)

	_, _ = a.Write([]byte("record\n"))

	done := make(chan bool, 1)
	go func() { done <- a.Flush() }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("flush reported success against a permanently stuck backing writer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Flush did not return within a bounded time against a stuck backing writer")
	}
}
