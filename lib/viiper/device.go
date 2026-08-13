package main

/*
#include <stdbool.h>
#include <stdint.h>
*/
import "C"

// GetUSBDeviceIdentity returns the logical VIIPER USB bus and device identity for a typed device handle.
// This does not indicate Windows attachment or PnP enumeration state.
//
//export GetUSBDeviceIdentity
func GetUSBDeviceIdentity(handle C.uintptr_t, outBusID *C.uint32_t, outDeviceID *C.uint32_t) bool {
	if outBusID == nil || outDeviceID == nil {
		return false
	}
	dhw, ok := lookupDeviceIdentity(uintptr(handle))
	if !ok {
		return false
	}
	*outBusID = C.uint32_t(dhw.exportMeta.BusID)
	*outDeviceID = C.uint32_t(dhw.exportMeta.DevID)
	return true
}
