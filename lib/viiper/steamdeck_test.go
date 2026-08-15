package main

import (
	"context"
	"log/slog"
	"reflect"
	"runtime/cgo"
	"testing"
	"unsafe"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/mouse"
	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/usbip"
)

func TestSteamDeckStateConversion(t *testing.T) {
	input := steamDeckState{
		A: true, X: true, B: true, Y: true,
		L1: true, R1: true,
		L2Digital: true, R2Digital: true,
		L5: true, Menu: true,
		Steam: true, Options: true,
		DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true,
		L3:        true,
		RPadTouch: true, LPadTouch: true, RPadPress: true, LPadPress: true,
		R5:          true,
		R3:          true,
		RStickTouch: true, LStickTouch: true,
		R4: true, L4: true,
		QuickAccess: true,
		LPadX:       1111, LPadY: -2222,
		RPadX: 3333, RPadY: -4444,
		AccelX: -5555, AccelY: 6666, AccelZ: -7777,
		Pitch: 111, Yaw: -222, Roll: 333,
		GyroQuatW: 4040, GyroQuatX: -5050, GyroQuatY: 6060, GyroQuatZ: -7070,
		LTrigger: 12345, RTrigger: 23456,
		LStickX: -8888, LStickY: 9999,
		RStickX: -1010, RStickY: 2020,
		LPadForce: 555, RPadForce: 666,
		LStickForce: 777, RStickForce: 888,
	}
	got := steamDeckInputState(input)
	want := &steamdeck.InputState{
		A: true, X: true, B: true, Y: true,
		L1: true, R1: true,
		L2Digital: true, R2Digital: true,
		L5: true, Menu: true,
		Steam: true, Options: true,
		DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true,
		L3:        true,
		RPadTouch: true, LPadTouch: true, RPadPress: true, LPadPress: true,
		R5:          true,
		R3:          true,
		RStickTouch: true, LStickTouch: true,
		R4: true, L4: true,
		QuickAccess: true,
		LPadX:       1111, LPadY: -2222,
		RPadX: 3333, RPadY: -4444,
		AccelX: -5555, AccelY: 6666, AccelZ: -7777,
		Pitch: 111, Yaw: -222, Roll: 333,
		GyroQuatW: 4040, GyroQuatX: -5050, GyroQuatY: 6060, GyroQuatZ: -7070,
		LTrigger: 12345, RTrigger: 23456,
		LStickX: -8888, LStickY: 9999,
		RStickX: -1010, RStickY: 2020,
		LPadForce: 555, RPadForce: 666,
		LStickForce: 777, RStickForce: 888,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state conversion lost fields: %#v", got)
	}
}

type steamDeckDeviceStateABI struct {
	A, X, B, Y                                 uint8
	L1, R1                                     uint8
	L2Digital, R2Digital                       uint8
	L5, Menu                                   uint8
	Steam, Options                             uint8
	DPadDown, DPadLeft, DPadRight, DPadUp      uint8
	L3                                         uint8
	RPadTouch, LPadTouch, RPadPress, LPadPress uint8
	R5                                         uint8
	R3                                         uint8
	RStickTouch, LStickTouch                   uint8
	R4, L4                                     uint8
	QuickAccess                                uint8
	LPadX, LPadY                               int16
	RPadX, RPadY                               int16
	AccelX, AccelY, AccelZ                     int16
	Pitch, Yaw, Roll                           int16
	GyroQuatW, GyroQuatX, GyroQuatY, GyroQuatZ int16
	LTrigger, RTrigger                         uint16
	LStickX, LStickY                           int16
	RStickX, RStickY                           int16
	LPadForce, RPadForce                       uint16
	LStickForce, RStickForce                   uint16
}

