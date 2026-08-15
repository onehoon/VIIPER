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

typedef enum {
    VIIPER_ATTACHMENT_DETACHED = 0,
    VIIPER_ATTACHMENT_ATTACHED = 1,
    VIIPER_ATTACHMENT_OUTCOME_UNKNOWN = 2
} USBDeviceAttachmentState;
*/
import "C"

import "unsafe"

func attachResultCSize() uintptr          { return unsafe.Sizeof(C.USBDeviceAttachResult(0)) }
func detachResultCSize() uintptr          { return unsafe.Sizeof(C.USBDeviceDetachResult(0)) }
func attachmentStateResultCSize() uintptr { return unsafe.Sizeof(C.USBDeviceAttachmentState(0)) }

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

// GetUSBDeviceAttachmentState reports VIIPER's tracked localhost attachment ownership state for a
// typed device handle: DETACHED, ATTACHED, or OUTCOME_UNKNOWN. This is VIIPER's own attachment
// bookkeeping only -- it does not report Windows PnP enumeration, HID interface readiness,
// XInput readiness, or Steam device discovery, and an ATTACHED result does not mean an
// application may skip its own exact PnP stabilization/ownership checks. This is a read-only
// diagnostic query: it never invokes the attach/detach backend and never mutates attachment
// state, the stored token, or server lifecycle. Like GetUSBDeviceIdentity, the query succeeds
// for a still-authoritative handle whether the owning server is active or close-failed; it fails
// for a NULL outState, an invalid/stale handle, or any other server lifecycle.
//
//export GetUSBDeviceAttachmentState
func GetUSBDeviceAttachmentState(handle C.uintptr_t, outState *C.USBDeviceAttachmentState) C.bool {
	return C.bool(getUSBDeviceAttachmentState(uintptr(handle), outState))
}

func getUSBDeviceAttachmentState(handle uintptr, outState *C.USBDeviceAttachmentState) bool {
	if outState == nil {
		return false
	}
	state, ok := queryDeviceAttachmentState(handle)
	if !ok {
		return false
	}
	switch state {
	case deviceAttachmentQueryDetached:
		*outState = C.VIIPER_ATTACHMENT_DETACHED
	case deviceAttachmentQueryAttached:
		*outState = C.VIIPER_ATTACHMENT_ATTACHED
	case deviceAttachmentQueryOutcomeUnknown:
		*outState = C.VIIPER_ATTACHMENT_OUTCOME_UNKNOWN
	default:
		return false
	}
	return true
}

// cAttachmentStateValue is a plain-Go mirror of the real C.USBDeviceAttachmentState numeric
// values, for use by _test.go files. Go's cgo does not permit "import C" in test files, so tests
// that need to exercise the real getUSBDeviceAttachmentState call (rather than the private
// queryDeviceAttachmentState it wraps) go through callGetUSBDeviceAttachmentStateForTest below,
// which does the actual C-typed call in this cgo-enabled file and returns only this plain type.
type cAttachmentStateValue uint32

const (
	cAttachmentStateDetached       cAttachmentStateValue = cAttachmentStateValue(C.VIIPER_ATTACHMENT_DETACHED)
	cAttachmentStateAttached       cAttachmentStateValue = cAttachmentStateValue(C.VIIPER_ATTACHMENT_ATTACHED)
	cAttachmentStateOutcomeUnknown cAttachmentStateValue = cAttachmentStateValue(C.VIIPER_ATTACHMENT_OUTCOME_UNKNOWN)
	cAttachmentStateSentinel       cAttachmentStateValue = 0xFF
)

// callGetUSBDeviceAttachmentStateForTest calls the real getUSBDeviceAttachmentState with a real
// *C.USBDeviceAttachmentState seeded with the sentinel value, and reports both the resulting
// value and whether the call succeeded -- so a test can assert the sentinel survives untouched on
// every failure path, exactly as fork-api.md documents.
func callGetUSBDeviceAttachmentStateForTest(handle uintptr) (cAttachmentStateValue, bool) {
	out := C.USBDeviceAttachmentState(cAttachmentStateSentinel)
	ok := getUSBDeviceAttachmentState(handle, &out)
	return cAttachmentStateValue(out), ok
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
