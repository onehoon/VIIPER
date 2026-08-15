package steamcontroller

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// capturingLogHandler records every slog.Record it receives, so tests can assert on
// diagnostic transition-suppression behavior without depending on the native VIIPERLogCallback
// C ABI plumbing (that lives in lib/viiper and is exercised separately).
type capturingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *capturingLogHandler) WithGroup(string) slog.Handler            { return h }
func (h *capturingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}

func (h *capturingLogHandler) countAtLevel(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, r := range h.records {
		if r.Level == level {
			count++
		}
	}
	return count
}

func withCapturingLogger(t *testing.T) *capturingLogHandler {
	t.Helper()
	handler := &capturingLogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return handler
}

func TestDPadMaskFromInputState_OneHotAndCombinedEncoding(t *testing.T) {
	cases := []struct {
		name string
		st   InputState
		want byte
	}{
		{"neutral", InputState{}, 0x00},
		{"up", InputState{DPadUp: true}, 0x01},
		{"right", InputState{DPadRight: true}, 0x02},
		{"left", InputState{DPadLeft: true}, 0x04},
		{"down", InputState{DPadDown: true}, 0x08},
		{"up+right diagonal", InputState{DPadUp: true, DPadRight: true}, 0x03},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dpadMaskFromInputState(tc.st); got != tc.want {
				t.Fatalf("dpadMaskFromInputState(%+v) = 0x%02X, want 0x%02X", tc.st, got, tc.want)
			}
		})
	}
}

func TestLogDPadReportTransitionIfChanged_SuppressesRepeatedMask(t *testing.T) {
	handler := withCapturingLogger(t)
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	report := make([]byte, InputReportLen)
	report[9] = buttonByte9Up

	d.logDPadReportTransitionIfChanged(InputState{DPadUp: true}, report)
	d.logDPadReportTransitionIfChanged(InputState{DPadUp: true}, report)
	d.logDPadReportTransitionIfChanged(InputState{DPadUp: true}, report)

	if got := handler.countAtLevel(slog.LevelDebug); got != 1 {
		t.Fatalf("Debug log count for 3 identical masks = %d, want 1 (transition-gated)", got)
	}
}

func TestLogDPadReportTransitionIfChanged_LogsReleaseTransition(t *testing.T) {
	handler := withCapturingLogger(t)
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	pressed := make([]byte, InputReportLen)
	pressed[9] = buttonByte9Up
	released := make([]byte, InputReportLen)

	d.logDPadReportTransitionIfChanged(InputState{DPadUp: true}, pressed)
	d.logDPadReportTransitionIfChanged(InputState{}, released)

	if got := handler.countAtLevel(slog.LevelDebug); got != 2 {
		t.Fatalf("Debug log count for press+release = %d, want 2 (0x00->0x01, 0x01->0x00)", got)
	}
}

func TestLogDPadReportTransitionIfChanged_IgnoresUnrelatedUpperNibbleChurn(t *testing.T) {
	handler := withCapturingLogger(t)
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Same D-pad mask (Down), but Menu (upper nibble) toggles across calls -- must not be
	// mistaken for a D-pad transition.
	withMenu := make([]byte, InputReportLen)
	withMenu[9] = buttonByte9Down | buttonByte9Menu
	withoutMenu := make([]byte, InputReportLen)
	withoutMenu[9] = buttonByte9Down

	d.logDPadReportTransitionIfChanged(InputState{DPadDown: true, Menu: true}, withMenu)
	d.logDPadReportTransitionIfChanged(InputState{DPadDown: true}, withoutMenu)

	if got := handler.countAtLevel(slog.LevelDebug); got != 1 {
		t.Fatalf("Debug log count across Menu-only churn = %d, want 1 (D-pad mask never changed)", got)
	}
}

func TestLogDPadReportTransitionIfChanged_WarnsOnInvariantMismatch(t *testing.T) {
	handler := withCapturingLogger(t)
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot says Up is pressed, but the (deliberately corrupted, for this test only)
	// report byte does not reflect it -- a genuine serialization invariant violation.
	report := make([]byte, InputReportLen)
	d.logDPadReportTransitionIfChanged(InputState{DPadUp: true}, report)

	if got := handler.countAtLevel(slog.LevelWarn); got != 1 {
		t.Fatalf("Warn count for ABI/report mismatch = %d, want 1", got)
	}
}

func TestLogDPadReportTransitionIfChanged_NoWarningOnAgreement(t *testing.T) {
	handler := withCapturingLogger(t)
	d, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}

	report := make([]byte, InputReportLen)
	report[9] = buttonByte9Down
	d.logDPadReportTransitionIfChanged(InputState{DPadDown: true}, report)

	if got := handler.countAtLevel(slog.LevelWarn); got != 0 {
		t.Fatalf("Warn count when ABI and report agree = %d, want 0", got)
	}
}
