package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef uintptr_t USBServerHandle;

typedef uintptr_t DS4DeviceHandle;

#define DS4_BUTTON_SQUARE    0x0010u
#define DS4_BUTTON_CROSS     0x0020u
#define DS4_BUTTON_CIRCLE    0x0040u
#define DS4_BUTTON_TRIANGLE  0x0080u
#define DS4_BUTTON_L1        0x0100u
#define DS4_BUTTON_R1        0x0200u
#define DS4_BUTTON_L2        0x0400u
#define DS4_BUTTON_R2        0x0800u
#define DS4_BUTTON_SHARE     0x1000u
#define DS4_BUTTON_OPTIONS   0x2000u
#define DS4_BUTTON_L3        0x4000u
#define DS4_BUTTON_R3        0x8000u
#define DS4_BUTTON_PS        0x0001u
#define DS4_BUTTON_TOUCHPAD  0x0002u

#define DS4_DPAD_UP        0x00u
#define DS4_DPAD_UP_RIGHT  0x01u
#define DS4_DPAD_RIGHT     0x02u
#define DS4_DPAD_DOWN_RIGHT 0x03u
#define DS4_DPAD_DOWN      0x04u
#define DS4_DPAD_DOWN_LEFT 0x05u
#define DS4_DPAD_LEFT      0x06u
#define DS4_DPAD_UP_LEFT   0x07u
#define DS4_DPAD_NEUTRAL   0x08u

typedef struct {
	int8_t   LX;
	int8_t   LY;
	int8_t   RX;
	int8_t   RY;
	uint16_t Buttons;
	uint8_t  DPad;
	uint8_t  L2;
	uint8_t  R2;
	uint16_t Touch1X;
	uint16_t Touch1Y;
	uint8_t  Touch1Active;
	uint16_t Touch2X;
	uint16_t Touch2Y;
	uint8_t  Touch2Active;
	int16_t  GyroX;
	int16_t  GyroY;
	int16_t  GyroZ;
	int16_t  AccelX;
	int16_t  AccelY;
	int16_t  AccelZ;
} DS4DeviceState;

typedef struct {
	const char* SerialNumber;       // NULL = use default
	const char* Board;              // NULL = use default
	uint8_t     BatteryStatus;      // 0 = use default
	double      TemperatureCelsius; // 0 = use default
	double      BatteryVoltage;     // 0 = use default
} DS4MetaState;

typedef void (*DS4OutputCallback)(DS4DeviceHandle handle, uint8_t rumbleSmall, uint8_t rumbleLarge, uint8_t ledRed, uint8_t ledGreen, uint8_t ledBlue, uint8_t flashOn, uint8_t flashOff);

static void viiper_call_ds4_output(DS4OutputCallback fn, DS4DeviceHandle handle, uint8_t rumbleSmall, uint8_t rumbleLarge, uint8_t ledRed, uint8_t ledGreen, uint8_t ledBlue, uint8_t flashOn, uint8_t flashOff) {
	fn(handle, rumbleSmall, rumbleLarge, ledRed, ledGreen, ledBlue, flashOn, flashOff);
}

*/
import "C"
import (
	"encoding/json"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/device/dualshock4"
)

// CreateDS4Device creates a new DualShock 4 device on the bus with the given ID on the server associated with the given handle.
// @param serverHandle Handle to the USB server.
// @param outDeviceHandle Output parameter for the created device handle.
// @param busID ID of the bus to add the device to.
// @param autoAttachLocalhost If true, the device will be automatically attached to a USBIP-Client/Driver running on THIS machine.
// @param idVendor Optional USB vendor ID (0 = default).
// @param idProduct Optional USB product ID (0 = default).
// @param meta Optional pointer to initial device metadata. Pass NULL to use defaults.
//
//export CreateDS4Device
func CreateDS4Device(
	serverHandle C.USBServerHandle,
	outDeviceHandle *C.DS4DeviceHandle,
	busID uint32,
	autoAttachLocalhost bool,
	idVendor uint16,
	idProduct uint16,
	meta *C.DS4MetaState,
) bool {
	if outDeviceHandle == nil {
		return false
	}
	shw, ok := lookupServerHandle(uintptr(serverHandle))
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
	if meta != nil {
		goMeta := dualshock4.MetaState{
			SerialNumber:       goStringOrEmpty(meta.SerialNumber),
			Board:              goStringOrEmpty(meta.Board),
			BatteryStatus:      uint8(meta.BatteryStatus),
			TemperatureCelsius: float64(meta.TemperatureCelsius),
			BatteryVoltage:     float64(meta.BatteryVoltage),
		}
		b, err := json.Marshal(goMeta)
		if err != nil {
			return false
		}
		opts.DeviceSpecific = string(b)
	}

	d, err := dualshock4.New(opts)
	if err != nil {
		return false
	}
	shw.lifecycleMu.Lock()
	defer shw.lifecycleMu.Unlock()
	h, ok := shw.createDeviceLocked(busID, d, autoAttachLocalhost)
	if !ok {
		return false
	}
	*outDeviceHandle = C.DS4DeviceHandle(h)
	return true
}