func TestSteamDeckABIStateBoundaryConversion(t *testing.T) {
	raw := steamDeckDeviceStateABI{
		A: 2, X: 3, B: 4, Y: 5,
		L1: 6, R1: 7,
		L2Digital: 8, R2Digital: 9,
		L5: 10, Menu: 11,
		Steam: 12, Options: 13,
		DPadDown: 14, DPadLeft: 15, DPadRight: 16, DPadUp: 17,
		L3:        18,
		RPadTouch: 19, LPadTouch: 20, RPadPress: 21, LPadPress: 22,
		R5:          23,
		R3:          24,
		RStickTouch: 25, LStickTouch: 26,
		R4: 27, L4: 28,
		QuickAccess: 29,
		LPadX:       1111, LPadY: -2222,
		RPadX: 3333, RPadY: -4444,
		AccelX: -5555, AccelY: 6666, AccelZ: -7777,
		Pitch: 111, Yaw: -222, Roll: 333,
		GyroQuatW: 4040, GyroQuatX: -5050, GyroQuatY: 6060, GyroQuatZ: -7070,
		LTrigger: 12345, RTrigger: 23456,
		LStickX: -8888, LStickY: 9999,
		RStickX: -1010, RStickY: 2020,
		LPadForce: 555, RPadForce: 666,
		LStickForce: 777, RStickForce: 888,
	}
	if unsafe.Sizeof(raw) != steamDeckDeviceStateSize() {
		t.Fatalf("Go ABI size = %d, C ABI size = %d", unsafe.Sizeof(raw), steamDeckDeviceStateSize())
	}
	got := steamDeckStateFromPointer(unsafe.Pointer(&raw))
	want := steamDeckState{
		A: true, X: true, B: true, Y: true,
		L1: true, R1: true,
		L2Digital: true, R2Digital: true,
		L5: true, Menu: true,
		Steam: true, Options: true,
		DPadDown: true, DPadLeft: true, DPadRight: true, DPadUp: true,
		L3:        true,
		RPadTouch: true, LPadTouch: true, RPadPress: true, LPadPress: true,
		R5:          true,
		R3:          true,
		RStickTouch: true, LStickTouch: true,
		R4: true, L4: true,
		QuickAccess: true,
		LPadX:       1111, LPadY: -2222,
		RPadX: 3333, RPadY: -4444,
		AccelX: -5555, AccelY: 6666, AccelZ: -7777,
		Pitch: 111, Yaw: -222, Roll: 333,
		GyroQuatW: 4040, GyroQuatX: -5050, GyroQuatY: 6060, GyroQuatZ: -7070,
		LTrigger: 12345, RTrigger: 23456,
		LStickX: -8888, LStickY: 9999,
		RStickX: -1010, RStickY: 2020,
		LPadForce: 555, RPadForce: 666,
		LStickForce: 777, RStickForce: 888,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("C ABI boundary conversion lost fields: %#v", got)
	}
}

func TestSteamDeckABIFieldOffsets(t *testing.T) {
	cOffsets := steamDeckDeviceStateABIOffsets()
	type fieldOffset struct {
		name            string
		c, mirror, want uintptr
	}
	fields := []fieldOffset{
		{"L2Digital", cOffsets.L2Digital, unsafe.Offsetof(steamDeckDeviceStateABI{}.L2Digital), 6},
		{"R2Digital", cOffsets.R2Digital, unsafe.Offsetof(steamDeckDeviceStateABI{}.R2Digital), 7},
		{"R3", cOffsets.R3, unsafe.Offsetof(steamDeckDeviceStateABI{}.R3), 22},
		{"QuickAccess", cOffsets.QuickAccess, unsafe.Offsetof(steamDeckDeviceStateABI{}.QuickAccess), 27},
		{"LPadX", cOffsets.LPadX, unsafe.Offsetof(steamDeckDeviceStateABI{}.LPadX), 28},
		{"AccelX", cOffsets.AccelX, unsafe.Offsetof(steamDeckDeviceStateABI{}.AccelX), 36},
		{"LTrigger", cOffsets.LTrigger, unsafe.Offsetof(steamDeckDeviceStateABI{}.LTrigger), 56},
		{"LStickX", cOffsets.LStickX, unsafe.Offsetof(steamDeckDeviceStateABI{}.LStickX), 60},
		{"RStickX", cOffsets.RStickX, unsafe.Offsetof(steamDeckDeviceStateABI{}.RStickX), 64},
	}
	for _, field := range fields {
		if field.c != field.mirror || field.c != field.want {
			t.Fatalf("%s offsets: C=%d, Go mirror=%d, want=%d", field.name, field.c, field.mirror, field.want)
		}
	}
	if got, want := steamDeckDeviceStateSize(), uintptr(76); got != want {
		t.Fatalf("SteamDeckDeviceState C ABI size = %d, want %d", got, want)
	}
	if got, want := steamDeckDeviceRemoveResultSize(), uintptr(4); got != want {
		t.Fatalf("SteamDeckDeviceRemoveResult C ABI size = %d, want %d", got, want)
	}
}

func TestSteamDeckDefaultAndOverrideIdentity(t *testing.T) {
	defaultDevice, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor := defaultDevice.GetDescriptor().Device; descriptor.IDVendor != steamdeck.DefaultVID || descriptor.IDProduct != steamdeck.DefaultPID {
		t.Fatalf("default identity = %04x:%04x", descriptor.IDVendor, descriptor.IDProduct)
	}
	if steamdeck.DefaultVID != 0x28de || steamdeck.DefaultPID != 0x1205 {
		t.Fatalf("unexpected default identity constants: %04x:%04x", steamdeck.DefaultVID, steamdeck.DefaultPID)
	}
	vid, pid := uint16(0x1234), uint16(0x5678)
	overridden, err := steamdeck.New(&device.CreateOptions{IDVendor: &vid, IDProduct: &pid})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor := overridden.GetDescriptor().Device; descriptor.IDVendor != vid || descriptor.IDProduct != pid {
		t.Fatalf("override identity = %04x:%04x", descriptor.IDVendor, descriptor.IDProduct)
	}
}

