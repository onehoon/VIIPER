package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/keyboard"
	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type classifiedRemovalFamily struct {
	name      string
	newDevice func() (usb.Device, error)
	remove    func(uintptr) typedDeviceRemoveResult
	legacy    func(uintptr) bool
	abi       func() (uintptr, [4]int)
}

func classifiedRemovalFamilies() []classifiedRemovalFamily {
	return []classifiedRemovalFamily{
		{"DualSense", func() (usb.Device, error) { return dualsense.New(nil) }, removeDualSenseDeviceResult, removeDualSenseDevice, dualSenseDeviceRemoveResultABI},
		{"DualShock4", func() (usb.Device, error) { return dualshock4.New(nil) }, removeDS4DeviceResult, removeDS4Device, ds4DeviceRemoveResultABI},
		{"NS2Pro", func() (usb.Device, error) { return ns2pro.New(nil) }, removeNS2ProDeviceResult, removeNS2ProDevice, ns2ProDeviceRemoveResultABI},
		{"Keyboard", func() (usb.Device, error) { return keyboard.New(nil) }, removeKeyboardDeviceResult, removeKeyboardDevice, keyboardDeviceRemoveResultABI},
		{"Mouse", func() (usb.Device, error) { return mouse.New(nil) }, removeMouseDeviceResult, removeMouseDevice, mouseDeviceRemoveResultABI},
	}
}

func TestClassifiedRemovalParityABI(t *testing.T) {
	for _, family := range classifiedRemovalFamilies() {
		t.Run(family.name, func(t *testing.T) {
			size, values := family.abi()
			if size != 4 || values != [4]int{0, 1, 2, 3} {
				t.Fatalf("ABI size=%d values=%v, want 4/[0 1 2 3]", size, values)
			}
		})
	}
}

func TestClassifiedRemovalParitySuccessAndWrongFamily(t *testing.T) {
	for i, family := range classifiedRemovalFamilies() {
		family := family
		t.Run(family.name, func(t *testing.T) {
			busID := uint32(9700 + i)
			hw, _ := newLifecycleTestServer(t, busID)
			deviceInstance, err := family.newDevice()
			if err != nil {
				t.Fatal(err)
			}
			hw.lifecycleMu.Lock()
			h, ok := hw.createDeviceLocked(busID, deviceInstance, false)
			hw.lifecycleMu.Unlock()
			if !ok {
				t.Fatal("device creation failed")
			}
			if got := family.remove(uintptr(h)); got != typedDeviceRemoveSuccess {
				t.Fatalf("result=%d, want success", got)
			}
			if family.remove(uintptr(h)) != typedDeviceRemoveInvalid || hw.s.GetBus(busID) == nil {
				t.Fatal("repeated removal was not invalid or caller-owned bus was removed")
			}
			if family.legacy(0) {
				t.Fatal("legacy bool removal accepted an invalid handle")
			}

			var foreign usb.Device
			if family.name == "Mouse" {
				foreign, err = keyboard.New(nil)
			} else {
				foreign, err = mouse.New(nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			hw.lifecycleMu.Lock()
			foreignHandle, ok := hw.createDeviceLocked(busID, foreign, false)
			hw.lifecycleMu.Unlock()
			if !ok {
				t.Fatal("foreign device creation failed")
			}
			if family.remove(uintptr(foreignHandle)) != typedDeviceRemoveInvalid || !lookupIdentityExists(uintptr(foreignHandle)) {
				t.Fatal("wrong-family removal mutated the foreign handle")
			}
		})
	}
}

func TestClassifiedRemovalParityUnsafeAndLegacyFailureProjection(t *testing.T) {
	for i, family := range classifiedRemovalFamilies() {
		family := family
		t.Run(family.name, func(t *testing.T) {
			busID := uint32(9720 + i)
			hw, _ := newLifecycleTestServer(t, busID)
			detachCalls := 0
			hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
				return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: int32(400 + i)}, nil
			}
			hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
				detachCalls++
				return api.ErrDetachmentOutcomeUnknown
			}
			deviceInstance, err := family.newDevice()
			if err != nil {
				t.Fatal(err)
			}
			hw.lifecycleMu.Lock()
			h, ok := hw.createDeviceLocked(busID, deviceInstance, true)
			hw.lifecycleMu.Unlock()
			if !ok {
				t.Fatal("attached device creation failed")
			}
			if family.remove(uintptr(h)) != typedDeviceRemoveUnsafeOutcomeUnknown || family.remove(uintptr(h)) != typedDeviceRemoveUnsafeOutcomeUnknown {
				t.Fatal("unsafe removal was not sticky")
			}
			if family.legacy(uintptr(h)) || detachCalls != 1 || !lookupIdentityExists(uintptr(h)) || hw.state != serverCloseFailed {
				t.Fatalf("detach calls=%d handle=%t state=%s, want 1/true/close-failed", detachCalls, lookupIdentityExists(uintptr(h)), hw.state)
			}

			knownHW, _ := newLifecycleTestServer(t, busID+100)
			knownDetachCalls := 0
			knownHW.ops.attachLocalhostTracked = hw.ops.attachLocalhostTracked
			knownHW.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
				knownDetachCalls++
				if knownDetachCalls == 1 {
					return errors.New("known detach failure")
				}
				return nil
			}
			knownDevice, err := family.newDevice()
			if err != nil {
				t.Fatal(err)
			}
			knownHW.lifecycleMu.Lock()
			knownHandle, ok := knownHW.createDeviceLocked(busID+100, knownDevice, true)
			knownHW.lifecycleMu.Unlock()
			if !ok {
				t.Fatal("known-failure device creation failed")
			}
			if got := family.remove(uintptr(knownHandle)); got != typedDeviceRemoveRetryableFailure {
				t.Fatalf("classified result=%d, want retryable failure", got)
			}
			if !lookupIdentityExists(uintptr(knownHandle)) || knownHW.state != serverActive {
				t.Fatal("classified retryable failure did not retain active authoritative handle")
			}
			if got := family.remove(uintptr(knownHandle)); got != typedDeviceRemoveSuccess || knownDetachCalls != 2 {
				t.Fatalf("explicit retry result=%d detach calls=%d, want success/2", got, knownDetachCalls)
			}

			legacyHW, _ := newLifecycleTestServer(t, busID+200)
			legacyHW.ops.attachLocalhostTracked = hw.ops.attachLocalhostTracked
			legacyHW.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
				return errors.New("known detach failure")
			}
			legacyDevice, err := family.newDevice()
			if err != nil {
				t.Fatal(err)
			}
			legacyHW.lifecycleMu.Lock()
			legacyHandle, ok := legacyHW.createDeviceLocked(busID+200, legacyDevice, true)
			legacyHW.lifecycleMu.Unlock()
			if !ok || family.legacy(uintptr(legacyHandle)) || !lookupIdentityExists(uintptr(legacyHandle)) || legacyHW.state != serverActive {
				t.Fatal("legacy bool did not project known retryable failure safely")
			}
		})
	}
}

func TestDualSenseEdgeUsesClassifiedDualSenseRemoval(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9740)
	edge, err := dualsense.NewEdge(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9740, edge, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("DualSense Edge creation failed")
	}
	if got := removeDualSenseDeviceResult(uintptr(h)); got != typedDeviceRemoveSuccess {
		t.Fatalf("DualSense Edge removal result=%d, want success", got)
	}
}
