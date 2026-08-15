package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

// These tests harden the classified attach/detach result APIs (AttachUSBDeviceEx /
// DetachUSBDeviceEx), which expose the native ownership classification that already exists
// internally (attachmentDetached/attachmentAttached/attachmentOutcomeUnknown, known-vs-unknown
// failure) rather than collapsing it into a bare bool. The legacy bool APIs must remain
// backward-compatible and must not perform an independent second mutation attempt.

func TestAttachUSBDeviceExClassification(t *testing.T) {
	t.Run("valid detached succeeds", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9300)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			calls++
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 80}, nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9300, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
			t.Fatalf("result = %d, want success", got)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
		if !attachUSBDevice(uintptr(h)) {
			t.Fatal("bool AttachUSBDevice disagreed with successful Ex result")
		}
	})

	t.Run("already attached succeeds without a second backend attach", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9301)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			calls++
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 81}, nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9301, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess {
			t.Fatalf("second attach result = %d, want success", got)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (idempotent attach must not re-invoke the backend)", calls)
		}
	})

	t.Run("known attach failure is retryable", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9302)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			calls++
			return api.LocalhostAttachment{}, errors.New("known transport failure")
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9302, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachRetryableFailure {
			t.Fatalf("result = %d, want retryable failure", got)
		}
		identity, ok := lookupDeviceIdentity(uintptr(h))
		if !ok || identity.attachment.state != attachmentDetached {
			t.Fatal("known failure did not leave the device safely detached")
		}
		if hw.state != serverActive {
			t.Fatalf("server state = %s, want active after a known retryable attach failure", hw.state)
		}
		// A retryable failure is safe to retry explicitly; the bool wrapper performing that retry
		// is a legitimate second attempt, not a violation of "no independent second mutation".
		if attachUSBDevice(uintptr(h)) {
			t.Fatal("bool AttachUSBDevice reported success for a retryable failure")
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2 (one initial attempt, one explicit retry)", calls)
		}
	})

	t.Run("unknown attach outcome is unsafe and never retried", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9303)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			calls++
			return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9303, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachUnsafeOutcomeUnknown {
			t.Fatalf("result = %d, want unsafe outcome unknown", got)
		}
		if hw.state != serverCloseFailed {
			t.Fatalf("server state = %s, want close-failed", hw.state)
		}
		// Second attempt after unsafe/unknown must remain UNSAFE_OUTCOME_UNKNOWN, must not call
		// the backend again, and must never be downgraded to INVALID: the handle is still valid,
		// only its native ownership evidence is unsafe.
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachUnsafeOutcomeUnknown {
			t.Fatalf("second result = %d, want unsafe outcome unknown again (not invalid)", got)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (must not retry after an unknown outcome)", calls)
		}
		if attachUSBDevice(uintptr(h)) {
			t.Fatal("bool AttachUSBDevice reported success for an unsafe unknown outcome")
		}
	})

	t.Run("invalid handle is rejected", func(t *testing.T) {
		if got := attachUSBDeviceResult(0); got != deviceAttachInvalid {
			t.Fatalf("zero handle result = %d, want invalid", got)
		}
		if got := attachUSBDeviceResult(0xDEADBEEF); got != deviceAttachInvalid {
			t.Fatalf("stale handle result = %d, want invalid", got)
		}
		if attachUSBDevice(0) {
			t.Fatal("bool AttachUSBDevice accepted an invalid handle")
		}
	})
}

