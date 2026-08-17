package main

import (
	"context"
	"log/slog"
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
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9600, pad, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Xbox360 creation failed")
	}

	results := make(chan deviceAttachResult, 2)
	go func() { results <- attachUSBDeviceResult(uintptr(h)) }()
	<-entered
	go func() { results <- attachUSBDeviceResult(uintptr(h)) }()
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

func TestXbox360AutoAttachCompletesBeforeCreateReturns(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9601)
	attachCalls := 0
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		attachCalls++
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 302}, nil
	}
	pad, err := xbox360.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9601, pad, true)
	hw.lifecycleMu.Unlock()
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
