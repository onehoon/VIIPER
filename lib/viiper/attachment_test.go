package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/internal/server/api"
	serverusb "github.com/Alia5/VIIPER/internal/server/usb"
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

func TestDrainedTransportCannotBeReactivatedAfterLogicalRemoveFailure(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9146)
	attachCalls, detachCalls, removeCalls := 0, 0, 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 77}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}
	hw.ops.removeDevice = func(s *serverusb.Server, busID uint32, deviceID string) error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("injected logical removal failure")
		}
		return s.RemoveDeviceByIDWithoutBusCleanup(busID, deviceID)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9146, mustNewTestMouse(t), false)
	hw.lifecycleMu.Unlock()
	if !ok || !attachUSBDevice(uintptr(h)) {
		t.Fatal("setup attach failed")
	}
	if removeMouseDevice(uintptr(h)) {
		t.Fatal("logical removal unexpectedly succeeded")
	}
	if attachUSBDevice(uintptr(h)) {
		t.Fatal("drained transport was reactivated")
	}
	if attachCalls != 1 || detachCalls != 1 {
		t.Fatalf("attachCalls=%d detachCalls=%d, want 1/1", attachCalls, detachCalls)
	}
	if !removeMouseDevice(uintptr(h)) {
		t.Fatal("logical removal retry failed")
	}
	if detachCalls != 1 || removeCalls != 2 {
		t.Fatalf("retry calls attach=%d detach=%d remove=%d, want detach=1 remove=2", attachCalls, detachCalls, removeCalls)
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

func TestMalformedAttachmentTokenFailsClosed(t *testing.T) {
	for _, attachment := range []api.LocalhostAttachment{
		{Port: 37},
		{Backend: api.LocalhostAttachmentBackendCommand, Port: 0},
		{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: -1},
	} {
		t.Run("invalid token", func(t *testing.T) {
			hw, _ := newLifecycleTestServer(t, 9144)
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				return attachment, nil
			}
			hw.lifecycleMu.Lock()
			h, ok := hw.createDeviceLocked(9144, mustNewTestMouse(t), false)
			hw.lifecycleMu.Unlock()
			if !ok || attachUSBDevice(uintptr(h)) {
				t.Fatal("malformed attachment token was accepted")
			}
			dhw, identityOK := lookupDeviceIdentity(uintptr(h))
			if !identityOK || hw.state != serverCloseFailed || dhw.attachment.state != attachmentOutcomeUnknown {
				t.Fatalf("state=%s attachment=%+v", hw.state, dhw.attachment)
			}
		})
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

func TestTypedRemovalHonorsDetachFailureAndDoesNotFinalize(t *testing.T) {
	for _, detachErr := range []struct {
		name            string
		err             error
		wantServerState serverLifecycleState
	}{
		{name: "known", err: errors.New("device unavailable"), wantServerState: serverActive},
		{name: "unknown", err: api.ErrDetachmentOutcomeUnknown, wantServerState: serverCloseFailed},
	} {
		t.Run(detachErr.name, func(t *testing.T) {
			hw, _ := newLifecycleTestServer(t, 9145)
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 61}, nil
			}
			hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return detachErr.err }
			hw.lifecycleMu.Lock()
			h, ok := hw.createDeviceLocked(9145, mustNewTestMouse(t), true)
			if !ok || hw.removeDeviceLocked(hw.deviceHandleRecords[h], h) {
				hw.lifecycleMu.Unlock()
				t.Fatal("typed removal ignored detach failure")
			}
			retained := hw.deviceHandleRecords[h] != nil
			hw.lifecycleMu.Unlock()
			if !retained || hw.state != detachErr.wantServerState || !lookupIdentityExists(uintptr(h)) {
				t.Fatalf("record retained=%t state=%s", retained, hw.state)
			}
		})
	}
}

func TestClassifiedGordonRemovalDistinguishesKnownAndUnknownFailures(t *testing.T) {
	t.Run("known detach failure is retryable", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9149)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 65}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			calls++
			if calls == 1 {
				return errors.New("known failure")
			}
			return nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9149, mustNewTestMouse(t), true)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("setup failed")
		}
		if got := removeTypedDeviceResult(uintptr(h), func(any) bool { return true }); got != typedDeviceRemoveRetryableFailure {
			t.Fatalf("first result = %d, want retryable", got)
		}
		if got := removeTypedDeviceResult(uintptr(h), func(any) bool { return true }); got != typedDeviceRemoveSuccess {
			t.Fatalf("second result = %d, want success", got)
		}
	})

	t.Run("unknown detach outcome is unsafe and never retried", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9152)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 66}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			calls++
			return api.ErrDetachmentOutcomeUnknown
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9152, mustNewTestMouse(t), true)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("setup failed")
		}
		if got := removeTypedDeviceResult(uintptr(h), func(any) bool { return true }); got != typedDeviceRemoveUnsafeOutcomeUnknown {
			t.Fatalf("first result = %d, want unsafe", got)
		}
		if got := removeTypedDeviceResult(uintptr(h), func(any) bool { return true }); got != typedDeviceRemoveUnsafeOutcomeUnknown || calls != 1 {
			t.Fatalf("second result = %d calls=%d, want unsafe and one detach", got, calls)
		}
	})

	t.Run("logical removal failure is retryable after detach", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9155)
		hw.ops.removeDevice = func(*serverusb.Server, uint32, string) error { return errors.New("logical remove failure") }
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 67}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9155, mustNewTestMouse(t), true)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("setup failed")
		}
		if got := removeTypedDeviceResult(uintptr(h), func(any) bool { return true }); got != typedDeviceRemoveRetryableFailure {
			t.Fatalf("result = %d, want retryable", got)
		}
	})

	if got := removeTypedDeviceResult(0, func(any) bool { return true }); got != typedDeviceRemoveInvalid {
		t.Fatalf("invalid handle result = %d, want invalid", got)
	}
}

