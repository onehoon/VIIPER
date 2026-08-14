package main

/*
#include <stdint.h>
#include <stdbool.h>
#include <stdlib.h>

typedef uintptr_t USBServerHandle;
typedef uintptr_t SteamControllerDeviceHandle;

typedef enum {
    VIIPER_REMOVE_SUCCESS = 0,
    VIIPER_REMOVE_RETRYABLE_FAILURE = 1,
    VIIPER_REMOVE_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_REMOVE_INVALID = 3
} SteamControllerDeviceRemoveResult;

typedef struct {
	uint8_t A, X, B, Y;
	uint8_t L1, R1;
	uint8_t L2, R2;
	uint8_t Menu, Steam, Options;
	uint8_t DPadDown, DPadLeft, DPadRight, DPadUp;
	uint8_t L3;
	uint8_t LGrip, RGrip;
	uint8_t LPadTouch, RPadTouch, LPadPress, RPadPress, LPadAndStick;
	int16_t LPadX, LPadY;
	int16_t RPadX, RPadY;
	uint16_t LTrigger, RTrigger;
	int16_t LStickX, LStickY;
	int16_t AccelX, AccelY, AccelZ;
	int16_t GyroX, GyroY, GyroZ;
	int16_t GyroQuatW, GyroQuatX, GyroQuatY, GyroQuatZ;
	uint16_t BatteryMilliVolts;
} SteamControllerDeviceState;

typedef void (*SteamControllerOutputCallback)(SteamControllerDeviceHandle handle, const uint8_t* data, uint32_t length);

static void viiper_call_steamcontroller_output(SteamControllerOutputCallback fn, SteamControllerDeviceHandle handle, const uint8_t* data, uint32_t length) {
	fn(handle, data, length);
}
*/
import "C"

import (
	"unsafe"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/steamcontroller"
)

// CreateSteamControllerDevice creates a Classic Steam Controller (Gordon) on a caller-owned bus.
// A zero vendor or product ID uses Gordon's default 28DE:1102 identity.
//
//export CreateSteamControllerDevice
func CreateSteamControllerDevice(serverHandle C.USBServerHandle, outDeviceHandle *C.SteamControllerDeviceHandle, busID uint32, autoAttachLocalhost bool, idVendor uint16, idProduct uint16) bool {
	if outDeviceHandle == nil {
		return false
	}
	var handle deviceHandle
	if !createSteamControllerDevice(uintptr(serverHandle), &handle, busID, autoAttachLocalhost, idVendor, idProduct) {
		return false
	}
	*outDeviceHandle = C.SteamControllerDeviceHandle(handle)
	return true
}

func createSteamControllerDevice(serverHandle uintptr, outDeviceHandle *deviceHandle, busID uint32, autoAttachLocalhost bool, idVendor uint16, idProduct uint16) bool {
	if outDeviceHandle == nil {
		return false
	}
	hw, ok := lookupServerHandle(serverHandle)
	if !ok {
		return false
	}
	opts := &device.CreateOptions{}
	if idVendor != 0 {
		opts.IDVendor = &idVendor
	}
	if idProduct != 0 {
		opts.IDProduct = &idProduct
	}
	d, err := steamcontroller.New(opts)
	if err != nil {
		return false
	}
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	h, ok := hw.createDeviceLocked(busID, d, autoAttachLocalhost)
	if !ok {
		return false
	}
	*outDeviceHandle = h
	return true
}

// SetSteamControllerDeviceState updates Gordon's logical input state. Frame ownership remains internal to Gordon.
//
//export SetSteamControllerDeviceState
func SetSteamControllerDeviceState(handle C.SteamControllerDeviceHandle, state C.SteamControllerDeviceState) bool {
	return setSteamControllerDeviceState(uintptr(handle), steamControllerStateFromC(state))
}

