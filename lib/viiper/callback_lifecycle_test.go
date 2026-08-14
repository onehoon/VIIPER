package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/keyboard"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type callbackLifecycleCase struct {
	name     string
	new      func() (viiperusb.Device, error)
	install  func(viiperusb.Device, *atomic.Int64)
	dispatch func(viiperusb.Device)
}

func canonicalCallbackCases() []callbackLifecycleCase {
	return []callbackLifecycleCase{
		{
			name: "gordon",
			new:  func() (viiperusb.Device, error) { return steamcontroller.New(nil) },
			install: func(dev viiperusb.Device, calls *atomic.Int64) {
				dev.(*steamcontroller.SteamController).SetOutputCallback(func(steamcontroller.OutputState) { calls.Add(1) })
			},
			dispatch: func(dev viiperusb.Device) {
				dev.(*steamcontroller.SteamController).HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
			},
		},
		{
			name: "xbox360",
			new:  func() (viiperusb.Device, error) { return xbox360.New(nil) },
			install: func(dev viiperusb.Device, calls *atomic.Int64) {
				dev.(*xbox360.Xbox360).SetRumbleCallback(func(xbox360.XRumbleState) { calls.Add(1) })
			},
			dispatch: func(dev viiperusb.Device) {
				dev.(*xbox360.Xbox360).HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{0x00, 0x08, 0x00, 0x11, 0x22, 0x00, 0x00, 0x00})
			},
		},
		{
			name: "dualsense",
			new:  func() (viiperusb.Device, error) { return dualsense.New(nil) },
			install: func(dev viiperusb.Device, calls *atomic.Int64) {
				dev.(*dualsense.DualSense).SetOutputCallback(func(dualsense.OutputState) { calls.Add(1) })
			},
			dispatch: func(dev viiperusb.Device) {
				report := make([]byte, 48)
				report[0] = dualsense.ReportIDOutput
				report[2] = 0x04
				dev.(*dualsense.DualSense).HandleTransfer(context.Background(), dualsense.EndpointOut, usbip.DirOut, report)
			},
		},
		{
			name: "dualshock4",
			new:  func() (viiperusb.Device, error) { return dualshock4.New(nil) },
			install: func(dev viiperusb.Device, calls *atomic.Int64) {
				dev.(*dualshock4.DualShock4).SetOutputCallback(func(dualshock4.OutputState) { calls.Add(1) })
			},
			dispatch: func(dev viiperusb.Device) {
				report := make([]byte, 11)
				report[0] = dualshock4.ReportIDOutput
				dev.(*dualshock4.DualShock4).HandleTransfer(context.Background(), dualshock4.EndpointOut, usbip.DirOut, report)
			},
		},
		{
			name: "keyboard",
			new:  func() (viiperusb.Device, error) { return keyboard.New(nil) },
			install: func(dev viiperusb.Device, calls *atomic.Int64) {
				dev.(*keyboard.Keyboard).SetLEDCallback(func(keyboard.LEDState) { calls.Add(1) })
			},
			dispatch: func(dev viiperusb.Device) {
				dev.(*keyboard.Keyboard).HandleTransfer(context.Background(), 1, usbip.DirOut, []byte{keyboard.LEDNumLock})
			},
		},
		{
			name: "ns2pro",
			new:  func() (viiperusb.Device, error) { return ns2pro.New(nil) },
			install: func(dev viiperusb.Device, calls *atomic.Int64) {
				dev.(*ns2pro.NS2Pro).SetOutputCallback(func(ns2pro.OutputState) { calls.Add(1) })
			},
			dispatch: func(dev viiperusb.Device) {
				report := make([]byte, ns2pro.OutputRumbleSize+1)
				report[0] = ns2pro.ReportIDOutput
				dev.(*ns2pro.NS2Pro).HandleTransfer(context.Background(), 1, usbip.DirOut, report)
			},
		},
	}
}

func TestCanonicalCallbackClearCoversEveryDevice(t *testing.T) {
	for _, tc := range canonicalCallbackCases() {
		t.Run(tc.name, func(t *testing.T) {
			hw, _ := newLifecycleTestServer(t, 9211)
			dev, err := tc.new()
			if err != nil {
				t.Fatal(err)
			}
			hw.lifecycleMu.Lock()
			h := addOwnershipTestDeviceOnBus(t, hw, 9211, dev)
			dhw := hw.deviceHandleRecords[h]
			hw.lifecycleMu.Unlock()
			if dhw == nil {
				t.Fatal("device registration failed")
			}
			var calls atomic.Int64
			tc.install(dev, &calls)
			tc.dispatch(dev)
			if calls.Load() != 1 {
				t.Fatalf("callback calls before clear = %d, want 1", calls.Load())
			}
			hw.lifecycleMu.Lock()
			hw.clearDeviceCallbackLocked(dhw)
			hw.lifecycleMu.Unlock()
			tc.dispatch(dev)
			if calls.Load() != 1 {
				t.Fatalf("callback calls after clear = %d, want 1", calls.Load())
			}
		})
	}
}

