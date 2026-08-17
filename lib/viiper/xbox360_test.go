package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/usbip"
)

func TestXbox360ClassifiedRemovalABI(t *testing.T) {
	if got := xbox360DeviceRemoveResultSize(); got != 4 {
		t.Fatalf("Xbox360DeviceRemoveResult C ABI size = %d, want 4", got)
	}
	if got, want := xbox360DeviceRemoveResultValues(), [4]int{0, 1, 2, 3}; got != want {
		t.Fatalf("Xbox360 C enum values = %v, want %v", got, want)
	}
}

func TestXbox360ClassifiedRemovalTypeGuardAndSuccess(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9400)
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	padHandle, ok := hw.createDeviceLocked(9400, pad, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}
	foreign, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	foreignHandle, ok := hw.createDeviceLocked(9400, foreign, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("foreign creation failed")
	}

	if got := removeXbox360DeviceResult(uintptr(foreignHandle)); got != typedDeviceRemoveInvalid {
		t.Fatalf("wrong-family result = %d, want invalid", got)
	}
	if !lookupIdentityExists(uintptr(foreignHandle)) {
		t.Fatal("wrong-family removal invalidated the foreign handle")
	}
	if got := removeXbox360DeviceResult(uintptr(padHandle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("Xbox360 result = %d, want success", got)
	}
	if got := removeXbox360DeviceResult(uintptr(padHandle)); got != typedDeviceRemoveInvalid {
		t.Fatalf("repeated result = %d, want invalid", got)
	}
	if hw.s.GetBus(9400) == nil {
		t.Fatal("typed removal removed caller-owned bus")
	}
}

func TestXbox360ClassifiedRemovalKnownFailureIsRetryable(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9401)
	detachCalls, callbackClears := 0, 0
	hw.onCallbackCleared = func(*deviceHandleWrapper) { callbackClears++ }
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 91}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		if detachCalls == 1 {
			return errors.New("known detach failure")
		}
		return nil
	}
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9401, pad, true)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveRetryableFailure {
		t.Fatalf("first result = %d, want retryable failure", got)
	}
	if !lookupIdentityExists(uintptr(h)) || hw.state != serverActive || callbackClears != 1 {
		t.Fatalf("known failure handle=%t state=%s callback clears=%d, want true/active/1", lookupIdentityExists(uintptr(h)), hw.state, callbackClears)
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveSuccess {
		t.Fatalf("retry result = %d, want success", got)
	}
	if detachCalls != 2 {
		t.Fatalf("detach calls = %d, want 2", detachCalls)
	}
}

func TestXbox360ClassifiedRemovalLogicalFailureIsRetryable(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9404)
	removeCalls := 0
	hw.ops.removeDevice = func(*serverusb.Server, uint32, string) error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("known logical remove failure")
		}
		return nil
	}
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9404, pad, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveRetryableFailure || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("first result=%d handle=%t, want retryable/true", got, lookupIdentityExists(uintptr(h)))
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveSuccess || removeCalls != 2 {
		t.Fatalf("retry result=%d remove calls=%d, want success/2", got, removeCalls)
	}
}

func TestXbox360ClassifiedRemovalUnknownIsStickyAndClearsCallback(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9403)
	detachCalls, callbackClears := 0, 0
	hw.onCallbackCleared = func(*deviceHandleWrapper) { callbackClears++ }
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 92}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return api.ErrDetachmentOutcomeUnknown
	}
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9403, pad, true)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveUnsafeOutcomeUnknown {
		t.Fatalf("first result = %d, want unsafe outcome unknown", got)
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveUnsafeOutcomeUnknown {
		t.Fatalf("repeated result = %d, want sticky unsafe outcome unknown", got)
	}
	if detachCalls != 1 || callbackClears != 1 || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("detach calls=%d callback clears=%d handle=%t, want 1/1/true", detachCalls, callbackClears, lookupIdentityExists(uintptr(h)))
	}
}

func TestXbox360LegacyRemoveProjectsClassifiedSuccess(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9402)
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9402, pad, false)
	hw.lifecycleMu.Unlock()
	if !ok || !removeXbox360Device(uintptr(h)) {
		t.Fatal("legacy bool removal did not project success")
	}
	if removeXbox360Device(uintptr(h)) {
		t.Fatal("legacy bool removal accepted stale handle")
	}
}

func TestXbox360LegacyRemoveProjectsFailureResults(t *testing.T) {
	t.Run("retryable failure", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9405)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 95}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			return errors.New("known detach failure")
		}
		pad, err := xbox360.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9405, pad, true)
		hw.lifecycleMu.Unlock()
		if !ok || removeXbox360Device(uintptr(h)) {
			t.Fatal("legacy bool removal did not return false for retryable failure")
		}
		if !lookupIdentityExists(uintptr(h)) || hw.state != serverActive {
			t.Fatal("retryable failure did not retain the authoritative active handle")
		}
	})

	t.Run("unsafe outcome unknown", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9406)
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 96}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			return api.ErrDetachmentOutcomeUnknown
		}
		pad, err := xbox360.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9406, pad, true)
		hw.lifecycleMu.Unlock()
		if !ok || removeXbox360Device(uintptr(h)) {
			t.Fatal("legacy bool removal did not return false for unsafe outcome")
		}
		if !lookupIdentityExists(uintptr(h)) || hw.state != serverCloseFailed {
			t.Fatal("unsafe outcome evidence was not retained fail-closed")
		}
	})
}
