package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

func TestAttachmentLifecycleKeepsSameLogicalHandle(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9140)
	attachCalls, detachCalls := 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: int32(70 + attachCalls)}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { detachCalls++; return nil }
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9140, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	if !attachUSBDevice(uintptr(h)) || !attachUSBDevice(uintptr(h)) {
		t.Fatal("attach was not idempotent")
	}
	dhw, _ := lookupDeviceIdentity(uintptr(h))
	if dhw.attachment.state != attachmentAttached || dhw.attachment.attachment.Port != 71 || attachCalls != 1 {
		t.Fatalf("unexpected attachment %+v calls=%d", dhw.attachment, attachCalls)
	}
	if !detachUSBDevice(uintptr(h)) || !detachUSBDevice(uintptr(h)) {
		t.Fatal("detach was not idempotent")
	}
	if detachCalls != 1 || dhw.attachment.state != attachmentDetached {
		t.Fatalf("detachCalls=%d state=%d", detachCalls, dhw.attachment.state)
	}
	if !attachUSBDevice(uintptr(h)) || attachCalls != 2 {
		t.Fatalf("reattach calls=%d", attachCalls)
	}
	identity, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identity.exportMeta.BusID != 9140 {
		t.Fatal("logical handle changed after reattach")
	}
}

func TestAttachmentUnknownFailsClosedWithoutDestroyingLogicalDevice(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9141)
	calls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		calls++
		return api.LocalhostAttachment{}, api.ErrAttachmentOutcomeUnknown
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9141, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("create failed")
	}
	if attachUSBDevice(uintptr(h)) {
		t.Fatal("unknown attach succeeded")
	}
	if calls != 1 || hw.state != serverCloseFailed {
		t.Fatalf("calls=%d state=%s", calls, hw.state)
	}
	dhw, identityOK := lookupDeviceIdentity(uintptr(h))
	if !identityOK || dhw.attachment.state != attachmentOutcomeUnknown {
		t.Fatal("unknown logical ownership was destroyed")
	}
	if attachUSBDevice(uintptr(h)) || calls != 1 {
		t.Fatal("unknown attachment was retried")
	}
}

func TestKnownDetachFailureRetainsTokenButUnknownFailsClosed(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9142)
	detachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 88}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		if detachCalls == 1 {
			return errors.New("open failed")
		}
		return api.ErrDetachmentOutcomeUnknown
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9142, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok || !attachUSBDevice(uintptr(h)) {
		t.Fatal("setup attach failed")
	}
	if detachUSBDevice(uintptr(h)) {
		t.Fatal("known detach failure succeeded")
	}
	dhw, _ := lookupDeviceIdentity(uintptr(h))
	if dhw.attachment.state != attachmentAttached || dhw.attachment.attachment.Port != 88 {
		t.Fatal("known failure lost token")
	}
	if detachUSBDevice(uintptr(h)) {
		t.Fatal("unknown detach succeeded")
	}
	if hw.state != serverCloseFailed || dhw.attachment.state != attachmentOutcomeUnknown {
		t.Fatal("unknown detach was not fail-closed")
	}
}

func TestRemoveBusDetachesDevicesInRegistrationOrderAndRetriesOnlySurvivors(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9143)
	ports := int32(100)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		ports++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: ports}, nil
	}
	detachedPorts := make([]int32, 0, 3)
	failSecond := true
	hw.ops.detachLocalhost = func(_ context.Context, attachment api.LocalhostAttachment, _ *slog.Logger) error {
		detachedPorts = append(detachedPorts, attachment.Port)
		if attachment.Port == 102 && failSecond {
			return errors.New("known detach failure")
		}
		return nil
	}

	hw.lifecycleMu.Lock()
	h1, ok1 := hw.createDeviceLocked(9143, mustNewTestMouse(t), true)
	h2, ok2 := hw.createDeviceLocked(9143, mustNewTestMouse(t), true)
	if !ok1 || !ok2 || h1 == h2 {
		hw.lifecycleMu.Unlock()
		t.Fatal("setup attach failed")
	}
	if hw.removeBusLocked(9143) {
		hw.lifecycleMu.Unlock()
		t.Fatal("bus removal unexpectedly succeeded")
	}
	if hw.s.GetBus(9143) == nil || hw.deviceHandleRecords[h1] == nil || hw.deviceHandleRecords[h2] == nil {
		hw.lifecycleMu.Unlock()
		t.Fatal("known detach failure performed destructive cleanup")
	}
	failSecond = false
	if !hw.removeBusLocked(9143) {
		hw.lifecycleMu.Unlock()
		t.Fatal("bus removal retry failed")
	}
	hw.lifecycleMu.Unlock()

	want := []int32{101, 102, 102}
	if len(detachedPorts) != len(want) {
		t.Fatalf("detach order = %v, want %v", detachedPorts, want)
	}
	for i := range want {
		if detachedPorts[i] != want[i] {
			t.Fatalf("detach order = %v, want %v", detachedPorts, want)
		}
	}
}
