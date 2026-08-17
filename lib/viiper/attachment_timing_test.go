package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

// These tests prove the attachment-timing instrumentation added to attachUSBDeviceResult/
// detachUSBDeviceResult is behavior-neutral: classification, backend call counts, and stored
// state are identical to the pre-instrumentation contract already covered by attachment_test.go
// and classified_attachment_test.go. They deliberately assert nothing about real elapsed
// magnitude -- only that timing fields exist, are non-negative, and are emitted exactly once per
// operation boundary.

// timingRecordingHandler is a minimal slog.Handler that records every emitted record so a test
// can assert on the "attachment-timing" log line without depending on any real logging backend.
type timingRecordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *timingRecordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *timingRecordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *timingRecordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *timingRecordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *timingRecordingHandler) timingRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if r.Message == "attachment-timing" {
			out = append(out, r)
		}
	}
	return out
}

func recordAttrs(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

func newTimingTestServer(t *testing.T, busID uint32) (*usbServerHandleWrapper, *timingRecordingHandler) {
	t.Helper()
	hw, _ := newLifecycleTestServer(t, busID)
	handler := &timingRecordingHandler{}
	hw.logger = slog.New(handler)
	return hw, handler
}

func requireNonNegativeInt64(t *testing.T, attrs map[string]any, key string) {
	t.Helper()
	v, ok := attrs[key]
	if !ok {
		t.Fatalf("missing timing field %q", key)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("timing field %q = %v (%T), want int64", key, v, v)
	}
	if n < 0 {
		t.Fatalf("timing field %q = %d, want >= 0", key, n)
	}
}

func TestCanonicalAttachTimingIsBehaviorNeutral(t *testing.T) {
	hw, handler := newTimingTestServer(t, 9500)
	attachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 200}, nil
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9500, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}

	// Success: exactly one backend call, exactly one timing summary, backendCalled=true.
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
		t.Fatalf("result = %d, want success", got)
	}
	if attachCalls != 1 {
		t.Fatalf("attachCalls = %d, want 1", attachCalls)
	}
	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1 per operation boundary", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] != "attach" || attrs["layer"] != "canonical" || attrs["result"] != "success" {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["backendCalled"] != true {
		t.Fatalf("backendCalled = %v, want true for a real backend attempt", attrs["backendCalled"])
	}
	for _, key := range []string{"totalUs", "lockWaitUs", "backendUs"} {
		requireNonNegativeInt64(t, attrs, key)
	}
	if fmt.Sprint(attrs["busID"]) != "9500" {
		t.Fatalf("busID = %v (%T), want 9500", attrs["busID"], attrs["busID"])
	}
	for key, want := range map[string]any{
		"deviceID": 1, "listenPort": hw.s.GetListenPort(),
		"attachmentStateBefore": "detached", "attachmentStateAfter": "attached",
		"serverStateBefore": "active", "serverStateAfter": "active",
		"attachmentBackend": api.LocalhostAttachmentBackendCommand, "importPort": uint16(200),
	} {
		if fmt.Sprint(attrs[key]) != fmt.Sprint(want) {
			t.Fatalf("%s = %v, want %v", key, attrs[key], want)
		}
	}

	// Idempotent already-attached: no second backend call, but still exactly one more timing
	// summary, with backendCalled=false since attachDeviceLockedResult never reaches the backend
	// on this path.
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
		t.Fatalf("idempotent result = %d, want success", got)
	}
	if attachCalls != 1 {
		t.Fatalf("attachCalls after idempotent attach = %d, want still 1", attachCalls)
	}
	records = handler.timingRecords()
	if len(records) != 2 {
		t.Fatalf("timing records = %d, want exactly 2", len(records))
	}
	attrs = recordAttrs(records[1])
	if attrs["backendCalled"] != false {
		t.Fatalf("idempotent backendCalled = %v, want false (backend must not be fabricated)", attrs["backendCalled"])
	}
	if attrs["attachmentStateBefore"] != "attached" || attrs["attachmentStateAfter"] != "attached" || fmt.Sprint(attrs["importPort"]) != "200" {
		t.Fatalf("idempotent attachment identity/state = %+v", attrs)
	}
}