type steamControllerState struct {
	A, X, B, Y, L1, R1, L2, R2, Menu, Steam, Options, DPadDown, DPadLeft, DPadRight, DPadUp, L3, LGrip, RGrip, LPadTouch, RPadTouch, LPadPress, RPadPress, LPadAndStick bool
	LPadX, LPadY, RPadX, RPadY                                                                                                                                          int16
	LTrigger, RTrigger                                                                                                                                                  uint16
	LStickX, LStickY, AccelX, AccelY, AccelZ, GyroX, GyroY, GyroZ, GyroQuatW, GyroQuatX, GyroQuatY, GyroQuatZ                                                           int16
	BatteryMilliVolts                                                                                                                                                   uint16
}

func steamControllerStateFromC(state C.SteamControllerDeviceState) steamControllerState {
	return steamControllerStateFromPointer(unsafe.Pointer(&state))
}

func steamControllerStateFromPointer(pointer unsafe.Pointer) steamControllerState {
	state := (*C.SteamControllerDeviceState)(pointer)
	return steamControllerState{A: state.A != 0, X: state.X != 0, B: state.B != 0, Y: state.Y != 0, L1: state.L1 != 0, R1: state.R1 != 0, L2: state.L2 != 0, R2: state.R2 != 0, Menu: state.Menu != 0, Steam: state.Steam != 0, Options: state.Options != 0, DPadDown: state.DPadDown != 0, DPadLeft: state.DPadLeft != 0, DPadRight: state.DPadRight != 0, DPadUp: state.DPadUp != 0, L3: state.L3 != 0, LGrip: state.LGrip != 0, RGrip: state.RGrip != 0, LPadTouch: state.LPadTouch != 0, RPadTouch: state.RPadTouch != 0, LPadPress: state.LPadPress != 0, RPadPress: state.RPadPress != 0, LPadAndStick: state.LPadAndStick != 0, LPadX: int16(state.LPadX), LPadY: int16(state.LPadY), RPadX: int16(state.RPadX), RPadY: int16(state.RPadY), LTrigger: uint16(state.LTrigger), RTrigger: uint16(state.RTrigger), LStickX: int16(state.LStickX), LStickY: int16(state.LStickY), AccelX: int16(state.AccelX), AccelY: int16(state.AccelY), AccelZ: int16(state.AccelZ), GyroX: int16(state.GyroX), GyroY: int16(state.GyroY), GyroZ: int16(state.GyroZ), GyroQuatW: int16(state.GyroQuatW), GyroQuatX: int16(state.GyroQuatX), GyroQuatY: int16(state.GyroQuatY), GyroQuatZ: int16(state.GyroQuatZ), BatteryMilliVolts: uint16(state.BatteryMilliVolts)}
}

func steamControllerDeviceStateSize() uintptr { return unsafe.Sizeof(C.SteamControllerDeviceState{}) }

type steamControllerDeviceStateCOffsets struct {
	L1, R1, L2, R2, Menu uintptr
	LPadX                uintptr
}

func steamControllerDeviceStateABIOffsets() steamControllerDeviceStateCOffsets {
	return steamControllerDeviceStateCOffsets{
		L1:    unsafe.Offsetof(C.SteamControllerDeviceState{}.L1),
		R1:    unsafe.Offsetof(C.SteamControllerDeviceState{}.R1),
		L2:    unsafe.Offsetof(C.SteamControllerDeviceState{}.L2),
		R2:    unsafe.Offsetof(C.SteamControllerDeviceState{}.R2),
		Menu:  unsafe.Offsetof(C.SteamControllerDeviceState{}.Menu),
		LPadX: unsafe.Offsetof(C.SteamControllerDeviceState{}.LPadX),
	}
}

func setSteamControllerDeviceState(handle uintptr, state steamControllerState) bool {
	return withActiveDeviceHandle(handle, func(dhw *deviceHandleWrapper) bool {
		d, ok := dhw.device.(*steamcontroller.SteamController)
		if !ok {
			return false
		}
		d.UpdateInputState(steamControllerInputState(state))
		return true
	})
}