func TestDetachUSBDeviceExClassification(t *testing.T) {
	t.Run("valid attached succeeds", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9310)
		attachCalls, detachCalls := 0, 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			attachCalls++
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 82}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			detachCalls++
			return nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9310, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess {
			t.Fatalf("result = %d, want success", got)
		}
		if attachCalls != 1 || detachCalls != 1 {
			t.Fatalf("attachCalls=%d detachCalls=%d, want 1/1", attachCalls, detachCalls)
		}
		if !detachUSBDevice(uintptr(h)) {
			t.Fatal("bool DetachUSBDevice disagreed with a successful Ex result (idempotent already-detached)")
		}
		if detachCalls != 1 {
			t.Fatalf("detachCalls = %d, want 1 (already-detached must not re-invoke the backend)", detachCalls)
		}
	})

	t.Run("known detach failure retains the exact token and is retryable", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9311)
		detachCalls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 83}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			detachCalls++
			if detachCalls == 1 {
				return errors.New("known detach failure")
			}
			return nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9311, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachRetryableFailure {
			t.Fatalf("result = %d, want retryable failure", got)
		}
		identity, ok := lookupDeviceIdentity(uintptr(h))
		if !ok || identity.attachment.state != attachmentAttached || identity.attachment.attachment.Port != 83 {
			t.Fatal("known failure lost the exact attachment token")
		}
		if hw.state != serverActive {
			t.Fatalf("server state = %s, want active after a known retryable detach failure", hw.state)
		}
		// Explicit retry using the same stored token must succeed once the backend recovers.
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess {
			t.Fatalf("retry result = %d, want success", got)
		}
		if detachCalls != 2 {
			t.Fatalf("detachCalls = %d, want 2", detachCalls)
		}
	})

	t.Run("unknown detach outcome is unsafe and never retried destructively", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9312)
		detachCalls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 84}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			detachCalls++
			return api.ErrDetachmentOutcomeUnknown
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9312, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachUnsafeOutcomeUnknown {
			t.Fatalf("result = %d, want unsafe outcome unknown", got)
		}
		if hw.state != serverCloseFailed {
			t.Fatalf("server state = %s, want close-failed", hw.state)
		}
		// Same fail-closed contract as attach: remains UNSAFE_OUTCOME_UNKNOWN, never re-runs the
		// backend, and is never downgraded to INVALID.
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachUnsafeOutcomeUnknown {
			t.Fatalf("second result = %d, want unsafe outcome unknown again (not invalid)", got)
		}
		if detachCalls != 1 {
			t.Fatalf("detachCalls = %d, want 1 (must not retry after an unknown outcome)", detachCalls)
		}
		if detachUSBDevice(uintptr(h)) {
			t.Fatal("bool DetachUSBDevice reported success for an unsafe unknown outcome")
		}
	})

	t.Run("invalid handle is rejected", func(t *testing.T) {
		if got := detachUSBDeviceResult(0); got != deviceDetachInvalid {
			t.Fatalf("zero handle result = %d, want invalid", got)
		}
		if got := detachUSBDeviceResult(0xDEADBEEF); got != deviceDetachInvalid {
			t.Fatalf("stale handle result = %d, want invalid", got)
		}
		if detachUSBDevice(0) {
			t.Fatal("bool DetachUSBDevice accepted an invalid handle")
		}
	})
}

func TestClassifiedAttachDetachSizeAndValues(t *testing.T) {
	if got := attachResultCSize(); got != 4 {
		t.Fatalf("sizeof(USBDeviceAttachResult) = %d, want 4", got)
	}
	if got := detachResultCSize(); got != 4 {
		t.Fatalf("sizeof(USBDeviceDetachResult) = %d, want 4", got)
	}
	if deviceAttachSuccess != 0 || deviceAttachRetryableFailure != 1 || deviceAttachUnsafeOutcomeUnknown != 2 || deviceAttachInvalid != 3 {
		t.Fatalf("attach enum values = %d/%d/%d/%d, want 0/1/2/3", deviceAttachSuccess, deviceAttachRetryableFailure, deviceAttachUnsafeOutcomeUnknown, deviceAttachInvalid)
	}
	if deviceDetachSuccess != 0 || deviceDetachRetryableFailure != 1 || deviceDetachUnsafeOutcomeUnknown != 2 || deviceDetachInvalid != 3 {
		t.Fatalf("detach enum values = %d/%d/%d/%d, want 0/1/2/3", deviceDetachSuccess, deviceDetachRetryableFailure, deviceDetachUnsafeOutcomeUnknown, deviceDetachInvalid)
	}
}
