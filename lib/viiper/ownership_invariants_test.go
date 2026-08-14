package main

import (
	"context"
	"errors"
	"log/slog"
	"runtime/cgo"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/keyboard"
	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/device/xbox360"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/usb"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

func TestRequiredCreatePointersRejectBeforeLookup(t *testing.T) {
	var configMarker, outputMarker byte
	if hasRequiredUSBServerPointers(nil, unsafe.Pointer(&outputMarker)) {
		t.Fatal("nil config was accepted")
	}
	if hasRequiredUSBServerPointers(unsafe.Pointer(&configMarker), nil) {
		t.Fatal("nil output was accepted")
	}
	if NewUSBServer(nil, nil, nil) {
		t.Fatal("NewUSBServer accepted nil config and output")
	}

	if CreateXbox360Device(0, nil, 1, false, 0, 0, 0) {
		t.Fatal("CreateXbox360Device accepted nil output")
	}
	if CreateDualSenseDevice(0, nil, 1, false, 0, 0, nil) {
		t.Fatal("CreateDualSenseDevice accepted nil output")
	}
	if CreateDualSenseEdgeDevice(0, nil, 1, false, 0, 0, nil) {
		t.Fatal("CreateDualSenseEdgeDevice accepted nil output")
	}
	if CreateDS4Device(0, nil, 1, false, 0, 0, nil) {
		t.Fatal("CreateDS4Device accepted nil output")
	}
	if CreateKeyboardDevice(0, nil, 1, false, 0, 0) {
		t.Fatal("CreateKeyboardDevice accepted nil output")
	}
	if CreateMouseDevice(0, nil, 1, false, 0, 0) {
		t.Fatal("CreateMouseDevice accepted nil output")
	}
	if CreateNS2ProDevice(0, nil, 1, false, 0, 0, nil) {
		t.Fatal("CreateNS2ProDevice accepted nil output")
	}
}

func TestTypedRemoveRejectsEveryWrongConcreteType(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9201)
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		t.Fatal("wrong-type removal attempted detach")
		return nil
	}
	deleteCalls := 0
	hw.ops.deleteHandle = func(h cgo.Handle) {
		deleteCalls++
		h.Delete()
	}

	hw.lifecycleMu.Lock()
	mouseHandle := addOwnershipTestDevice(t, hw, 9201, func() (viiperusb.Device, error) { return mouse.New(nil) })
	gordonHandle := addOwnershipTestDevice(t, hw, 9201, func() (viiperusb.Device, error) { return steamcontroller.New(nil) })
	hw.lifecycleMu.Unlock()

	wrongCalls := []struct {
		name string
		call func(uintptr) bool
		h    deviceHandle
	}{
		{"steamcontroller with mouse", removeSteamControllerDevice, mouseHandle},
		{"xbox360 with gordon", removeXbox360Device, gordonHandle},
		{"dualsense with gordon", removeDualSenseDevice, gordonHandle},
		{"ds4 with gordon", removeDS4Device, gordonHandle},
		{"keyboard with gordon", removeKeyboardDevice, gordonHandle},
		{"mouse with gordon", removeMouseDevice, gordonHandle},
		{"ns2pro with gordon", removeNS2ProDevice, gordonHandle},
	}
	for _, tc := range wrongCalls {
		t.Run(tc.name, func(t *testing.T) {
			if tc.call(uintptr(tc.h)) {
				t.Fatal("wrong-type removal succeeded")
			}
			if hw.deviceHandleRecords[tc.h] == nil {
				t.Fatal("wrong-type removal deleted the canonical record")
			}
			if !lookupIdentityExists(uintptr(tc.h)) {
				t.Fatal("wrong-type removal invalidated the handle")
			}
		})
	}
	if deleteCalls != 0 {
		t.Fatalf("wrong-type removal delete calls = %d, want 0", deleteCalls)
	}
	if hw.state != serverActive || hw.s.GetBus(9201) == nil {
		t.Fatalf("wrong-type removal changed server state=%s or bus presence", hw.state)
	}
	hw.lifecycleMu.Lock()
	if !hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("cleanup close failed")
	}
	hw.lifecycleMu.Unlock()
}

func TestDualSenseEdgeUsesSharedRemoveIdentity(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9202)
	hw.lifecycleMu.Lock()
	h := addOwnershipTestDevice(t, hw, 9202, func() (viiperusb.Device, error) { return dualsense.NewEdge(nil) })
	hw.lifecycleMu.Unlock()
	if !removeDualSenseDevice(uintptr(h)) {
		t.Fatal("DualSense Edge was rejected by RemoveDualSenseDevice")
	}
	if lookupIdentityExists(uintptr(h)) {
		t.Fatal("DualSense Edge handle remained valid after removal")
	}
	if hw.s.GetBus(9202) == nil {
		t.Fatal("DualSense Edge removal removed caller-owned bus")
	}
}

