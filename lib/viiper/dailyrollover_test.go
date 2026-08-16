package main

import (
	"errors"
	"testing"
	"time"
)

// These tests cover the daily single-file retention policy: one libVIIPER.log, current-local-
// calendar-day diagnostics only, reset in place (never a dated archive, never size-based
// rotation) on the first write after the local date changes. No real filesystem, no real wall
// clock, no sleeps used as synchronization -- the clock is always an injected func() time.Time.

// fakeDailyLogWriter is an in-memory dailyLogWriter fake: Write appends, Reset clears (simulating
// truncation of the same logical file) and is counted, and can be told to fail to prove rollover
// failures are diagnostic-only.
type fakeDailyLogWriter struct {
	writes     [][]byte
	resetCalls int
	resetErr   error
}

func (f *fakeDailyLogWriter) Write(p []byte) (int, error) {
	f.writes = append(f.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (f *fakeDailyLogWriter) Reset() error {
	f.resetCalls++
	if f.resetErr != nil {
		return f.resetErr
	}
	f.writes = nil
	return nil
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func dateAt(day, hour int) time.Time {
	return time.Date(2026, time.August, day, hour, 0, 0, 0, time.Local)
}

func TestDailyRolloverWriterSameDayAppendsNoReset(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	today := dateAt(16, 9)
	w := newDailyRolloverWriter(backing, fixedClock(today), calendarDayOf(today), true)

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}

	if backing.resetCalls != 0 {
		t.Fatalf("resetCalls = %d, want 0 (existing file already belongs to today)", backing.resetCalls)
	}
	if len(backing.writes) != 2 {
		t.Fatalf("writes = %d, want 2 (both appended)", len(backing.writes))
	}
}

func TestDailyRolloverWriterOlderExistingFileResetsBeforeFirstRecord(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	yesterday := calendarDayOf(dateAt(15, 23))
	today := dateAt(16, 0)
	w := newDailyRolloverWriter(backing, fixedClock(today), yesterday, true)

	if _, err := w.Write([]byte("first record of the new day\n")); err != nil {
		t.Fatal(err)
	}

	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want 1 (existing file was from an earlier day)", backing.resetCalls)
	}
	if len(backing.writes) != 1 || string(backing.writes[0]) != "first record of the new day\n" {
		t.Fatalf("writes = %+v, want exactly the new day's first record preserved after reset", backing.writes)
	}
}

func TestDailyRolloverWriterMidnightRolloverWhileRunning(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	day1 := calendarDayOf(dateAt(16, 8))
	current := dateAt(16, 8)
	now := func() time.Time { return current }
	w := newDailyRolloverWriter(backing, now, day1, true)

	if _, err := w.Write([]byte("day1-a\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("day1-b\n")); err != nil {
		t.Fatal(err)
	}
	if backing.resetCalls != 0 {
		t.Fatalf("resetCalls = %d, want 0 before the date changes", backing.resetCalls)
	}

	// The process stays running across midnight: the injected clock advances, no restart.
	current = dateAt(17, 0)
	if _, err := w.Write([]byte("day2-a\n")); err != nil {
		t.Fatal(err)
	}

	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want exactly 1 (reset on the first write of the new day)", backing.resetCalls)
	}
	if len(backing.writes) != 1 || string(backing.writes[0]) != "day2-a\n" {
		t.Fatalf("writes after midnight rollover = %+v, want only the new day's record (old day's content discarded, first new record preserved)", backing.writes)
	}

	// Further same-day writes must not reset again.
	if _, err := w.Write([]byte("day2-b\n")); err != nil {
		t.Fatal(err)
	}
	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d after a second same-day write, want still 1 (no repeated truncate)", backing.resetCalls)
	}
	if len(backing.writes) != 2 {
		t.Fatalf("writes = %d, want 2 after two same-day records post-rollover", len(backing.writes))
	}
}

