package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef uintptr_t USBServerHandle;
*/
import "C"
import (
	"fmt"
	"slices"
	"time"

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
	opStart := time.Now()
	hw, ok := lookupServerHandle(uintptr(handle))
	if !ok {
		logTeardownDiagnostic(invalidHandleLoggerFunc(), teardownDiagnostic{operation: "RemoveUSBBus", phase: "preflight", result: "invalid"}, time.Since(opStart).Microseconds())
		return false
	}
	hw.lifecycleMu.Lock()
	stateBefore := hw.state
	if hw.state != serverActive {
		stateAfter := hw.state
		hw.lifecycleMu.Unlock()
		logTeardownDiagnostic(hw.logger, teardownDiagnostic{operation: "RemoveUSBBus", phase: "preflight", result: "invalid", busID: busID, serverStateBefore: stateBefore, serverStateAfter: stateAfter}, time.Since(opStart).Microseconds())
		return false
	}
	result := hw.removeBusLockedWithDrains(busID)
	hw.lifecycleMu.Unlock()
	waitTransportDrains(result.drains)
	d := result.diagnostic
	d.operation = "RemoveUSBBus"
	if d.serverStateBefore == 0 && stateBefore != serverActive {
		d.serverStateBefore = stateBefore
	}
	logTeardownDiagnostic(hw.logger, d, time.Since(opStart).Microseconds())
	return result.ok
}

func (hw *usbServerHandleWrapper) removeBusLocked(busID uint32) bool {
	if hw.state != serverActive {
		return false
	}
	return hw.removeBusLockedWithDrains(busID).ok
}

func (hw *usbServerHandleWrapper) removeBusLockedWithDrains(busID uint32) transportTeardownResult {
	d := teardownDiagnostic{operation: "RemoveUSBBus", phase: "preflight", result: "invalid", busID: busID, serverStateBefore: hw.state, serverStateAfter: hw.state, remainingBusCount: len(hw.s.ListBuses()), serverStatePresent: true}
	if hw.state != serverActive && hw.state != serverClosing {
		hw.warnMutationRejectedLocked("RemoveUSBBus")
		return transportTeardownResult{diagnostic: d}
	}
	if hw.s.GetBus(busID) == nil {
		return transportTeardownResult{diagnostic: d}
	}
	d.deviceCountBefore = len(hw.deviceHandles[busID])
	if failed, ok := hw.preflightBusDevicesLocked(busID); !ok {
		if failed != nil {
			d.deviceID = fmt.Sprintf("%d", failed.exportMeta.DevID)
			d.attachmentStateBefore = attachmentStateName(failed.attachment.state)
			d.attachmentStateAfter = d.attachmentStateBefore
			d.attachmentBackend = failed.attachment.attachment.Backend
			d.importPort = failed.attachment.attachment.Port
		}
		if hw.hasUnknownAttachmentLocked() {
			hw.state = serverCloseFailed
			d.result = "unsafe-outcome-unknown"
		}
		d.serverStateAfter = hw.state
		return transportTeardownResult{diagnostic: d}
	}
	if !hw.logicalCloseInProgress {
		hw.clearBusCallbacksLocked(busID)
	}
	if failed, ok, backendCalled := hw.detachBusDevicesLocked(busID); !ok {
		if failed != nil {
			d.deviceID = fmt.Sprintf("%d", failed.exportMeta.DevID)
			d.attachmentStateBefore = attachmentStateName(failed.attachment.state)
			d.attachmentStateAfter = d.attachmentStateBefore
			d.attachmentBackend = failed.attachment.attachment.Backend
			d.importPort = failed.attachment.attachment.Port
		}
		d.detachBackendCalled, d.detachBackendCalledPresent = backendCalled, true
		if hw.hasUnknownAttachmentLocked() {
			hw.state = serverCloseFailed
			d.result = "unsafe-outcome-unknown"
		} else {
			d.result = "retryable-failure"
		}
		d.phase = "detach"
		d.serverStateAfter = hw.state
		return transportTeardownResult{diagnostic: d}
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
		d.backendReportedError = true
		d.error = err.Error()
		if hw.s.GetBus(busID) == nil {
			hw.finalizeBusLocked(busID)
			for _, dev := range devices {
				hw.s.ForgetDeviceTransport(dev)
			}
			d.phase, d.result, d.busPresentAfter = "complete", "success", false
			d.busPresentAfter = false
			d.remainingBusCount = len(hw.s.ListBuses())
			return transportTeardownResult{ok: true, drains: drains, diagnostic: d}
		}
		d.phase, d.result, d.busPresentAfter = "bus-remove", "retryable-failure", true
		d.serverStateAfter = hw.state
		return transportTeardownResult{drains: drains, diagnostic: d}
	}
	hw.finalizeBusLocked(busID)
	for _, dev := range devices {
		hw.s.ForgetDeviceTransport(dev)
	}

	d.phase, d.result = "complete", "success"
	d.remainingBusCount = len(hw.s.ListBuses())
	return transportTeardownResult{ok: true, drains: drains, diagnostic: d}
}