func TestTypedRemoveAcceptsEveryConcreteType(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9208)
	tests := []struct {
		name        string
		constructor func() (viiperusb.Device, error)
		remove      func(uintptr) bool
	}{
		{"steamcontroller", func() (viiperusb.Device, error) { return steamcontroller.New(nil) }, removeSteamControllerDevice},
		{"xbox360", func() (viiperusb.Device, error) { return xbox360.New(nil) }, removeXbox360Device},
		{"dualsense", func() (viiperusb.Device, error) { return dualsense.New(nil) }, removeDualSenseDevice},
		{"ds4", func() (viiperusb.Device, error) { return dualshock4.New(nil) }, removeDS4Device},
		{"keyboard", func() (viiperusb.Device, error) { return keyboard.New(nil) }, removeKeyboardDevice},
		{"mouse", func() (viiperusb.Device, error) { return mouse.New(nil) }, removeMouseDevice},
		{"ns2pro", func() (viiperusb.Device, error) { return ns2pro.New(nil) }, removeNS2ProDevice},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hw.lifecycleMu.Lock()
			h := addOwnershipTestDevice(t, hw, 9208, tc.constructor)
			hw.lifecycleMu.Unlock()
			if !tc.remove(uintptr(h)) {
				t.Fatal("correctly typed removal failed")
			}
			if lookupIdentityExists(uintptr(h)) {
				t.Fatal("removed handle remained valid")
			}
		})
	}
	if hw.s.GetBus(9208) == nil {
		t.Fatal("typed removal removed caller-owned bus")
	}
}

func TestKnownAttachRollbackFailureRetainsRegisteredOwnership(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9203)
	hw.lifecycleMu.Lock()
	h := addOwnershipTestDevice(t, hw, 9203, func() (viiperusb.Device, error) { return mouse.New(nil) })
	registered := hw.deviceHandleRecords[h]
	if registered == nil {
		hw.lifecycleMu.Unlock()
		t.Fatal("test device was not registered")
	}
	dev, ok := registered.device.(viiperusb.Device)
	if !ok {
		hw.lifecycleMu.Unlock()
		t.Fatal("registered device does not implement the USB device contract")
	}
	if hw.rollbackCreatedDeviceLocked(9203, registered.exportMeta.DevID, failingRollbackBus{}.Remove, dev, "injected rollback failure") {
		hw.lifecycleMu.Unlock()
		t.Fatal("rollback unexpectedly succeeded")
	}
	hw.lifecycleMu.Unlock()
	if hw.state != serverCloseFailed || hw.deviceHandleRecords[h] != registered || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("rollback failure lost ownership: state=%s record=%v identity=%t", hw.state, hw.deviceHandleRecords[h] != nil, lookupIdentityExists(uintptr(h)))
	}
	hw.lifecycleMu.Lock()
	if !hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("cleanup close failed")
	}
	hw.lifecycleMu.Unlock()
}

func TestCreateKnownAttachRollbackFailureRetainsRegisteredOwnership(t *testing.T) {
	hw, bus := newLifecycleTestServer(t, 9209)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{}, errors.New("injected known attach failure")
	}
	hw.ops.rollbackDevice = func(*virtualbus.VirtualBus, viiperusb.Device) error {
		return errors.New("injected rollback failure")
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9209, mustNewTestMouse(t), true)
	hw.lifecycleMu.Unlock()
	if ok || h == 0 {
		t.Fatalf("create result = (%v, %v), want (nonzero handle, false)", h, ok)
	}
	if hw.state != serverCloseFailed || hw.deviceHandleRecords[h] == nil || !lookupIdentityExists(uintptr(h)) {
		t.Fatalf("create rollback lost ownership: state=%s record=%v identity=%t", hw.state, hw.deviceHandleRecords[h] != nil, lookupIdentityExists(uintptr(h)))
	}
	if len(bus.Devices()) != 1 {
		t.Fatalf("failed rollback removed logical device: devices=%d", len(bus.Devices()))
	}
	hw.lifecycleMu.Lock()
	if !hw.closeLocked() {
		hw.lifecycleMu.Unlock()
		t.Fatal("cleanup close failed")
	}
	hw.lifecycleMu.Unlock()
}

func TestRemoveUSBBusMissingBusHasNoSideEffects(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9204)
	removeCalls, detachCalls, deleteCalls := 0, 0, 0
	hw.ops.removeBus = func(*usb.Server, uint32) error {
		removeCalls++
		return errors.New("must not be called")
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}
	hw.ops.deleteHandle = func(h cgo.Handle) {
		deleteCalls++
		h.Delete()
	}
	hw.lifecycleMu.Lock()
	if hw.removeBusLocked(9999) {
		hw.lifecycleMu.Unlock()
		t.Fatal("missing bus removal succeeded")
	}
	hw.lifecycleMu.Unlock()
	if removeCalls != 0 || detachCalls != 0 || deleteCalls != 0 {
		t.Fatalf("missing bus side effects: remove=%d detach=%d delete=%d", removeCalls, detachCalls, deleteCalls)
	}
	if hw.state != serverActive || hw.s.GetBus(9204) == nil {
		t.Fatalf("missing bus changed server state=%s or unrelated bus", hw.state)
	}
}

