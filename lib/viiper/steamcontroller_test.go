package main

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/usbip"
)

func TestSteamControllerStateConversion(t *testing.T) {
	input := steamControllerState{A: true, X: true, B: true, Y: true, L1: true, R1: true, Menu: true, Steam: true, Options: true, DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true, L3: true, LGrip: true, RGrip: true, LPadTouch: true, RPadTouch: true, LPadPress: true, RPadPress: true, LPadAndStick: true, LPadX: 1111, LPadY: -2222, RPadX: 3333, RPadY: -4444, LTrigger: 12345, RTrigger: 23456, LStickX: -5555, LStickY: 6666, AccelX: -7777, AccelY: 8888, AccelZ: -9999, GyroX: 1010, GyroY: -2020, GyroZ: 3030, GyroQuatW: 4040, GyroQuatX: -5050, GyroQuatY: 6060, GyroQuatZ: -7070, BatteryMilliVolts: 4321}
	got := steamControllerInputState(input)
	want := steamcontroller.InputState{A: true, X: true, B: true, Y: true, L1: true, R1: true, Menu: true, Steam: true, Options: true, DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true, L3: true, LGrip: true, RGrip: true, LPadTouch: true, RPadTouch: true, LPadPress: true, RPadPress: true, LPadAndStick: true, LPadX: 1111, LPadY: -2222, RPadX: 3333, RPadY: -4444, LTrigger: 12345, RTrigger: 23456, LStickX: -5555, LStickY: 6666, AccelX: -7777, AccelY: 8888, AccelZ: -9999, GyroX: 1010, GyroY: -2020, GyroZ: 3030, GyroQuatW: 4040, GyroQuatX: -5050, GyroQuatY: 6060, GyroQuatZ: -7070, BatteryMilliVolts: 4321}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("state conversion lost fields: %#v", got)
	}
	for _, grips := range []struct{ left, right bool }{{}, {true, false}, {false, true}, {true, true}} {
		converted := steamControllerInputState(steamControllerState{LGrip: grips.left, RGrip: grips.right})
		if converted.LGrip != grips.left || converted.RGrip != grips.right {
			t.Fatalf("grips = (%t, %t), want (%t, %t)", converted.LGrip, converted.RGrip, grips.left, grips.right)
		}
	}
}

func TestSteamControllerDefaultAndOverrideIdentity(t *testing.T) {
	defaultDevice, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor := defaultDevice.GetDescriptor().Device; descriptor.IDVendor != steamcontroller.DefaultVID || descriptor.IDProduct != steamcontroller.DefaultPID {
		t.Fatalf("default identity = %04x:%04x", descriptor.IDVendor, descriptor.IDProduct)
	}
	vid, pid := uint16(0x1234), uint16(0x5678)
	overridden, err := steamcontroller.New(&device.CreateOptions{IDVendor: &vid, IDProduct: &pid})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor := overridden.GetDescriptor().Device; descriptor.IDVendor != vid || descriptor.IDProduct != pid {
		t.Fatalf("override identity = %04x:%04x", descriptor.IDVendor, descriptor.IDProduct)
	}
}

func TestSteamControllerFrameIsOwnedByGordon(t *testing.T) {
	d, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	d.UpdateInputState(&steamcontroller.InputState{A: true})
	first := d.HandleTransfer(context.Background(), 3, usbip.DirIn, nil)
	d.UpdateInputState(&steamcontroller.InputState{B: true})
	second := d.HandleTransfer(context.Background(), 3, usbip.DirIn, nil)
	if got, want := binary.LittleEndian.Uint32(second[4:8]), binary.LittleEndian.Uint32(first[4:8])+1; got != want {
		t.Fatalf("frame = %d, want %d", got, want)
	}
}

func TestSteamControllerOutputCopyPreservesRawBytes(t *testing.T) {
	var out steamcontroller.OutputState
	for i := range out.Data {
		out.Data[i] = byte(i)
	}
	copy := copySteamControllerOutput(out)
	if len(copy) != steamcontroller.InputReportLen {
		t.Fatalf("copy length = %d", len(copy))
	}
	for i, value := range copy {
		if value != byte(i) {
			t.Fatalf("copy[%d] = %d", i, value)
		}
	}
	copy[0] = 0xff
	if out.Data[0] != 0 {
		t.Fatal("copy aliases Go-owned output data")
	}
}

func TestSteamControllerTypedHandleSafetyAndRemoval(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9130)
	d, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9130, d, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}
	if !setSteamControllerDeviceState(uintptr(h), steamControllerState{A: true}) {
		t.Fatal("valid handle rejected")
	}
	wrongDevice, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	wrongHandle, ok := hw.createDeviceLocked(9130, wrongDevice, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("wrong-type setup failed")
	}
	if setSteamControllerDeviceState(uintptr(wrongHandle), steamControllerState{}) {
		t.Fatal("wrong device type accepted")
	}
	if setSteamControllerDeviceState(0, steamControllerState{}) {
		t.Fatal("invalid handle accepted")
	}
	if !removeSteamControllerDevice(uintptr(h)) {
		t.Fatal("removal failed")
	}
	if removeSteamControllerDevice(uintptr(h)) || setSteamControllerDeviceState(uintptr(h), steamControllerState{}) {
		t.Fatal("finalized handle accepted")
	}
	if hw.s.GetBus(9130) == nil {
		t.Fatal("typed removal removed caller-owned bus")
	}
}

func TestSteamControllerWrapperClearsOutputCallback(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9131)
	d, err := steamcontroller.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9131, d, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}
	calls := 0
	if !setSteamControllerOutputCallback(uintptr(h), func(steamcontroller.OutputState) { calls++ }) {
		t.Fatal("callback registration failed")
	}
	d.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x99})
	if !setSteamControllerOutputCallback(uintptr(h), nil) {
		t.Fatal("callback clear failed")
	}
	d.HandleTransfer(context.Background(), 3, usbip.DirOut, []byte{0x98})
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
}