func TestTypedRemovalClearsCallbackBeforeKnownDetachFailure(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9212)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 91}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		return errors.New("known detach failure")
	}
	dev, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9212, dev, true)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("device creation failed")
	}
	var calls atomic.Int64
	dev.SetOutputCallback(func(steamcontroller.OutputState) { calls.Add(1) })
	if removeSteamControllerDevice(uintptr(h)) {
		t.Fatal("removal succeeded despite known detach failure")
	}
	dev.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
	if calls.Load() != 0 || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("callback calls=%d retained=%t", calls.Load(), lookupIdentityExists(uintptr(h)))
	}
}

func TestRemoveUSBBusClearsCallbacksInRegistrationOrderBeforeDetach(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9213)
	var eventsMu sync.Mutex
	var events []uint32
	hw.onCallbackCleared = func(dhw *deviceHandleWrapper) {
		eventsMu.Lock()
		events = append(events, dhw.exportMeta.DevID)
		eventsMu.Unlock()
	}
	var firstDetachObserved int
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 92}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		eventsMu.Lock()
		if firstDetachObserved == 0 {
			firstDetachObserved = len(events)
		}
		eventsMu.Unlock()
		return nil
	}
	dev1, _ := xbox360.New(nil)
	dev2, _ := keyboard.New(nil)
	hw.lifecycleMu.Lock()
	h1, ok1 := hw.createDeviceLocked(9213, dev1, true)
	h2, ok2 := hw.createDeviceLocked(9213, dev2, true)
	devID1 := hw.deviceHandleRecords[h1].exportMeta.DevID
	devID2 := hw.deviceHandleRecords[h2].exportMeta.DevID
	hw.lifecycleMu.Unlock()
	if !ok1 || !ok2 {
		t.Fatal("device setup failed")
	}
	dev1.SetRumbleCallback(func(xbox360.XRumbleState) {})
	dev2.SetLEDCallback(func(keyboard.LEDState) {})
	hw.lifecycleMu.Lock()
	removed := hw.removeBusLocked(9213)
	hw.lifecycleMu.Unlock()
	if !removed {
		t.Fatal("bus removal failed")
	}
	eventsMu.Lock()
	gotEvents := append([]uint32(nil), events...)
	observed := firstDetachObserved
	eventsMu.Unlock()
	want := []uint32{devID1, devID2}
	if len(gotEvents) != 2 || gotEvents[0] != want[0] || gotEvents[1] != want[1] {
		t.Fatalf("clear order=%v, want=%v", gotEvents, want)
	}
	if observed != len(gotEvents) {
		t.Fatalf("first detach observed %d clears, want %d", observed, len(gotEvents))
	}
}

func TestRemoveUSBBusUnknownPreflightPreservesCallback(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9214)
	var clearCalls atomic.Int64
	hw.onCallbackCleared = func(*deviceHandleWrapper) { clearCalls.Add(1) }
	dev, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9214, dev, false)
	if !ok {
		hw.lifecycleMu.Unlock()
		t.Fatal("device creation failed")
	}
	hw.deviceHandleRecords[h].attachment.state = attachmentOutcomeUnknown
	hw.lifecycleMu.Unlock()
	var callbacks atomic.Int64
	dev.SetOutputCallback(func(steamcontroller.OutputState) { callbacks.Add(1) })
	hw.lifecycleMu.Lock()
	removed := hw.removeBusLocked(9214)
	hw.lifecycleMu.Unlock()
	if removed {
		t.Fatal("unknown attachment bus removal succeeded")
	}
	dev.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
	if clearCalls.Load() != 0 || callbacks.Load() != 1 {
		t.Fatalf("unknown preflight clearCalls=%d callbacks=%d", clearCalls.Load(), callbacks.Load())
	}
}

func TestWrongTypedRemoveDoesNotClearCallback(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9215)
	dev, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h := addOwnershipTestDeviceOnBus(t, hw, 9215, dev)
	hw.lifecycleMu.Unlock()
	var callbacks atomic.Int64
	dev.SetOutputCallback(func(steamcontroller.OutputState) { callbacks.Add(1) })
	if removeXbox360Device(uintptr(h)) {
		t.Fatal("wrong typed remove succeeded")
	}
	dev.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
	if callbacks.Load() != 1 {
		t.Fatalf("wrong typed remove changed callback count to %d", callbacks.Load())
	}
}

