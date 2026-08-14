package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef uintptr_t USBServerHandle;
*/
import "C"
import (
	"slices"

	"github.com/Alia5/VIIPER/internal/server/usb"
	viiperusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/virtualbus"
)

// CreateUSBBus creates a new USB bus on the server associated with the given handle.
// @param handle Handle to the USB server.
// @param busID ID of the bus to create. If 0 or NULL, the server will assign the next free bus ID.
//
//export CreateUSBBus
func CreateUSBBus(handle C.USBServerHandle, busID *uint32) bool {
	hw, ok := lookupServerHandle(uintptr(handle))
	if !ok {
		return false
	}
	hw.lifecycleMu.Lock()
	defer hw.lifecycleMu.Unlock()
	return hw.createBusLocked(busID)
}

func (hw *usbServerHandleWrapper) createBusLocked(busID *uint32) bool {
	if hw.state != serverActive {
		hw.warnMutationRejectedLocked("CreateUSBBus")
		return false
	}
	if busID == nil {
		id := hw.s.NextFreeBusID()
		busID = &id
	} else if *busID == 0 {
		*busID = hw.s.NextFreeBusID()
	}

	b, err := virtualbus.NewWithBusID(*busID)
	if err != nil {
		return false
	}
	if err := hw.s.AddBus(b); err != nil {
		_ = b.Close()
		return false
	}
	hw.mtx.Lock()
	defer hw.mtx.Unlock()
	hw.deviceHandles[*busID] = make([]deviceHandle, 0)

	return true
}

// RemoveUSBBus removes the USB bus with the given ID from the server associated with the given handle.
// Automatically removes devices associated with the bus.
// @param handle Handle to the USB server.
// @param busID ID of the bus to remove.
//
//export RemoveUSBBus
func RemoveUSBBus(handle C.USBServerHandle, busID uint32) bool {
	hw, ok := lookupServerHandle(uintptr(handle))
	if !ok {
		return false
	}
	hw.lifecycleMu.Lock()
	if hw.state != serverActive {
		hw.warnMutationRejectedLocked("RemoveUSBBus")
		hw.lifecycleMu.Unlock()
		return false
	}
	result := hw.removeBusLockedWithDrains(busID)
	hw.lifecycleMu.Unlock()
	waitTransportDrains(result.drains)
	return result.ok
}

func (hw *usbServerHandleWrapper) removeBusLocked(busID uint32) bool {
	if hw.state != serverActive {
		return false
	}
	return hw.removeBusLockedWithDrains(busID).ok
}

func (hw *usbServerHandleWrapper) removeBusLockedWithDrains(busID uint32) transportTeardownResult {
	if hw.state != serverActive && hw.state != serverClosing {
		hw.warnMutationRejectedLocked("RemoveUSBBus")
		return transportTeardownResult{}
	}
	if hw.s.GetBus(busID) == nil {
		return transportTeardownResult{}
	}
	if !hw.preflightBusDevicesLocked(busID) {
		if hw.hasUnknownAttachmentLocked() {
			hw.state = serverCloseFailed
		}
		return transportTeardownResult{}
	}
	if !hw.logicalCloseInProgress {
		hw.clearBusCallbacksLocked(busID)
	}
	if !hw.detachBusDevicesLocked(busID) {
		if hw.hasUnknownAttachmentLocked() {
			hw.state = serverCloseFailed
		}
		return transportTeardownResult{}
	}
	var drains []*usb.TransportDrain
	var devices []viiperusb.Device
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		if dhw := hw.deviceHandleRecords[h]; dhw != nil {
			dev := dhw.device.(viiperusb.Device)
			devices = append(devices, dev)
			drains = append(drains, hw.s.BeginDeviceDrain(dev))
		}
	}
	if err := hw.ops.removeBus(hw.s, busID); err != nil {
		if hw.s.GetBus(busID) == nil {
			hw.finalizeBusLocked(busID)
			for _, dev := range devices {
				hw.s.ForgetDeviceTransport(dev)
			}
			return transportTeardownResult{ok: true, drains: drains}
		}
		return transportTeardownResult{drains: drains}
	}
	hw.finalizeBusLocked(busID)
	for _, dev := range devices {
		hw.s.ForgetDeviceTransport(dev)
	}

	return transportTeardownResult{ok: true, drains: drains}
}
