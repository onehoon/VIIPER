package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

// These tests harden the detached-ready typed-device contract that future
// embedded consumers (SteamInputAddonforClaw) depend on: a typed device
// created with autoAttachLocalhost=false accepts state/callback mutation
// before its first attachment, an explicit Detach does not end the logical
// device's lifetime, and the same typed handle may be reattached while the
// server remains active. Only logical removal ends that lifetime.

func TestSteamDeckDetachedReadyLifecycle(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9260)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(90 + attachCalls)}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}

	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9260, deck, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}
	if attachCalls != 0 {
		t.Fatalf("autoAttachLocalhost=false invoked localhost attach %d times at creation", attachCalls)
	}

	if !setSteamDeckDeviceState(uintptr(h), steamDeckState{A: true}) {
		t.Fatal("SetSteamDeckDeviceState rejected while detached")
	}
	received := make(chan struct{}, 1)
	if !setSteamDeckOutputCallback(uintptr(h), func(steamdeck.OutputState) {
		select {
		case received <- struct{}{}:
		default:
		}
	}) {
		t.Fatal("SetSteamDeckOutputCallback registration rejected while detached")
	}
	if !setSteamDeckOutputCallback(uintptr(h), nil) {
		t.Fatal("SetSteamDeckOutputCallback clear rejected while detached")
	}

	if !attachUSBDevice(uintptr(h)) {
		t.Fatal("initial attach failed")
	}
	if !detachUSBDevice(uintptr(h)) {
		t.Fatal("initial detach failed")
	}
	identityAfterFirstCycle, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identityAfterFirstCycle.attachment.state != attachmentDetached {
		t.Fatal("device did not end the first attach/detach cycle in the detached state")
	}

	if !setSteamDeckDeviceState(uintptr(h), steamDeckState{B: true}) {
		t.Fatal("SetSteamDeckDeviceState rejected after detach")
	}

	if !attachUSBDevice(uintptr(h)) {
		t.Fatal("reattach using the same typed handle failed")
	}
	if attachCalls != 2 {
		t.Fatalf("attachCalls = %d, want 2 (reattach must not be a no-op or a new device)", attachCalls)
	}
	identityAfterReattach, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identityAfterReattach.exportMeta.BusID != identityAfterFirstCycle.exportMeta.BusID || identityAfterReattach.exportMeta.DevID != identityAfterFirstCycle.exportMeta.DevID {
		t.Fatal("logical USB identity changed across reattach with the same typed handle")
	}

	if !detachUSBDevice(uintptr(h)) {
		t.Fatal("final detach failed")
	}
	if detachCalls != 2 {
		t.Fatalf("detachCalls = %d, want 2", detachCalls)
	}
	if !removeSteamDeckDevice(uintptr(h)) {
		t.Fatal("typed removal failed")
	}
	if lookupIdentityExists(uintptr(h)) {
		t.Fatal("typed handle remained valid after removal")
	}
	if hw.s.GetBus(9260) == nil {
		t.Fatal("typed removal removed the caller-owned bus")
	}
}

