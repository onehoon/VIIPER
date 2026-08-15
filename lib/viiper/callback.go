package main

import (
	"slices"

	"github.com/Alia5/VIIPER/device/dualsense"
	"github.com/Alia5/VIIPER/device/dualshock4"
	"github.com/Alia5/VIIPER/device/keyboard"
	"github.com/Alia5/VIIPER/device/ns2pro"
	"github.com/Alia5/VIIPER/device/steamcontroller"
	"github.com/Alia5/VIIPER/device/steamdeck"
	"github.com/Alia5/VIIPER/device/xbox360"
)

// clearDeviceCallbackLocked clears the callback reference for a canonical
// callback-bearing device. The caller must hold the owning server's
// lifecycleMu. The optional observer is internal lifecycle instrumentation
// only; application callbacks are never invoked here.
func (hw *usbServerHandleWrapper) clearDeviceCallbackLocked(dhw *deviceHandleWrapper) {
	if dhw == nil {
		return
	}
	cleared := false
	switch d := dhw.device.(type) {
	case *steamcontroller.SteamController:
		d.SetOutputCallback(nil)
		cleared = true
	case *xbox360.Xbox360:
		d.SetRumbleCallback(nil)
		cleared = true
	case *dualsense.DualSense:
		d.SetOutputCallback(nil)
		cleared = true
	case *dualshock4.DualShock4:
		d.SetOutputCallback(nil)
		cleared = true
	case *keyboard.Keyboard:
		d.SetLEDCallback(nil)
		cleared = true
	case *ns2pro.NS2Pro:
		d.SetOutputCallback(nil)
		cleared = true
	case *steamdeck.SteamDeck:
		d.SetOutputCallback(nil)
		cleared = true
	}
	if cleared && hw.onCallbackCleared != nil {
		hw.onCallbackCleared(dhw)
	}
}

func (hw *usbServerHandleWrapper) clearBusCallbacksLocked(busID uint32) {
	for _, h := range slices.Clone(hw.deviceHandles[busID]) {
		hw.clearDeviceCallbackLocked(hw.deviceHandleRecords[h])
	}
}

func (hw *usbServerHandleWrapper) clearAllCallbacksLocked() {
	busIDs := make([]uint32, 0, len(hw.deviceHandles))
	for busID := range hw.deviceHandles {
		busIDs = append(busIDs, busID)
	}
	slices.Sort(busIDs)
	for _, busID := range busIDs {
		hw.clearBusCallbacksLocked(busID)
	}
}
