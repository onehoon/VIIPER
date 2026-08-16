package main

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// asyncLogQueueCapacity is a modest fixed capacity: enough to absorb a burst of diagnostic
// records without unbounded memory growth. This is not a tunable in this PR.
const asyncLogQueueCapacity = 512

// asyncLogFlushTimeout bounds how long Flush ever waits, in either direction (enqueueing the
// barrier, or waiting for the writer goroutine to reach it). Flush is best-effort and must never
// wait indefinitely for a stuck filesystem.
const asyncLogFlushTimeout = 500 * time.Millisecond

type asyncLogQueueItem struct {
	data      []byte
	flushDone chan struct{}
}

// asyncLogWriter is an io.Writer that never blocks its caller on the backing writer. Writes are
// copied and enqueued on a bounded FIFO channel; a single goroutine drains the channel and
// performs the actual (potentially slow) write. When the queue is full, the record is dropped
// and counted rather than blocking the caller -- this is what keeps filesystem/AV/filter-driver
// stalls off the Attach/Detach calling thread. This is deliberately just an io.Writer underneath
// slog.NewTextHandler, not a second logging framework: existing formatting, WithAttrs, WithGroup,
// and handler composition are all unaffected.
type asyncLogWriter struct {
	w       io.Writer
	queue   chan asyncLogQueueItem
	dropped atomic.Uint64
}

func newAsyncLogWriter(w io.Writer, capacity int) *asyncLogWriter {
	a := &asyncLogWriter{w: w, queue: make(chan asyncLogQueueItem, capacity)}
	go a.run()
	return a
}

// Write never blocks on the backing writer. It always reports success to the caller (matching
// the "logging failures must never become routing failures" contract) even when the record was
// actually dropped due to queue saturation.
func (a *asyncLogWriter) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	select {
	case a.queue <- asyncLogQueueItem{data: cp}:
	default:
		a.dropped.Add(1)
	}
	return len(p), nil
}

// Flush is a best-effort barrier: it waits (bounded by asyncLogFlushTimeout, in each direction)
// for every record enqueued before this call to reach the backing writer. It returns false,
// never an error, on any timeout -- callers must treat a false result as diagnostic-only and
// never as a reason to fail an operation such as CloseUSBServer.
func (a *asyncLogWriter) Flush() bool {
	done := make(chan struct{})
	select {
	case a.queue <- asyncLogQueueItem{flushDone: done}:
	case <-time.After(asyncLogFlushTimeout):
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(asyncLogFlushTimeout):
		return false
	}
}

func (a *asyncLogWriter) run() {
	for item := range a.queue {
		if item.flushDone != nil {
			close(item.flushDone)
			continue
		}
		_, _ = a.w.Write(item.data) // Best-effort: a write error here is diagnostic-only.
		if dropped := a.dropped.Swap(0); dropped > 0 {
			// Written directly to the backing writer, bypassing Write()/the queue entirely, so
			// this can never re-enqueue into the same saturated path or loop.
			_, _ = fmt.Fprintf(a.w, "libVIIPER logging backlog droppedLogRecords=%d\n", dropped)
		}
	}
}