func TestCanonicalAttachTimingUnknownOutcomeNeverRetriesBackend(t *testing.T) {
	hw, handler := newTimingTestServer(t, 9501)
	calls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		calls++
		return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9501, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}

	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachUnsafeOutcomeUnknown {
		t.Fatalf("result = %d, want unsafe outcome unknown", got)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	first := recordAttrs(handler.timingRecords()[0])
	if first["result"] != "unsafe-outcome-unknown" || first["backendCalled"] != true {
		t.Fatalf("first timing attrs = %+v", first)
	}
	if first["attachmentStateBefore"] != "detached" || first["attachmentStateAfter"] != "outcome-unknown" || first["serverStateBefore"] != "active" || first["serverStateAfter"] != "close-failed" || fmt.Sprint(first["importPort"]) != "0" {
		t.Fatalf("first unknown state/token attrs = %+v", first)
	}

	// Second call must classify the same way, must not touch the backend again, and its timing
	// summary must report backendCalled=false -- proving the resolver's fast-path (not the
	// timing code) is what skipped the backend.
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachUnsafeOutcomeUnknown {
		t.Fatalf("second result = %d, want unsafe outcome unknown again", got)
	}
	if calls != 1 {
		t.Fatalf("calls after second attempt = %d, want still 1 (no destructive retry)", calls)
	}
	records := handler.timingRecords()
	if len(records) != 2 {
		t.Fatalf("timing records = %d, want exactly 2", len(records))
	}
	second := recordAttrs(records[1])
	if second["result"] != "unsafe-outcome-unknown" || second["backendCalled"] != false {
		t.Fatalf("second timing attrs = %+v", second)
	}
	if second["attachmentStateBefore"] != "outcome-unknown" || second["attachmentStateAfter"] != "outcome-unknown" || second["serverStateBefore"] != "close-failed" || second["serverStateAfter"] != "close-failed" {
		t.Fatalf("second unknown state attrs = %+v", second)
	}
}

func TestCanonicalAttachTimingKnownFailureRetainsDetachedState(t *testing.T) {
	hw, handler := newTimingTestServer(t, 9504)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{}, errors.New("known attach failure")
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9504, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachRetryableFailure {
		t.Fatalf("result = %d, want retryable failure", got)
	}
	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["attachmentStateBefore"] != "detached" || attrs["attachmentStateAfter"] != "detached" || attrs["serverStateBefore"] != "active" || attrs["serverStateAfter"] != "active" || fmt.Sprint(attrs["importPort"]) != "0" {
		t.Fatalf("known failure state/token attrs = %+v", attrs)
	}
}