func TestCreateUSBBusAddFailureReleasesAllocation(t *testing.T) {
	hw, existing := newLifecycleTestServer(t, 9207)
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	id := uint32(9207)
	if hw.createBusLocked(&id) {
		hw.lifecycleMu.Unlock()
		t.Fatal("bus creation succeeded despite an existing server registration")
	}
	hw.lifecycleMu.Unlock()
	if err := hw.s.RemoveBus(9207); err != nil {
		t.Fatal(err)
	}
	reused, err := virtualbus.NewWithBusID(9207)
	if err != nil {
		t.Fatalf("failed AddBus leaked bus allocation: %v", err)
	}
	if err := reused.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveUSBBusErrorAfterBusGoneFinalizes(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9205)
	hw.lifecycleMu.Lock()
	h := addOwnershipTestDevice(t, hw, 9205, func() (viiperusb.Device, error) { return mouse.New(nil) })
	deleteCalls := 0
	hw.ops.deleteHandle = func(ch cgo.Handle) {
		deleteCalls++
		ch.Delete()
	}
	hw.ops.removeBus = func(s *usb.Server, busID uint32) error {
		if err := s.RemoveBus(busID); err != nil {
			return err
		}
		return errors.New("removed then reported failure")
	}
	if !hw.removeBusLocked(9205) {
		hw.lifecycleMu.Unlock()
		t.Fatal("error-after-removal was not treated as completed")
	}
	hw.lifecycleMu.Unlock()
	if hw.s.GetBus(9205) != nil {
		t.Fatal("bus remained after injected removal")
	}
	if deleteCalls != 1 || lookupIdentityExists(uintptr(h)) {
		t.Fatalf("finalization deleteCalls=%d identity=%t", deleteCalls, lookupIdentityExists(uintptr(h)))
	}
}

func TestRemoveUSBBusErrorWithBusPresentPreservesDetachedOwnership(t *testing.T) {
	hw, bus := newLifecycleTestServer(t, 9206)
	hw.lifecycleMu.Lock()
	h := addOwnershipTestDevice(t, hw, 9206, func() (viiperusb.Device, error) { return mouse.New(nil) })
	dhw := hw.deviceHandleRecords[h]
	dhw.attachment = deviceAttachmentRecord{state: attachmentAttached, attachment: api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 94}}
	detachCalls := 0
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		detachCalls++
		return nil
	}
	hw.ops.removeBus = func(*usb.Server, uint32) error { return errors.New("bus remains") }
	if hw.removeBusLocked(9206) {
		hw.lifecycleMu.Unlock()
		t.Fatal("bus-present error was treated as success")
	}
	hw.lifecycleMu.Unlock()
	if bus == nil || hw.s.GetBus(9206) == nil || !lookupIdentityExists(uintptr(h)) {
		t.Fatal("bus or ownership was lost after failed removal")
	}
	if detachCalls != 1 || dhw.attachment.state != attachmentDetached {
		t.Fatalf("detachCalls=%d attachmentState=%d", detachCalls, dhw.attachment.state)
	}
	hw.lifecycleMu.Lock()
	hw.ops.removeBus = func(s *usb.Server, busID uint32) error { return s.RemoveBus(busID) }
	if !hw.removeBusLocked(9206) {
		hw.lifecycleMu.Unlock()
		t.Fatal("retry removal failed")
	}
	hw.lifecycleMu.Unlock()
	if detachCalls != 1 || lookupIdentityExists(uintptr(h)) {
		t.Fatalf("retry repeated detach or retained identity: detachCalls=%d identity=%t", detachCalls, lookupIdentityExists(uintptr(h)))
	}
}

func addOwnershipTestDevice(t *testing.T, hw *usbServerHandleWrapper, busID uint32, constructor func() (viiperusb.Device, error)) deviceHandle {
	t.Helper()
	dev, err := constructor()
	if err != nil {
		t.Fatal(err)
	}
	return addOwnershipTestDeviceOnBus(t, hw, busID, dev)
}

func addOwnershipTestDeviceOnBus(t *testing.T, hw *usbServerHandleWrapper, busID uint32, dev viiperusb.Device) deviceHandle {
	t.Helper()
	h, ok := hw.createDeviceLocked(busID, dev, false)
	if !ok {
		t.Fatal("test device creation failed")
	}
	return h
}

type failingRollbackBus struct{}

func (failingRollbackBus) Remove(viiperusb.Device) error {
	return errors.New("injected rollback failure")
}