func TestXbox360DetachedReadyLifecycle(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9262)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: int32(110 + attachCalls)}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}

	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9262, pad, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}
	if attachCalls != 0 {
		t.Fatalf("autoAttachLocalhost=false invoked localhost attach %d times at creation", attachCalls)
	}

	setState := func() bool {
		return withActiveDeviceHandle(uintptr(h), func(dhw *deviceHandleWrapper) bool {
			pad, ok := dhw.device.(*xbox360.Xbox360)
			if !ok {
				return false
			}
			pad.UpdateInputState(xbox360.InputState{Buttons: 0x1000})
			return true
		})
	}
	setCallback := func(cb func(xbox360.XRumbleState)) bool {
		return withActiveDeviceHandle(uintptr(h), func(dhw *deviceHandleWrapper) bool {
			pad, ok := dhw.device.(*xbox360.Xbox360)
			if !ok {
				return false
			}
			pad.SetRumbleCallback(cb)
			return true
		})
	}

	if !setState() {
		t.Fatal("Xbox360 state update rejected while detached")
	}
	if !setCallback(func(xbox360.XRumbleState) {}) {
		t.Fatal("Xbox360 rumble callback registration rejected while detached")
	}
	if !setCallback(nil) {
		t.Fatal("Xbox360 rumble callback clear rejected while detached")
	}

	if !attachUSBDevice(uintptr(h)) {
		t.Fatal("initial attach failed")
	}
	if !detachUSBDevice(uintptr(h)) {
		t.Fatal("initial detach failed")
	}
	identityAfterFirstCycle, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identityAfterFirstCycle.attachment.state != attachmentDetached {
		t.Fatal("device did not end the first attach/detach cycle in the detached state")
	}

	if !setState() {
		t.Fatal("Xbox360 state update rejected after detach")
	}

	if !attachUSBDevice(uintptr(h)) {
		t.Fatal("reattach using the same typed handle failed")
	}
	if attachCalls != 2 {
		t.Fatalf("attachCalls = %d, want 2 (reattach must not be a no-op or a new device)", attachCalls)
	}
	identityAfterReattach, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identityAfterReattach.exportMeta.BusID != identityAfterFirstCycle.exportMeta.BusID || identityAfterReattach.exportMeta.DevID != identityAfterFirstCycle.exportMeta.DevID {
		t.Fatal("logical USB identity changed across reattach with the same typed handle")
	}

	if !detachUSBDevice(uintptr(h)) {
		t.Fatal("final detach failed")
	}
	if detachCalls != 2 {
		t.Fatalf("detachCalls = %d, want 2", detachCalls)
	}
	if !removeXbox360Device(uintptr(h)) {
		t.Fatal("typed removal failed")
	}
	if lookupIdentityExists(uintptr(h)) {
		t.Fatal("typed handle remained valid after removal")
	}
	if hw.s.GetBus(9262) == nil {
		t.Fatal("typed removal removed the caller-owned bus")
	}
}

// TestTypedDeviceRepeatedAttachDetachSameHandle guards against an
// accidental one-shot assumption anywhere in the attach/detach path: the
// same typed handle must survive many explicit attach/detach cycles without
// its logical identity changing, and must accept a state update in every
// detached window. Deterministic iteration count, no timing dependency.
func TestTypedDeviceRepeatedAttachDetachSameHandle(t *testing.T) {
	const iterations = 5

	hw, _ := newLifecycleTestServer(t, 9264)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(120 + attachCalls)}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}

	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9264, deck, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}

	baseline, ok := lookupDeviceIdentity(uintptr(h))
	if !ok {
		t.Fatal("identity lookup failed after creation")
	}

	for i := 1; i <= iterations; i++ {
		if !setSteamDeckDeviceState(uintptr(h), steamDeckState{A: i%2 == 0}) {
			t.Fatalf("iteration %d: state update rejected before attach", i)
		}
		if !attachUSBDevice(uintptr(h)) {
			t.Fatalf("iteration %d: attach failed", i)
		}
		if !detachUSBDevice(uintptr(h)) {
			t.Fatalf("iteration %d: detach failed", i)
		}
		identity, ok := lookupDeviceIdentity(uintptr(h))
		if !ok || identity.exportMeta.BusID != baseline.exportMeta.BusID || identity.exportMeta.DevID != baseline.exportMeta.DevID {
			t.Fatalf("iteration %d: logical identity changed across the cycle", i)
		}
		if identity.attachment.state != attachmentDetached {
			t.Fatalf("iteration %d: device did not end the cycle detached", i)
		}
	}

	if attachCalls != iterations || detachCalls != iterations {
		t.Fatalf("attachCalls=%d detachCalls=%d, want %d/%d", attachCalls, detachCalls, iterations, iterations)
	}
	if !removeSteamDeckDevice(uintptr(h)) {
		t.Fatal("typed removal failed after repeated cycles")
	}
	if hw.s.GetBus(9264) == nil {
		t.Fatal("typed removal removed the caller-owned bus")
	}
}