func TestDailyRolloverWriterUnknownInitialDayNeverResetsOnFirstWrite(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	today := dateAt(16, 9)
	// haveInitialDay=false: the destination is new, or its age could not be determined (e.g. stat
	// failed). The first write must establish today as the active day without resetting anything
	// -- there is nothing stale to discard, and forcing a reset here would be an unforced failure
	// mode for a brand-new file.
	w := newDailyRolloverWriter(backing, fixedClock(today), calendarDay{}, false)

	if _, err := w.Write([]byte("first ever record\n")); err != nil {
		t.Fatal(err)
	}
	if backing.resetCalls != 0 {
		t.Fatalf("resetCalls = %d, want 0 when the initial day is unknown", backing.resetCalls)
	}
}

// TestDailyRolloverWriterResetFailureSuppressesStaleFileNotAppendsToIt proves the file-of-
// unknown-age contract: when a day-change Reset fails, the writer must NOT fall back to
// appending onto the pre-reset (now stale) file -- that would defeat daily retention by letting
// old-day content accumulate indefinitely. It must instead suppress file persistence for the
// rest of that day (reporting success to the caller regardless; diagnostic loss only), attempt
// Reset exactly once per day (no retry storm), and try again exactly once on the next real day
// change.
func TestDailyRolloverWriterResetFailureSuppressesStaleFileNotAppendsToIt(t *testing.T) {
	backing := &fakeDailyLogWriter{resetErr: errors.New("simulated truncate failure")}
	yesterday := calendarDayOf(dateAt(15, 12))
	current := dateAt(16, 9)
	now := func() time.Time { return current }
	w := newDailyRolloverWriter(backing, now, yesterday, true)

	if _, err := w.Write([]byte("record on the day reset failed\n")); err != nil {
		t.Fatalf("Write returned an error (%v); reset failures must never propagate", err)
	}
	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want 1 (reset was attempted)", backing.resetCalls)
	}
	if len(backing.writes) != 0 {
		t.Fatalf("writes = %d, want 0 (must not append onto the stale, un-reset file)", len(backing.writes))
	}

	// A second same-day write must not retry (and fail) Reset again -- no retry storm -- and
	// must also stay suppressed rather than accumulating into the stale file.
	if _, err := w.Write([]byte("second same-day record\n")); err != nil {
		t.Fatal(err)
	}
	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d after a second same-day write, want still 1 (no retry storm)", backing.resetCalls)
	}
	if len(backing.writes) != 0 {
		t.Fatalf("writes = %d after a second same-day write, want still 0", len(backing.writes))
	}

	// The next real day change must try Reset exactly once more; if it succeeds this time,
	// persistence resumes for the new day's records.
	backing.resetErr = nil
	current = dateAt(17, 0)
	if _, err := w.Write([]byte("first record of the day after the failure\n")); err != nil {
		t.Fatal(err)
	}
	if backing.resetCalls != 2 {
		t.Fatalf("resetCalls = %d, want 2 (exactly one more attempt on the next day change)", backing.resetCalls)
	}
	if len(backing.writes) != 1 || string(backing.writes[0]) != "first record of the day after the failure\n" {
		t.Fatalf("writes = %+v, want exactly the new day's record now that reset succeeded", backing.writes)
	}
}