// SetDS4DeviceState updates the input state of the DualShock 4 device associated with the given handle.
// @param handle Handle to the DS4 device.
// @param state New input state to set on the device.
//
//export SetDS4DeviceState
func SetDS4DeviceState(handle C.DS4DeviceHandle, state C.DS4DeviceState) bool {
	return withActiveDeviceHandle(uintptr(handle), func(dhw *deviceHandleWrapper) bool {
		ds4device, ok := dhw.device.(*dualshock4.DualShock4)
		if !ok {
			return false
		}
		s := &dualshock4.InputState{
			LX:           int8(state.LX),
			LY:           int8(state.LY),
			RX:           int8(state.RX),
			RY:           int8(state.RY),
			Buttons:      uint16(state.Buttons),
			DPad:         uint8(state.DPad),
			L2:           uint8(state.L2),
			R2:           uint8(state.R2),
			Touch1X:      uint16(state.Touch1X),
			Touch1Y:      uint16(state.Touch1Y),
			Touch1Active: state.Touch1Active != 0,
			Touch2X:      uint16(state.Touch2X),
			Touch2Y:      uint16(state.Touch2Y),
			Touch2Active: state.Touch2Active != 0,
			GyroX:        int16(state.GyroX),
			GyroY:        int16(state.GyroY),
			GyroZ:        int16(state.GyroZ),
			AccelX:       int16(state.AccelX),
			AccelY:       int16(state.AccelY),
			AccelZ:       int16(state.AccelZ),
		}
		ds4device.UpdateInputState(s)
		return true
	})
}

// SetDS4OutputCallback sets a callback to be invoked when the host sends output (rumble/LED) commands to the device.
// @param handle Handle to the DS4 device.
// @param callback Callback receiving rumbleSmall, rumbleLarge, ledRed, ledGreen, ledBlue, flashOn, flashOff. Pass NULL to clear.
//
//export SetDS4OutputCallback
func SetDS4OutputCallback(handle C.DS4DeviceHandle, cb C.DS4OutputCallback) bool {
	return withActiveDeviceHandle(uintptr(handle), func(dhw *deviceHandleWrapper) bool {
		ds4device, ok := dhw.device.(*dualshock4.DualShock4)
		if !ok {
			return false
		}
		if cb == nil {
			ds4device.SetOutputCallback(nil)
			return true
		}
		ds4device.SetOutputCallback(func(out dualshock4.OutputState) {
			C.viiper_call_ds4_output(cb, handle,
				C.uint8_t(out.RumbleSmall),
				C.uint8_t(out.RumbleLarge),
				C.uint8_t(out.LedRed),
				C.uint8_t(out.LedGreen),
				C.uint8_t(out.LedBlue),
				C.uint8_t(out.FlashOn),
				C.uint8_t(out.FlashOff),
			)
		})
		return true
	})
}

// RemoveDS4Device removes the DualShock 4 device associated with the given handle from the server.
// @param handle Handle to the DS4 device to remove.
//
//export RemoveDS4Device
func RemoveDS4Device(handle C.DS4DeviceHandle) bool {
	return removeDS4Device(uintptr(handle))
}

func removeDS4Device(handle uintptr) bool {
	return removeTypedDevice(handle, func(device any) bool { _, ok := device.(*dualshock4.DualShock4); return ok })
}
