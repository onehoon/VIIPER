package main

import (
	"context"
	"log/slog"
	"runtime/cgo"
	"sync"
	"testing"
)

// capturingLogHandler mirrors the equivalent test helper in device/steamcontroller; it
// exists separately here because slog.Default() is process-global and lib/viiper's own
// production code (server.go) also mutates it via slog.SetDefault, so each package needs
// its own isolated test double rather than sharing one across module boundaries.
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

func TestDPadDiagMaskFromState_OneHotAndCombinedEncoding(t *testing.T) {
	cases := []struct {
		name string
		st   steamControllerState
		want uint8
	}{
		{"neutral", steamControllerState{}, 0x00},
		{"up", steamControllerState{DPadUp: true}, 0x01},
		{"right", steamControllerState{DPadRight: true}, 0x02},
		{"left", steamControllerState{DPadLeft: true}, 0x04},
		{"down", steamControllerState{DPadDown: true}, 0x08},
		{"up+right diagonal", steamControllerState{DPadUp: true, DPadRight: true}, 0x03},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dpadDiagMaskFromState(tc.st); got != tc.want {
				t.Fatalf("dpadDiagMaskFromState(%+v) = 0x%02X, want 0x%02X", tc.st, got, tc.want)
			}
		})
	}
}

func TestLogDPadABIDecodedIfChanged_SuppressesRepeatedMaskAndLogsRelease(t *testing.T) {
	handler := withCapturingLogger(t)
	hw, _ := newLifecycleTestServer(t, 9433)
	serverHandle := cgo.NewHandle(hw)
	serverHandleRecords.Store(uintptr(serverHandle), hw)
	t.Cleanup(func() { serverHandleRecords.Delete(uintptr(serverHandle)); serverHandle.Delete() })

	var handle deviceHandle
	if !createSteamControllerDevice(uintptr(serverHandle), &handle, 9433, false, 0, 0) {
		t.Fatal("failed to create test steam controller device")
	}

	logDPadABIDecodedIfChanged(uintptr(handle), steamControllerState{DPadUp: true})
	logDPadABIDecodedIfChanged(uintptr(handle), steamControllerState{DPadUp: true})
	logDPadABIDecodedIfChanged(uintptr(handle), steamControllerState{DPadUp: true})
	if got := handler.countAtLevel(slog.LevelDebug); got != 1 {
		t.Fatalf("Debug log count for 3 identical masks = %d, want 1 (transition-gated)", got)
	}

	logDPadABIDecodedIfChanged(uintptr(handle), steamControllerState{})
	if got := handler.countAtLevel(slog.LevelDebug); got != 2 {
		t.Fatalf("Debug log count after release = %d, want 2 (press + release)", got)
	}
}

func TestLogDPadABIDecodedIfChanged_UnknownHandleNoOps(t *testing.T) {
	handler := withCapturingLogger(t)
	// No device registered for this handle: must not log and must not panic.
	logDPadABIDecodedIfChanged(0xDEADBEEF, steamControllerState{DPadUp: true})
	if got := handler.countAtLevel(slog.LevelDebug); got != 0 {
		t.Fatalf("Debug log count for unknown handle = %d, want 0", got)
	}
}
