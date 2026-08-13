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
