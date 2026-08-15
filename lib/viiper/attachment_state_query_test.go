package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

// These tests harden GetUSBDeviceAttachmentState: a read-only diagnostic query over VIIPER's own
// tracked localhost attachment ownership. They never assert anything about Windows PnP/HID/XInput
// readiness -- that is explicitly out of scope for this API -- only about the exact internal
// attachment state (detached/attached/outcome-unknown) and its query-lifecycle behavior.

func TestGetUSBDeviceAttachmentStateBasicTransitions(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9400)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 91}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }

	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9400, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}

	if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
		t.Fatalf("post-create state = %d/%t, want detached/true", got, ok)
	}

	if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
		t.Fatal("attach failed")
	}
	if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryAttached {
		t.Fatalf("post-attach state = %d/%t, want attached/true", got, ok)
	}

	if detachUSBDeviceResult(uintptr(h)) != deviceDetachSuccess {
		t.Fatal("detach failed")
	}
	if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
		t.Fatalf("post-detach state = %d/%t, want detached/true", got, ok)
	}
}

func TestGetUSBDeviceAttachmentStatePreservedAcrossClassifiedFailures(t *testing.T) {
	t.Run("known attach failure -> DETACHED", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9401)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{}, errors.New("known transport failure")
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9401, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if attachUSBDeviceResult(uintptr(h)) != deviceAttachRetryableFailure {
			t.Fatal("expected a retryable attach failure")
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
			t.Fatalf("state after known attach failure = %d/%t, want detached/true", got, ok)
		}
	})

	t.Run("known detach failure -> ATTACHED with the exact token unchanged", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9402)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 92}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			return errors.New("known detach failure")
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9402, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		if detachUSBDeviceResult(uintptr(h)) != deviceDetachRetryableFailure {
			t.Fatal("expected a retryable detach failure")
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryAttached {
			t.Fatalf("state after known detach failure = %d/%t, want attached/true", got, ok)
		}
		identity, ok := lookupDeviceIdentity(uintptr(h))
		if !ok || identity.attachment.attachment.Port != 92 {
			t.Fatal("known detach failure did not preserve the exact attachment token")
		}
	})

	t.Run("unknown attach outcome -> OUTCOME_UNKNOWN, query still succeeds under close-failed", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9403)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9403, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if attachUSBDeviceResult(uintptr(h)) != deviceAttachUnsafeOutcomeUnknown {
			t.Fatal("expected an unsafe unknown attach outcome")
		}
		if hw.state != serverCloseFailed {
			t.Fatalf("server state = %s, want close-failed", hw.state)
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryOutcomeUnknown {
			t.Fatalf("state after unknown attach outcome = %d/%t, want outcome-unknown/true", got, ok)
		}
	})

	t.Run("unknown detach outcome -> OUTCOME_UNKNOWN, query still succeeds under close-failed", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9404)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 93}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			return api.ErrDetachmentOutcomeUnknown
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9404, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		if detachUSBDeviceResult(uintptr(h)) != deviceDetachUnsafeOutcomeUnknown {
			t.Fatal("expected an unsafe unknown detach outcome")
		}
		if hw.state != serverCloseFailed {
			t.Fatalf("server state = %s, want close-failed", hw.state)
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryOutcomeUnknown {
			t.Fatalf("state after unknown detach outcome = %d/%t, want outcome-unknown/true", got, ok)
		}
	})
}

func TestGetUSBDeviceAttachmentStateDiagnosticLifecycle(t *testing.T) {
	t.Run("known DETACHED survives an unrelated close-failed server", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9405)
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9405, mustNewTestMouse(t), false)
		hw.state = serverCloseFailed
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
			t.Fatalf("state = %d/%t, want detached/true", got, ok)
		}
	})

	t.Run("known ATTACHED survives an unrelated close-failed server", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9406)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 94}, nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9406, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
			t.Fatal("setup attach failed")
		}
		hw.lifecycleMu.Lock()
		hw.state = serverCloseFailed
		hw.lifecycleMu.Unlock()
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryAttached {
			t.Fatalf("state = %d/%t, want attached/true", got, ok)
		}
	})

	t.Run("serverClosing rejects the query with no backend call and no mutation", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9407)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			calls++
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 95}, nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9407, mustNewTestMouse(t), false)
		hw.state = serverClosing
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("create failed")
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); ok {
			t.Fatalf("query succeeded under serverClosing: %d", got)
		}
		if calls != 0 {
			t.Fatalf("calls = %d, want 0 (query must never invoke the backend)", calls)
		}
		hw.lifecycleMu.Lock()
		stateUnchanged := hw.deviceHandleRecords[h].attachment.state
		hw.lifecycleMu.Unlock()
		if stateUnchanged != attachmentDetached {
			t.Fatalf("query mutated attachment state to %d", stateUnchanged)
		}
	})
}