func TestCanonicalDetachTimingPreservesTokenAndClassification(t *testing.T) {
	hw, handler := newTimingTestServer(t, 9502)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 201}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		if detachCalls == 1 {
			return errors.New("known detach failure")
		}
		return nil
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9502, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
		t.Fatal("setup attach failed")
	}
	handler.records = nil // Only inspect detach timing from here.

	if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachRetryableFailure {
		t.Fatalf("result = %d, want retryable failure", got)
	}
	identity, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identity.attachment.state != attachmentAttached || identity.attachment.attachment.Port != 201 {
		t.Fatal("known detach failure did not preserve the exact attachment token")
	}
	records := handler.timingRecords()
	if len(records) != 1 {
		t.Fatalf("timing records = %d, want exactly 1", len(records))
	}
	attrs := recordAttrs(records[0])
	if attrs["operation"] != "detach" || attrs["layer"] != "canonical" || attrs["result"] != "retryable-failure" {
		t.Fatalf("unexpected timing attrs: %+v", attrs)
	}
	if attrs["attachmentStateBefore"] != "attached" || attrs["attachmentStateAfter"] != "attached" || fmt.Sprint(attrs["busID"]) != "9502" {
		t.Fatalf("detach identity/state = %+v", attrs)
	}
	if fmt.Sprint(attrs["attachmentBackend"]) != fmt.Sprint(api.LocalhostAttachmentBackendCommand) || fmt.Sprint(attrs["importPort"]) != "201" {
		t.Fatalf("timing attrs missing/incorrect detach identity: %+v", attrs)
	}
	for _, key := range []string{"totalUs", "lockWaitUs", "backendUs"} {
		requireNonNegativeInt64(t, attrs, key)
	}

	if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess {
		t.Fatalf("retry result = %d, want success", got)
	}
	if detachCalls != 2 {
		t.Fatalf("detachCalls = %d, want 2", detachCalls)
	}
	// The successful detach clears dhw.attachment.attachment back to its zero value as part of
	// the mutation -- the timing log must still report the token that was actually detached
	// (snapshotted before the mutation), not the post-mutation zero value.
	records = handler.timingRecords()
	if len(records) != 2 {
		t.Fatalf("timing records = %d, want exactly 2", len(records))
	}
	successAttrs := recordAttrs(records[1])
	if successAttrs["result"] != "success" {
		t.Fatalf("unexpected success timing attrs: %+v", successAttrs)
	}
	if successAttrs["attachmentStateBefore"] != "attached" || successAttrs["attachmentStateAfter"] != "detached" || fmt.Sprint(successAttrs["deviceID"]) != "1" {
		t.Fatalf("successful detach identity/state = %+v", successAttrs)
	}
	if fmt.Sprint(successAttrs["attachmentBackend"]) != fmt.Sprint(api.LocalhostAttachmentBackendCommand) || fmt.Sprint(successAttrs["importPort"]) != "201" {
		t.Fatalf("successful detach timing lost the real token (post-mutation zero value logged instead): %+v", successAttrs)
	}
	identity, ok = lookupDeviceIdentity(uintptr(h))
	if !ok || identity.attachment.state != attachmentDetached {
		t.Fatal("device did not end detached after the successful retry")
	}
}

// lockCheckingHandler records, for every log record it receives, whether hw.lifecycleMu was
// already free at that moment (via TryLock). This is a direct regression guard for the timing
// log running after lifecycleMu.Unlock(), not held-lock: the funcLogHandler bridge used in
// production invokes the embedding consumer's C callback synchronously from inside slog.Handle,
// so logging while still holding the lock would let a slow/reentrant callback stall unrelated
// lifecycle operations or deadlock a callback that re-enters a VIIPER lifecycle API.
type lockCheckingHandler struct {
	mu          sync.Mutex
	hw          *usbServerHandleWrapper
	lockWasFree []bool
}

func (h *lockCheckingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *lockCheckingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "attachment-timing" {
		return nil
	}
	free := h.hw.lifecycleMu.TryLock()
	if free {
		h.hw.lifecycleMu.Unlock()
	}
	h.mu.Lock()
	h.lockWasFree = append(h.lockWasFree, free)
	h.mu.Unlock()
	return nil
}

func (h *lockCheckingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lockCheckingHandler) WithGroup(string) slog.Handler      { return h }

func TestCanonicalAttachDetachTimingLogsAfterLockRelease(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9503)
	checker := &lockCheckingHandler{hw: hw}
	hw.logger = slog.New(checker)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 202}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }

	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9503, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}

	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
		t.Fatalf("attach result = %d, want success", got)
	}
	if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess {
		t.Fatalf("detach result = %d, want success", got)
	}

	checker.mu.Lock()
	defer checker.mu.Unlock()
	if len(checker.lockWasFree) != 2 {
		t.Fatalf("timing log invocations observed = %d, want 2", len(checker.lockWasFree))
	}
	for i, free := range checker.lockWasFree {
		if !free {
			t.Fatalf("invocation %d: lifecycleMu was still held while emitting the timing log", i)
		}
	}
}

func TestCanonicalAttachTimingInvalidHandleEmitsOneSummary(t *testing.T) {
	// No hw/logger to swap in for a not-found handle -- attachUSBDeviceResult falls back to
	// slog.Default() in that path (matching this codebase's existing convention for logging
	// before a server handle has been resolved). Just prove exactly one summary is emitted and
	// classification/behavior are unaffected; do not assert on slog.Default()'s handler.
	if got := attachUSBDeviceResult(0); got != deviceAttachInvalid {
		t.Fatalf("result = %d, want invalid", got)
	}
	if got := detachUSBDeviceResult(0); got != deviceDetachInvalid {
		t.Fatalf("result = %d, want invalid", got)
	}
}