func TestCloseClearsAllBusesInDeterministicOrderBeforeDetach(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9216)
	bus2 := addTestBus(t, hw, 9217)
	_ = bus2
	var eventsMu sync.Mutex
	var events []struct{ busID, devID uint32 }
	hw.onCallbackCleared = func(dhw *deviceHandleWrapper) {
		eventsMu.Lock()
		events = append(events, struct{ busID, devID uint32 }{dhw.exportMeta.BusID, dhw.exportMeta.DevID})
		eventsMu.Unlock()
	}
	firstDetachObserved := -1
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendNativeIOCTL, Port: 93}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		eventsMu.Lock()
		if firstDetachObserved < 0 {
			firstDetachObserved = len(events)
		}
		eventsMu.Unlock()
		return nil
	}
	devA, _ := xbox360.New(nil)
	devB, _ := keyboard.New(nil)
	devC, _ := steamcontroller.New(nil)
	devD, _ := dualshock4.New(nil)
	hw.lifecycleMu.Lock()
	hA, okA := hw.createDeviceLocked(9217, devA, true)
	hB, okB := hw.createDeviceLocked(9217, devB, true)
	hC, okC := hw.createDeviceLocked(9216, devC, true)
	hD, okD := hw.createDeviceLocked(9216, devD, true)
	want := []struct{ busID, devID uint32 }{
		{9216, hw.deviceHandleRecords[hC].exportMeta.DevID},
		{9216, hw.deviceHandleRecords[hD].exportMeta.DevID},
		{9217, hw.deviceHandleRecords[hA].exportMeta.DevID},
		{9217, hw.deviceHandleRecords[hB].exportMeta.DevID},
	}
	hw.lifecycleMu.Unlock()
	if !okA || !okB || !okC || !okD {
		t.Fatal("device setup failed")
	}
	devA.SetRumbleCallback(func(xbox360.XRumbleState) {})
	devB.SetLEDCallback(func(keyboard.LEDState) {})
	devC.SetOutputCallback(func(steamcontroller.OutputState) {})
	devD.SetOutputCallback(func(dualshock4.OutputState) {})
	hw.lifecycleMu.Lock()
	closed := hw.closeLocked()
	hw.lifecycleMu.Unlock()
	if !closed {
		t.Fatal("close failed")
	}
	eventsMu.Lock()
	got := append([]struct{ busID, devID uint32 }(nil), events...)
	observed := firstDetachObserved
	eventsMu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("clear events=%v, want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("clear order=%v, want=%v", got, want)
		}
	}
	if observed != len(want) {
		t.Fatalf("first detach observed %d clears, want %d", observed, len(want))
	}
}

func TestCloseUnknownPreflightPreservesCallbacks(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9218)
	dev, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9218, dev, false)
	if !ok {
		hw.lifecycleMu.Unlock()
		t.Fatal("device creation failed")
	}
	hw.deviceHandleRecords[h].attachment.state = attachmentOutcomeUnknown
	hw.lifecycleMu.Unlock()
	var callbacks atomic.Int64
	dev.SetOutputCallback(func(steamcontroller.OutputState) { callbacks.Add(1) })
	hw.lifecycleMu.Lock()
	closed := hw.closeLocked()
	hw.lifecycleMu.Unlock()
	if closed || hw.state != serverCloseFailed {
		t.Fatal("unknown preflight unexpectedly closed server")
	}
	dev.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
	if callbacks.Load() != 1 {
		t.Fatalf("unknown preflight changed callback count to %d", callbacks.Load())
	}
}

func TestCloseKnownFailureLeavesCallbacksCleared(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9220)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 94}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		return errors.New("known detach failure")
	}
	dev, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9220, dev, true)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("device creation failed")
	}
	var callbacks atomic.Int64
	dev.SetOutputCallback(func(steamcontroller.OutputState) { callbacks.Add(1) })
	hw.lifecycleMu.Lock()
	closed := hw.closeLocked()
	hw.lifecycleMu.Unlock()
	if closed || hw.state != serverCloseFailed {
		t.Fatal("known detach failure unexpectedly closed server")
	}
	dev.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
	if callbacks.Load() != 0 || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("known close failure callbacks=%d retained=%t", callbacks.Load(), lookupIdentityExists(uintptr(h)))
	}
}
