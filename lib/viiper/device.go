package main

/*
#include <stdbool.h>
#include <stdint.h>

typedef enum {
    VIIPER_ATTACH_SUCCESS = 0,
    VIIPER_ATTACH_RETRYABLE_FAILURE = 1,
    VIIPER_ATTACH_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_ATTACH_INVALID = 3
} USBDeviceAttachResult;

typedef enum {
    VIIPER_DETACH_SUCCESS = 0,
    VIIPER_DETACH_RETRYABLE_FAILURE = 1,
    VIIPER_DETACH_UNSAFE_OUTCOME_UNKNOWN = 2,
    VIIPER_DETACH_INVALID = 3
} USBDeviceDetachResult;
*/
import "C"

import "unsafe"

func attachResultCSize() uintptr { return unsafe.Sizeof(C.USBDeviceAttachResult(0)) }
func detachResultCSize() uintptr { return unsafe.Sizeof(C.USBDeviceDetachResult(0)) }

// GetUSBDeviceIdentity returns the logical VIIPER USB bus and device identity for a typed device handle.
// This does not indicate Windows attachment or PnP enumeration state.
//
//export GetUSBDeviceIdentity
func GetUSBDeviceIdentity(handle C.uintptr_t, outBusID *C.uint32_t, outDeviceID *C.uint32_t) C.bool {
	return C.bool(getUSBDeviceIdentity(uintptr(handle), (*uint32)(outBusID), (*uint32)(outDeviceID)))
}

func getUSBDeviceIdentity(handle uintptr, outBusID *uint32, outDeviceID *uint32) bool {
	if outBusID == nil || outDeviceID == nil {
		return false
	}
	dhw, ok := lookupDeviceIdentity(handle)
	if !ok {
		return false
	}
	*outBusID = dhw.exportMeta.BusID
	*outDeviceID = dhw.exportMeta.DevID
	return true
}

// AttachUSBDevice performs a tracked localhost attachment. It returns true only when the
// classified result is VIIPER_ATTACH_SUCCESS; every other classified result reports false here.
// Use AttachUSBDeviceEx to distinguish a safe retryable failure from an unsafe unknown outcome.
//
//export AttachUSBDevice
func AttachUSBDevice(handle C.uintptr_t) C.bool {
	return C.bool(attachUSBDevice(uintptr(handle)))
}

func attachUSBDevice(handle uintptr) bool {
	return attachUSBDeviceResult(handle) == deviceAttachSuccess
}

// AttachUSBDeviceEx performs the same tracked localhost attachment as AttachUSBDevice, but
// returns the classified USBDeviceAttachResult instead of a bare bool.
//
//export AttachUSBDeviceEx
func AttachUSBDeviceEx(handle C.uintptr_t) C.USBDeviceAttachResult {
	return C.USBDeviceAttachResult(attachUSBDeviceResult(uintptr(handle)))
}

// DetachUSBDevice releases a tracked localhost attachment. It returns true only when the
// classified result is VIIPER_DETACH_SUCCESS; every other classified result reports false here.
// Use DetachUSBDeviceEx to distinguish a safe retryable failure from an unsafe unknown outcome.
//
//export DetachUSBDevice
func DetachUSBDevice(handle C.uintptr_t) C.bool {
	return C.bool(detachUSBDevice(uintptr(handle)))
}

func detachUSBDevice(handle uintptr) bool {
	return detachUSBDeviceResult(handle) == deviceDetachSuccess
}

// DetachUSBDeviceEx performs the same tracked localhost detachment as DetachUSBDevice, but
// returns the classified USBDeviceDetachResult instead of a bare bool.
//
//export DetachUSBDeviceEx
func DetachUSBDeviceEx(handle C.uintptr_t) C.USBDeviceDetachResult {
	return C.USBDeviceDetachResult(detachUSBDeviceResult(uintptr(handle)))
}
