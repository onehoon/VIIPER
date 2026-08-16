package main

import (
	"io"
	"time"
)

// dailyLogWriter is what the daily-rollover layer writes to: the real destination for
// libVIIPER.log's bytes, plus the ability to reset (truncate) it in place when the local
// calendar day changes. Deliberately does not require Close: the real file is never closed
// during the process lifetime (see openRealEmbeddedLogFileHandler).
type dailyLogWriter interface {
	io.Writer
	// Reset truncates the destination back to empty, keeping the same logical file/destination.
	Reset() error
}

// calendarDay is the local (never UTC) year/month/day a record or a file's last-modified time
// belongs to. There is deliberately no public timezone configuration; this always reflects
// whatever the local machine considers "today."
type calendarDay struct {
	year  int
	month time.Month
	day   int
}

func calendarDayOf(t time.Time) calendarDay {
	y, m, d := t.Local().Date()
	return calendarDay{year: y, month: m, day: d}
}

// dailyRolloverWriter is the sole owner of libVIIPER.log's daily retention: exactly one file,
// current-local-calendar-day diagnostics only, reset in place (never a dated archive, never
// size-based rotation) on the first write of a new day. It lives entirely inside the async
// writer goroutine's call chain (asyncLogWriter.run -> this Write), so the day check and any
// reset never happen on a VIIPER operation thread; a producer only ever touches the bounded
// queue in asyncLogWriter.Write, never this.
//
// haveDay/day distinguish "we know the destination's existing content is from calendar day X"
// (typically seeded from the file's on-disk modification time at open) from "we don't know," in
// which case the first Write establishes today as the active day without resetting -- the safe
// assumption when the destination is new, since there is nothing stale to discard.
//
// If a same-day-change Reset fails, this deliberately does NOT fall back to appending onto the
// stale (pre-reset) file -- that would defeat the entire point of daily retention by letting
// old-day content accumulate indefinitely. Instead it suppresses file writes for the remainder
// of that day (the record is reported as "written" to the caller regardless -- diagnostic loss,
// never a caller-visible error -- and VIIPERLogCallback is entirely unaffected, since it is a
// separate handler that never goes through this writer). Suppression does not retry Reset on
// every subsequent record of the same day; the next real day change tries Reset again exactly
// once. This is the same fail-safe posture as "logging failures must never become routing
// failures," applied specifically so a failure never turns into unbounded historical
// accumulation either.
type dailyRolloverWriter struct {
	backing    dailyLogWriter
	now        func() time.Time
	haveDay    bool
	day        calendarDay
	suppressed bool
}

func newDailyRolloverWriter(backing dailyLogWriter, now func() time.Time, initialDay calendarDay, haveInitialDay bool) *dailyRolloverWriter {
	return &dailyRolloverWriter{backing: backing, now: now, day: initialDay, haveDay: haveInitialDay}
}

func (d *dailyRolloverWriter) Write(p []byte) (int, error) {
	today := calendarDayOf(d.now())
	switch {
	case !d.haveDay:
		d.day = today
		d.haveDay = true
		d.suppressed = false
	case today != d.day:
		d.day = today
		d.suppressed = d.backing.Reset() != nil
	}
	if d.suppressed {
		// Diagnostic loss only: report success to the caller (matching every other "logging
		// failure must never become a routing failure" path) without persisting into what would
		// otherwise be a stale, unbounded-accumulating file.
		return len(p), nil
	}
	return d.backing.Write(p)
}
