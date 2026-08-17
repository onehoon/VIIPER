package main

import (
	"context"
	"log/slog"
	"runtime/cgo"
	"sync"
	"testing"

	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

func TestConcurrentXbox360AttachUsesOneBackendInitiator(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9600)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var callsMu sync.Mutex
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(entered)
			<-release
		}
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 301}, nil
	}
	var h deviceHandle
	ok := createXbox360Device(serverHandleForTest(t, hw), &h, 9600, false, 0, 0, 0)
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}

	results := make(chan deviceAttachResult, 2)
	secondContending := make(chan struct{})
	secondNotContending := make(chan struct{})
	var lockAttempts int
	var lockAttemptsMu sync.Mutex
	hw.onAttachLockAttempt = func() {
		lockAttemptsMu.Lock()
		lockAttempts++
		attempt := lockAttempts
		lockAttemptsMu.Unlock()
		if attempt == 2 {
			if hw.lifecycleMu.TryLock() {
				hw.lifecycleMu.Unlock()
				close(secondNotContending)
				return
			}
			close(secondContending)
		}
	}
	go func() { results <- attachUSBDeviceResult(uintptr(h)) }()
	<-entered
	go func() {
		results <- attachUSBDeviceResult(uintptr(h))
	}()
	select {
	case <-secondContending:
	case <-secondNotContending:
		t.Fatal("second AttachEx did not contend with the first lifecycle lock")
	}
	close(release)
	if got := <-results; got != deviceAttachSuccess {
		t.Fatalf("first concurrent attach result = %d, want success", got)
	}
	if got := <-results; got != deviceAttachSuccess {
		t.Fatalf("second concurrent attach result = %d, want success", got)
	}
	callsMu.Lock()
	gotCalls := calls
	callsMu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("backend attach calls = %d, want exactly 1", gotCalls)
	}
	identity, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identity.attachment.state != attachmentAttached || identity.attachment.attachment.Port != 301 {
		t.Fatalf("final attachment identity = %#v, want attached token port 301", identity)
	}
	if hw.state != serverActive {
		t.Fatalf("server state = %s, want active", hw.state)
	}
}

func serverHandleForTest(t *testing.T, hw *usbServerHandleWrapper) uintptr {
	t.Helper()
	h := cgo.NewHandle(hw)
	serverHandleRecords.Store(uintptr(h), hw)
	t.Cleanup(func() {
		serverHandleRecords.Delete(uintptr(h))
		h.Delete()
	})
	return uintptr(h)
}

func TestXbox360AutoAttachCompletesBeforeCreateReturns(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9601)
	attachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 302}, nil
	}
	var h deviceHandle
	ok := createXbox360Device(serverHandleForTest(t, hw), &h, 9601, true, 0, 0, 0)
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}
	if attachCalls != 1 {
		t.Fatalf("auto-attach calls = %d, want 1 before create returns", attachCalls)
	}
	identity, ok := lookupDeviceIdentity(uintptr(h))
	if !ok || identity.attachment.state != attachmentAttached || identity.attachment.attachment.Port != 302 {
		t.Fatalf("created attachment identity = %#v, want attached port 302", identity)
	}
	if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 1 {
		t.Fatalf("immediate explicit attach result=%d calls=%d, want success/1", got, attachCalls)
	}
}

func TestDetachedReadyTypedConsumerContracts(t *testing.T) {
	t.Run("Xbox360", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9602)
		attachCalls, detachCalls := 0, 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			attachCalls++
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 303}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			detachCalls++
			return nil
		}
		var h deviceHandle
		ok := createXbox360Device(serverHandleForTest(t, hw), &h, 9602, false, 0, 0, 0)
		if !ok || attachCalls != 0 {
			t.Fatalf("create ok=%t attach calls=%d, want true/0", ok, attachCalls)
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
			t.Fatalf("initial attachment state=%d ok=%t, want detached/true", got, ok)
		}
		var busID, deviceID uint32
		if !getUSBDeviceIdentity(uintptr(h), &busID, &deviceID) {
			t.Fatal("detached-safe Xbox360 operation failed")
		}
		if !setXbox360DeviceState(uintptr(h), xbox360.InputState{}) || !setXbox360RumbleCallback(uintptr(h), nil) {
			t.Fatal("detached-safe Xbox360 wrapper operation failed")
		}
		if attachCalls != 0 {
			t.Fatalf("detached-safe operations invoked attach %d times", attachCalls)
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 1 {
			t.Fatalf("first attach result=%d calls=%d, want success/1", got, attachCalls)
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 1 {
			t.Fatalf("idempotent attach result=%d calls=%d, want success/1", got, attachCalls)
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess || detachCalls != 1 {
			t.Fatalf("detach result=%d calls=%d, want success/1", got, detachCalls)
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess || detachCalls != 1 {
			t.Fatalf("idempotent detach result=%d calls=%d, want success/1", got, detachCalls)
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 2 {
			t.Fatalf("reattach result=%d calls=%d, want success/2", got, attachCalls)
		}
	})

	t.Run("Steam Deck", func(t *testing.T) {
		hw, _ := newLifecycleTestServer(t, 9603)
		attachCalls, detachCalls := 0, 0
		hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
			attachCalls++
			return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 304}, nil
		}
		hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
			detachCalls++
			return nil
		}
		var h deviceHandle
		ok := createSteamDeckDevice(serverHandleForTest(t, hw), &h, 9603, false, 0, 0)
		if !ok || attachCalls != 0 {
			t.Fatalf("create ok=%t attach calls=%d, want true/0", ok, attachCalls)
		}
		if got, ok := queryDeviceAttachmentState(uintptr(h)); !ok || got != deviceAttachmentQueryDetached {
			t.Fatalf("initial attachment state=%d ok=%t, want detached/true", got, ok)
		}
		var busID, deviceID uint32
		if !getUSBDeviceIdentity(uintptr(h), &busID, &deviceID) || !setSteamDeckDeviceState(uintptr(h), steamDeckState{A: true}) || !setSteamDeckOutputCallback(uintptr(h), nil) {
			t.Fatal("detached-safe Steam Deck operation failed")
		}
		if attachCalls != 0 {
			t.Fatalf("detached-safe operations invoked attach %d times", attachCalls)
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 1 {
			t.Fatalf("first attach result=%d calls=%d, want success/1", got, attachCalls)
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 1 {
			t.Fatalf("idempotent attach result=%d calls=%d, want success/1", got, attachCalls)
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess || detachCalls != 1 {
			t.Fatalf("detach result=%d calls=%d, want success/1", got, detachCalls)
		}
		if got := detachUSBDeviceResult(uintptr(h)); got != deviceDetachSuccess || detachCalls != 1 {
			t.Fatalf("idempotent detach result=%d calls=%d, want success/1", got, detachCalls)
		}
		if got := attachUSBDeviceResult(uintptr(h)); got != deviceAttachSuccess || attachCalls != 2 {
			t.Fatalf("reattach result=%d calls=%d, want success/2", got, attachCalls)
		}
	})
}