func TestSteamDeckCreateWrapperContracts(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9232)
	if createSteamDeckDevice(0, new(deviceHandle), 9232, false, 0, 0) {
		t.Fatal("invalid server handle accepted")
	}
	if createSteamDeckDevice(uintptr(0), nil, 9232, false, 0, 0) {
		t.Fatal("nil output handle accepted")
	}
	serverHandle := cgo.NewHandle(hw)
	serverHandleRecords.Store(uintptr(serverHandle), hw)
	t.Cleanup(func() { serverHandleRecords.Delete(uintptr(serverHandle)); serverHandle.Delete() })
	if createSteamDeckDevice(uintptr(serverHandle), new(deviceHandle), 9999, false, 0, 0) {
		t.Fatal("missing bus accepted")
	}
	var defaultHandle deviceHandle
	if !createSteamDeckDevice(uintptr(serverHandle), &defaultHandle, 9232, false, 0, 0) {
		t.Fatal("default creation failed")
	}
	defaultDevice := hw.deviceHandleRecords[defaultHandle].device.(*steamdeck.SteamDeck)
	if descriptor := defaultDevice.GetDescriptor().Device; descriptor.IDVendor != steamdeck.DefaultVID || descriptor.IDProduct != steamdeck.DefaultPID {
		t.Fatalf("wrapper default identity = %04x:%04x", descriptor.IDVendor, descriptor.IDProduct)
	}
	var overriddenHandle deviceHandle
	if !createSteamDeckDevice(uintptr(serverHandle), &overriddenHandle, 9232, false, 0x1234, 0x5678) {
		t.Fatal("override creation failed")
	}
	overriddenDevice := hw.deviceHandleRecords[overriddenHandle].device.(*steamdeck.SteamDeck)
	if descriptor := overriddenDevice.GetDescriptor().Device; descriptor.IDVendor != 0x1234 || descriptor.IDProduct != 0x5678 {
		t.Fatalf("wrapper override identity = %04x:%04x", descriptor.IDVendor, descriptor.IDProduct)
	}
}

func TestSteamDeckTypedHandleSafetyAndRemoval(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9230)
	d, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9230, d, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}
	if !setSteamDeckDeviceState(uintptr(h), steamDeckState{A: true}) {
		t.Fatal("valid handle rejected")
	}
	wrongDevice, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	wrongHandle, ok := hw.createDeviceLocked(9230, wrongDevice, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("wrong-type setup failed")
	}
	if setSteamDeckDeviceState(uintptr(wrongHandle), steamDeckState{}) {
		t.Fatal("wrong device type accepted")
	}
	if setSteamDeckDeviceState(0, steamDeckState{}) {
		t.Fatal("invalid handle accepted")
	}
	if !removeSteamDeckDevice(uintptr(h)) {
		t.Fatal("removal failed")
	}
	if removeSteamDeckDevice(uintptr(h)) || setSteamDeckDeviceState(uintptr(h), steamDeckState{}) {
		t.Fatal("finalized handle accepted (stale handle / repeated removal)")
	}
	if hw.s.GetBus(9230) == nil {
		t.Fatal("typed removal removed caller-owned bus")
	}
}

func TestSteamDeckClassifiedRemovalUsesTypeGuard(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9234)
	deck, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	deckHandle, ok := hw.createDeviceLocked(9234, deck, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("Steam Deck creation failed")
	}

	mouseDevice, err := mouse.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	mouseHandle, ok := hw.createDeviceLocked(9234, mouseDevice, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("mouse creation failed")
	}

	if got := removeSteamDeckDeviceResult(uintptr(deckHandle)); got != typedDeviceRemoveSuccess {
		t.Fatalf("Steam Deck removal result = %d, want success", got)
	}
	if got := removeSteamDeckDeviceResult(uintptr(mouseHandle)); got != typedDeviceRemoveInvalid {
		t.Fatalf("mouse removal result = %d, want invalid", got)
	}
}

func TestSteamDeckSharedIdentityAndAttachDetach(t *testing.T) {
	hw, _ := newLifecycleTestServer(t, 9236)
	hw.ops.attachLocalhostTracked = func(context.Context, *usbip.ExportMeta, uint16, bool, *slog.Logger) (api.LocalhostAttachment, error) {
		return api.LocalhostAttachment{Backend: api.LocalhostAttachmentBackendCommand, Port: 68}, nil
	}
	hw.ops.detachLocalhost = func(context.Context, api.LocalhostAttachment, *slog.Logger) error {
		return nil
	}
	d, err := steamdeck.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hw.lifecycleMu.Lock()
	h, ok := hw.createDeviceLocked(9236, d, false)
	hw.lifecycleMu.Unlock()
	if !ok {
		t.Fatal("creation failed")
	}

	var busID, devID uint32
	if !getUSBDeviceIdentity(uintptr(h), &busID, &devID) {
		t.Fatal("GetUSBDeviceIdentity rejected typed Steam Deck handle")
	}
	if busID != 9236 {
		t.Fatalf("busID = %d, want 9236", busID)
	}

	if !attachUSBDevice(uintptr(h)) {
		t.Fatal("shared AttachUSBDevice rejected typed Steam Deck handle")
	}
	if !detachUSBDevice(uintptr(h)) {
		t.Fatal("shared DetachUSBDevice rejected typed Steam Deck handle")
	}
}
