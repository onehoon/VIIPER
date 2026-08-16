package main

/*
#include <stdint.h>
#include <stdbool.h>
#include <stdlib.h>

typedef uintptr_t USBServerHandle;
typedef uintptr_t SteamDeckDeviceHandle;
typedef void (*SteamDeckOutputCallback)(SteamDeckDeviceHandle handle, const uint8_t* data, uint32_t length);

static void viiper_call_steamdeck_output(SteamDeckOutputCallback fn, SteamDeckDeviceHandle handle, const uint8_t* data, uint32_t length) {
	fn(handle, data, length);
}

typedef enum {
    VIIPER_STEAMDECK_REMOVE_SUCCESS = 0,
    VIIPER_STEAMDECK_REMOVE_RETRYABLE_FAILURE = 1,
    VIIPER_STEAMDECK_REMOVE_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_STEAMDECK_REMOVE_INVALID = 3
} SteamDeckDeviceRemoveResult;

typedef struct {
	uint8_t A, X, B, Y;
	uint8_t L1, R1;
	uint8_t L2Digital, R2Digital;
	uint8_t L5, Menu;
	uint8_t Steam, Options;
	uint8_t DPadDown, DPadLeft, DPadRight, DPadUp;
	uint8_t L3;
	uint8_t RPadTouch, LPadTouch, RPadPress, LPadPress;
	uint8_t R5;
	uint8_t R3;
	uint8_t RStickTouch, LStickTouch;
	uint8_t R4, L4;
	uint8_t QuickAccess;
	int16_t LPadX, LPadY;
	int16_t RPadX, RPadY;
	int16_t AccelX, AccelY, AccelZ;
	int16_t Pitch, Yaw, Roll;
	int16_t GyroQuatW, GyroQuatX, GyroQuatY, GyroQuatZ;
	uint16_t LTrigger, RTrigger;
	int16_t LStickX, LStickY;
	int16_t RStickX, RStickY;
	uint16_t LPadForce, RPadForce;
} SteamDeckDeviceState;
*/
import "C"

import (
	"unsafe"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/steamdeck"
)

// CreateSteamDeckDevice creates a Steam Deck logical device on a caller-owned bus.
// A zero vendor or product ID uses the Steam Deck's default 28DE:1205 identity.
//
//export CreateSteamDeckDevice
func CreateSteamDeckDevice(serverHandle C.USBServerHandle, outDeviceHandle *C.SteamDeckDeviceHandle, busID uint32, autoAttachLocalhost bool, idVendor uint16, idProduct uint16) bool {
	if outDeviceHandle == nil {
		return false
	}
	var handle deviceHandle
	if !createSteamDeckDevice(uintptr(serverHandle), &handle, busID, autoAttachLocalhost, idVendor, idProduct) {
		return false
	}
	*outDeviceHandle = C.SteamDeckDeviceHandle(handle)
	return true
}