func TestGetUSBDeviceAttachmentStateInvalidQueries(t *testing.T) {
	if _, ok := queryDeviceAttachmentState(0); ok {
		t.Fatal("zero handle accepted")
	}
	if _, ok := queryDeviceAttachmentState(0xDEADBEEF); ok {
		t.Fatal("stale/arbitrary handle accepted")
	}

	hw, _ := newLifecycleTestServer(t, 9408)
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9408, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	if !removeMouseDevice(uintptr(h)) {
		t.Fatal("removal failed")
	}
	if _, ok := queryDeviceAttachmentState(uintptr(h)); ok {
		t.Fatal("removed typed handle accepted")
	}

	if getUSBDeviceAttachmentState(uintptr(h), nil) {
		t.Fatal("NULL outState accepted")
	}
}

func TestGetUSBDeviceAttachmentStateRepeatedQueriesNeverInvokeBackend(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9409)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 96}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9409, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok || attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
		t.Fatal("setup attach failed")
	}
	for i := 0; i < 5; i++ {
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryAttached {
			t.Fatalf("iteration %d: state = %d/%t, want attached/true", i, got, ok)
		}
	}
	if attachCalls != 1 || detachCalls != 0 {
		t.Fatalf("attachCalls=%d detachCalls=%d, want 1/0 (queries must never invoke either backend)", attachCalls, detachCalls)
	}
}

func TestAttachmentStateEnumSizeAndValues(t *testing.T) {
	if got := attachmentStateResultCSize(); got != 4 {
		t.Fatalf("sizeof(USBDeviceAttachmentState) = %d, want 4", got)
	}
	if deviceAttachmentQueryDetached != 0 || deviceAttachmentQueryAttached != 1 || deviceAttachmentQueryOutcomeUnknown != 2 {
		t.Fatalf("query enum values = %d/%d/%d, want 0/1/2", deviceAttachmentQueryDetached, deviceAttachmentQueryAttached, deviceAttachmentQueryOutcomeUnknown)
	}
}

// TestGetUSBDeviceAttachmentStateAcceptsTypedFamilies proves the shared query works for the
// typed device families relevant to the future runtime composition. It does not duplicate every
// state transition per family -- that's already covered generically above -- only that the
// typed create/attach/detach path for each family produces a queryable, correct state.
func TestGetUSBDeviceAttachmentStateAcceptsTypedFamilies(t *testing.T) {
	cases := []struct {
		name string
		make func(t *testing.T) viiperusb.Device
	}{
		{name: "SteamDeck", make: func(t *testing.T) viiperusb.Device {
			d, err := steamdeck.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}},
		{name: "Xbox360", make: func(t *testing.T) viiperusb.Device {
			d, err := xbox360.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			return d
		}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			busID := uint32(9410 + i)
			hw, _ := newLifecycleTestServer(t, busID)
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 97}, nil
			}
			hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }

			dev := tc.make(t)
			hw.lifecycleMu.Lock()
			h, ok := hw.createDeviceLocked(busID, dev, false)
			hw.lifecycleMu.Unlock()
			if !ok {
				t.Fatal("create failed")
			}

			if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
				t.Fatalf("post-create state = %d/%t, want detached/true", got, ok)
			}
			if attachUSBDeviceResult(uintptr(h)) != deviceAttachSuccess {
				t.Fatal("attach failed")
			}
			if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryAttached {
				t.Fatalf("post-attach state = %d/%t, want attached/true", got, ok)
			}
		})
	}
}
