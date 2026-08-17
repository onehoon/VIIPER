package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

func TestXbox360ClassifiedRemovalABI(t *testing.T) {
	if got := xbox360DeviceRemoveResultSize(); got != 4 {
		t.Fatalf("Xbox360DeviceRemoveResult C ABI size = %d, want 4", got)
	}
	if typedDeviceRemoveSuccess != 0 || typedDeviceRemoveRetryableFailure != 1 ||
		typedDeviceRemoveUnsafeOutcomeUnknown != 2 || typedDeviceRemoveInvalid != 3 {
		t.Fatal("shared removal result values changed")
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
	detachCalls := 0
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
	if !lookupIdentityExists(uintptr(h)) || hw.state != serverActive {
		t.Fatal("known failure did not retain an active authoritative handle")
	}
	if got := removeXbox360DeviceResult(uintptr(h)); got != typedDeviceRemoveSuccess {
		t.Fatalf("retry result = %d, want success", got)
	}
	if detachCalls != 2 {
		t.Fatalf("detach calls = %d, want 2", detachCalls)
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