func createSteamDeckDevice(serverHandle uintptr, outDeviceHandle *deviceHandle, busID uint32, autoAttachLocalhost bool, idVendor uint16, idProduct uint16) bool {
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
	d, err := steamdeck.New(opts)
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

// SetSteamDeckDeviceState updates the Steam Deck's logical input state. Frame ownership remains internal
// to device/steamdeck.
//
//export SetSteamDeckDeviceState
func SetSteamDeckDeviceState(handle C.SteamDeckDeviceHandle, state C.SteamDeckDeviceState) bool {
	return setSteamDeckDeviceState(uintptr(handle), steamDeckStateFromC(state))
}

type steamDeckState struct {
	A, X, B, Y                                 bool
	L1, R1                                     bool
	L2Digital, R2Digital                       bool
	L5, Menu                                   bool
	Steam, Options                             bool
	DPadDown, DPadLeft, DPadRight, DPadUp      bool
	L3                                         bool
	RPadTouch, LPadTouch, RPadPress, LPadPress bool
	R5                                         bool
	R3                                         bool
	RStickTouch, LStickTouch                   bool
	R4, L4                                     bool
	QuickAccess                                bool
	LPadX, LPadY                               int16
	RPadX, RPadY                               int16
	AccelX, AccelY, AccelZ                     int16
	Pitch, Yaw, Roll                           int16
	GyroQuatW, GyroQuatX, GyroQuatY, GyroQuatZ int16
	LTrigger, RTrigger                         uint16
	LStickX, LStickY                           int16
	RStickX, RStickY                           int16
	LPadForce, RPadForce                       uint16
}

func steamDeckStateFromC(state C.SteamDeckDeviceState) steamDeckState {
	return steamDeckStateFromPointer(unsafe.Pointer(&state))
}

func steamDeckStateFromPointer(pointer unsafe.Pointer) steamDeckState {
	state := (*C.SteamDeckDeviceState)(pointer)
	return steamDeckState{
		A: state.A != 0, X: state.X != 0, B: state.B != 0, Y: state.Y != 0,
		L1: state.L1 != 0, R1: state.R1 != 0,
		L2Digital: state.L2Digital != 0, R2Digital: state.R2Digital != 0,
		L5: state.L5 != 0, Menu: state.Menu != 0,
		Steam: state.Steam != 0, Options: state.Options != 0,
		DPadDown: state.DPadDown != 0, DPadLeft: state.DPadLeft != 0, DPadRight: state.DPadRight != 0, DPadUp: state.DPadUp != 0,
		L3:        state.L3 != 0,
		RPadTouch: state.RPadTouch != 0, LPadTouch: state.LPadTouch != 0, RPadPress: state.RPadPress != 0, LPadPress: state.LPadPress != 0,
		R5:          state.R5 != 0,
		R3:          state.R3 != 0,
		RStickTouch: state.RStickTouch != 0, LStickTouch: state.LStickTouch != 0,
		R4: state.R4 != 0, L4: state.L4 != 0,
		QuickAccess: state.QuickAccess != 0,
		LPadX:       int16(state.LPadX), LPadY: int16(state.LPadY),
		RPadX: int16(state.RPadX), RPadY: int16(state.RPadY),
		AccelX: int16(state.AccelX), AccelY: int16(state.AccelY), AccelZ: int16(state.AccelZ),
		Pitch: int16(state.Pitch), Yaw: int16(state.Yaw), Roll: int16(state.Roll),
		GyroQuatW: int16(state.GyroQuatW), GyroQuatX: int16(state.GyroQuatX), GyroQuatY: int16(state.GyroQuatY), GyroQuatZ: int16(state.GyroQuatZ),
		LTrigger: uint16(state.LTrigger), RTrigger: uint16(state.RTrigger),
		LStickX: int16(state.LStickX), LStickY: int16(state.LStickY),
		RStickX: int16(state.RStickX), RStickY: int16(state.RStickY),
		LPadForce: uint16(state.LPadForce), RPadForce: uint16(state.RPadForce),
	}
}

func steamDeckDeviceStateSize() uintptr { return unsafe.Sizeof(C.SteamDeckDeviceState{}) }

func steamDeckDeviceRemoveResultSize() uintptr {
	return unsafe.Sizeof(C.SteamDeckDeviceRemoveResult(0))
}

type steamDeckDeviceStateCOffsets struct {
	L2Digital, R2Digital, R3, QuickAccess uintptr
	LPadX, AccelX, LTrigger, LStickX      uintptr
	RStickX                               uintptr
	LPadForce, RPadForce                  uintptr
}

func steamDeckDeviceStateABIOffsets() steamDeckDeviceStateCOffsets {
	return steamDeckDeviceStateCOffsets{
		L2Digital:   unsafe.Offsetof(C.SteamDeckDeviceState{}.L2Digital),
		R2Digital:   unsafe.Offsetof(C.SteamDeckDeviceState{}.R2Digital),
		R3:          unsafe.Offsetof(C.SteamDeckDeviceState{}.R3),
		QuickAccess: unsafe.Offsetof(C.SteamDeckDeviceState{}.QuickAccess),
		LPadX:       unsafe.Offsetof(C.SteamDeckDeviceState{}.LPadX),
		AccelX:      unsafe.Offsetof(C.SteamDeckDeviceState{}.AccelX),
		LTrigger:    unsafe.Offsetof(C.SteamDeckDeviceState{}.LTrigger),
		LStickX:     unsafe.Offsetof(C.SteamDeckDeviceState{}.LStickX),
		RStickX:     unsafe.Offsetof(C.SteamDeckDeviceState{}.RStickX),
		LPadForce:   unsafe.Offsetof(C.SteamDeckDeviceState{}.LPadForce),
		RPadForce:   unsafe.Offsetof(C.SteamDeckDeviceState{}.RPadForce),
	}
}

func setSteamDeckDeviceState(handle uintptr, state steamDeckState) bool {
	return withActiveDeviceHandle(handle, func(dhw *deviceHandleWrapper) bool {
		d, ok := dhw.device.(*steamdeck.SteamDeck)
		if !ok {
			return false
		}
		d.UpdateInputState(steamDeckInputState(state))
		return true
	})
}

func steamDeckInputState(state steamDeckState) *steamdeck.InputState {
	return &steamdeck.InputState{
		A: state.A, X: state.X, B: state.B, Y: state.Y,
		L1: state.L1, R1: state.R1,
		L2Digital: state.L2Digital, R2Digital: state.R2Digital,
		L5: state.L5, Menu: state.Menu,
		Steam: state.Steam, Options: state.Options,
		DPadDown: state.DPadDown, DPadLeft: state.DPadLeft, DPadRight: state.DPadRight, DPadUp: state.DPadUp,
		L3:        state.L3,
		RPadTouch: state.RPadTouch, LPadTouch: state.LPadTouch, RPadPress: state.RPadPress, LPadPress: state.LPadPress,
		R5:          state.R5,
		R3:          state.R3,
		RStickTouch: state.RStickTouch, LStickTouch: state.LStickTouch,
		R4: state.R4, L4: state.L4,
		QuickAccess: state.QuickAccess,
		LPadX:       state.LPadX, LPadY: state.LPadY,
		RPadX: state.RPadX, RPadY: state.RPadY,
		AccelX: state.AccelX, AccelY: state.AccelY, AccelZ: state.AccelZ,
		Pitch: state.Pitch, Yaw: state.Yaw, Roll: state.Roll,
		GyroQuatW: state.GyroQuatW, GyroQuatX: state.GyroQuatX, GyroQuatY: state.GyroQuatY, GyroQuatZ: state.GyroQuatZ,
		LTrigger: state.LTrigger, RTrigger: state.RTrigger,
		LStickX: state.LStickX, LStickY: state.LStickY,
		RStickX: state.RStickX, RStickY: state.RStickY,
		LPadForce: state.LPadForce, RPadForce: state.RPadForce,
	}
}

// SetSteamDeckOutputCallback registers a raw normalized Steam Deck host-output callback.
// The supplied buffer is C-owned, valid only for the synchronous callback, and must be copied
// by callers that need to retain it. A nil callback clears the callback.
//
//export SetSteamDeckOutputCallback
func SetSteamDeckOutputCallback(handle C.SteamDeckDeviceHandle, cb C.SteamDeckOutputCallback) bool {
	if cb == nil {
		return setSteamDeckOutputCallback(uintptr(handle), nil)
	}
	return setSteamDeckOutputCallback(uintptr(handle), func(out steamdeck.OutputState) {
		invokeSteamDeckOutputCallback(cb, handle, out)
	})
}

func setSteamDeckOutputCallback(handle uintptr, callback func(steamdeck.OutputState)) bool {
	return withActiveDeviceHandle(handle, func(dhw *deviceHandleWrapper) bool {
		d, ok := dhw.device.(*steamdeck.SteamDeck)
		if !ok {
			return false
		}
		d.SetOutputCallback(callback)
		return true
	})
}

func invokeSteamDeckOutputCallback(cb C.SteamDeckOutputCallback, handle C.SteamDeckDeviceHandle, out steamdeck.OutputState) {
	length := out.Length()
	payload := C.CBytes(out.Data[:length])
	defer C.free(payload)
	C.viiper_call_steamdeck_output(cb, handle, (*C.uint8_t)(payload), C.uint32_t(length))
}

// RemoveSteamDeckDevice removes and finalizes only the Steam Deck logical device. The bus remains caller-owned.
//
//export RemoveSteamDeckDevice
func RemoveSteamDeckDevice(handle C.SteamDeckDeviceHandle) bool {
	return removeSteamDeckDevice(uintptr(handle))
}

func removeSteamDeckDevice(handle uintptr) bool {
	return removeSteamDeckDeviceResult(handle) == typedDeviceRemoveSuccess
}

// RemoveSteamDeckDeviceEx returns the classified Steam Deck logical-device removal result.
// The legacy RemoveSteamDeckDevice bool export remains available for compatibility.
//
//export RemoveSteamDeckDeviceEx
func RemoveSteamDeckDeviceEx(handle C.SteamDeckDeviceHandle) C.SteamDeckDeviceRemoveResult {
	return C.SteamDeckDeviceRemoveResult(removeSteamDeckDeviceResult(uintptr(handle)))
}

func removeSteamDeckDeviceResult(handle uintptr) typedDeviceRemoveResult {
	return removeTypedDeviceResult(handle, func(device any) bool { _, ok := device.(*steamdeck.SteamDeck); return ok })
}