func steamControllerInputState(state steamControllerState) *steamcontroller.InputState {
	return &steamcontroller.InputState{
		A: state.A, X: state.X, B: state.B, Y: state.Y, L1: state.L1, R1: state.R1, L2: state.L2, R2: state.R2, Menu: state.Menu, Steam: state.Steam, Options: state.Options,
		DPadDown: state.DPadDown, DPadLeft: state.DPadLeft, DPadRight: state.DPadRight, DPadUp: state.DPadUp, L3: state.L3,
		LGrip: state.LGrip, RGrip: state.RGrip, LPadTouch: state.LPadTouch, RPadTouch: state.RPadTouch, LPadPress: state.LPadPress, RPadPress: state.RPadPress, LPadAndStick: state.LPadAndStick,
		LPadX: state.LPadX, LPadY: state.LPadY, RPadX: state.RPadX, RPadY: state.RPadY, LTrigger: state.LTrigger, RTrigger: state.RTrigger,
		LStickX: state.LStickX, LStickY: state.LStickY, AccelX: state.AccelX, AccelY: state.AccelY, AccelZ: state.AccelZ,
		GyroX: state.GyroX, GyroY: state.GyroY, GyroZ: state.GyroZ, GyroQuatW: state.GyroQuatW, GyroQuatX: state.GyroQuatX, GyroQuatY: state.GyroQuatY, GyroQuatZ: state.GyroQuatZ, BatteryMilliVolts: state.BatteryMilliVolts,
	}
}

// SetSteamControllerOutputCallback registers a raw 64-byte host-output callback. The supplied buffer is C-owned,
// valid only for the synchronous callback, and must be copied by callers that need to retain it.
//
//export SetSteamControllerOutputCallback
func SetSteamControllerOutputCallback(handle C.SteamControllerDeviceHandle, cb C.SteamControllerOutputCallback) bool {
	if cb == nil {
		return setSteamControllerOutputCallback(uintptr(handle), nil)
	}
	return setSteamControllerOutputCallback(uintptr(handle), func(out steamcontroller.OutputState) {
		invokeSteamControllerOutputCallback(cb, handle, out)
	})
}

func setSteamControllerOutputCallback(handle uintptr, callback func(steamcontroller.OutputState)) bool {
	return withActiveDeviceHandle(handle, func(dhw *deviceHandleWrapper) bool {
		d, ok := dhw.device.(*steamcontroller.SteamController)
		if !ok {
			return false
		}
		d.SetOutputCallback(callback)
		return true
	})
}

func invokeSteamControllerOutputCallback(cb C.SteamControllerOutputCallback, handle C.SteamControllerDeviceHandle, out steamcontroller.OutputState) {
	payload := C.CBytes(copySteamControllerOutput(out))
	defer C.free(payload)
	C.viiper_call_steamcontroller_output(cb, handle, (*C.uint8_t)(payload), C.uint32_t(len(out.Data)))
}

func copySteamControllerOutput(out steamcontroller.OutputState) []byte {
	return append([]byte(nil), out.Data[:]...)
}

// RemoveSteamControllerDevice removes and finalizes only the Gordon logical device. The bus remains caller-owned.
//
//export RemoveSteamControllerDevice
func RemoveSteamControllerDevice(handle C.SteamControllerDeviceHandle) bool {
	return removeSteamControllerDevice(uintptr(handle))
}

func removeSteamControllerDevice(handle uintptr) bool {
	return removeSteamControllerDeviceResult(handle) == typedDeviceRemoveSuccess
}

// RemoveSteamControllerDeviceEx returns the classified Gordon logical-device removal result.
// The legacy RemoveSteamControllerDevice bool export remains available for compatibility.
//
//export RemoveSteamControllerDeviceEx
func RemoveSteamControllerDeviceEx(handle C.SteamControllerDeviceHandle) C.SteamControllerDeviceRemoveResult {
	return C.SteamControllerDeviceRemoveResult(removeSteamControllerDeviceResult(uintptr(handle)))
}

func removeSteamControllerDeviceResult(handle uintptr) typedDeviceRemoveResult {
	return removeTypedDeviceResult(handle, func(device any) bool { _, ok := device.(*steamcontroller.SteamController); return ok })
}