func TestLogicalRemoveFailureDoesNotRepeatSuccessfulDetach(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9146)
	detachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: 62}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { detachCalls++; return nil }
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9146, mustNewTestMouse(t), true)
	dhw := hw.deviceHandleRecords[h]
	if !ok || hw.s.RemoveDeviceByIDWithoutBusCleanup(dhw.exportMeta.BusID, "1") != nil {
		hw.lifecycleMu.Unlock()
		t.Fatal("setup failed")
	}
	if hw.removeDeviceLocked(dhw, h) || hw.removeDeviceLocked(dhw, h) {
		hw.lifecycleMu.Unlock()
		t.Fatal("logical remove unexpectedly succeeded")
	}
	hw.lifecycleMu.Unlock()
	if detachCalls != 1 || dhw.attachment.state != attachmentDetached {
		t.Fatalf("detach calls=%d state=%d", detachCalls, dhw.attachment.state)
	}
}

func TestCloseDetachFailureRetriesOnlyWhenOutcomeIsKnown(t *testing.T) {
	t.Run("known failure retries", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9147)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 63}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			calls++
			if calls == 1 {
				return errors.New("known failure")
			}
			return nil
		}
		hw.lifecycleMu.Lock()
		_, ok := hw.createDeviceLocked(9147, mustNewTestMouse(t), true)
		first := hw.closeLocked()
		second := hw.closeLocked()
		hw.lifecycleMu.Unlock()
		if !ok || first || !second || calls != 2 {
			t.Fatalf("ok=%t first=%t second=%t calls=%d", ok, first, second, calls)
		}
	})

	t.Run("unknown failure is never retried", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9148)
		calls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: 64}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			calls++
			return api.ErrDetachmentOutcomeUnknown
		}
		hw.lifecycleMu.Lock()
		_, ok := hw.createDeviceLocked(9148, mustNewTestMouse(t), true)
		first := hw.closeLocked()
		second := hw.closeLocked()
		hw.lifecycleMu.Unlock()
		if !ok || first || second || calls != 1 {
			t.Fatalf("ok=%t first=%t second=%t calls=%d", ok, first, second, calls)
		}
	})
}

func TestAttachmentMutationsSerializeWithCloseAndRemoval(t *testing.T) {
	t.Run("attach then close", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9149)
		attachStarted := make(chan struct{})
		releaseAttach := make(chan struct{})
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			close(attachStarted)
			<-releaseAttach
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: 65}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9149, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("setup failed")
		}
		attachDone := make(chan bool, 1)
		closeDone := make(chan bool, 1)
		go func() { attachDone <- attachUSBDevice(uintptr(h)) }()
		<-attachStarted
		go func() {
			hw.lifecycleMu.Lock()
			closeDone <- hw.closeLocked()
			hw.lifecycleMu.Unlock()
		}()
		close(releaseAttach)
		if !<-attachDone || !<-closeDone || hw.state != serverClosed {
			t.Fatalf("attach/close serialization failed: %s", hw.state)
		}
	})

	t.Run("detach then typed remove", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9150)
		detachStarted := make(chan struct{})
		releaseDetach := make(chan struct{})
		detachCalls := 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 66}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			detachCalls++
			close(detachStarted)
			<-releaseDetach
			return nil
		}
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9150, mustNewTestMouse(t), true)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("setup failed")
		}
		detachDone := make(chan bool, 1)
		removeDone := make(chan bool, 1)
		go func() { detachDone <- detachUSBDevice(uintptr(h)) }()
		<-detachStarted
		go func() {
			removeDone <- withActiveDeviceHandle(uintptr(h), func(dhw *deviceHandleWrapper) bool {
				return hw.removeDeviceLocked(dhw, h)
			})
		}()
		close(releaseDetach)
		if !<-detachDone || !<-removeDone || detachCalls != 1 || lookupIdentityExists(uintptr(h)) {
			t.Fatalf("detach/remove serialization failed: calls=%d", detachCalls)
		}
	})

	t.Run("attach then detach", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9151)
		attachStarted := make(chan struct{})
		releaseAttach := make(chan struct{})
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			close(attachStarted)
			<-releaseAttach
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: 67}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error { return nil }
		hw.lifecycleMu.Lock()
		h, ok := hw.createDeviceLocked(9151, mustNewTestMouse(t), false)
		hw.lifecycleMu.Unlock()
		if !ok {
			t.Fatal("setup failed")
		}
		attachDone := make(chan bool, 1)
		detachDone := make(chan bool, 1)
		go func() { attachDone <- attachUSBDevice(uintptr(h)) }()
		<-attachStarted
		go func() { detachDone <- detachUSBDevice(uintptr(h)) }()
		close(releaseAttach)
		if !<-attachDone || !<-detachDone {
			t.Fatal("attach/detach serialization failed")
		}
		dhw, identityOK := lookupDeviceIdentity(uintptr(h))
		if !identityOK || dhw.attachment.state != attachmentDetached {
			t.Fatal("attach/detach did not finish detached")
		}
	})
}