// TestDailyRolloverProducerNonBlockingEvenWhenResetIsSlow proves that a slow/stuck Reset() inside
// the daily-rollover layer -- reached only from inside the async writer goroutine -- still never
// blocks a producer's asyncLogWriter.Write() call, exactly like a slow plain backing write.
func TestDailyRolloverProducerNonBlockingEvenWhenResetIsSlow(t *testing.T) {
	release := make(chan struct{})
	gate := &gateOnResetWriter{release: release}
	yesterday := calendarDayOf(dateAt(15, 12))
	current := dateAt(16, 9)
	now := func() time.Time { return current }
	rolling := newDailyRolloverWriter(gate, now, yesterday, true)
	a := newAsyncLogWriter(rolling, asyncLogQueueCapacity)

	done := make(chan struct{})
	go func() {
		_, _ = a.Write([]byte("triggers a same-write reset\n"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("producer Write blocked while a same-write daily reset was stuck; must never block")
	}
	close(release) // Let the gated reset finish so the writer goroutine doesn't leak past the test.
}

type gateOnResetWriter struct {
	release chan struct{}
	writes  [][]byte
}

func (g *gateOnResetWriter) Write(p []byte) (int, error) {
	g.writes = append(g.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (g *gateOnResetWriter) Reset() error {
	<-g.release
	return nil
}

// TestDailyRolloverCallbackUnaffectedByFileRollover proves file-sink rollover is entirely
// independent of the VIIPERLogCallback observer: the callback keeps receiving every record with
// no gap, duplication, or dependency on the file's reset, across a simulated day change.
func TestDailyRolloverCallbackUnaffectedByFileRollover(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	yesterday := calendarDayOf(dateAt(15, 12))
	current := dateAt(16, 9)
	now := func() time.Time { return current }
	rolling := newDailyRolloverWriter(backing, now, yesterday, true)

	callback := &recordingHandler{}
	logger := buildEmbeddedLogger(embeddedFileHandler(rolling), callback)

	logger.Info("first of the new day")
	logger.Info("second of the new day")

	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want 1", backing.resetCalls)
	}
	if len(callback.records) != 2 {
		t.Fatalf("callback received %d records, want 2 (unaffected by file rollover)", len(callback.records))
	}
}

// TestDailyRolloverSharedByMultipleServersHasNoCompetingRolloverState proves the architectural
// requirement that all NewUSBServer instances in a process share exactly one daily-rollover
// writer (the same singleton openRealEmbeddedLogFileHandler produces) rather than each server
// keeping its own rollover state: two independent slog.Logger wrappers built over the very same
// handler must observe exactly one reset when the day changes, never one reset per logger.
func TestDailyRolloverSharedByMultipleServersHasNoCompetingRolloverState(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	yesterday := calendarDayOf(dateAt(15, 12))
	current := dateAt(16, 9)
	now := func() time.Time { return current }
	rolling := newDailyRolloverWriter(backing, now, yesterday, true)

	sharedHandler := embeddedFileHandler(rolling)
	// Simulates two NewUSBServer instances, each with its own hw.logger, both built over the
	// exact same process-wide file handler -- exactly as openRealEmbeddedLogFileHandler's
	// sync.Once-cached handler is shared in production.
	serverALogger := buildEmbeddedLogger(sharedHandler, nil)
	serverBLogger := buildEmbeddedLogger(sharedHandler, nil)

	serverALogger.Info("server A's first record of the new day")
	serverBLogger.Info("server B's first record")
	serverALogger.Info("server A's second record")

	if backing.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want exactly 1 (one shared rollover state, not one per server)", backing.resetCalls)
	}
	if len(backing.writes) != 3 {
		t.Fatalf("writes = %d, want 3 (all three records preserved)", len(backing.writes))
	}
}

// TestDailyRolloverNeverCreatesADatedOrArchiveFile proves the rollover mechanism only ever calls
// Reset on the exact same backing destination instance -- it never constructs a new path, dated
// filename, or archive; there is structurally only one file involved.
func TestDailyRolloverNeverCreatesADatedOrArchiveFile(t *testing.T) {
	backing := &fakeDailyLogWriter{}
	yesterday := calendarDayOf(dateAt(15, 12))
	current := dateAt(16, 9)
	now := func() time.Time { return current }
	w := newDailyRolloverWriter(backing, now, yesterday, true)

	if _, err := w.Write([]byte("record\n")); err != nil {
		t.Fatal(err)
	}

	if w.backing != backing {
		t.Fatal("dailyRolloverWriter swapped its backing destination instead of resetting the same one")
	}
}
